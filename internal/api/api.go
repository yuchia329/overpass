// Package api serves the Overpass HTTP API.
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuchia329/overpass/internal/customer"
	"github.com/yuchia329/overpass/internal/deposit"
	"github.com/yuchia329/overpass/internal/ledger"
	"github.com/yuchia329/overpass/internal/payout"
	"github.com/yuchia329/overpass/internal/queue"
	"github.com/yuchia329/overpass/internal/session"
	"github.com/yuchia329/overpass/internal/solana"
	"github.com/yuchia329/overpass/internal/store"
	"github.com/yuchia329/overpass/internal/task"
	"github.com/yuchia329/overpass/internal/web"
)

const (
	recentTasksLimit    = 20
	recentDepositsLimit = 20
	maxBodyBytes        = 64 << 10
)

// Config holds the settings chosen by whoever runs the backend.
type Config struct {
	DBPath        string
	ClaimWindow   time.Duration
	SolveWindow   time.Duration
	Price         int64 // USDC base units (6 decimals)
	ServiceWallet string
	DevMode       bool          // enables the dev credit endpoint
	ChallengeTTL  time.Duration // how long a registration challenge can be signed
	RPCURL        string        // Solana JSON-RPC endpoint polled for Deposits; empty disables polling
	PollInterval  time.Duration // how often to poll for Deposits
	PingInterval  time.Duration // how often to ping Bridge and Queue sockets; zero disables
	// STUNURLs and TURNURLs are handed to a Session's peers for a direct
	// WebRTC connection. TURN needs TURNSecret, coturn's static-auth-secret.
	STUNURLs   []string
	TURNURLs   []string
	TURNSecret string
	// PayoutKey is the hot wallet that pays Solvers' Withdrawals; nil
	// disables Withdrawals. It needs PayoutRPCURL, and USDC and SOL to pay with.
	PayoutKey          ed25519.PrivateKey
	PayoutRPCURL       string
	USDCMint           string        // defaults to mainnet USDC
	MinWithdrawal      int64         // least a Solver receives per Withdrawal, USDC base units
	AccountFee         int64         // kept back when the Solver has no USDC token account yet
	PayoutPollInterval time.Duration // how often sent Withdrawals are checked; defaults to 2s
}

// Server is the backend: an http.Handler plus the resources behind it.
type Server struct {
	cfg       Config
	db        *sql.DB
	customers *customer.Registry
	tasks     *task.Lifecycle
	queue     *queue.Hub
	relay     *session.Relay
	deposits  *deposit.Poller
	payouts   *payout.Service
	mux       *http.ServeMux
}

func (c Config) validate() error {
	switch {
	case c.Price <= 0:
		return fmt.Errorf("price must be positive, got %d", c.Price)
	case c.ClaimWindow <= 0 || c.SolveWindow <= 0:
		return fmt.Errorf("claim and solve windows must be positive, got %v and %v", c.ClaimWindow, c.SolveWindow)
	case !solana.IsPubkey(c.ServiceWallet):
		return fmt.Errorf("service wallet %q is not a Solana public key", c.ServiceWallet)
	case c.ChallengeTTL <= 0:
		return fmt.Errorf("challenge TTL must be positive, got %v", c.ChallengeTTL)
	case c.PingInterval < 0:
		return fmt.Errorf("ping interval must not be negative, got %v", c.PingInterval)
	case len(c.TURNURLs) > 0 && c.TURNSecret == "":
		return errors.New("TURN servers need a TURN secret")
	case c.RPCURL != "" && c.PollInterval <= 0:
		return fmt.Errorf("poll interval must be positive, got %v", c.PollInterval)
	case c.PayoutKey == nil:
		return nil
	case len(c.PayoutKey) != ed25519.PrivateKeySize:
		return errors.New("payout key is not an ed25519 keypair")
	case c.PayoutRPCURL == "":
		return errors.New("payouts need an RPC URL")
	case c.USDCMint != "" && !solana.IsPubkey(c.USDCMint):
		return fmt.Errorf("USDC mint %q is not a Solana public key", c.USDCMint)
	case c.MinWithdrawal <= 0:
		return fmt.Errorf("minimum withdrawal must be positive, got %d", c.MinWithdrawal)
	case c.AccountFee < 0:
		return fmt.Errorf("account fee must not be negative, got %d", c.AccountFee)
	}
	return nil
}

