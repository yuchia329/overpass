package api_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
	"github.com/yuchia329/overpass/internal/solana"
)

const (
	testMinWithdrawal = 10_000
	testAccountFee    = 5_000
	testEarning       = testPrice * 80 / 100
	tokenProgram      = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	ataProgram        = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	testBlockhash     = "BWHQNZvWBEDkdHQ5iqFiG26B5wVA3aN2ZKhL8ZWFuU8e"
)

// fakeChain is a Solana JSON-RPC node for payouts. It records every
// transaction sent and settles each as the test says.
type fakeChain struct {
	t   *testing.T
	url string

	mu       sync.Mutex
	accounts map[string]bool             // token accounts that exist
	sent     [][]byte                    // transactions received, in order
	statuses map[string]*json.RawMessage // by signature: nil until it lands
	confirm  bool                        // land every transaction sent, confirmed
	reject   string                      // refuse sends with this message
	height   uint64                      // current block height
}

const lastValid = 1_000

func newFakeChain(t *testing.T) *fakeChain {
	t.Helper()
	f := &fakeChain{t: t, accounts: map[string]bool{}, statuses: map[string]*json.RawMessage{}, confirm: true, height: 1}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// option turns Withdrawals on, paid from hot through this node.
func (f *fakeChain) option(hot ed25519.PrivateKey) func(*api.Config) {
	return func(c *api.Config) {
		c.PayoutKey = hot
		c.PayoutRPCURL = f.url
		c.MinWithdrawal = testMinWithdrawal
		c.AccountFee = testAccountFee
		c.PayoutPollInterval = 10 * time.Millisecond
	}
}

func (f *fakeChain) set(change func(f *fakeChain)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeChain) sentTxs() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func (f *fakeChain) serve(w http.ResponseWriter, r *http.Request) {
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
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	ctx := map[string]any{"slot": 1}
	switch req.Method {
	case "getAccountInfo":
		var value any
		if f.accounts[req.Params[0].(string)] {
			value = map[string]any{"lamports": 2_039_280, "owner": tokenProgram}
		}
		reply(map[string]any{"context": ctx, "value": value})
	case "getLatestBlockhash":
		reply(map[string]any{"context": ctx, "value": map[string]any{"blockhash": testBlockhash, "lastValidBlockHeight": lastValid}})
	case "getBlockHeight":
		reply(f.height)
	case "sendTransaction":
		tx, err := base64.StdEncoding.DecodeString(req.Params[0].(string))
		if err != nil {
			f.t.Errorf("sendTransaction: %v", err)
		}
		f.sent = append(f.sent, tx)
		if f.reject != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32002, "message": f.reject}})
			return
		}
		sig := solana.EncodeBase58(tx[1:65])
		if f.confirm {
			ok := json.RawMessage(`{"confirmationStatus":"confirmed","err":null}`)
			f.statuses[sig] = &ok
		}
		reply(sig)
	case "getSignatureStatuses":
		var value []any
		for _, sig := range req.Params[0].([]any) {
			if st := f.statuses[sig.(string)]; st != nil {
				value = append(value, *st)
			} else {
				value = append(value, nil)
			}
		}
		reply(map[string]any{"context": ctx, "value": value})
	default:
		f.t.Errorf("unexpected RPC method %s", req.Method)
	}
}

// payment is what a payout transaction does, decoded from its wire bytes.
type payment struct {
	signer         string
	signatureValid bool
	createsAccount bool // for the recipient
	source, mint   string
	destination    string
	amount         uint64
	decimals       byte
}

// decodePayment parses a legacy transaction with one signer.
func decodePayment(t *testing.T, tx []byte) payment {
	t.Helper()
	if tx[0] != 1 {
		t.Fatalf("transaction has %d signatures, want 1", tx[0])
	}
	sig, msg := tx[1:65], tx[65:]
	nKeys := int(msg[3])
	keys := make([]string, nKeys)
	for i := range keys {
		keys[i] = solana.EncodeBase58(msg[4+32*i : 36+32*i])
	}
	p := payment{signer: keys[0]}
	pub, _ := solana.DecodeBase58(keys[0])
	p.signatureValid = ed25519.Verify(pub, msg, sig)
	at := 4 + 32*nKeys
	if got := solana.EncodeBase58(msg[at : at+32]); got != testBlockhash {
		t.Errorf("blockhash = %s, want %s", got, testBlockhash)
	}
	at += 32
	nIx := int(msg[at])
	at++
	for range nIx {
		program := keys[msg[at]]
		nAcc := int(msg[at+1])
		accs := make([]string, nAcc)
		for i := range accs {
			accs[i] = keys[msg[at+2+i]]
		}
		at += 2 + nAcc
		nData := int(msg[at])
		data := msg[at+1 : at+1+nData]
		at += 1 + nData
		switch {
		case program == ataProgram && len(data) == 1 && data[0] == 1:
			p.createsAccount = true
		case program == tokenProgram && data[0] == 12:
			p.source, p.mint, p.destination = accs[0], accs[1], accs[2]
			p.amount = binary.LittleEndian.Uint64(data[1:9])
			p.decimals = data[9]
		default:
			t.Errorf("unexpected instruction to %s: %x", program, data)
		}
	}
	return p
}

