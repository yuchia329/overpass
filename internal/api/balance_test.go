package api_test

import (
	"net/http"
	"testing"

	"github.com/yuchia329/overpass/internal/api"
)

func TestNewCustomerHasZeroBalance(t *testing.T) {
	h := newHarness(t)
	key := h.register()

	available, held := h.balance(key)

	if available != 0 || held != 0 {
		t.Errorf("balance = %d/%d, want 0/0", available, held)
	}
}

func TestDevCreditAddsToAvailableBalance(t *testing.T) {
	h := newHarness(t)
	key := h.register()

	h.credit(key, 1_000_000)
	h.credit(key, 250_000)

	available, held := h.balance(key)
	if available != 1_250_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 1250000/0", available, held)
	}
}

func TestDevCreditIsDisabledWithoutDevFlag(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.DevMode = false })
	key := h.register()

	res := h.do("POST", "/v1/dev/credit", key, map[string]any{"amount": 1_000_000})

	if res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.status)
	}
	if available, _ := h.balance(key); available != 0 {
		t.Errorf("available = %d, want 0", available)
	}
}

func TestDevCreditRejectsNonPositiveAmount(t *testing.T) {
	h := newHarness(t)
	key := h.register()

	for _, amount := range []int64{0, -5} {
		res := h.do("POST", "/v1/dev/credit", key, map[string]any{"amount": amount})
		if res.status != http.StatusBadRequest {
			t.Errorf("amount %d: status = %d, want 400", amount, res.status)
		}
	}
}

func TestAuthenticatedEndpointsRejectMissingOrInvalidAPIKey(t *testing.T) {
	h := newHarness(t)
	h.register()

	endpoints := []struct{ method, path string }{
		{"GET", "/v1/balance"},
		{"POST", "/v1/dev/credit"},
		{"POST", "/v1/tasks"},
	}
	for _, e := range endpoints {
		for _, key := range []string{"", "op_wrong"} {
			res := h.do(e.method, e.path, key, map[string]any{"amount": 1, "page_url": "https://example.com"})
			if res.status != http.StatusUnauthorized {
				t.Errorf("%s %s with key %q: status = %d, want 401", e.method, e.path, key, res.status)
			}
		}
	}
}
