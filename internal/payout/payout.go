// Package payout pays Solvers their Earnings in USDC from a hot wallet the
// backend holds.
//
// A Solver withdraws all their available Earnings at once, after proving
// they own the wallet by signing a single-use withdrawal challenge. The
// payout always goes to that wallet. A Withdrawal is pending until its
// transaction is signed, sent once its signature is recorded, then confirmed
// or failed. Only a transaction seen failing on chain, or one whose blockhash
// expired unseen, fails a Withdrawal, so a Withdrawal is never paid twice;
// a failed one's Earnings are available again.
package payout

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuchia329/unstuck/internal/secret"
	"github.com/yuchia329/unstuck/internal/solana"
)

// Schema is the payout module's part of the database schema.
const Schema = `
CREATE TABLE IF NOT EXISTS withdrawals (
	id                TEXT PRIMARY KEY,
	wallet            TEXT NOT NULL,
	amount            INTEGER NOT NULL CHECK (amount > 0),
	account_fee       INTEGER NOT NULL CHECK (account_fee >= 0 AND account_fee < amount),
	state             TEXT NOT NULL CHECK (state IN ('pending', 'sent', 'confirmed', 'failed')),
	signature         TEXT,
	last_valid_height INTEGER,
	error             TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	ended_at          INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS withdrawals_open ON withdrawals (wallet) WHERE state IN ('pending', 'sent');
CREATE INDEX IF NOT EXISTS withdrawals_wallet ON withdrawals (wallet, created_at);
CREATE TABLE IF NOT EXISTS withdrawal_challenges (
	nonce      TEXT PRIMARY KEY,
	wallet     TEXT NOT NULL,
	message    TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
`

// usdcDecimals is USDC's precision: Earnings are kept in its base units.
const usdcDecimals = 6

// recentLimit bounds the Withdrawals listed with a Solver's Earnings.
const recentLimit = 10

var (
	// ErrDisabled means the backend has no hot wallet to pay from.
	ErrDisabled = errors.New("withdrawals are disabled")
	// ErrInvalidWallet means the address is not a Solana public key.
	ErrInvalidWallet = errors.New("invalid wallet")
	// ErrInvalidProof means the challenge is unknown, used, expired or for
	// another wallet, or the signature does not verify.
	ErrInvalidProof = errors.New("invalid ownership proof")
	// ErrOpen means the Solver already has a Withdrawal on its way.
	ErrOpen = errors.New("a withdrawal is already open")
)

// BelowMinimumError means the Solver would receive less than the minimum.
type BelowMinimumError struct {
	Available, Minimum, AccountFee int64
}

func (e *BelowMinimumError) Error() string {
	return fmt.Sprintf("available %d less account fee %d is below the minimum %d", e.Available, e.AccountFee, e.Minimum)
}

type Config struct {
	RPCURL string
	// Key is the hot wallet's keypair: it signs and pays for every payout.
	// Nil disables Withdrawals.
	Key  ed25519.PrivateKey
	Mint string // the USDC mint
	// Minimum is the least a Solver may receive in one Withdrawal.
	Minimum int64
	// AccountFee is kept back from a Withdrawal to a wallet that has no USDC
	// token account yet: it covers that account's rent, which the hot wallet pays.
	AccountFee   int64
	ChallengeTTL time.Duration
	// PollInterval is how often sent Withdrawals are checked on chain.
	PollInterval time.Duration
}

// Withdrawal is one payout of a Solver's Earnings.
type Withdrawal struct {
	ID         string
	Wallet     string
	Amount     int64 // the Earnings it takes
	AccountFee int64 // kept back for the Solver's new USDC token account
	State      string
	Signature  string // the payout transaction, once signed
	Error      string // why it failed
	CreatedAt  time.Time
}

// Payout is what the Solver receives.
func (w Withdrawal) Payout() int64 { return w.Amount - w.AccountFee }

// Challenge is what the Solver must sign to withdraw.
type Challenge struct {
	Nonce     string
	Message   string // sign these exact UTF-8 bytes
	ExpiresAt time.Time
}

// Service takes Withdrawal requests and pays them in the background.
type Service struct {
	db   *sql.DB
	cfg  Config
	rpc  *solana.RPC
	hot  string // the hot wallet's address
	wake chan struct{}
	stop chan struct{}
	done sync.WaitGroup
}

func New(db *sql.DB, cfg Config) *Service {
	s := &Service{db: db, cfg: cfg, wake: make(chan struct{}, 1), stop: make(chan struct{})}
	if cfg.Key != nil {
		s.rpc = solana.NewRPC(cfg.RPCURL, &http.Client{Timeout: 15 * time.Second})
		s.hot = solana.EncodeBase58(cfg.Key.Public().(ed25519.PublicKey))
	}
	return s
}

// Enabled reports whether Withdrawals are on.
func (s *Service) Enabled() bool { return s.cfg.Key != nil }

// HotWallet is the address payouts come from, or empty when disabled.
func (s *Service) HotWallet() string { return s.hot }

