package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/yuchia329/overpass/internal/queue"
	"github.com/yuchia329/overpass/internal/session"
	"github.com/yuchia329/overpass/internal/solana"
	"github.com/yuchia329/overpass/internal/task"
)

const queueWriteTimeout = 5 * time.Second

// handleQueue serves a Solver's Queue socket. The Solver identifies with a
// wallet address only; there is no other authentication.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	wallet := r.URL.Query().Get("wallet")
	if !solana.IsPubkey(wallet) {
		writeError(w, http.StatusBadRequest, "invalid_wallet")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already responded
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		s.readSolver(ctx, conn, wallet)
	}()

	sub, snapshot := s.queue.Subscribe(wallet)
	defer s.queue.Unsubscribe(sub)
	viewer := s.relay.Watch(wallet)
	defer s.relay.Unwatch(viewer)
	for _, t := range snapshot {
		if !writeMsg(ctx, conn, taskAdded(t)) {
			return
		}
	}
	for {
		select {
		case m, ok := <-sub.C:
			if !ok {
				conn.Close(websocket.StatusPolicyViolation, "too slow")
				return
			}
			if !writeMsg(ctx, conn, queueMessage(m)) {
				return
			}
		case v := <-viewer.C:
			if !writeMsg(ctx, conn, frameMessage(v)) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// readSolver handles what the Solver sends until the socket closes.
func (s *Server) readSolver(ctx context.Context, conn *websocket.Conn, wallet string) {
	for {
		var msg struct {
			Type   string  `json:"type"`
			TaskID string  `json:"task_id"`
			Action string  `json:"action"`
			X      float64 `json:"x"`
			Y      float64 `json:"y"`
			T      float64 `json:"t"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		var reply map[string]any
		switch msg.Type {
		case "claim":
			reply = s.claim(ctx, msg.TaskID, wallet)
		case "give_up":
			reply = s.giveUp(ctx, msg.TaskID, wallet)
		case "pointer":
			reply = s.input(msg.TaskID, wallet, session.Pointer{Action: msg.Action, X: msg.X, Y: msg.Y, T: msg.T})
		default:
			reply = map[string]any{"type": "error", "error": "unknown_type"}
		}
		if reply != nil && !writeMsg(ctx, conn, reply) {
			return
		}
	}
}

func (s *Server) claim(ctx context.Context, taskID, wallet string) map[string]any {
	solveDeadline, err := s.tasks.Claim(ctx, taskID, wallet)
	refused := func(code string) map[string]any {
		return map[string]any{"type": "claim_failed", "task_id": taskID, "error": code}
	}
	switch {
	case errors.Is(err, task.ErrAlreadyClaimed):
		return refused("already_claimed")
	case errors.Is(err, task.ErrExpired):
		return refused("expired")
	case errors.Is(err, task.ErrUnknownTask):
		return refused("unknown_task")
	case errors.Is(err, task.ErrHoldingClaim):
		return refused("holding_claim")
	case err != nil:
		log.Printf("claim %s: %v", taskID, err)
		return refused("internal")
	}
	return map[string]any{
		"type":            "claimed",
		"task_id":         taskID,
		"solve_deadline":  solveDeadline.UTC().Format(time.RFC3339Nano),
		"solve_window_ms": s.cfg.SolveWindow.Milliseconds(),
	}
}

// giveUp replies only on refusal; success arrives as task_failed.
func (s *Server) giveUp(ctx context.Context, taskID, wallet string) map[string]any {
	err := s.tasks.GiveUp(ctx, taskID, wallet)
	switch {
	case errors.Is(err, task.ErrNotYourClaim):
		return map[string]any{"type": "error", "task_id": taskID, "error": "not_your_claim"}
	case err != nil:
		log.Printf("give up %s: %v", taskID, err)
		return map[string]any{"type": "error", "task_id": taskID, "error": "internal"}
	}
	return nil
}

// input forwards a pointer event to the Bridge. It replies only on refusal.
func (s *Server) input(taskID, wallet string, p session.Pointer) map[string]any {
	inViewport := func(v float64) bool { return v >= 0 && v <= 1 }
	if (p.Action != "down" && p.Action != "move" && p.Action != "up") || !inViewport(p.X) || !inViewport(p.Y) {
		return map[string]any{"type": "error", "task_id": taskID, "error": "invalid_input"}
	}
	err := s.relay.Input(taskID, wallet, p)
	if errors.Is(err, task.ErrNotYourClaim) {
		return map[string]any{"type": "error", "task_id": taskID, "error": "not_your_claim"}
	}
	return nil // forwarded, or dropped because no Bridge is connected
}

func queueMessage(m queue.Message) map[string]any {
	switch {
	case m.Added != nil:
		return taskAdded(*m.Added)
	case m.Removed != "":
		return map[string]any{"type": "task_removed", "task_id": m.Removed}
	case m.Failed != nil:
		return map[string]any{"type": "task_failed", "task_id": m.Failed.TaskID, "reason": m.Failed.Reason}
	case m.Solved != nil:
		return map[string]any{"type": "task_solved", "task_id": m.Solved.TaskID, "earning": m.Solved.Earning, "fee": m.Solved.Fee}
	}
	return nil
}

func frameMessage(v session.View) map[string]any {
	return map[string]any{
		"type":     "frame",
		"task_id":  v.TaskID,
		"url":      v.URL,
		"data":     v.Frame.Data,
		"metadata": v.Frame.Metadata,
	}
}

func taskAdded(t queue.Task) map[string]any {
	return map[string]any{
		"type":      "task_added",
		"task_id":   t.ID,
		"page_url":  t.PageURL,
		"waited_ms": time.Since(t.CreatedAt).Milliseconds(),
	}
}

func writeMsg(ctx context.Context, conn *websocket.Conn, v any) bool {
	ctx, cancel := context.WithTimeout(ctx, queueWriteTimeout)
	defer cancel()
	return wsjson.Write(ctx, conn, v) == nil
}
