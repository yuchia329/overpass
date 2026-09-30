package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
	"github.com/yuchia329/overpass/internal/solana"
)

const (
	testServiceWallet = "CW82aTEMcqsqwLaxppzrpEnM41bC83R8JUXpZgYcrhGt"
	testPrice         = 10_000
)

// wallet is a Solana keypair a test Customer signs with.
type wallet struct {
	address string
	key     ed25519.PrivateKey
}

func newWallet(t *testing.T) wallet {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return wallet{address: solana.EncodeBase58(pub), key: priv}
}

func (w wallet) sign(message string) string {
	return solana.EncodeBase58(ed25519.Sign(w.key, []byte(message)))
}

// harness runs the real backend on an httptest server with short windows.
type harness struct {
	t    *testing.T
	cfg  api.Config
	url  string
	stop func()
}

func newHarness(t *testing.T, mutate ...func(*api.Config)) *harness {
	t.Helper()
	cfg := api.Config{
		DBPath:        filepath.Join(t.TempDir(), "overpass.db"),
		ClaimWindow:   100 * time.Millisecond,
		SolveWindow:   100 * time.Millisecond,
		Price:         testPrice,
		ServiceWallet: testServiceWallet,
		DevMode:       true,
		ChallengeTTL:  time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	h := &harness{t: t, cfg: cfg}
	h.start()
	t.Cleanup(func() { h.stop() })
	return h
}

func (h *harness) start() {
	h.t.Helper()
	srv, err := api.New(h.cfg)
	if err != nil {
		h.t.Fatalf("api.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	h.url = ts.URL
	h.stop = func() {
		ts.Close()
		srv.Close()
	}
}

// restart stops the backend and starts a new one on the same database.
func (h *harness) restart() {
	h.t.Helper()
	h.stop()
	h.start()
}

type response struct {
	status int
	body   map[string]any
}

// do sends a JSON request. apiKey may be empty.
func (h *harness) do(method, path, apiKey string, body any) response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.url+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{status: res.StatusCode}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

// challenge asks for a registration challenge and returns its nonce and message.
func (h *harness) challenge(address string) (nonce, message string) {
	h.t.Helper()
	res := h.do("POST", "/v1/customers/challenge", "", map[string]any{"wallet": address})
	if res.status != http.StatusCreated {
		h.t.Fatalf("challenge: status %d body %v", res.status, res.body)
	}
	return res.body["nonce"].(string), res.body["message"].(string)
}

// registerAs proves ownership of w and returns the raw registration response.
func (h *harness) registerAs(w wallet) response {
	h.t.Helper()
	nonce, message := h.challenge(w.address)
	return h.do("POST", "/v1/customers", "", map[string]any{
		"wallet": w.address, "nonce": nonce, "signature": w.sign(message),
	})
}

// register registers a fresh wallet and returns its API key.
func (h *harness) register() string {
	h.t.Helper()
	res := h.registerAs(newWallet(h.t))
	if res.status != http.StatusCreated {
		h.t.Fatalf("register: status %d body %v", res.status, res.body)
	}
	return res.body["api_key"].(string)
}

func (h *harness) credit(apiKey string, amount int64) {
	h.t.Helper()
	res := h.do("POST", "/v1/dev/credit", apiKey, map[string]any{"amount": amount})
	if res.status != http.StatusOK {
		h.t.Fatalf("credit: status %d body %v", res.status, res.body)
	}
}

// balance returns available and held Balance.
func (h *harness) balance(apiKey string) (available, held int64) {
	h.t.Helper()
	res := h.do("GET", "/v1/balance", apiKey, nil)
	if res.status != http.StatusOK {
		h.t.Fatalf("balance: status %d body %v", res.status, res.body)
	}
	return num(res.body["available"]), num(res.body["held"])
}

// tasks returns the Customer's recent Tasks from the Balance endpoint.
func (h *harness) tasks(apiKey string) []map[string]any {
	h.t.Helper()
	res := h.do("GET", "/v1/balance", apiKey, nil)
	if res.status != http.StatusOK {
		h.t.Fatalf("tasks: status %d body %v", res.status, res.body)
	}
	raw, _ := res.body["tasks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

// taskState returns the state of one of the Customer's recent Tasks.
func (h *harness) taskState(apiKey, taskID string) string {
	h.t.Helper()
	for _, tk := range h.tasks(apiKey) {
		if tk["task_id"] == taskID {
			s, _ := tk["state"].(string)
			return s
		}
	}
	h.t.Fatalf("task %s not in recent tasks", taskID)
	return ""
}

func (h *harness) createTask(apiKey string) response {
	h.t.Helper()
	return h.do("POST", "/v1/tasks", apiKey, map[string]any{"page_url": "https://www.google.com/recaptcha/api2/demo"})
}

// eventually polls cond until it holds or the deadline passes.
func (h *harness) eventually(within time.Duration, cond func() bool, msg string) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("condition not met within %v: %s", within, msg)
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}
