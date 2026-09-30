package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yuchia329/overpass/internal/api"
)

func TestSessionTokenCannotJoinAnotherTasksSession(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 20_000)
	a, b := h.createTask(key), h.createTask(key)

	for name, token := range map[string]string{
		"other task's token": a.body["session_token"].(string),
		"made-up token":      "st_nope",
		"no token":           "",
	} {
		if conn, status := h.dialBridge(b.body["task_id"].(string), token); conn != nil || status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, status)
		}
	}
	if conn, status := h.dialBridge(a.body["task_id"].(string), a.body["session_token"].(string)); conn == nil {
		t.Errorf("own token: status %d, want upgrade", status)
	}
}

// frame is a Bridge screencast frame as the Bridge sends it.
func frame(data string) map[string]any {
	return map[string]any{
		"type": "frame",
		"data": data,
		"metadata": map[string]any{
			"deviceWidth": 1280.0, "deviceHeight": 800.0, "pageScaleFactor": 1.0,
			"offsetTop": 0.0, "scrollOffsetX": 0.0, "scrollOffsetY": 0.0,
		},
	}
}

func TestClaimingSolverSeesLiveFramesOfTheAgentsPage(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	b.send(map[string]any{"type": "url", "url": "http://localhost:9000/challenge"})
	b.send(frame("before-claim"))

	s.mustClaim(id)

	// The page may be static, so the frame sent before the Claim is shown at once.
	m := s.next("frame", id)
	if m["data"] != "before-claim" || m["url"] != "http://localhost:9000/challenge" {
		t.Errorf("first frame = %v, want before-claim with the page URL", m)
	}
	if md, _ := m["metadata"].(map[string]any); md["deviceWidth"] != 1280.0 {
		t.Errorf("metadata = %v, want the Bridge's screencast metadata", m["metadata"])
	}
	b.send(frame("after-claim"))
	if m := s.next("frame", id); m["data"] != "after-claim" {
		t.Errorf("next frame = %v, want after-claim", m["data"])
	}
}

func TestFramesAreShownOnlyToTheClaimingSolver(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	claimer, other := h.connectSolver(), h.connectSolver()
	b.send(frame("unclaimed"))
	other.never("frame", id, 100*time.Millisecond)

	claimer.mustClaim(id)
	b.send(frame("claimed"))

	claimer.next("frame", id)
	other.never("frame", id, 100*time.Millisecond)
}

func TestClaimingSolversPointerEventsReachTheBridge(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	s.send(map[string]any{"type": "pointer", "task_id": id, "action": "down", "x": 0.25, "y": 0.5, "t": 1000.0})
	s.send(map[string]any{"type": "pointer", "task_id": id, "action": "up", "x": 0.25, "y": 0.5, "t": 1080.0})

	down, up := b.next("pointer"), b.next("pointer")
	if down["action"] != "down" || down["x"] != 0.25 || down["y"] != 0.5 || down["t"] != 1000.0 {
		t.Errorf("first pointer = %v, want down at 0.25,0.5 t=1000", down)
	}
	if up["action"] != "up" || up["t"] != 1080.0 {
		t.Errorf("second pointer = %v, want up t=1080", up)
	}
}

func TestInputFromASolverWithoutTheClaimIsNotForwarded(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	other := h.connectSolver()
	other.send(map[string]any{"type": "pointer", "task_id": id, "action": "down", "x": 0.5, "y": 0.5, "t": 1.0})
	h.connectSolver().mustClaim(id)

	other.send(map[string]any{"type": "pointer", "task_id": id, "action": "down", "x": 0.5, "y": 0.5, "t": 2.0})

	if m := other.next("error", id); m["error"] != "not_your_claim" {
		t.Errorf("error = %v, want not_your_claim", m["error"])
	}
	b.never("pointer", 100*time.Millisecond)
}

func TestSolverInputIsValidated(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	for _, bad := range []map[string]any{
		{"type": "pointer", "task_id": id, "action": "tap", "x": 0.5, "y": 0.5},
		{"type": "pointer", "task_id": id, "action": "down", "x": 1.5, "y": 0.5},
		{"type": "pointer", "task_id": id, "action": "down", "x": 0.5, "y": -0.1},
	} {
		s.send(bad)
		if m := s.next("error", id); m["error"] != "invalid_input" {
			t.Errorf("%v: error = %v, want invalid_input", bad, m["error"])
		}
	}
	b.never("pointer", 100*time.Millisecond)
}

