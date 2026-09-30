// Package deposit credits Customer Balances from USDC Deposits to the service
// wallet on Solana mainnet.
//
// The Poller watches the service wallet's USDC associated token account, not
// the wallet: SPL transfers only reference token accounts. Deposits are
// deduplicated by transaction signature. A Deposit from a wallet no Customer
// has registered is stored Unattributed and credited by Attribute once that
// wallet registers.
package deposit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/yuchia329/overpass/internal/ledger"
	"github.com/yuchia329/overpass/internal/solana"
)

// Schema is the Deposits module's part of the database schema.
const Schema = `
CREATE TABLE IF NOT EXISTS deposits (
	signature   TEXT PRIMARY KEY,
	sender      TEXT NOT NULL,
	amount      INTEGER NOT NULL CHECK (amount > 0),
	customer_id TEXT REFERENCES customers(id), -- NULL while Unattributed
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deposits_unattributed ON deposits (sender) WHERE customer_id IS NULL;
CREATE INDEX IF NOT EXISTS deposits_customer ON deposits (customer_id, created_at);
`

// maxBackoff caps the wait between polls after repeated RPC errors.
const maxBackoff = time.Minute

type Config struct {
	RPCURL        string // Solana JSON-RPC endpoint; empty disables polling
	ServiceWallet string
	PollInterval  time.Duration
}

// Deposit is one credited USDC transfer to the service wallet.
type Deposit struct {
	Signature string
	Amount    int64 // USDC base units
	CreatedAt time.Time
}

// Poller finds new Deposits and credits them.
type Poller struct {
	db           *sql.DB
	cfg          Config
	rpc          *solana.RPC
	tokenAccount string
	cursor       string // newest signature already processed; owned by the poll goroutine

	cancel context.CancelFunc
	done   chan struct{}
}

// New derives the service wallet's USDC token account. It panics if
// cfg.ServiceWallet is not a Solana public key; callers validate it first.
func New(db *sql.DB, cfg Config) *Poller {
	ata, err := solana.AssociatedTokenAddress(cfg.ServiceWallet, solana.USDCMint)
	if err != nil {
		panic(fmt.Sprintf("deposit: %v", err))
	}
	return &Poller{
		db:           db,
		cfg:          cfg,
		rpc:          solana.NewRPC(cfg.RPCURL, &http.Client{Timeout: 10 * time.Second}),
		tokenAccount: ata,
	}
}

// Start polls in the background until Close. It does nothing without an RPC URL.
func (p *Poller) Start() {
	if p.cfg.RPCURL == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	log.Printf("watching USDC token account %s for Deposits", p.tokenAccount)
	go p.run(ctx)
}

// Close stops polling and waits for an in-flight poll to finish.
func (p *Poller) Close() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}

