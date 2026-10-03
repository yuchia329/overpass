// Package customer registers Customers and authenticates their API keys.
//
// Registration requires proof of wallet ownership: the Customer signs a
// server-issued, single-use challenge message with the wallet's private key,
// and the signature must verify against the wallet's public key.
package customer

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yuchia329/unstuck/internal/secret"
	"github.com/yuchia329/unstuck/internal/solana"
)

// Schema is the customers module's part of the database schema.
const Schema = `
CREATE TABLE IF NOT EXISTS customers (
	id           TEXT PRIMARY KEY,
	wallet       TEXT NOT NULL UNIQUE,
	api_key_hash TEXT NOT NULL UNIQUE,
	available    INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
	held         INTEGER NOT NULL DEFAULT 0 CHECK (held >= 0),
	created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS challenges (
	nonce      TEXT PRIMARY KEY,
	wallet     TEXT NOT NULL,
	message    TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
`

var (
	// ErrInvalidWallet means the address is not a Solana public key.
	ErrInvalidWallet = errors.New("invalid wallet")
	// ErrInvalidProof means the challenge is unknown, used, expired or for
	// another wallet, or the signature does not verify.
	ErrInvalidProof = errors.New("invalid ownership proof")
	// ErrUnknownAPIKey means no Customer has this API key.
	ErrUnknownAPIKey = errors.New("unknown api key")
)

type Registry struct {
	db           *sql.DB
	challengeTTL time.Duration
}

func New(db *sql.DB, challengeTTL time.Duration) *Registry {
	return &Registry{db: db, challengeTTL: challengeTTL}
}

// Challenge is what the Customer must sign to prove they own a wallet.
type Challenge struct {
	Nonce     string
	Message   string // sign these exact UTF-8 bytes
	ExpiresAt time.Time
}

// Challenge issues a single-use challenge for wallet.
func (r *Registry) Challenge(ctx context.Context, wallet string) (Challenge, error) {
	if !solana.IsPubkey(wallet) {
		return Challenge{}, ErrInvalidWallet
	}
	now := time.Now()
	c := Challenge{Nonce: secret.New(""), ExpiresAt: now.Add(r.challengeTTL)}
	c.Message = fmt.Sprintf("Unstuck wants you to prove you own this Solana wallet.\n\nWallet: %s\nNonce: %s\nIssued At: %s\nExpires At: %s",
		wallet, c.Nonce, now.UTC().Format(time.RFC3339), c.ExpiresAt.UTC().Format(time.RFC3339))
	// Expired challenges are pruned here so the table stays small.
	if _, err := r.db.ExecContext(ctx, `DELETE FROM challenges WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Challenge{}, fmt.Errorf("prune challenges: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO challenges (nonce, wallet, message, expires_at) VALUES (?, ?, ?, ?)`,
		c.Nonce, wallet, c.Message, c.ExpiresAt.UnixMilli()); err != nil {
		return Challenge{}, fmt.Errorf("store challenge: %w", err)
	}
	return c, nil
}

// Registered is the outcome of a successful registration.
type Registered struct {
	CustomerID string
	APIKey     string // shown once; only its hash is stored
	New        bool   // false when an existing Customer's API key was rotated
}

// Register consumes the challenge and verifies the base58 ed25519 signature
// over its message against wallet. On success it creates the Customer, or,
// if the wallet is already registered, rotates that Customer's API key.
func (r *Registry) Register(ctx context.Context, wallet, nonce, signature string) (Registered, error) {
	if !solana.IsPubkey(wallet) {
		return Registered{}, ErrInvalidWallet
	}
	// Consume first: a nonce is spent whether or not the signature verifies.
	var message string
	err := r.db.QueryRowContext(ctx,
		`DELETE FROM challenges WHERE nonce = ? AND wallet = ? AND expires_at > ? RETURNING message`,
		nonce, wallet, time.Now().UnixMilli()).Scan(&message)
	if errors.Is(err, sql.ErrNoRows) {
		return Registered{}, ErrInvalidProof
	}
	if err != nil {
		return Registered{}, fmt.Errorf("consume challenge: %w", err)
	}
	pub, _ := solana.DecodeBase58(wallet)
	sig, ok := solana.DecodeBase58(signature)
	if !ok || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, []byte(message), sig) {
		return Registered{}, ErrInvalidProof
	}

	proposedID := secret.New("cus_")
	reg := Registered{APIKey: secret.New("unstuck_")}
	// On conflict the existing row keeps its id and gets the new key hash.
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO customers (id, wallet, api_key_hash, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (wallet) DO UPDATE SET api_key_hash = excluded.api_key_hash
		 RETURNING id`,
		proposedID, wallet, secret.Hash(reg.APIKey), time.Now().UnixMilli()).Scan(&reg.CustomerID)
	if err != nil {
		return Registered{}, fmt.Errorf("register: %w", err)
	}
	reg.New = reg.CustomerID == proposedID
	return reg, nil
}

// Authenticate resolves an API key to its Customer id.
func (r *Registry) Authenticate(ctx context.Context, apiKey string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM customers WHERE api_key_hash = ?`, secret.Hash(apiKey)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownAPIKey
	}
	if err != nil {
		return "", fmt.Errorf("authenticate: %w", err)
	}
	return id, nil
}
