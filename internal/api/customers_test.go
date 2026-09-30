package api_test

import (
	"net/http"
	"testing"
)

func TestRegisterReturnsCustomerIDAndAPIKey(t *testing.T) {
	h := newHarness(t)

	res := h.do("POST", "/v1/customers", "", map[string]any{"wallet": testCustomerWallet})

	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %v", res.status, res.body)
	}
	if id, _ := res.body["customer_id"].(string); id == "" {
		t.Errorf("missing customer_id: %v", res.body)
	}
	if key, _ := res.body["api_key"].(string); key == "" {
		t.Errorf("missing api_key: %v", res.body)
	}
}

func TestRegisteringSameWalletTwiceDoesNotDuplicateCustomer(t *testing.T) {
	h := newHarness(t)
	first := h.do("POST", "/v1/customers", "", map[string]any{"wallet": testCustomerWallet})

	second := h.do("POST", "/v1/customers", "", map[string]any{"wallet": testCustomerWallet})

	if second.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %v", second.status, second.body)
	}
	if second.body["customer_id"] != first.body["customer_id"] {
		t.Errorf("customer_id = %v, want existing %v", second.body["customer_id"], first.body["customer_id"])
	}
	// The wallet address is public, so re-registering must not hand out a key.
	if _, ok := second.body["api_key"]; ok {
		t.Errorf("second registration leaked an api_key: %v", second.body)
	}
}

func TestRegisterRejectsInvalidWallet(t *testing.T) {
	h := newHarness(t)

	for _, wallet := range []string{"", "not-a-wallet", "0OIl" + testCustomerWallet[4:], "2Qemdsc7rwW9SFDD2HbjXV"} {
		res := h.do("POST", "/v1/customers", "", map[string]any{"wallet": wallet})
		if res.status != http.StatusBadRequest {
			t.Errorf("wallet %q: status = %d, want 400", wallet, res.status)
		}
	}
}
