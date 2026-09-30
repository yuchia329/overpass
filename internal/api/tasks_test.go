package api_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
)

func TestCreateTaskHoldsExactlyOnePrice(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 50_000)

	res := h.createTask(key)

	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %v", res.status, res.body)
	}
	if available, held := h.balance(key); available != 40_000 || held != 10_000 {
		t.Errorf("balance = %d/%d, want 40000/10000", available, held)
	}
}

func TestCreateTaskReturnsTaskIDSessionTokenAndDeadlines(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)
	before := time.Now()

	res := h.createTask(key)

	if id, _ := res.body["task_id"].(string); id == "" {
		t.Errorf("missing task_id: %v", res.body)
	}
	if tok, _ := res.body["session_token"].(string); tok == "" {
		t.Errorf("missing session_token: %v", res.body)
	}
	deadline, err := time.Parse(time.RFC3339Nano, res.body["claim_deadline"].(string))
	if err != nil {
		t.Fatalf("claim_deadline: %v", err)
	}
	if d := deadline.Sub(before); d < 90*time.Millisecond || d > time.Second {
		t.Errorf("claim_deadline is %v after request, want ~100ms", d)
	}
	if got := num(res.body["solve_window_ms"]); got != 100 {
		t.Errorf("solve_window_ms = %d, want 100", got)
	}
}

func TestEachTaskHasItsOwnSessionToken(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 20_000)

	a, b := h.createTask(key), h.createTask(key)

	if a.body["task_id"] == b.body["task_id"] || a.body["session_token"] == b.body["session_token"] {
		t.Errorf("tasks share id or token: %v %v", a.body, b.body)
	}
}

func TestCreateTaskWithInsufficientBalanceReturns402AndCreatesNoTask(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 9_999)

	res := h.createTask(key)

	if res.status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %v", res.status, res.body)
	}
	if got := num(res.body["available"]); got != 9_999 {
		t.Errorf("available = %d, want 9999", got)
	}
	if got := num(res.body["price"]); got != 10_000 {
		t.Errorf("price = %d, want 10000", got)
	}
	if got := res.body["service_wallet"]; got != testServiceWallet {
		t.Errorf("service_wallet = %v, want %s", got, testServiceWallet)
	}
	if _, ok := res.body["task_id"]; ok {
		t.Errorf("402 response carries a task_id: %v", res.body)
	}
	if available, held := h.balance(key); available != 9_999 || held != 0 {
		t.Errorf("balance = %d/%d, want 9999/0", available, held)
	}
	if tasks := h.tasks(key); len(tasks) != 0 {
		t.Errorf("tasks = %v, want none", tasks)
	}
}

func TestBalanceListsRecentTasks(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 20_000)
	first, second := h.createTask(key), h.createTask(key)

	tasks := h.tasks(key)

	if len(tasks) != 2 {
		t.Fatalf("tasks = %v, want 2", tasks)
	}
	if tasks[0]["task_id"] != second.body["task_id"] || tasks[1]["task_id"] != first.body["task_id"] {
		t.Errorf("tasks not newest first: %v", tasks)
	}
	if tasks[0]["state"] != "pending" {
		t.Errorf("state = %v, want pending", tasks[0]["state"])
	}
}

func TestConcurrentTaskCreationNeverOverspends(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 5*testPrice)

	const attempts = 20
	statuses := make(chan int, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- h.createTask(key).status
		}()
	}
	wg.Wait()
	close(statuses)

	created, refused := 0, 0
	for s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusPaymentRequired:
			refused++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if created != 5 || refused != 15 {
		t.Errorf("created/refused = %d/%d, want 5/15", created, refused)
	}
	if available, held := h.balance(key); available != 0 || held != 5*testPrice {
		t.Errorf("balance = %d/%d, want 0/50000", available, held)
	}
}

func TestUnclaimedTaskExpiresAndReleasesHoldInFull(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 30_000)
	id := h.createTask(key).body["task_id"].(string)

	h.eventually(time.Second, func() bool { return h.taskState(key, id) == "expired" }, "task expired")

	if available, held := h.balance(key); available != 30_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 30000/0", available, held)
	}
}

func TestTaskStaysPendingWithinClaimWindow(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTask(key).body["task_id"].(string)

	time.Sleep(200 * time.Millisecond)

	if got := h.taskState(key, id); got != "pending" {
		t.Errorf("state = %s, want pending", got)
	}
	if available, held := h.balance(key); available != 0 || held != 10_000 {
		t.Errorf("balance = %d/%d, want 0/10000", available, held)
	}
}

func TestTaskOverdueAcrossRestartExpiresOnStartup(t *testing.T) {
	// Long enough that the first restart always beats the claim timer.
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = 500 * time.Millisecond })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTask(key).body["task_id"].(string)

	h.restart() // stops the claim timer before it fires
	if got := h.taskState(key, id); got != "pending" {
		t.Fatalf("state after first restart = %s, want pending", got)
	}
	time.Sleep(600 * time.Millisecond)
	h.restart()

	h.eventually(time.Second, func() bool { return h.taskState(key, id) == "expired" }, "task expired after restart")
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestPriceAndServiceWalletAreConfigurable(t *testing.T) {
	const otherWallet = "11111111111111111111111111111111"
	h := newHarness(t, func(c *api.Config) {
		c.Price = 25_000
		c.ServiceWallet = otherWallet
		c.ClaimWindow = time.Hour
	})
	key := h.register()
	h.credit(key, 30_000)

	if res := h.createTask(key); res.status != http.StatusCreated {
		t.Fatalf("first task: status = %d, want 201", res.status)
	}
	if available, held := h.balance(key); available != 5_000 || held != 25_000 {
		t.Errorf("balance = %d/%d, want 5000/25000", available, held)
	}
	res := h.createTask(key)
	if res.status != http.StatusPaymentRequired || num(res.body["price"]) != 25_000 || res.body["service_wallet"] != otherWallet {
		t.Errorf("second task: status %d body %v, want 402 with price 25000 and %s", res.status, res.body, otherWallet)
	}
}

func TestCreateTaskRejectsMissingOrInvalidPageURL(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)

	for _, pageURL := range []string{"", "not a url", "ftp://example.com/x", "/relative"} {
		res := h.do("POST", "/v1/tasks", key, map[string]any{"page_url": pageURL})
		if res.status != http.StatusBadRequest {
			t.Errorf("page_url %q: status = %d, want 400", pageURL, res.status)
		}
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestInvalidConfigIsRejectedAtStartup(t *testing.T) {
	bad := []func(*api.Config){
		func(c *api.Config) { c.Price = 0 },
		func(c *api.Config) { c.ClaimWindow = 0 },
		func(c *api.Config) { c.SolveWindow = -time.Second },
		func(c *api.Config) { c.ServiceWallet = "not-a-wallet" },
		func(c *api.Config) { c.ChallengeTTL = 0 },
	}
	for i, mutate := range bad {
		cfg := api.Config{
			DBPath:        t.TempDir() + "/overpass.db",
			ClaimWindow:   time.Second,
			SolveWindow:   time.Second,
			Price:         testPrice,
			ServiceWallet: testServiceWallet,
			ChallengeTTL:  time.Second,
		}
		mutate(&cfg)
		if srv, err := api.New(cfg); err == nil {
			srv.Close()
			t.Errorf("config %d: api.New succeeded, want error", i)
		}
	}
}
