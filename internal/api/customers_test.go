package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
)

func TestChallengeMessageNamesWalletAndNonce(t *testing.T) {
	h := newHarness(t)
	w := newWallet(t)

	nonce, message := h.challenge(w.address)

	if !strings.Contains(message, w.address) || !strings.Contains(message, nonce) {
		t.Errorf("message %q does not name wallet %s and nonce %s", message, w.address, nonce)
	}
}

func TestRegisterWithValidSignatureReturnsCustomerIDAndAPIKey(t *testing.T) {
	h := newHarness(t)

	res := h.registerAs(newWallet(t))

	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %v", res.status, res.body)
	}
	if id, _ := res.body["customer_id"].(string); id == "" {
		t.Errorf("missing customer_id: %v", res.body)
	}
	key, _ := res.body["api_key"].(string)
	if key == "" {
		t.Fatalf("missing api_key: %v", res.body)
	}
	if res := h.do("GET", "/v1/balance", key, nil); res.status != http.StatusOK {
		t.Errorf("new api_key rejected: status %d", res.status)
	}
}

func TestRegisterRejectsSignatureFromAnotherKey(t *testing.T) {
	h := newHarness(t)
	victim, attacker := newWallet(t), newWallet(t)
	nonce, message := h.challenge(victim.address)

	res := h.do("POST", "/v1/customers", "", map[string]any{
		"wallet": victim.address, "nonce": nonce, "signature": attacker.sign(message),
	})

	if res.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %v", res.status, res.body)
	}
	if _, ok := res.body["api_key"]; ok {
		t.Errorf("rejected registration returned an api_key: %v", res.body)
	}
	// The victim can still register their own wallet as a new Customer.
	if res := h.registerAs(victim); res.status != http.StatusCreated {
		t.Errorf("victim registration: status = %d, want 201", res.status)
	}
}

func TestRegisterRejectsSignatureOverDifferentMessage(t *testing.T) {
	h := newHarness(t)
	w := newWallet(t)
	nonce, message := h.challenge(w.address)

	res := h.do("POST", "/v1/customers", "", map[string]any{
		"wallet": w.address, "nonce": nonce, "signature": w.sign(message + " tampered"),
	})

	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.status)
	}
}

func TestRegisterRejectsMalformedOrMissingSignature(t *testing.T) {
	h := newHarness(t)
	w := newWallet(t)

	for _, sig := range []string{"", "not-base58-0OIl", "3xyz"} {
		nonce, _ := h.challenge(w.address)
		res := h.do("POST", "/v1/customers", "", map[string]any{"wallet": w.address, "nonce": nonce, "signature": sig})
		if res.status != http.StatusUnauthorized {
			t.Errorf("signature %q: status = %d, want 401", sig, res.status)
		}
	}
}

func TestChallengeCanBeUsedOnlyOnce(t *testing.T) {
	h := newHarness(t)
	w := newWallet(t)
	nonce, message := h.challenge(w.address)
	body := map[string]any{"wallet": w.address, "nonce": nonce, "signature": w.sign(message)}
	if res := h.do("POST", "/v1/customers", "", body); res.status != http.StatusCreated {
		t.Fatalf("first use: status = %d, want 201", res.status)
	}

	replay := h.do("POST", "/v1/customers", "", body)

	if replay.status != http.StatusUnauthorized {
		t.Errorf("replay: status = %d, want 401", replay.status)
	}
	if _, ok := replay.body["api_key"]; ok {
		t.Errorf("replay returned an api_key: %v", replay.body)
	}
}

func TestChallengeIsBoundToItsWallet(t *testing.T) {
	h := newHarness(t)
	a, b := newWallet(t), newWallet(t)
	nonce, message := h.challenge(a.address)

	// b signs a's message and presents a's nonce for its own wallet.
	res := h.do("POST", "/v1/customers", "", map[string]any{
		"wallet": b.address, "nonce": nonce, "signature": b.sign(message),
	})

	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.status)
	}
}

func TestExpiredChallengeIsRejected(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ChallengeTTL = 50 * time.Millisecond })
	w := newWallet(t)
	nonce, message := h.challenge(w.address)

	time.Sleep(100 * time.Millisecond)
	res := h.do("POST", "/v1/customers", "", map[string]any{
		"wallet": w.address, "nonce": nonce, "signature": w.sign(message),
	})

	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.status)
	}
}

func TestReRegisteringSameWalletKeepsCustomerAndRotatesAPIKey(t *testing.T) {
	h := newHarness(t)
	w := newWallet(t)
	first := h.registerAs(w)
	oldKey := first.body["api_key"].(string)
	h.credit(oldKey, 50_000)

	second := h.registerAs(w)

	if second.status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", second.status, second.body)
	}
	if second.body["customer_id"] != first.body["customer_id"] {
		t.Errorf("customer_id = %v, want existing %v", second.body["customer_id"], first.body["customer_id"])
	}
	newKey, _ := second.body["api_key"].(string)
	if newKey == "" || newKey == oldKey {
		t.Fatalf("api_key = %q, want a new key", newKey)
	}
	if res := h.do("GET", "/v1/balance", oldKey, nil); res.status != http.StatusUnauthorized {
		t.Errorf("old key: status = %d, want 401", res.status)
	}
	if available, _ := h.balance(newKey); available != 50_000 {
		t.Errorf("available = %d, want 50000 kept on the same Customer", available)
	}
}

func TestChallengeRejectsInvalidWallet(t *testing.T) {
	h := newHarness(t)

	for _, address := range []string{"", "not-a-wallet", "0OIldsc7rwW9SFDD2HbjXVSLmn9xo54sT7m9FFGCHJ7b", "2Qemdsc7rwW9SFDD2HbjXV"} {
		res := h.do("POST", "/v1/customers/challenge", "", map[string]any{"wallet": address})
		if res.status != http.StatusBadRequest {
			t.Errorf("wallet %q: status = %d, want 400", address, res.status)
		}
	}
}
