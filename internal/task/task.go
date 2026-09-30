// Package task owns Task state and its transitions.
//
// Every transition is a conditional update that only succeeds from the
// expected prior state, so the first outcome recorded is final and later
// attempts are no-ops. Each transition moves money through the Ledger inside
// the same database transaction.
package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/yuchia329/overpass/internal/ledger"
	"github.com/yuchia329/overpass/internal/secret"
)

// Schema is the Task lifecycle's part of the database schema.
const Schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id                 TEXT PRIMARY KEY,
	customer_id        TEXT NOT NULL REFERENCES customers(id),
	page_url           TEXT NOT NULL,
	state              TEXT NOT NULL CHECK (state IN ('pending', 'claimed', 'solved', 'expired', 'failed')),
	session_token_hash TEXT NOT NULL UNIQUE,
	created_at         INTEGER NOT NULL,
	claim_deadline     INTEGER NOT NULL,
	solver_wallet      TEXT,
	solve_deadline     INTEGER,
	ended_at           INTEGER
);
CREATE INDEX IF NOT EXISTS tasks_customer ON tasks (customer_id, created_at);
`

// addedColumns were added to the tasks table after it was first created.
// CREATE TABLE IF NOT EXISTS leaves an older table as it was, so Migrate adds them.
var addedColumns = []struct{ name, decl string }{
	{"solver_wallet", "TEXT"},
	{"solve_deadline", "INTEGER"},
}

// Migrate brings a tasks table created by an earlier version up to Schema.
func Migrate(db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('tasks') WHERE name = ?`, c.name).Scan(&n); err != nil {
			return fmt.Errorf("migrate tasks: %w", err)
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("migrate tasks: %w", err)
		}
	}
	return nil
}

type State string

const (
	Pending State = "pending"
	Claimed State = "claimed"
	Solved  State = "solved"
	Expired State = "expired"
	Failed  State = "failed"
)

type Config struct {
	ClaimWindow time.Duration
	SolveWindow time.Duration
	Price       int64 // USDC base units held per Task
}

// Event reports a committed change to a Task. Events for a Task are
// delivered in the order its changes were committed.
type Event struct {
	TaskID        string
	State         State
	PageURL       string
	CreatedAt     time.Time
	SolverWallet  string    // set once the Task is claimed
	SolveDeadline time.Time // set on the Claimed Event
	Reason        Reason    // why a Task Failed
	Earning, Fee  int64     // the captured Hold's split, set on the Solved Event
}

// Reason says why a Task Failed.
type Reason string

const (
	SolveWindowPassed  Reason = "solve_window"
	GaveUp             Reason = "gave_up"
	BridgeDisconnected Reason = "bridge_disconnected"
)

var (
	ErrUnknownTask    = errors.New("unknown task")
	ErrExpired        = errors.New("task expired")
	ErrAlreadyClaimed = errors.New("task already claimed")
	ErrNotYourClaim   = errors.New("task is not claimed by this solver")
	ErrHoldingClaim   = errors.New("solver already holds a claim")
	ErrBadToken       = errors.New("session token does not match task")
)

// Created is what the Agent gets back from creating a Task.
type Created struct {
	ID            string
	SessionToken  string // shown once; only its hash is stored
	ClaimDeadline time.Time
}

// Lifecycle creates Tasks and drives their transitions and timers.
type Lifecycle struct {
	db     *sql.DB
	cfg    Config
	notify func(Event)

	// commitMu spans each change's transaction and its notify, so Events
	// are delivered in commit order. SQLite serializes the writes anyway.
	commitMu sync.Mutex

	mu       sync.Mutex
	closed   bool
	timers   map[string]*time.Timer
	inflight sync.WaitGroup
}

// New returns a Lifecycle that calls notify after every committed change.
// notify must not block or call back into the Lifecycle.
func New(db *sql.DB, cfg Config, notify func(Event)) *Lifecycle {
	return &Lifecycle{db: db, cfg: cfg, notify: notify, timers: map[string]*time.Timer{}}
}