// earn has solver claim and Solve n Tasks.
func (h *harness) earn(solver wallet, n int) {
	h.t.Helper()
	key := h.register()
	h.credit(key, int64(n)*testPrice)
	s := h.connectSolverAs(solver.address)
	for range n {
		created := h.createTask(key)
		id := created.body["task_id"].(string)
		b := h.connectBridge(created)
		s.mustClaim(id)
		b.send(map[string]any{"type": "solved"})
		b.next("solved")
		s.next("task_solved", id)
	}
}

func (h *harness) earnings(address string) map[string]any {
	h.t.Helper()
	res := h.do("GET", "/v1/solvers/"+address+"/earnings", "", nil)
	if res.status != http.StatusOK {
		h.t.Fatalf("earnings: status %d %v", res.status, res.body)
	}
	return res.body
}

// withdraw signs a fresh withdrawal challenge as signer and withdraws for w.
func (h *harness) withdrawAs(w, signer wallet) response {
	h.t.Helper()
	c := h.do("POST", "/v1/withdrawals/challenge", "", map[string]any{"wallet": w.address})
	if c.status != http.StatusCreated {
		h.t.Fatalf("withdrawal challenge: status %d %v", c.status, c.body)
	}
	return h.do("POST", "/v1/withdrawals", "", map[string]any{
		"wallet":    w.address,
		"nonce":     c.body["nonce"],
		"signature": signer.sign(c.body["message"].(string)),
	})
}

func (h *harness) withdraw(w wallet) response { return h.withdrawAs(w, w) }

// latestWithdrawal waits until w's newest Withdrawal is in state.
func (h *harness) latestWithdrawal(w wallet, state string) map[string]any {
	h.t.Helper()
	var latest map[string]any
	h.eventually(2*time.Second, func() bool {
		list := h.earnings(w.address)["withdrawals"].([]any)
		if len(list) == 0 {
			return false
		}
		latest = list[0].(map[string]any)
		return latest["state"] == state
	}, "withdrawal reaches "+state)
	return latest
}

func newHotWallet(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	w := newWallet(t)
	return w.key, w.address
}