func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.USDCMint == "" {
		cfg.USDCMint = solana.USDCMint
	}
	if cfg.PayoutPollInterval <= 0 {
		cfg.PayoutPollInterval = 2 * time.Second
	}
	db, err := store.Open(cfg.DBPath, customer.Schema, ledger.Schema, task.Schema, deposit.Schema, payout.Schema)
	if err != nil {
		return nil, err
	}
	if err := task.Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	hub := queue.New()
	relay := session.New()
	s := &Server{
		cfg:       cfg,
		db:        db,
		customers: customer.New(db, cfg.ChallengeTTL),
		deposits:  deposit.New(db, deposit.Config{RPCURL: cfg.RPCURL, ServiceWallet: cfg.ServiceWallet, PollInterval: cfg.PollInterval}),
		tasks: task.New(db, task.Config{
			ClaimWindow: cfg.ClaimWindow,
			SolveWindow: cfg.SolveWindow,
			Price:       cfg.Price,
		}, func(e task.Event) {
			hub.Publish(e)
			relay.Publish(e)
		}),
		payouts: payout.New(db, payout.Config{
			RPCURL:       cfg.PayoutRPCURL,
			Key:          cfg.PayoutKey,
			Mint:         cfg.USDCMint,
			Minimum:      cfg.MinWithdrawal,
			AccountFee:   cfg.AccountFee,
			ChallengeTTL: cfg.ChallengeTTL,
			PollInterval: cfg.PayoutPollInterval,
		}),
		queue: hub,
		relay: relay,
		mux:   http.NewServeMux(),
	}
	if err := s.tasks.Resume(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	s.deposits.Start()
	s.payouts.Start()
	s.mux.HandleFunc("POST /v1/customers/challenge", s.handleChallenge)
	s.mux.HandleFunc("POST /v1/customers", s.handleRegister)
	s.mux.HandleFunc("GET /v1/balance", s.auth(s.handleBalance))
	s.mux.HandleFunc("POST /v1/tasks", s.auth(s.handleCreateTask))
	if cfg.DevMode {
		s.mux.HandleFunc("POST /v1/dev/credit", s.auth(s.handleDevCredit))
	}
	s.mux.HandleFunc("GET /v1/queue", s.handleQueue)
	s.mux.HandleFunc("GET /v1/tasks/{id}/bridge", s.handleBridge)
	s.mux.HandleFunc("GET /v1/solvers/{wallet}/earnings", s.handleEarnings)
	s.mux.HandleFunc("POST /v1/withdrawals/challenge", s.handleWithdrawalChallenge)
	s.mux.HandleFunc("POST /v1/withdrawals", s.handleWithdraw)
	if err := s.routeQueuePage(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// routeQueuePage serves each Queue page file at its own path, index.html at
// "/", so unknown API paths still 404. Every file is sent with no-cache, and
// index.html loads queue.js under a content version, so a proxy or browser
// that cached an older copy cannot pair it with a newer page.
func (s *Server) routeQueuePage() error {
	files, err := fs.ReadDir(web.Queue, ".")
	if err != nil {
		return fmt.Errorf("queue page: %w", err)
	}
	index, err := versionedIndex()
	if err != nil {
		return fmt.Errorf("queue page: %w", err)
	}
	noCache := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			h(w, r)
		}
	}
	s.mux.HandleFunc("GET /{$}", noCache(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	}))
	fileServer := http.FileServerFS(web.Queue)
	for _, f := range files {
		if f.Name() != "index.html" {
			s.mux.HandleFunc("GET /"+f.Name(), noCache(fileServer.ServeHTTP))
		}
	}
	return nil
}

