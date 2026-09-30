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
	ended_at           INTEGER
);
CREATE INDEX IF NOT EXISTS tasks_customer ON tasks (customer_id, created_at);
`

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

// Created is what the Agent gets back from creating a Task.
type Created struct {
	ID            string
	SessionToken  string // shown once; only its hash is stored
	ClaimDeadline time.Time
}

// Lifecycle creates Tasks and drives their transitions and timers.
type Lifecycle struct {
	db  *sql.DB
	cfg Config

	mu       sync.Mutex
	closed   bool
	timers   map[string]*time.Timer
	inflight sync.WaitGroup
}

func New(db *sql.DB, cfg Config) *Lifecycle {
	return &Lifecycle{db: db, cfg: cfg, timers: map[string]*time.Timer{}}
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
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		// Insert first: the Hold references the Task id.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, customer_id, page_url, state, session_token_hash, created_at, claim_deadline)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ID, customerID, pageURL, Pending, secret.Hash(c.SessionToken), now.UnixMilli(), c.ClaimDeadline.UnixMilli()); err != nil {
			return fmt.Errorf("insert task: %w", err)
		}
		return ledger.Hold(ctx, tx, customerID, c.ID, l.cfg.Price)
	})
	if err != nil {
		return Created{}, err
	}
	l.schedule(c.ID, time.Until(c.ClaimDeadline), l.Expire)
	return c, nil
}

// Expire moves a Pending Task to Expired and releases its Hold.
// It reports whether this call recorded the outcome.
func (l *Lifecycle) Expire(ctx context.Context, id string) (bool, error) {
	return l.transition(ctx, id, Pending, Expired, func(tx *sql.Tx) error {
		return ledger.Release(ctx, tx, id)
	})
}

// Resume re-arms claim timers after a restart, expiring overdue Tasks at once.
func (l *Lifecycle) Resume(ctx context.Context) error {
	rows, err := l.db.QueryContext(ctx, `SELECT id, claim_deadline FROM tasks WHERE state = ?`, Pending)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	type pending struct {
		id       string
		deadline int64
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.deadline); err != nil {
			rows.Close()
			return fmt.Errorf("resume: %w", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	for _, p := range ps {
		l.schedule(p.id, time.Until(time.UnixMilli(p.deadline)), l.Expire)
	}
	return nil
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
// `from`, running money in the same transaction. It reports whether it won.
func (l *Lifecycle) transition(ctx context.Context, id string, from, to State, money func(*sql.Tx) error) (bool, error) {
	won := false
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, ended_at = ? WHERE id = ? AND state = ?`,
			to, time.Now().UnixMilli(), id, from)
		if err != nil {
			return fmt.Errorf("transition %s -> %s: %w", from, to, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // another outcome was recorded first
		}
		won = true
		return money(tx)
	})
	return won && err == nil, err
}

// schedule runs fn for the Task after d, unless the Lifecycle is closed first.
func (l *Lifecycle) schedule(id string, d time.Duration, fn func(context.Context, string) (bool, error)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.timers[id] = time.AfterFunc(d, func() {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		delete(l.timers, id)
		l.inflight.Add(1)
		l.mu.Unlock()
		defer l.inflight.Done()
		if _, err := fn(context.Background(), id); err != nil {
			log.Printf("task %s timer: %v", id, err)
		}
	})
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