// Create places a Hold of one Price and queues a new Pending Task.
// It returns *ledger.InsufficientError when available Balance is below the Price.
func (l *Lifecycle) Create(ctx context.Context, customerID, pageURL string) (Created, error) {
	now := time.Now()
	c := Created{
		ID:            secret.New("tsk_"),
		SessionToken:  secret.New("st_"),
		ClaimDeadline: now.Add(l.cfg.ClaimWindow),
	}
	_, err := l.commit(ctx, func(tx *sql.Tx) (*Event, error) {
		// Insert first: the Hold references the Task id.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, customer_id, page_url, state, session_token_hash, created_at, claim_deadline)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ID, customerID, pageURL, Pending, secret.Hash(c.SessionToken), now.UnixMilli(), c.ClaimDeadline.UnixMilli()); err != nil {
			return nil, fmt.Errorf("insert task: %w", err)
		}
		return &Event{TaskID: c.ID, State: Pending, PageURL: pageURL, CreatedAt: now},
			ledger.Hold(ctx, tx, customerID, c.ID, l.cfg.Price)
	})
	if err != nil {
		return Created{}, err
	}
	l.schedule(c.ID, time.Until(c.ClaimDeadline), l.Expire)
	return c, nil
}

// Claim gives a Pending Task to the Solver with wallet and starts its solve
// window. Only the first Claim succeeds; a Task is never requeued, so any
// later Claim returns ErrAlreadyClaimed. A Solver holds at most one Claim at
// a time: until their claimed Task ends, Claim returns ErrHoldingClaim.
func (l *Lifecycle) Claim(ctx context.Context, id, wallet string) (solveDeadline time.Time, err error) {
	won, err := l.commit(ctx, func(tx *sql.Tx) (*Event, error) {
		// Measured from the moment the Claim is recorded.
		now := time.Now()
		solveDeadline = now.Add(l.cfg.SolveWindow)
		e := &Event{TaskID: id, State: Claimed, SolverWallet: wallet, SolveDeadline: solveDeadline}
		var created int64
		// The claim deadline is checked here too, in case the Expire timer runs late.
		err := tx.QueryRowContext(ctx,
			`UPDATE tasks SET state = ?1, solver_wallet = ?2, solve_deadline = ?3
			 WHERE id = ?4 AND state = ?5 AND claim_deadline > ?6
			   AND NOT EXISTS (SELECT 1 FROM tasks WHERE solver_wallet = ?2 AND state = ?1)
			 RETURNING page_url, created_at`,
			Claimed, wallet, solveDeadline.UnixMilli(), id, Pending, now.UnixMilli()).Scan(&e.PageURL, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, claimRefusal(ctx, tx, id, now)
		}
		if err != nil {
			return nil, fmt.Errorf("claim: %w", err)
		}
		e.CreatedAt = time.UnixMilli(created)
		return e, nil
	})
	if err != nil || !won {
		return time.Time{}, err
	}
	l.schedule(id, time.Until(solveDeadline), l.failOverdue)
	return solveDeadline, nil
}

// claimRefusal explains why a Task could not be claimed at now.
func claimRefusal(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	var state State
	var claimDeadline int64
	err := tx.QueryRowContext(ctx, `SELECT state, claim_deadline FROM tasks WHERE id = ?`, id).Scan(&state, &claimDeadline)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrUnknownTask
	case err != nil:
		return fmt.Errorf("claim: %w", err)
	case state == Expired:
		return ErrExpired
	case state != Pending:
		return ErrAlreadyClaimed
	case claimDeadline <= now.UnixMilli(): // about to Expire
		return ErrExpired
	}
	return ErrHoldingClaim // the Task is claimable, but the Solver is busy
}

// failOverdue Fails a claimed Task whose solve window has passed.
func (l *Lifecycle) failOverdue(ctx context.Context, id string) (bool, error) {
	return l.transition(ctx, id, Claimed, Failed, SolveWindowPassed, "", release(ctx))
}

// GiveUp Fails a Task at once for the Solver holding its Claim and releases
// its Hold. It returns ErrNotYourClaim if wallet does not hold a live Claim.
func (l *Lifecycle) GiveUp(ctx context.Context, id, wallet string) error {
	won, err := l.transition(ctx, id, Claimed, Failed, GaveUp, wallet, release(ctx))
	if err == nil && !won {
		return ErrNotYourClaim
	}
	return err
}