func ata(t *testing.T, owner string) string {
	t.Helper()
	a, err := solana.AssociatedTokenAddress(owner, solana.USDCMint)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestEarningsAddUpTheSolversSolvedTasks(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	solver := newWallet(t)

	h.earn(solver, 2)

	e := h.earnings(solver.address)
	if num(e["available"]) != 2*testEarning {
		t.Errorf("available = %v, want %d", e["available"], 2*testEarning)
	}
	if e["withdrawals_enabled"] != false {
		t.Errorf("withdrawals_enabled = %v, want false with no payout key", e["withdrawals_enabled"])
	}
}

func TestWithdrawalPaysAllAvailableEarningsToTheSolversWallet(t *testing.T) {
	chain := newFakeChain(t)
	hot, hotAddress := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	chain.set(func(f *fakeChain) { f.accounts[ata(t, solver.address)] = true })
	h.earn(solver, 2)

	res := h.withdraw(solver)

	if res.status != http.StatusCreated {
		t.Fatalf("withdraw: status %d %v", res.status, res.body)
	}
	if num(res.body["payout"]) != 2*testEarning || num(res.body["account_fee"]) != 0 {
		t.Errorf("withdrawal = %v, want payout %d and no account fee", res.body, 2*testEarning)
	}
	w := h.latestWithdrawal(solver, "confirmed")
	txs := chain.sentTxs()
	if len(txs) != 1 {
		t.Fatalf("sent %d transactions, want 1", len(txs))
	}
	p := decodePayment(t, txs[0])
	if p.signer != hotAddress || !p.signatureValid {
		t.Errorf("signed by %s (valid %v), want the hot wallet %s", p.signer, p.signatureValid, hotAddress)
	}
	if p.createsAccount {
		t.Error("created the Solver's USDC token account, which exists")
	}
	if p.source != ata(t, hotAddress) || p.destination != ata(t, solver.address) || p.mint != solana.USDCMint {
		t.Errorf("transfer %s -> %s of %s, want the hot wallet's USDC account to the Solver's", p.source, p.destination, p.mint)
	}
	if p.amount != 2*testEarning || p.decimals != 6 {
		t.Errorf("amount = %d (decimals %d), want %d (6)", p.amount, p.decimals, 2*testEarning)
	}
	if w["signature"] != solana.EncodeBase58(txs[0][1:65]) {
		t.Errorf("signature = %v, want the transaction's", w["signature"])
	}
	if a := num(h.earnings(solver.address)["available"]); a != 0 {
		t.Errorf("available after withdrawal = %d, want 0", a)
	}
}

func TestWithdrawalToAWalletWithoutAUSDCAccountCreatesItAndKeepsBackItsFee(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	h.earn(solver, 2)

	res := h.withdraw(solver)

	if res.status != http.StatusCreated {
		t.Fatalf("withdraw: status %d %v", res.status, res.body)
	}
	if num(res.body["amount"]) != 2*testEarning || num(res.body["account_fee"]) != testAccountFee {
		t.Errorf("withdrawal = %v, want amount %d with account fee %d", res.body, 2*testEarning, testAccountFee)
	}
	h.latestWithdrawal(solver, "confirmed")
	p := decodePayment(t, chain.sentTxs()[0])
	if !p.createsAccount {
		t.Error("did not create the Solver's USDC token account")
	}
	if p.amount != 2*testEarning-testAccountFee {
		t.Errorf("amount = %d, want %d", p.amount, 2*testEarning-testAccountFee)
	}
}

func TestWithdrawalBelowTheMinimumIsRefused(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	h.earn(solver, 1) // 8000 available: under the 10000 minimum

	res := h.withdraw(solver)

	if res.status != http.StatusUnprocessableEntity || res.body["error"] != "below_minimum" {
		t.Fatalf("withdraw = %d %v, want 422 below_minimum", res.status, res.body)
	}
	if num(res.body["available"]) != testEarning || num(res.body["minimum"]) != testMinWithdrawal {
		t.Errorf("body = %v", res.body)
	}
	if len(chain.sentTxs()) != 0 || num(h.earnings(solver.address)["available"]) != testEarning {
		t.Error("a refused withdrawal moved money")
	}
}

func TestAccountFeeCountsTowardTheMinimum(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) {
		c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour
	}, chain.option(hot), func(c *api.Config) { c.AccountFee = 8_000 })
	solver := newWallet(t)
	h.earn(solver, 2) // 16000, less an 8000 account fee, is under the minimum

	res := h.withdraw(solver)

	if res.status != http.StatusUnprocessableEntity || num(res.body["account_fee"]) != 8_000 {
		t.Fatalf("withdraw = %d %v, want 422 with account_fee 8000", res.status, res.body)
	}
}

func TestWithdrawalNeedsTheWalletsOwnFreshSignature(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver, thief := newWallet(t), newWallet(t)
	chain.set(func(f *fakeChain) { f.accounts[ata(t, solver.address)] = true })
	h.earn(solver, 2)

	if res := h.withdrawAs(solver, thief); res.status != http.StatusUnauthorized {
		t.Errorf("signed by another wallet: status %d, want 401", res.status)
	}

	// A registration challenge, signed by the wallet itself, cannot authorize a withdrawal.
	nonce, message := h.challenge(solver.address)
	res := h.do("POST", "/v1/withdrawals", "", map[string]any{"wallet": solver.address, "nonce": nonce, "signature": solver.sign(message)})
	if res.status != http.StatusUnauthorized {
		t.Errorf("registration challenge: status %d, want 401", res.status)
	}

	// A withdrawal challenge is single use.
	c := h.do("POST", "/v1/withdrawals/challenge", "", map[string]any{"wallet": solver.address})
	body := map[string]any{"wallet": solver.address, "nonce": c.body["nonce"], "signature": solver.sign(c.body["message"].(string))}
	if first := h.do("POST", "/v1/withdrawals", "", body); first.status != http.StatusCreated {
		t.Fatalf("first use: status %d %v", first.status, first.body)
	}
	if again := h.do("POST", "/v1/withdrawals", "", body); again.status != http.StatusUnauthorized {
		t.Errorf("reused challenge: status %d, want 401", again.status)
	}
	// And a withdrawal challenge cannot register a Customer.
	c = h.do("POST", "/v1/withdrawals/challenge", "", map[string]any{"wallet": solver.address})
	reg := h.do("POST", "/v1/customers", "", map[string]any{"wallet": solver.address, "nonce": c.body["nonce"], "signature": solver.sign(c.body["message"].(string))})
	if reg.status != http.StatusUnauthorized {
		t.Errorf("registering with a withdrawal challenge: status %d, want 401", reg.status)
	}
}

