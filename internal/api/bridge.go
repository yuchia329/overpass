package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

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
			if !writeMsg(ctx, conn, s.bridgeMessage(m)) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) bridgeMessage(m session.ToBridge) map[string]any {
	if m.Offer != "" {
		return map[string]any{"type": "rtc_offer", "sdp": m.Offer}
	}
	if m.Done {
		return map[string]any{"type": "done"}
	}
	if in := m.Input; in != nil {
		switch in.Type {
		case "wheel":
			return map[string]any{"type": "wheel", "x": in.X, "y": in.Y, "dx": in.DX, "dy": in.DY, "t": in.T}
		case "text":
			return map[string]any{"type": "text", "text": in.Text, "t": in.T}
		case "key":
			return map[string]any{"type": "key", "key": in.Key, "t": in.T}
		}
		return map[string]any{"type": "pointer", "action": in.Action, "x": in.X, "y": in.Y, "t": in.T}
	}
	e := m.Event
	out := map[string]any{"type": string(e.State)}
	switch e.State {
	case task.Claimed:
		out["solve_deadline"] = e.SolveDeadline.UTC().Format(time.RFC3339Nano)
		out["peer_token"] = m.PeerToken
		out["ice_servers"] = s.iceServers()
	case task.Failed:
		out["reason"] = e.Reason
	}
	return out
}

// readBridge handles what the Bridge sends until the socket closes, then
// logs how much frame data the Bridge sent through the backend: a Session's
// relay bandwidth, which drops to almost nothing once its peers connect
// directly.
func (s *Server) readBridge(ctx context.Context, conn *websocket.Conn, b *session.Bridge) {
	start := time.Now()
	var frames, frameBytes int
	defer func() {
		secs := time.Since(start).Seconds()
		log.Printf("session %s: bridge sent %d frames, %d KB in %.0fs (%.1f KB/s)",
			b.TaskID, frames, frameBytes>>10, secs, float64(frameBytes)/1024/secs)
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type     string          `json:"type"`
			Data     string          `json:"data"`
			Metadata json.RawMessage `json:"metadata"`
			URL      string          `json:"url"`
			SDP      string          `json:"sdp"`
		}
		if json.Unmarshal(data, &msg) != nil {
			return
		}
		switch msg.Type {
		case "frame":
			frames++
			frameBytes += len(data)
			s.relay.Frame(b, session.Frame{Data: msg.Data, Metadata: msg.Metadata})
		case "url":
			s.relay.URL(b, msg.URL)
		case "rtc_answer":
			if len(msg.SDP) <= maxSDP {
				s.relay.Answer(b, msg.SDP)
			}
		case "not_cleared":
			// The Agent checked after the Solver's done: the obstacle is still there.
			s.relay.NotCleared(b)
		case "solved":
			// The outcome reaches the Bridge as an Event. A Solved report on a
			// Task that already ended is a no-op: the first outcome is final.
			if _, err := s.tasks.Solve(ctx, b.TaskID); err != nil {
				log.Printf("solve %s: %v", b.TaskID, err)
			}
		}
	}
}