// Expire moves a Pending Task to Expired and releases its Hold.
// It reports whether this call recorded the outcome.
func (l *Lifecycle) Expire(ctx context.Context, id string) (bool, error) {
	return l.transition(ctx, id, Pending, Expired, "", "", release(ctx))
}

// Solve records that the Bridge's cleared check passed on a claimed Task and
// captures its Hold, split into the Solver's Earning and the Fee. It reports
// whether this call recorded the outcome: a Task that already ended, or was
// never claimed, is left as it is.
func (l *Lifecycle) Solve(ctx context.Context, id string) (bool, error) {
	return l.transition(ctx, id, Claimed, Solved, "", "", func(tx *sql.Tx, e *Event) error {
		var err error
		e.Earning, e.Fee, err = ledger.Capture(ctx, tx, id, e.SolverWallet)
		return err
	})
}

// BridgeLost Fails a Pending or claimed Task whose Bridge disconnected and
// releases its Hold, so no Solver claims or keeps working on a dead Session.
func (l *Lifecycle) BridgeLost(ctx context.Context, id string) (bool, error) {
	// Pending first: a Task never returns to Pending, so if it was claimed
	// in between, the second attempt still sees it.
	won, err := l.transition(ctx, id, Pending, Failed, BridgeDisconnected, "", release(ctx))
	if err != nil || won {
		return won, err
	}
	return l.transition(ctx, id, Claimed, Failed, BridgeDisconnected, "", release(ctx))
}

// release returns an ended Task's Hold to the Customer in full.
func release(ctx context.Context) func(*sql.Tx, *Event) error {
	return func(tx *sql.Tx, e *Event) error { return ledger.Release(ctx, tx, e.TaskID) }
}

