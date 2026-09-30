package api_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
)

func TestQueuePageLoadsFromBackend(t *testing.T) {
	h := newHarness(t)

	for path, want := range map[string]string{"/": "<html", "/queue.js": "/v1/queue"} {
		res, err := http.Get(h.url + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s: status %d, body lacks %q", path, res.StatusCode, want)
		}
	}
}

func TestQueueSocketRequiresASolanaWallet(t *testing.T) {
	h := newHarness(t)

	for _, wallet := range []string{"", "not-a-wallet"} {
		res, err := http.Get(h.url + "/v1/queue?wallet=" + wallet)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("wallet %q: status = %d, want 400", wallet, res.StatusCode)
		}
	}
}

func TestNewTaskAppearsOnEveryQueueWithoutRefresh(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	a, b := h.connectSolver(), h.connectSolver()

	id := h.createTask(key).body["task_id"].(string)

	for _, s := range []*solver{a, b} {
		m := s.next("task_added", id)
		if m["page_url"] != "https://www.google.com/recaptcha/api2/demo" {
			t.Errorf("page_url = %v", m["page_url"])
		}
		if _, ok := m["waited_ms"].(float64); !ok {
			t.Errorf("missing waited_ms: %v", m)
		}
	}
}

func TestQueueShowsTasksAlreadyWaitingWhenSolverConnects(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTask(key).body["task_id"].(string)
	time.Sleep(20 * time.Millisecond)

	m := h.connectSolver().next("task_added", id)

	if waited := num(m["waited_ms"]); waited < 20 {
		t.Errorf("waited_ms = %d, want >= 20", waited)
	}
}

func TestExpiredTaskIsRemovedFromEveryQueue(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)
	a, b := h.connectSolver(), h.connectSolver()

	id := h.createTask(key).body["task_id"].(string)

	for _, s := range []*solver{a, b} {
		s.next("task_added", id)
		s.next("task_removed", id)
	}
	if got := h.taskState(key, id); got != "expired" {
		t.Errorf("state = %s, want expired", got)
	}
}

func TestClaimedTaskIsRemovedFromEveryQueue(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	a, b := h.connectSolver(), h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	a.next("task_added", id)
	b.next("task_added", id)

	reply := a.claim(id)

	if reply["type"] != "claimed" {
		t.Fatalf("claim reply = %v, want claimed", reply)
	}
	if got := num(reply["solve_window_ms"]); got != time.Hour.Milliseconds() {
		t.Errorf("solve_window_ms = %d, want %d", got, time.Hour.Milliseconds())
	}
	a.next("task_removed", id)
	b.next("task_removed", id)
	if got := h.taskState(key, id); got != "claimed" {
		t.Errorf("state = %s, want claimed", got)
	}
}

func TestOnlyFirstOfConcurrentClaimsWins(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	solvers := make([]*solver, 5)
	for i := range solvers {
		solvers[i] = h.connectSolver()
	}
	id := h.createTask(key).body["task_id"].(string)
	for _, s := range solvers {
		s.next("task_added", id)
	}

	replies := make(chan map[string]any, len(solvers))
	var wg sync.WaitGroup
	for _, s := range solvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies <- s.claim(id)
		}()
	}
	wg.Wait()
	close(replies)

	won, lost := 0, 0
	for r := range replies {
		switch {
		case r["type"] == "claimed":
			won++
		case r["type"] == "claim_failed" && r["error"] == "already_claimed":
			lost++
		default:
			t.Errorf("unexpected reply %v", r)
		}
	}
	if won != 1 || lost != len(solvers)-1 {
		t.Errorf("won/lost = %d/%d, want 1/%d", won, lost, len(solvers)-1)
	}
}

func TestClaimingAnExpiredTaskFails(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)
	s := h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	s.next("task_removed", id)

	if reply := s.claim(id); reply["type"] != "claim_failed" || reply["error"] != "expired" {
		t.Errorf("claim reply = %v, want claim_failed expired", reply)
	}
	if reply := s.claim("tsk_nope"); reply["type"] != "claim_failed" || reply["error"] != "unknown_task" {
		t.Errorf("claim reply = %v, want claim_failed unknown_task", reply)
	}
}

func TestClaimStopsTheClaimWindow(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.SolveWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	s := h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	s.claim(id)

	time.Sleep(200 * time.Millisecond) // past the 100ms claim window

	if got := h.taskState(key, id); got != "claimed" {
		t.Errorf("state = %s, want claimed", got)
	}
	if available, held := h.balance(key); available != 0 || held != 10_000 {
		t.Errorf("balance = %d/%d, want 0/10000", available, held)
	}
}

func TestClaimedTaskFailsAfterSolveWindowAndReleasesHold(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	s := h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	s.claim(id)

	m := s.next("task_failed", id)

	if m["reason"] != "solve_window" {
		t.Errorf("reason = %v, want solve_window", m["reason"])
	}
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestGivingUpFailsTaskAndReleasesHold(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	s := h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	s.claim(id)

	s.send(map[string]any{"type": "give_up", "task_id": id})

	if m := s.next("task_failed", id); m["reason"] != "gave_up" {
		t.Errorf("reason = %v, want gave_up", m["reason"])
	}
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestOnlyTheClaimingSolverCanGiveUp(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	claimer, other := h.connectSolver(), h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	claimer.claim(id)

	other.send(map[string]any{"type": "give_up", "task_id": id})

	if m := other.next("error", id); m["error"] != "not_your_claim" {
		t.Errorf("error = %v, want not_your_claim", m["error"])
	}
	if got := h.taskState(key, id); got != "claimed" {
		t.Errorf("state = %s, want claimed", got)
	}
}

func TestFailedTaskIsNeverRequeued(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	claimer := h.connectSolver()
	id := h.createTask(key).body["task_id"].(string)
	claimer.claim(id)
	claimer.send(map[string]any{"type": "give_up", "task_id": id})
	claimer.next("task_failed", id)

	late := h.connectSolver()

	late.never("task_added", id, 100*time.Millisecond)
	if reply := late.claim(id); reply["type"] != "claim_failed" || reply["error"] != "already_claimed" {
		t.Errorf("claim reply = %v, want claim_failed already_claimed", reply)
	}
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
}

func TestPendingTaskIsStillQueuedAfterRestart(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTask(key).body["task_id"].(string)

	h.restart()

	h.connectSolver().next("task_added", id)
}

func TestClaimedTaskOverdueAcrossRestartFailsOnStartup(t *testing.T) {
	// Long enough that the first restart always beats the solve timer.
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, 500*time.Millisecond })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTask(key).body["task_id"].(string)
	h.connectSolver().claim(id)

	h.restart() // stops the solve timer before it fires
	if got := h.taskState(key, id); got != "claimed" {
		t.Fatalf("state after first restart = %s, want claimed", got)
	}
	time.Sleep(600 * time.Millisecond)
	h.restart()

	h.eventually(time.Second, func() bool { return h.taskState(key, id) == "failed" }, "task failed after restart")
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}
