// Package api serves the Overpass HTTP API.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuchia329/overpass/internal/ledger"
	"github.com/yuchia329/overpass/internal/secret"
	"github.com/yuchia329/overpass/internal/solana"
	"github.com/yuchia329/overpass/internal/store"
	"github.com/yuchia329/overpass/internal/task"
)

const (
	recentTasksLimit = 20
	maxBodyBytes     = 64 << 10
)

// Config holds the settings chosen by whoever runs the backend.
type Config struct {
	DBPath        string
	ClaimWindow   time.Duration
	SolveWindow   time.Duration
	Price         int64 // USDC base units (6 decimals)
	ServiceWallet string
	DevMode       bool // enables the dev credit endpoint
}

// Server is the backend: an http.Handler plus the resources behind it.
type Server struct {
	cfg   Config
	db    *sql.DB
	tasks *task.Lifecycle
	mux   *http.ServeMux
}

func (c Config) validate() error {
	switch {
	case c.Price <= 0:
		return fmt.Errorf("price must be positive, got %d", c.Price)
	case c.ClaimWindow <= 0 || c.SolveWindow <= 0:
		return fmt.Errorf("claim and solve windows must be positive, got %v and %v", c.ClaimWindow, c.SolveWindow)
	case !solana.IsPubkey(c.ServiceWallet):
		return fmt.Errorf("service wallet %q is not a Solana public key", c.ServiceWallet)
	}
	return nil
}

func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	db, err := store.Open(cfg.DBPath, ledger.Schema, task.Schema)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg,
		db:  db,
		tasks: task.New(db, task.Config{
			ClaimWindow: cfg.ClaimWindow,
			SolveWindow: cfg.SolveWindow,
			Price:       cfg.Price,
		}),
		mux: http.NewServeMux(),
	}
	if err := s.tasks.Resume(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	s.mux.HandleFunc("POST /v1/customers", s.handleRegister)
	s.mux.HandleFunc("GET /v1/balance", s.auth(s.handleBalance))
	s.mux.HandleFunc("POST /v1/tasks", s.auth(s.handleCreateTask))
	if cfg.DevMode {
		s.mux.HandleFunc("POST /v1/dev/credit", s.auth(s.handleDevCredit))
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) Close() error {
	s.tasks.Close()
	return s.db.Close()
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wallet string `json:"wallet"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !solana.IsPubkey(req.Wallet) {
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	}
	id, key := secret.New("cus_"), secret.New("op_")
	res, err := s.db.ExecContext(r.Context(),
		`INSERT INTO customers (id, wallet, api_key_hash, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (wallet) DO NOTHING`,
		id, req.Wallet, secret.Hash(key), time.Now().UnixMilli())
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Wallet addresses are public, so never hand out the existing key.
		var existing string
		if err := s.db.QueryRowContext(r.Context(), `SELECT id FROM customers WHERE wallet = ?`, req.Wallet).Scan(&existing); err != nil {
			s.internalError(w, err)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "wallet_registered", "customer_id": existing})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"customer_id": id, "api_key": key})
}

// auth resolves the Bearer API key to a Customer id, or responds 401.
func (s *Server) auth(next func(w http.ResponseWriter, r *http.Request, customerID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var customerID string
		err := s.db.QueryRowContext(r.Context(), `SELECT id FROM customers WHERE api_key_hash = ?`, secret.Hash(key)).Scan(&customerID)
		if errors.Is(err, sql.ErrNoRows) {
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
	writeJSON(w, http.StatusOK, map[string]any{"available": b.Available, "held": b.Held, "tasks": tasks})
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
		PageURL string `json:"page_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !isPageURL(req.PageURL) {
		writeError(w, http.StatusBadRequest, "invalid_page_url")
		return
	}
	created, err := s.tasks.Create(r.Context(), customerID, req.PageURL)
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