// Resume re-arms timers after a restart, ending overdue Tasks at once, and
// reports every Pending Task so the Queue is rebuilt.
func (l *Lifecycle) Resume(ctx context.Context) error {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, state, page_url, created_at, claim_deadline, solve_deadline FROM tasks
		 WHERE state IN (?, ?) ORDER BY created_at`, Pending, Claimed)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	type live struct {
		Event
		claimDeadline, solveDeadline int64
	}
	var ls []live
	for rows.Next() {
		var t live
		var created int64
		var solveDeadline sql.NullInt64
		if err := rows.Scan(&t.TaskID, &t.State, &t.PageURL, &created, &t.claimDeadline, &solveDeadline); err != nil {
			rows.Close()
			return fmt.Errorf("resume: %w", err)
		}
		t.CreatedAt = time.UnixMilli(created)
		t.solveDeadline = solveDeadline.Int64
		ls = append(ls, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	for _, t := range ls {
		if t.State == Claimed {
			l.schedule(t.TaskID, time.Until(time.UnixMilli(t.solveDeadline)), l.failOverdue)
			continue
		}
		l.commitMu.Lock()
		l.notify(t.Event)
		l.commitMu.Unlock()
		l.schedule(t.TaskID, time.Until(time.UnixMilli(t.claimDeadline)), l.Expire)
	}
	return nil
}

// Session checks that token is the Task's session token and returns where
// the Task stands, as an Event. It returns ErrBadToken for an unknown Task or
// a wrong token, so a token can never reach another Task's Session.
func (l *Lifecycle) Session(ctx context.Context, id, token string) (Event, error) {
	e := Event{TaskID: id}
	var created int64
	var solver sql.NullString
	var solveDeadline sql.NullInt64
	err := l.db.QueryRowContext(ctx,
		`SELECT state, page_url, created_at, solver_wallet, solve_deadline FROM tasks WHERE id = ? AND session_token_hash = ?`,
		id, secret.Hash(token)).Scan(&e.State, &e.PageURL, &created, &solver, &solveDeadline)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrBadToken
	}
	if err != nil {
		return Event{}, fmt.Errorf("session: %w", err)
	}
	e.CreatedAt = time.UnixMilli(created)
	e.SolverWallet = solver.String
	if solveDeadline.Valid {
		e.SolveDeadline = time.UnixMilli(solveDeadline.Int64)
	}
	return e, nil
}

// ClaimOf returns the Task whose live Claim wallet holds, as a Claimed Event,
// so a Solver who reconnects can resume its Session. ok is false if the
// Solver holds no Claim or its solve window has passed.
func (l *Lifecycle) ClaimOf(ctx context.Context, wallet string) (e Event, ok bool, err error) {
	e = Event{State: Claimed, SolverWallet: wallet}
	var created, solveDeadline int64
	err = l.db.QueryRowContext(ctx,
		`SELECT id, page_url, created_at, solve_deadline FROM tasks
		 WHERE solver_wallet = ? AND state = ? AND solve_deadline > ?`,
		wallet, Claimed, time.Now().UnixMilli()).Scan(&e.TaskID, &e.PageURL, &created, &solveDeadline)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("claim of: %w", err)
	}
	e.CreatedAt = time.UnixMilli(created)
	e.SolveDeadline = time.UnixMilli(solveDeadline)
	return e, true, nil
}

// Summary is a Task as shown in a Customer's history.
type Summary struct {
	ID        string
	PageURL   string
	State     State
	CreatedAt time.Time
}

// Recent lists a Customer's most recent Tasks, newest first.
func (l *Lifecycle) Recent(ctx context.Context, customerID string, limit int) ([]Summary, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, page_url, state, created_at FROM tasks WHERE customer_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		customerID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent tasks: %w", err)
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		var created int64
		if err := rows.Scan(&s.ID, &s.PageURL, &s.State, &created); err != nil {
			return nil, fmt.Errorf("recent tasks: %w", err)
		}
		s.CreatedAt = time.UnixMilli(created)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Close stops all timers and waits for any running transition to finish.
func (l *Lifecycle) Close() {
	l.mu.Lock()
	l.closed = true
	for id, t := range l.timers {
		t.Stop()
		delete(l.timers, id)
	}
	l.mu.Unlock()
	l.inflight.Wait()
}

// transition moves a Task from one state to another only if it is still in
// `from` (and, when solver is set, claimed by solver), running money in the
// same transaction. money may add to the Event. It reports whether it won.
func (l *Lifecycle) transition(ctx context.Context, id string, from, to State, reason Reason, solver string, money func(*sql.Tx, *Event) error) (bool, error) {
	return l.commit(ctx, func(tx *sql.Tx) (*Event, error) {
		e := &Event{TaskID: id, State: to, Reason: reason}
		var created int64
		var claimant sql.NullString
		err := tx.QueryRowContext(ctx,
			`UPDATE tasks SET state = ?1, ended_at = ?2 WHERE id = ?3 AND state = ?4 AND (?5 = '' OR solver_wallet = ?5)
			 RETURNING page_url, created_at, solver_wallet`,
			to, time.Now().UnixMilli(), id, from, solver).Scan(&e.PageURL, &created, &claimant)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // another outcome was recorded first
		}
		if err != nil {
			return nil, fmt.Errorf("transition %s -> %s: %w", from, to, err)
		}
		e.CreatedAt = time.UnixMilli(created)
		e.SolverWallet = claimant.String
		return e, money(tx, e)
	})
}

// commit runs change in a transaction. If change returns an Event, the
// change counts as recorded: the Event is delivered after commit and commit
// reports true.
func (l *Lifecycle) commit(ctx context.Context, change func(*sql.Tx) (*Event, error)) (bool, error) {
	l.commitMu.Lock()
	defer l.commitMu.Unlock()
	var e *Event
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		e, err = change(tx)
		return err
	})
	if err != nil {
		return false, err
	}
	if e != nil {
		l.notify(*e)
	}
	return e != nil, nil
}

// schedule runs fn for the Task after d, unless the Lifecycle is closed first.
// It replaces the Task's earlier timer, if any.
func (l *Lifecycle) schedule(id string, d time.Duration, fn func(context.Context, string) (bool, error)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if old, ok := l.timers[id]; ok {
		old.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		if l.timers[id] == t {
			delete(l.timers, id)
		}
		l.inflight.Add(1)
		l.mu.Unlock()
		defer l.inflight.Done()
		if _, err := fn(context.Background(), id); err != nil {
			log.Printf("task %s timer: %v", id, err)
		}
	})
	l.timers[id] = t
}

func (l *Lifecycle) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