// run polls every interval. RPC errors and rate limits are logged and the
// wait doubles, up to maxBackoff, until a poll succeeds.
func (p *Poller) run(ctx context.Context) {
	defer close(p.done)
	delay := p.cfg.PollInterval
	for {
		if err := p.poll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			delay = min(2*delay, max(maxBackoff, p.cfg.PollInterval))
			log.Printf("deposit poll failed, retrying in %v: %v", delay, err)
		} else {
			delay = p.cfg.PollInterval
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// errNotYet means the RPC node has the signature but not yet its transaction.
var errNotYet = errors.New("transaction not available yet")

// poll processes signatures oldest first, so the cursor never passes one
// that failed to process; it is retried on the next poll. A poll returns at
// most the newest 1000 signatures, far more than the service wallet gets
// between polls.
func (p *Poller) poll(ctx context.Context) error {
	sigs, err := p.rpc.SignaturesForAddress(ctx, p.tokenAccount)
	if err != nil {
		return err
	}
	// The cursor is matched here, not sent as `until`: mainnet RPC is load
	// balanced, and a node that has not indexed the cursor's transaction yet
	// rejects `until` as not found.
	newer := len(sigs)
	for i, s := range sigs {
		if s.Signature == p.cursor {
			newer = i
			break
		}
	}
	for i := newer - 1; i >= 0; i-- {
		if !sigs[i].Failed() {
			err := p.process(ctx, sigs[i].Signature)
			if errors.Is(err, errNotYet) {
				return nil // retried on the next poll, without backing off
			}
			if err != nil {
				return err
			}
		}
		p.cursor = sigs[i].Signature
	}
	return nil
}

func (p *Poller) process(ctx context.Context, signature string) error {
	var seen bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM deposits WHERE signature = ?)`, signature).Scan(&seen); err != nil {
		return fmt.Errorf("check deposit: %w", err)
	}
	if seen {
		return nil
	}
	tx, err := p.rpc.Transaction(ctx, signature)
	if err != nil {
		return err
	}
	if tx == nil {
		return errNotYet
	}
	sender, amount, ok := parse(tx, p.cfg.ServiceWallet)
	if !ok {
		return nil // not a Deposit, e.g. a transfer out of the service wallet
	}
	return record(ctx, p.db, signature, sender, amount)
}

// parse reads a Deposit from the transaction's USDC balance changes. The
// amount is the service wallet's increase. The sender is the owner whose USDC
// decreased most, not the signer or fee payer: `pay send` is gasless, so a
// pay.sh fee payer signs, and it also takes a USDC network fee from the sender.
func parse(tx *solana.Transaction, serviceWallet string) (sender string, amount int64, ok bool) {
	delta := map[string]int64{}
	for _, side := range []struct {
		balances []solana.TokenBalance
		sign     int64
	}{{tx.Meta.PreTokenBalances, -1}, {tx.Meta.PostTokenBalances, 1}} {
		for _, b := range side.balances {
			if b.Mint != solana.USDCMint {
				continue
			}
			n, err := strconv.ParseInt(b.UITokenAmount.Amount, 10, 64)
			if err != nil {
				return "", 0, false
			}
			delta[b.Owner] += side.sign * n
		}
	}
	for owner, d := range delta {
		if owner == serviceWallet || d >= 0 {
			continue
		}
		if sender == "" || d < delta[sender] || (d == delta[sender] && owner < sender) {
			sender = owner
		}
	}
	amount = delta[serviceWallet]
	return sender, amount, sender != "" && amount > 0
}

// record stores a Deposit once per signature, crediting the sender's
// Customer if the sender is registered.
func record(ctx context.Context, db *sql.DB, signature, sender string, amount int64) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		var customerID sql.NullString
		err := tx.QueryRowContext(ctx,
			`INSERT INTO deposits (signature, sender, amount, customer_id, created_at)
			 VALUES (?1, ?2, ?3, (SELECT id FROM customers WHERE wallet = ?2), ?4)
			 ON CONFLICT (signature) DO NOTHING
			 RETURNING customer_id`,
			signature, sender, amount, time.Now().UnixMilli()).Scan(&customerID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already recorded
		}
		if err != nil {
			return fmt.Errorf("record deposit: %w", err)
		}
		if !customerID.Valid {
			log.Printf("unattributed deposit %s: %d from %s", signature, amount, sender)
			return nil
		}
		return ledger.Credit(ctx, tx, customerID.String, amount)
	})
}

// Attribute credits the Customer with every Unattributed Deposit from wallet.
// Each Deposit is attributed once, so calling it again credits nothing new.
func Attribute(ctx context.Context, db *sql.DB, customerID, wallet string) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`UPDATE deposits SET customer_id = ? WHERE sender = ? AND customer_id IS NULL RETURNING amount`,
			customerID, wallet)
		if err != nil {
			return fmt.Errorf("attribute deposits: %w", err)
		}
		var total int64
		for rows.Next() {
			var amount int64
			if err := rows.Scan(&amount); err != nil {
				rows.Close()
				return fmt.Errorf("attribute deposits: %w", err)
			}
			total += amount
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("attribute deposits: %w", err)
		}
		if total == 0 {
			return nil
		}
		return ledger.Credit(ctx, tx, customerID, total)
	})
}

// Recent returns the Customer's newest Deposits first.
func Recent(ctx context.Context, db *sql.DB, customerID string, limit int) ([]Deposit, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT signature, amount, created_at FROM deposits WHERE customer_id = ?
		 ORDER BY created_at DESC, rowid DESC LIMIT ?`, customerID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent deposits: %w", err)
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var createdAt int64
		if err := rows.Scan(&d.Signature, &d.Amount, &createdAt); err != nil {
			return nil, fmt.Errorf("recent deposits: %w", err)
		}
		d.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
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