// Start pays Withdrawals in the background, picking up any left open by an
// earlier run, until Close.
func (s *Service) Start() {
	if !s.Enabled() {
		return
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		t := time.NewTicker(s.cfg.PollInterval)
		defer t.Stop()
		for {
			s.work()
			select {
			case <-s.stop:
				return
			case <-s.wake:
			case <-t.C:
			}
		}
	}()
}

func (s *Service) Close() {
	close(s.stop)
	s.done.Wait()
}

// Challenge issues a single-use withdrawal challenge for wallet. Its message
// says what signing it authorizes, and it is stored apart from registration
// challenges, so a signature for one can never be used for the other.
func (s *Service) Challenge(ctx context.Context, wallet string) (Challenge, error) {
	if !s.Enabled() {
		return Challenge{}, ErrDisabled
	}
	if !solana.IsPubkey(wallet) {
		return Challenge{}, ErrInvalidWallet
	}
	now := time.Now()
	c := Challenge{Nonce: secret.New(""), ExpiresAt: now.Add(s.cfg.ChallengeTTL)}
	c.Message = fmt.Sprintf("Unstuck: withdraw all my available Earnings to this Solana wallet.\n\nWallet: %s\nNonce: %s\nIssued At: %s\nExpires At: %s",
		wallet, c.Nonce, now.UTC().Format(time.RFC3339), c.ExpiresAt.UTC().Format(time.RFC3339))
	if _, err := s.db.ExecContext(ctx, `DELETE FROM withdrawal_challenges WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Challenge{}, fmt.Errorf("prune withdrawal challenges: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO withdrawal_challenges (nonce, wallet, message, expires_at) VALUES (?, ?, ?, ?)`,
		c.Nonce, wallet, c.Message, c.ExpiresAt.UnixMilli()); err != nil {
		return Challenge{}, fmt.Errorf("store withdrawal challenge: %w", err)
	}
	return c, nil
}

// Available is wallet's Earnings not yet withdrawn or on their way.
func (s *Service) Available(ctx context.Context, wallet string) (int64, error) {
	return available(ctx, s.db, wallet)
}

func available(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, wallet string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx,
		`SELECT (SELECT COALESCE(SUM(earning), 0) FROM captures WHERE solver_wallet = ?1)
		      - (SELECT COALESCE(SUM(amount), 0) FROM withdrawals WHERE wallet = ?1 AND state != 'failed')`,
		wallet).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("available earnings: %w", err)
	}
	return n, nil
}

// Recent lists wallet's newest Withdrawals.
func (s *Service) Recent(ctx context.Context, wallet string) ([]Withdrawal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wallet, amount, account_fee, state, COALESCE(signature, ''), error, created_at FROM withdrawals
		 WHERE wallet = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, wallet, recentLimit)
	if err != nil {
		return nil, fmt.Errorf("recent withdrawals: %w", err)
	}
	defer rows.Close()
	var out []Withdrawal
	for rows.Next() {
		var w Withdrawal
		var created int64
		if err := rows.Scan(&w.ID, &w.Wallet, &w.Amount, &w.AccountFee, &w.State, &w.Signature, &w.Error, &created); err != nil {
			return nil, fmt.Errorf("recent withdrawals: %w", err)
		}
		w.CreatedAt = time.UnixMilli(created)
		out = append(out, w)
	}
	return out, rows.Err()
}

// Request consumes the challenge, verifies the base58 ed25519 signature over
// its message against wallet, and opens a Withdrawal of all of wallet's
// available Earnings. If wallet has no USDC token account, AccountFee is kept
// back. It returns *BelowMinimumError if the payout would be under Minimum.
func (s *Service) Request(ctx context.Context, wallet, nonce, signature string) (Withdrawal, error) {
	if !s.Enabled() {
		return Withdrawal{}, ErrDisabled
	}
	if !solana.IsPubkey(wallet) {
		return Withdrawal{}, ErrInvalidWallet
	}
	// Consume first: a nonce is spent whether or not the signature verifies.
	var message string
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM withdrawal_challenges WHERE nonce = ? AND wallet = ? AND expires_at > ? RETURNING message`,
		nonce, wallet, time.Now().UnixMilli()).Scan(&message)
	if errors.Is(err, sql.ErrNoRows) {
		return Withdrawal{}, ErrInvalidProof
	}
	if err != nil {
		return Withdrawal{}, fmt.Errorf("consume withdrawal challenge: %w", err)
	}
	pub, _ := solana.DecodeBase58(wallet)
	sig, ok := solana.DecodeBase58(signature)
	if !ok || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, []byte(message), sig) {
		return Withdrawal{}, ErrInvalidProof
	}

	account, err := solana.AssociatedTokenAddress(wallet, s.cfg.Mint)
	if err != nil {
		return Withdrawal{}, err
	}
	exists, err := s.rpc.AccountExists(ctx, account)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("look up the USDC token account: %w", err)
	}
	fee := int64(0)
	if !exists {
		fee = s.cfg.AccountFee
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("withdraw: %w", err)
	}
	defer tx.Rollback()
	amount, err := available(ctx, tx, wallet)
	if err != nil {
		return Withdrawal{}, err
	}
	if amount-fee < s.cfg.Minimum {
		return Withdrawal{}, &BelowMinimumError{Available: amount, Minimum: s.cfg.Minimum, AccountFee: fee}
	}
	w := Withdrawal{ID: secret.New("wd_"), Wallet: wallet, Amount: amount, AccountFee: fee, State: "pending", CreatedAt: time.Now()}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO withdrawals (id, wallet, amount, account_fee, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.ID, w.Wallet, w.Amount, w.AccountFee, w.State, w.CreatedAt.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Withdrawal{}, ErrOpen
		}
		return Withdrawal{}, fmt.Errorf("withdraw: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Withdrawal{}, ErrOpen
		}
		return Withdrawal{}, fmt.Errorf("withdraw: %w", err)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return w, nil
}

// work sends every pending Withdrawal and checks every sent one.
func (s *Service) work() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, wallet, amount, account_fee, state, COALESCE(signature, ''), COALESCE(last_valid_height, 0)
		 FROM withdrawals WHERE state IN ('pending', 'sent') ORDER BY created_at`)
	if err != nil {
		log.Printf("payout: %v", err)
		return
	}
	type open struct {
		Withdrawal
		lastValid uint64
	}
	var ws []open
	for rows.Next() {
		var w open
		if err := rows.Scan(&w.ID, &w.Wallet, &w.Amount, &w.AccountFee, &w.State, &w.Signature, &w.lastValid); err != nil {
			log.Printf("payout: %v", err)
			break
		}
		ws = append(ws, w)
	}
	rows.Close()
	for _, w := range ws {
		var err error
		if w.State == "pending" {
			err = s.send(ctx, w.Withdrawal)
		} else {
			err = s.check(ctx, w.Withdrawal, w.lastValid)
		}
		if err != nil {
			log.Printf("payout %s: %v", w.ID, err)
		}
	}
}