func TestBridgeIsToldWhenItsTaskIsClaimed(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	b := h.connectBridge(created)

	h.connectSolver().mustClaim(created.body["task_id"].(string))

	if m := b.next("claimed"); m["solve_deadline"] == nil {
		t.Errorf("claimed = %v, want solve_deadline", m)
	}
}

func TestSolvedCapturesTheHoldAsEarningAndFee(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 15_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	b.send(map[string]any{"type": "solved"})

	b.next("solved")
	m := s.next("task_solved", id)
	if num(m["earning"]) != 8_000 || num(m["fee"]) != 2_000 {
		t.Errorf("task_solved = %v, want earning 8000 and fee 2000", m)
	}
	if got := h.taskState(key, id); got != "solved" {
		t.Errorf("state = %s, want solved", got)
	}
	if available, held := h.balance(key); available != 5_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 5000/0", available, held)
	}
}

func TestSolvedBeforeAnyClaimIsIgnored(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)

	b.send(map[string]any{"type": "solved"})

	b.never("solved", 100*time.Millisecond)
	if got := h.taskState(key, id); got != "pending" {
		t.Errorf("state = %s, want pending", got)
	}
	if available, held := h.balance(key); available != 0 || held != 10_000 {
		t.Errorf("balance = %d/%d, want 0/10000", available, held)
	}
}

func TestClosingTheBridgeWhileClaimedFailsTheTask(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	b.conn.Close(websocket.StatusNormalClosure, "")

	if m := s.next("task_failed", id); m["reason"] != "bridge_disconnected" {
		t.Errorf("reason = %v, want bridge_disconnected", m["reason"])
	}
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestClosingTheBridgeBeforeAClaimFailsTheTaskAndRemovesItFromTheQueue(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.next("task_added", id)

	b.conn.Close(websocket.StatusNormalClosure, "")

	s.next("task_removed", id)
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
	if reply := s.claim(id); reply["type"] != "claim_failed" {
		t.Errorf("claim reply = %v, want claim_failed", reply)
	}
}

func TestTaskWhoseBridgeNeverJoinedStaysQueued(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	// A refused join is not a Bridge leaving.
	h.dialBridge(id, "st_nope")

	h.connectSolver().mustClaim(id)
}

func TestASecondBridgeCannotJoinALiveSession(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)

	second, _ := h.dialBridge(id, created.body["session_token"].(string))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := second.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("second bridge read: %v, want close with policy violation", err)
	}
	s.never("task_failed", id, 100*time.Millisecond)
	if got := h.taskState(key, id); got != "claimed" {
		t.Errorf("state = %s, want claimed", got)
	}
}

func TestSolvedArrivingAfterTheTaskFailedIsIgnored(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow = time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()
	s.mustClaim(id)
	if m := b.next("failed"); m["reason"] != "solve_window" {
		t.Fatalf("failed = %v, want reason solve_window", m)
	}

	b.send(map[string]any{"type": "solved"})

	b.never("solved", 100*time.Millisecond)
	s.never("task_solved", id, 0)
	if got := h.taskState(key, id); got != "failed" {
		t.Errorf("state = %s, want failed", got)
	}
	if available, held := h.balance(key); available != 10_000 || held != 0 {
		t.Errorf("balance = %d/%d, want 10000/0", available, held)
	}
}

func TestBridgeIsToldWhenItsTaskExpires(t *testing.T) {
	h := newHarness(t)
	key := h.register()
	h.credit(key, 10_000)
	b := h.connectBridge(h.createTask(key))

	b.next("expired")
}

func TestBridgeJoiningAfterTheClaimIsCaughtUp(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	s := h.connectSolver()
	s.mustClaim(id)

	b := h.connectBridge(created)
	b.next("claimed")
	b.send(frame("late"))

	if m := s.next("frame", id); m["data"] != "late" {
		t.Errorf("frame = %v, want late", m["data"])
	}
}

func TestReconnectingSolverSeesTheLatestFrameAtOnce(t *testing.T) {
	h := newHarness(t, func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour })
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	first := h.connectSolver()
	first.mustClaim(id)
	b.send(frame("latest"))
	first.next("frame", id)

	again := h.connectSolverAs(first.wallet)

	if m := again.next("frame", id); m["data"] != "latest" {
		t.Errorf("frame = %v, want latest", m["data"])
	}
}