// versionedIndex returns index.html with its queue.js reference carrying a
// hash of queue.js.
func versionedIndex() ([]byte, error) {
	index, err := fs.ReadFile(web.Queue, "index.html")
	if err != nil {
		return nil, err
	}
	script, err := fs.ReadFile(web.Queue, "queue.js")
	if err != nil {
		return nil, err
	}
	const ref = `src="queue.js"`
	if !bytes.Contains(index, []byte(ref)) {
		return nil, fmt.Errorf("index.html has no %s", ref)
	}
	sum := sha256.Sum256(script)
	versioned := fmt.Sprintf(`src="queue.js?v=%x"`, sum[:6])
	return bytes.Replace(index, []byte(ref), []byte(versioned), 1), nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) Close() error {
	s.deposits.Close()
	s.payouts.Close()
	s.tasks.Close()
	return s.db.Close()
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wallet string `json:"wallet"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := s.customers.Challenge(r.Context(), req.Wallet)
	if errors.Is(err, customer.ErrInvalidWallet) {
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"nonce":      c.Nonce,
		"message":    c.Message,
		"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

// handleRegister creates a Customer, or rotates an existing Customer's API key,
// once the caller proves they own the wallet by signing its challenge.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wallet    string `json:"wallet"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"` // base58 ed25519 over the challenge message
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	reg, err := s.customers.Register(r.Context(), req.Wallet, req.Nonce, req.Signature)
	if err == nil {
		// If this fails the caller gets a 500 and registers again, which
		// rotates the key and retries the attribution.
		err = deposit.Attribute(r.Context(), s.db, reg.CustomerID, req.Wallet)
	}
	switch {
	case errors.Is(err, customer.ErrInvalidWallet):
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	case errors.Is(err, customer.ErrInvalidProof):
		writeError(w, http.StatusUnauthorized, "invalid_proof")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	status := http.StatusOK
	if reg.New {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"customer_id": reg.CustomerID, "api_key": reg.APIKey})
}

// auth resolves the Bearer API key to a Customer id, or responds 401.
func (s *Server) auth(next func(w http.ResponseWriter, r *http.Request, customerID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		customerID, err := s.customers.Authenticate(r.Context(), key)
		if errors.Is(err, customer.ErrUnknownAPIKey) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		next(w, r, customerID)
	}
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request, customerID string) {
	b, err := ledger.GetBalance(r.Context(), s.db, customerID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	recent, err := s.tasks.Recent(r.Context(), customerID, recentTasksLimit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	tasks := make([]map[string]any, 0, len(recent))
	for _, t := range recent {
		tasks = append(tasks, map[string]any{
			"task_id":    t.ID,
			"page_url":   t.PageURL,
			"state":      t.State,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	recentDeposits, err := deposit.Recent(r.Context(), s.db, customerID, recentDepositsLimit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	deposits := make([]map[string]any, 0, len(recentDeposits))
	for _, d := range recentDeposits {
		deposits = append(deposits, map[string]any{
			"signature":  d.Signature,
			"amount":     d.Amount,
			"created_at": d.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": b.Available, "held": b.Held, "tasks": tasks, "deposits": deposits,
	})
}

// handleDevCredit stands in for Deposits; it is only routed in dev mode.
func (s *Server) handleDevCredit(w http.ResponseWriter, r *http.Request, customerID string) {
	var req struct {
		Amount int64 `json:"amount"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount")
		return
	}
	if err := ledger.Credit(r.Context(), s.db, customerID, req.Amount); err != nil {
		s.internalError(w, err)
		return
	}
	s.handleBalance(w, r, customerID)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request, customerID string) {
	var req struct {
		PageURL  string `json:"page_url"`
		Obstacle string `json:"obstacle"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !isPageURL(req.PageURL) {
		writeError(w, http.StatusBadRequest, "invalid_page_url")
		return
	}
	obstacle := strings.TrimSpace(req.Obstacle)
	if !validObstacle(obstacle) {
		writeError(w, http.StatusBadRequest, "invalid_obstacle")
		return
	}
	created, err := s.tasks.Create(r.Context(), customerID, req.PageURL, obstacle)
	var insufficient *ledger.InsufficientError
	if errors.As(err, &insufficient) {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error":          "insufficient_balance",
			"available":      insufficient.Available,
			"price":          s.cfg.Price,
			"service_wallet": s.cfg.ServiceWallet,
		})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"task_id":         created.ID,
		"session_token":   created.SessionToken,
		"claim_deadline":  created.ClaimDeadline.UTC().Format(time.RFC3339Nano),
		"solve_window_ms": s.cfg.SolveWindow.Milliseconds(),
	})
}

// maxObstacle bounds the characters of a Task's obstacle description.
const maxObstacle = 200

// validObstacle reports whether s can describe a Task's obstacle to Solvers:
// empty, or one line of at most maxObstacle characters.
func validObstacle(s string) bool {
	return utf8.RuneCountInString(s) <= maxObstacle && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// isPageURL reports whether s is an absolute http(s) URL a Solver can be shown.
func isPageURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// decodeJSON reads a bounded JSON body into v, or responds 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": code})
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal")
}
