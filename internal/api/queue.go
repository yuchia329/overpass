package api

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	go keepAlive(ctx, conn, s.cfg.PingInterval)

	sub, snapshot := s.queue.Subscribe(wallet)
	defer s.queue.Unsubscribe(sub)
	viewer := s.relay.Watch(wallet)
	defer s.relay.Unwatch(viewer)
	// A Solver reconnecting mid-Session resumes it. The Claim is looked up
	// after subscribing, so if the Task ends meanwhile its outcome follows.
	resume, ok, err := s.tasks.ClaimOf(ctx, wallet)
	if err != nil {
		log.Printf("claim of %s: %v", wallet, err)
	}
	if ok && !writeMsg(ctx, conn, map[string]any{
		"type":          "claimed",
		"task_id":       resume.TaskID,
		"page_url":      resume.PageURL,
		"obstacle":      resume.Obstacle,
		"solve_left_ms": time.Until(resume.SolveDeadline).Milliseconds(),
		"ice_servers":   s.iceServers(),
	}) {
		return
	}
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
		case a := <-viewer.Answers:
			if !writeMsg(ctx, conn, map[string]any{
				"type": "rtc_answer", "task_id": a.TaskID, "sdp": a.SDP, "peer_token": a.PeerToken,
			}) {
				return
			}
		case n := <-viewer.Notices:
			if !writeMsg(ctx, conn, map[string]any{"type": n.Type, "task_id": n.TaskID}) {
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
			DX     float64 `json:"dx"`
			DY     float64 `json:"dy"`
			Text   string  `json:"text"`
			Key    string  `json:"key"`
			T      float64 `json:"t"`
			SDP    string  `json:"sdp"`
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
		case "rtc_offer":
			reply = s.offer(msg.TaskID, wallet, msg.SDP)
		case "done":
			reply = s.done(msg.TaskID, wallet)
		case "pointer", "wheel", "text", "key":
			reply = s.input(msg.TaskID, wallet, session.Input{
				Type: msg.Type, Action: msg.Action, X: msg.X, Y: msg.Y, DX: msg.DX, DY: msg.DY,
				Text: msg.Text, Key: msg.Key, T: msg.T,
			})
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
		"ice_servers":     s.iceServers(),
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

// maxWheel bounds one wheel event's scroll, in frame widths or heights.
const maxWheel = 10

// maxText bounds the characters one text event types.
const maxText = 64

// keys are the named keys a Solver may press. Modifiers are left out, so a
// Solver cannot send shortcuts such as Ctrl+L. The Bridge keeps the same list.
var keys = map[string]bool{
	"Enter": true, "Tab": true, "Backspace": true, "Delete": true, "Escape": true,
	"ArrowLeft": true, "ArrowRight": true, "ArrowUp": true, "ArrowDown": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true,
}

// validText reports whether t is something a Solver may type: 1 to maxText
// characters, none of them control characters (Enter and Tab are keys).
func validText(t string) bool {
	n := utf8.RuneCountInString(t)
	return n >= 1 && n <= maxText && utf8.ValidString(t) && !strings.ContainsFunc(t, unicode.IsControl)
}

// input forwards a Solver's input event to the Bridge. It replies only on refusal.
func (s *Server) input(taskID, wallet string, in session.Input) map[string]any {
	inFrame := func(v float64) bool { return v >= 0 && v <= 1 }
	var valid bool
	switch in.Type {
	case "pointer":
		valid = inFrame(in.X) && inFrame(in.Y) && (in.Action == "down" || in.Action == "move" || in.Action == "up")
	case "wheel":
		valid = inFrame(in.X) && inFrame(in.Y) && math.Abs(in.DX) <= maxWheel && math.Abs(in.DY) <= maxWheel
	case "text":
		valid = validText(in.Text)
	case "key":
		valid = keys[in.Key]
	}
	if !valid {
		return map[string]any{"type": "error", "task_id": taskID, "error": "invalid_input"}
	}
	err := s.relay.Input(taskID, wallet, in)
	if errors.Is(err, task.ErrNotYourClaim) {
		return map[string]any{"type": "error", "task_id": taskID, "error": "not_your_claim"}
	}
	return nil // forwarded, or dropped because no Bridge is connected
}

// offer forwards the Solver's WebRTC offer to the Bridge. It replies only on
// refusal; the Bridge's answer arrives as rtc_answer.
func (s *Server) offer(taskID, wallet, sdp string) map[string]any {
	if sdp == "" || len(sdp) > maxSDP {
		return map[string]any{"type": "error", "task_id": taskID, "error": "invalid_offer"}
	}
	if errors.Is(s.relay.Offer(taskID, wallet, sdp), task.ErrNotYourClaim) {
		return map[string]any{"type": "error", "task_id": taskID, "error": "not_your_claim"}
	}
	return nil // forwarded, or dropped because no Bridge is connected
}

// done tells the Bridge the Solver reports the obstacle cleared, so the
// Agent can check. It replies only on refusal; the outcome arrives as
// task_solved, or as not_cleared with the Solver keeping the page.
func (s *Server) done(taskID, wallet string) map[string]any {
	if errors.Is(s.relay.Done(taskID, wallet), task.ErrNotYourClaim) {
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
		"obstacle":  t.Obstacle,
		"waited_ms": time.Since(t.CreatedAt).Milliseconds(),
	}
}

// keepAlive pings conn every interval until ctx is done. Proxies close
// sockets that carry nothing for 60–100s, and a Challenge page that sits
// still sends no frames. A zero interval disables it.
func keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// Sending the ping is what keeps the socket busy; a late pong is harmless.
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			_ = conn.Ping(pingCtx)
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

func writeMsg(ctx context.Context, conn *websocket.Conn, v any) bool {
	ctx, cancel := context.WithTimeout(ctx, queueWriteTimeout)
	defer cancel()
	return wsjson.Write(ctx, conn, v) == nil
}