func TestOnlyOneWithdrawalIsOpenAtATime(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	chain.set(func(f *fakeChain) { f.accounts[ata(t, solver.address)] = true; f.confirm = false })
	h.earn(solver, 2)
	if res := h.withdraw(solver); res.status != http.StatusCreated {
		t.Fatalf("first withdraw: status %d %v", res.status, res.body)
	}
	h.latestWithdrawal(solver, "sent")
	h.earn(solver, 2) // more Earnings while the first is on its way

	res := h.withdraw(solver)

	if res.status != http.StatusConflict || res.body["error"] != "withdrawal_open" {
		t.Errorf("second withdraw = %d %v, want 409 withdrawal_open", res.status, res.body)
	}
	if a := num(h.earnings(solver.address)["available"]); a != 2*testEarning {
		t.Errorf("available = %d, want the %d earned since", a, 2*testEarning)
	}
}

func TestRefusedPayoutFailsOnceItsBlockhashExpiresAndFreesTheEarnings(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	chain.set(func(f *fakeChain) {
		f.accounts[ata(t, solver.address)] = true
		f.reject = "Transaction simulation failed: insufficient funds"
	})
	h.earn(solver, 2)
	h.withdraw(solver)

	// Refused, but it could still land until its blockhash expires.
	h.latestWithdrawal(solver, "sent")
	time.Sleep(50 * time.Millisecond)
	if a := num(h.earnings(solver.address)["available"]); a != 0 {
		t.Errorf("available while it could still land = %d, want 0", a)
	}
	chain.set(func(f *fakeChain) { f.height = lastValid + 1 })

	w := h.latestWithdrawal(solver, "failed")
	if w["error"] == "" {
		t.Error("failed withdrawal shows no reason")
	}
	if a := num(h.earnings(solver.address)["available"]); a != 2*testEarning {
		t.Errorf("available after failure = %d, want %d back", a, 2*testEarning)
	}
	if n := len(chain.sentTxs()); n != 1 {
		t.Errorf("sent %d transactions, want 1: a payout is never resent", n)
	}
}

func TestPayoutThatFailsOnChainFailsTheWithdrawal(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	chain.set(func(f *fakeChain) { f.accounts[ata(t, solver.address)] = true; f.confirm = false })
	h.earn(solver, 2)
	h.withdraw(solver)
	h.latestWithdrawal(solver, "sent")

	chain.set(func(f *fakeChain) {
		failed := json.RawMessage(`{"confirmationStatus":"confirmed","err":{"InstructionError":[0,{"Custom":1}]}}`)
		f.statuses[solana.EncodeBase58(f.sent[0][1:65])] = &failed
	})

	h.latestWithdrawal(solver, "failed")
	if a := num(h.earnings(solver.address)["available"]); a != 2*testEarning {
		t.Errorf("available = %d, want %d back", a, 2*testEarning)
	}
}

func TestSentWithdrawalIsSettledAfterARestartWithoutBeingResent(t *testing.T) {
	chain := newFakeChain(t)
	hot, _ := newHotWallet(t)
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }, chain.option(hot))
	solver := newWallet(t)
	chain.set(func(f *fakeChain) { f.accounts[ata(t, solver.address)] = true; f.confirm = false })
	h.earn(solver, 2)
	h.withdraw(solver)
	h.latestWithdrawal(solver, "sent")

	h.restart()
	chain.set(func(f *fakeChain) {
		ok := json.RawMessage(`{"confirmationStatus":"finalized","err":null}`)
		f.statuses[solana.EncodeBase58(f.sent[0][1:65])] = &ok
	})

	h.latestWithdrawal(solver, "confirmed")
	if n := len(chain.sentTxs()); n != 1 {
		t.Errorf("sent %d transactions, want 1", n)
	}
}

func TestWithdrawalsAreRefusedWithoutAPayoutKey(t *testing.T) {
	h := newHarness(t)
	solver := newWallet(t)

	res := h.do("POST", "/v1/withdrawals/challenge", "", map[string]any{"wallet": solver.address})

	if res.status != http.StatusServiceUnavailable || res.body["error"] != "withdrawals_disabled" {
		t.Errorf("challenge = %d %v, want 503 withdrawals_disabled", res.status, res.body)
	}
}
