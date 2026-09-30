package api_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
	"github.com/yuchia329/overpass/internal/solana"
)

// The recorded fixture is the first real Deposit: `pay send 1` from the demo
// wallet to the service wallet on mainnet (memos "Transfer" and "Network fee
// (include account creation)"). The pay.sh fee payer BcdwLA62… signs first and
// pays gas; the sender's USDC covers the transfer plus a USDC network fee that
// also pays to create the service wallet's USDC token account. So neither the
// signer nor the sender's spend identifies the Deposit.
const (
	fixturePath          = "testdata/pay_send.json"
	fixtureSignature     = "5oFKCc6aoEbkofbUjqugpngQP2bXe4uh3xB3hQ2nTq7qw5KS8rf87rtfcuaFjLbB3bJz4V5EGKa5pJje9XeARgf6"
	fixtureSender        = "FAGwq2UkSAp7mTcrgRGyvXCDnAmmfs6DpUtVoHmBYFey"
	fixtureReceived      = 1_000_000 // service wallet's USDC increase
	fixtureSenderSpent   = 1_175_913 // transfer plus the USDC network fee
	testServiceWalletATA = "81Qfr2NMVJeqN6Y89GdXnE9cpeaNuJZU1iEgLGG8G5ov"
)

// fakeRPC is a Solana JSON-RPC server that serves recorded Deposits to the
// service wallet's USDC token account. Every poll sees every Deposit again,
// and it rejects `until` the way lagging mainnet nodes do.
type fakeRPC struct {
	t   *testing.T
	url string

	mu       sync.Mutex
	sigs     []string                   // newest first
	txs      map[string]json.RawMessage // getTransaction results by signature
	failures int                        // upcoming requests to fail
	polls    int                        // getSignaturesForAddress calls served
}

func newFakeRPC(t *testing.T) *fakeRPC {
	t.Helper()
	f := &fakeRPC{t: t, txs: map[string]json.RawMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// option points the backend's Deposit poller at this server.
func (f *fakeRPC) option() func(*api.Config) {
	return func(c *api.Config) {
		c.RPCURL = f.url
		c.PollInterval = 10 * time.Millisecond
	}
}

// deposit adds the recorded `pay send` as if sender sent it, under a fresh
// signature, and returns that signature. Test wallets stand in for the demo
// wallet because they can sign registration challenges.
func (f *fakeRPC) deposit(sender string) string {
	f.t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		f.t.Fatal(err)
	}
	sig := randomSignature(f.t)
	tx := strings.NewReplacer(
		fixtureSignature, sig,
		fixtureSender, sender,
	).Replace(string(raw))
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(tx), &res); err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sigs = append([]string{sig}, f.sigs...)
	f.txs[sig] = res.Result
	return sig
}

// failNext makes the next n requests fail, alternating rate limits and server errors.
func (f *fakeRPC) failNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = n
}

func (f *fakeRPC) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// waitPolls waits until n more polls have started. Once two have, everything
// added before the call has been fully processed.
func (f *fakeRPC) waitPolls(h *harness, n int) {
	h.t.Helper()
	start := f.pollCount()
	h.eventually(2*time.Second, func() bool { return f.pollCount() >= start+n }, "poller keeps polling")
}

