package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/yuchia329/overpass/internal/session"
	"github.com/yuchia329/overpass/internal/task"
)

// maxBridgeMessage bounds one message from the Bridge; a JPEG frame of a
// 1280×800 viewport is well under it.
const maxBridgeMessage = 4 << 20

// handleBridge serves the Bridge socket for one Task. The Bridge
// authenticates with the Task's session token and can only join that Task.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	id, token := r.PathValue("id"), r.URL.Query().Get("token")
	_, err := s.tasks.Session(r.Context(), id, token)
	if errors.Is(err, task.ErrBadToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxBridgeMessage)

	b, err := s.relay.JoinBridge(id)
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "already_joined")
		return
	}
	defer func() {
		s.relay.LeaveBridge(b)
		// The request context is done by now; failing the Task must still run.
		if _, err := s.tasks.BridgeLost(context.WithoutCancel(r.Context()), id); err != nil {
			log.Printf("bridge lost %s: %v", id, err)
		}
	}()
	// Catch the Bridge up: changes recorded before it joined were not relayed.
	current, err := s.tasks.Session(r.Context(), id, token)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "internal")
		return
	}
	s.relay.Publish(current)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		s.readBridge(ctx, conn, b)
	}()
	go keepAlive(ctx, conn, s.cfg.PingInterval)
	for {
		select {
		case m, ok := <-b.C:
			if !ok {
				conn.Close(websocket.StatusPolicyViolation, "too slow")
				return
			}
			if !writeMsg(ctx, conn, bridgeMessage(m)) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func bridgeMessage(m session.ToBridge) map[string]any {
	if in := m.Input; in != nil {
		if in.Type == "wheel" {
			return map[string]any{"type": "wheel", "x": in.X, "y": in.Y, "dx": in.DX, "dy": in.DY, "t": in.T}
		}
		return map[string]any{"type": "pointer", "action": in.Action, "x": in.X, "y": in.Y, "t": in.T}
	}
	e := m.Event
	out := map[string]any{"type": string(e.State)}
	switch e.State {
	case task.Claimed:
		out["solve_deadline"] = e.SolveDeadline.UTC().Format(time.RFC3339Nano)
	case task.Failed:
		out["reason"] = e.Reason
	}
	return out
}

// readBridge handles what the Bridge sends until the socket closes.
func (s *Server) readBridge(ctx context.Context, conn *websocket.Conn, b *session.Bridge) {
	for {
		var msg struct {
			Type     string          `json:"type"`
			Data     string          `json:"data"`
			Metadata json.RawMessage `json:"metadata"`
			URL      string          `json:"url"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "frame":
			s.relay.Frame(b, session.Frame{Data: msg.Data, Metadata: msg.Metadata})
		case "url":
			s.relay.URL(b, msg.URL)
		case "solved":
			// The outcome reaches the Bridge as an Event. A Solved report on a
			// Task that already ended is a no-op: the first outcome is final.
			if _, err := s.tasks.Solve(ctx, b.TaskID); err != nil {
				log.Printf("solve %s: %v", b.TaskID, err)
			}
		}
	}
}
