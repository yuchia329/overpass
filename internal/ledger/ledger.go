// Package ledger owns all money movement: credits, Holds, captures, releases
// and Balances. Amounts are integer USDC base units (6 decimals).
//
// Every operation runs on a caller-supplied transaction so the Task lifecycle
// can move money in the same transaction as its state change. Available
// Balance can never go negative: Hold only succeeds when it is covered.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Schema is the Ledger's part of the database schema.
const Schema = `
CREATE TABLE IF NOT EXISTS holds (
	task_id     TEXT PRIMARY KEY,
	customer_id TEXT NOT NULL REFERENCES customers(id),
	amount      INTEGER NOT NULL CHECK (amount > 0),
	state       TEXT NOT NULL CHECK (state IN ('held', 'captured', 'released')),
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS captures (
	task_id       TEXT PRIMARY KEY REFERENCES holds(task_id),
	solver_wallet TEXT NOT NULL,
	earning       INTEGER NOT NULL,
	fee           INTEGER NOT NULL,
	created_at    INTEGER NOT NULL
);
`

// EarningPercent is the Solver's share of a captured Hold; the rest is the Fee.
const EarningPercent = 80

// DBTX is satisfied by *sql.DB and *sql.Tx.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Balance is a Customer's funds held by Overpass.
type Balance struct {
	Available int64
	Held      int64
}

// InsufficientError is returned by Hold when available Balance is below the amount.
type InsufficientError struct {
	Available int64
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("insufficient balance: %d available", e.Available)
}

// ErrNoHold is returned when there is no open Hold for the Task.
var ErrNoHold = errors.New("no open hold for task")

// Credit adds amount to the Customer's available Balance.
func Credit(ctx context.Context, tx DBTX, customerID string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("credit amount must be positive, got %d", amount)
	}
	res, err := tx.ExecContext(ctx, `UPDATE customers SET available = available + ? WHERE id = ?`, amount, customerID)
	if err != nil {
		return fmt.Errorf("credit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("credit: unknown customer %s", customerID)
	}
	return nil
}

// Hold moves amount from available to held for one Task.
// It returns *InsufficientError if available Balance does not cover it.
func Hold(ctx context.Context, tx DBTX, customerID, taskID string, amount int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE customers SET available = available - ?1, held = held + ?1 WHERE id = ?2 AND available >= ?1`,
		amount, customerID)
	if err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		b, err := GetBalance(ctx, tx, customerID)
		if err != nil {
			return err
		}
		return &InsufficientError{Available: b.Available}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO holds (task_id, customer_id, amount, state, created_at) VALUES (?, ?, ?, 'held', ?)`,
		taskID, customerID, amount, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	return nil
}

// Release returns a Task's open Hold to the Customer's available Balance in full.
func Release(ctx context.Context, tx DBTX, taskID string) error {
	customerID, amount, err := closeHold(ctx, tx, taskID, "released")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE customers SET available = available + ?1, held = held - ?1 WHERE id = ?2`, amount, customerID)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// Capture takes a Task's open Hold, splitting it into the Solver's Earning and the Fee.
func Capture(ctx context.Context, tx DBTX, taskID, solverWallet string) (earning, fee int64, err error) {
	customerID, amount, err := closeHold(ctx, tx, taskID, "captured")
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE customers SET held = held - ? WHERE id = ?`, amount, customerID); err != nil {
		return 0, 0, fmt.Errorf("capture: %w", err)
	}
	earning = amount * EarningPercent / 100
	fee = amount - earning
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO captures (task_id, solver_wallet, earning, fee, created_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, solverWallet, earning, fee, time.Now().UnixMilli()); err != nil {
		return 0, 0, fmt.Errorf("capture: %w", err)
	}
	return earning, fee, nil
}

// GetBalance reads a Customer's available and held Balance.
func GetBalance(ctx context.Context, db DBTX, customerID string) (Balance, error) {
	var b Balance
	err := db.QueryRowContext(ctx, `SELECT available, held FROM customers WHERE id = ?`, customerID).Scan(&b.Available, &b.Held)
	if err != nil {
		return Balance{}, fmt.Errorf("balance: %w", err)
	}
	return b, nil
}

// closeHold moves an open Hold to its final state and returns what it held.
func closeHold(ctx context.Context, tx DBTX, taskID, state string) (customerID string, amount int64, err error) {
	err = tx.QueryRowContext(ctx,
		`UPDATE holds SET state = ? WHERE task_id = ? AND state = 'held' RETURNING customer_id, amount`,
		state, taskID).Scan(&customerID, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNoHold
	}
	if err != nil {
		return "", 0, fmt.Errorf("close hold: %w", err)
	}
	return customerID, amount, nil
}
