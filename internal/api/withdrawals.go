package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/yuchia329/unstuck/internal/payout"
	"github.com/yuchia329/unstuck/internal/solana"
)

// handleEarnings shows a Solver's available Earnings and recent Withdrawals.
// Earnings are tied to a wallet address, not a secret, so anyone may read them.
func (s *Server) handleEarnings(w http.ResponseWriter, r *http.Request) {
	wallet := r.PathValue("wallet")
	if !solana.IsPubkey(wallet) {
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	}
	available, err := s.payouts.Available(r.Context(), wallet)
	if err != nil {
		s.internalError(w, err)
		return
	}
	recent, err := s.payouts.Recent(r.Context(), wallet)
	if err != nil {
		s.internalError(w, err)
		return
	}
	withdrawals := make([]map[string]any, 0, len(recent))
	for _, wd := range recent {
		withdrawals = append(withdrawals, withdrawalJSON(wd))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":           available,
		"withdrawals_enabled": s.payouts.Enabled(),
		"minimum":             s.cfg.MinWithdrawal,
		"account_fee":         s.cfg.AccountFee,
		"withdrawals":         withdrawals,
	})
}

func (s *Server) handleWithdrawalChallenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wallet string `json:"wallet"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := s.payouts.Challenge(r.Context(), req.Wallet)
	switch {
	case errors.Is(err, payout.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "withdrawals_disabled")
		return
	case errors.Is(err, payout.ErrInvalidWallet):
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"nonce":      c.Nonce,
		"message":    c.Message,
		"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

// handleWithdraw opens a Withdrawal of all the Solver's available Earnings,
// once they prove they own the wallet by signing its withdrawal challenge.
func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wallet    string `json:"wallet"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"` // base58 ed25519 over the challenge message
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	wd, err := s.payouts.Request(r.Context(), req.Wallet, req.Nonce, req.Signature)
	var below *payout.BelowMinimumError
	switch {
	case errors.Is(err, payout.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "withdrawals_disabled")
	case errors.Is(err, payout.ErrInvalidWallet):
		writeError(w, http.StatusBadRequest, "invalid_wallet")
	case errors.Is(err, payout.ErrInvalidProof):
		writeError(w, http.StatusUnauthorized, "invalid_proof")
	case errors.Is(err, payout.ErrOpen):
		writeError(w, http.StatusConflict, "withdrawal_open")
	case errors.As(err, &below):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       "below_minimum",
			"available":   below.Available,
			"minimum":     below.Minimum,
			"account_fee": below.AccountFee,
		})
	case err != nil:
		s.internalError(w, err)
	default:
		writeJSON(w, http.StatusCreated, withdrawalJSON(wd))
	}
}

func withdrawalJSON(wd payout.Withdrawal) map[string]any {
	return map[string]any{
		"withdrawal_id": wd.ID,
		"amount":        wd.Amount,
		"account_fee":   wd.AccountFee,
		"payout":        wd.Payout(),
		"state":         wd.State,
		"signature":     wd.Signature,
		"error":         wd.Error,
		"created_at":    wd.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}