func (f *fakeRPC) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params []any           `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		if f.failures%2 == 0 {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
		} else {
			http.Error(w, "upstream down", http.StatusInternalServerError)
		}
		return
	}
	var result any
	switch req.Method {
	case "getSignaturesForAddress":
		f.polls++
		if req.Params[0] != testServiceWalletATA {
			f.t.Errorf("polled %v, want the service wallet's USDC token account %s", req.Params[0], testServiceWalletATA)
		}
		if opts, ok := req.Params[1].(map[string]any); ok && opts["until"] != nil {
			// Like a load-balanced mainnet node that has not indexed that transaction yet.
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32020, "message": fmt.Sprintf("Transaction %v not found", opts["until"])}})
			return
		}
		list := []map[string]any{}
		for _, s := range f.sigs {
			list = append(list, map[string]any{"signature": s, "err": nil, "confirmationStatus": "confirmed"})
		}
		result = list
	case "getTransaction":
		tx, ok := f.txs[req.Params[0].(string)]
		if !ok {
			tx = json.RawMessage("null")
		}
		result = tx
	default:
		f.t.Errorf("unexpected RPC method %s", req.Method)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func randomSignature(t *testing.T) string {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return solana.EncodeBase58(b)
}

// registerKey registers w and returns its API key.
func (h *harness) registerKey(w wallet) string {
	h.t.Helper()
	res := h.registerAs(w)
	if res.status != http.StatusCreated && res.status != http.StatusOK {
		h.t.Fatalf("register: status %d body %v", res.status, res.body)
	}
	return res.body["api_key"].(string)
}

// deposits returns the Customer's recent Deposits from the Balance endpoint.
func (h *harness) deposits(apiKey string) []map[string]any {
	h.t.Helper()
	res := h.do("GET", "/v1/balance", apiKey, nil)
	if res.status != http.StatusOK {
		h.t.Fatalf("deposits: status %d body %v", res.status, res.body)
	}
	raw, _ := res.body["deposits"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func (h *harness) available(apiKey string) int64 {
	h.t.Helper()
	available, _ := h.balance(apiKey)
	return available
}

func TestDepositCreditsTheSourceTokenAccountOwnerNotTheFeePayer(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	key := h.registerKey(sender)

	rpc.deposit(sender.address)

	h.eventually(2*time.Second, func() bool { return h.available(key) != 0 }, "deposit credited")
	if got := h.available(key); got != fixtureReceived {
		t.Errorf("available = %d, want %d (the service wallet's increase, not the %d the sender spent)",
			got, fixtureReceived, fixtureSenderSpent)
	}
}

func TestDepositSeenAgainIsCreditedOnce(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	key := h.registerKey(sender)
	rpc.deposit(sender.address)
	h.eventually(2*time.Second, func() bool { return h.available(key) != 0 }, "deposit credited")

	rpc.waitPolls(h, 3)
	h.restart() // a fresh poller re-reads every signature
	rpc.waitPolls(h, 3)

	if got := h.available(key); got != fixtureReceived {
		t.Errorf("available = %d, want %d", got, fixtureReceived)
	}
}

func TestUnattributedDepositIsCreditedWhenWalletRegisters(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	sig := rpc.deposit(sender.address)
	rpc.waitPolls(h, 2)

	key := h.registerKey(sender)

	if got := h.available(key); got != fixtureReceived {
		t.Errorf("available = %d, want %d", got, fixtureReceived)
	}
	if d := h.deposits(key); len(d) != 1 || d[0]["signature"] != sig {
		t.Errorf("deposits = %v, want the one Unattributed Deposit %s", d, sig)
	}
}

func TestReRegisteringDoesNotCreditUnattributedDepositsAgain(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	rpc.deposit(sender.address)
	rpc.waitPolls(h, 2)
	h.registerKey(sender)

	key := h.registerKey(sender) // rotates the API key

	if got := h.available(key); got != fixtureReceived {
		t.Errorf("available = %d, want %d", got, fixtureReceived)
	}
}

func TestDepositFromAnotherWalletIsNotCredited(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	key := h.registerKey(newWallet(t))

	rpc.deposit(newWallet(t).address)
	rpc.waitPolls(h, 2)

	if got := h.available(key); got != 0 {
		t.Errorf("available = %d, want 0", got)
	}
}

func TestBalanceListsRecentDeposits(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	key := h.registerKey(sender)

	first := rpc.deposit(sender.address)
	rpc.waitPolls(h, 2)
	second := rpc.deposit(sender.address)
	rpc.waitPolls(h, 2)

	d := h.deposits(key)
	if len(d) != 2 {
		t.Fatalf("deposits = %v, want 2", d)
	}
	if d[0]["signature"] != second || d[1]["signature"] != first {
		t.Errorf("deposits = %v, want newest first: %s then %s", d, second, first)
	}
	for _, dep := range d {
		if num(dep["amount"]) != fixtureReceived {
			t.Errorf("deposit %v: amount = %v, want %d", dep["signature"], dep["amount"], fixtureReceived)
		}
		if _, ok := dep["created_at"].(string); !ok {
			t.Errorf("deposit %v: missing created_at", dep["signature"])
		}
	}
	if got := h.available(key); got != 2*fixtureReceived {
		t.Errorf("available = %d, want %d", got, 2*fixtureReceived)
	}
}

func TestLaterDepositsAreCreditedWhenRPCNodesLag(t *testing.T) {
	rpc := newFakeRPC(t)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	key := h.registerKey(sender)
	rpc.deposit(sender.address)
	h.eventually(2*time.Second, func() bool { return h.available(key) == fixtureReceived }, "first deposit credited")

	rpc.deposit(sender.address)

	h.eventually(2*time.Second, func() bool { return h.available(key) == 2*fixtureReceived }, "second deposit credited")
}

func TestPollerSurvivesRPCErrors(t *testing.T) {
	rpc := newFakeRPC(t)
	rpc.failNext(6)
	h := newHarness(t, rpc.option())
	sender := newWallet(t)
	key := h.registerKey(sender)

	rpc.deposit(sender.address)

	h.eventually(5*time.Second, func() bool { return h.available(key) == fixtureReceived }, "deposit credited after RPC errors")
}

func TestPollIntervalMustBePositiveWithRPCURL(t *testing.T) {
	_, err := api.New(api.Config{
		DBPath:        filepath.Join(t.TempDir(), "overpass.db"),
		ClaimWindow:   time.Second,
		SolveWindow:   time.Second,
		Price:         testPrice,
		ServiceWallet: testServiceWallet,
		ChallengeTTL:  time.Second,
		RPCURL:        "http://127.0.0.1:1",
	})
	if err == nil {
		t.Error("want config error for zero poll interval")
	}
}
