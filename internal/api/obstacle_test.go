package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yuchia329/overpass/internal/api"
)

const obstacle = "Pass the reCAPTCHA check below the search form."

func (h *harness) createTaskWithObstacle(apiKey, obstacle string) response {
	h.t.Helper()
	return h.do("POST", "/v1/tasks", apiKey, map[string]any{
		"page_url": "https://www.google.com/recaptcha/api2/demo",
		"obstacle": obstacle,
	})
}

func TestQueueShowsTheObstacleTheAgentDescribed(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	live := h.connectSolver()

	id := h.createTaskWithObstacle(key, "  "+obstacle+" ").body["task_id"].(string)

	if m := live.next("task_added", id); m["obstacle"] != obstacle {
		t.Errorf("live task_added obstacle = %q, want %q", m["obstacle"], obstacle)
	}
	if m := h.connectSolver().next("task_added", id); m["obstacle"] != obstacle {
		t.Errorf("snapshot task_added obstacle = %q, want %q", m["obstacle"], obstacle)
	}
}

func TestTaskWithoutAnObstacleShowsNone(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)

	id := h.createTask(key).body["task_id"].(string)

	if m := h.connectSolver().next("task_added", id); m["obstacle"] != "" {
		t.Errorf("obstacle = %q, want empty", m["obstacle"])
	}
}

func TestObstacleSurvivesARestart(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	id := h.createTaskWithObstacle(key, obstacle).body["task_id"].(string)

	h.restart()

	if m := h.connectSolver().next("task_added", id); m["obstacle"] != obstacle {
		t.Errorf("obstacle after restart = %q, want %q", m["obstacle"], obstacle)
	}
}

func TestReconnectingSolverSeesTheObstacleOfTheirClaim(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTaskWithObstacle(key, obstacle)
	id := created.body["task_id"].(string)
	h.connectBridge(created)
	first := h.connectSolver()
	first.mustClaim(id)
	first.conn.Close(websocket.StatusNormalClosure, "")

	m := h.connectSolverAs(first.wallet).next("claimed", id)

	if m["obstacle"] != obstacle {
		t.Errorf("claimed obstacle = %q, want %q", m["obstacle"], obstacle)
	}
}

func TestCreateTaskRejectsAnInvalidObstacle(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)

	for _, o := range []string{strings.Repeat("x", 201), "Pass the check.\nThen book a flight.", "\x00"} {
		if res := h.createTaskWithObstacle(key, o); res.status != http.StatusBadRequest {
			t.Errorf("obstacle %q: status = %d, want 400", o, res.status)
		}
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestClaimingSolversDoneReachesTheBridge(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	s.send(map[string]any{"type": "done", "task_id": id})

	b.next("done")
}

func TestDoneFromASolverWithoutTheClaimIsNotForwarded(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	other := h.connectSolver()
	h.connectSolver().mustClaim(id)

	other.send(map[string]any{"type": "done", "task_id": id})

	if m := other.next("error", id); m["error"] != "not_your_claim" {
		t.Errorf("error = %v, want not_your_claim", m["error"])
	}
	b.never("done", 100*time.Millisecond)
}

func TestNotClearedReachesOnlyTheClaimingSolver(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	other := h.connectSolver()
	s := h.connectSolver()
	s.mustClaim(id)

	b.send(map[string]any{"type": "not_cleared"})

	s.next("not_cleared", id)
	other.never("not_cleared", id, 100*time.Millisecond)
}