// send signs the Withdrawal's transaction, records its signature, then sends
// it. Recording first means a crash after sending still finds it to check.
func (s *Service) send(ctx context.Context, w Withdrawal) error {
	source, err := solana.AssociatedTokenAddress(s.hot, s.cfg.Mint)
	if err != nil {
		return err
	}
	destination, err := solana.AssociatedTokenAddress(w.Wallet, s.cfg.Mint)
	if err != nil {
		return err
	}
	var ixs []solana.Instruction
	if w.AccountFee > 0 {
		ixs = append(ixs, solana.CreateAssociatedTokenAccountIdempotent(s.hot, destination, w.Wallet, s.cfg.Mint))
	}
	ixs = append(ixs, solana.TransferChecked(source, s.cfg.Mint, destination, s.hot, uint64(w.Payout()), usdcDecimals))
	blockhash, lastValid, err := s.rpc.LatestBlockhash(ctx)
	if err != nil {
		return err
	}
	tx, sig, err := solana.SignedTransaction(s.cfg.Key, blockhash, ixs)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE withdrawals SET state = 'sent', signature = ?, last_valid_height = ? WHERE id = ? AND state = 'pending'`,
		sig, lastValid, w.ID)
	if err != nil {
		return fmt.Errorf("record signature: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // another run got to it first
	}
	if err := s.rpc.SendTransaction(ctx, tx); err != nil {
		// Even a refusal may have been forwarded: check decides, once the
		// blockhash expires. Keep the reason to show if it fails.
		_, _ = s.db.ExecContext(ctx, `UPDATE withdrawals SET error = ? WHERE id = ?`, err.Error(), w.ID)
		return err
	}
	return nil
}

// check settles a sent Withdrawal: confirmed once its transaction is,
// failed if it failed on chain or can no longer land.
func (s *Service) check(ctx context.Context, w Withdrawal, lastValid uint64) error {
	st, err := s.rpc.Status(ctx, w.Signature)
	if err != nil {
		return err
	}
	switch {
	case st != nil && st.Failed():
		return s.end(ctx, w.ID, "failed", "transaction failed: "+string(st.Err))
	case st != nil && st.Confirmed():
		return s.end(ctx, w.ID, "confirmed", "")
	case st != nil:
		return nil // processed: not confirmed yet
	}
	height, err := s.rpc.BlockHeight(ctx)
	if err != nil {
		return err
	}
	if height > lastValid {
		return s.end(ctx, w.ID, "failed", "") // keeps the error recorded on send, if any
	}
	return nil
}

// end records a sent Withdrawal's outcome. A failure keeps the error its
// send recorded, if any, unless reason says more.
func (s *Service) end(ctx context.Context, id, state, reason string) error {
	query := `UPDATE withdrawals SET state = ?, error = ?, ended_at = ? WHERE id = ? AND state = 'sent'`
	args := []any{state, reason, time.Now().UnixMilli(), id}
	if state == "failed" && reason == "" {
		query = `UPDATE withdrawals SET state = ?, error = CASE error WHEN '' THEN 'transaction expired' ELSE error END,
		         ended_at = ? WHERE id = ? AND state = 'sent'`
		args = []any{state, time.Now().UnixMilli(), id}
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("end withdrawal: %w", err)
	}
	log.Printf("payout %s %s", id, state)
	return nil
}
