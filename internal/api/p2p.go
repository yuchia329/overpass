package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

const (
	// maxSDP bounds one WebRTC offer or answer. A data-channel-only SDP with
	// a handful of candidates is 1–3 KB.
	maxSDP = 16 << 10
	// turnGrace is how long TURN credentials outlast the solve window, so a
	// Solver who resumes late in the window can still allocate.
	turnGrace = 10 * time.Minute
)

// iceServers lists the STUN and TURN servers a Session's peers may use, in
// the shape RTCPeerConnection takes. TURN credentials follow coturn's
// use-auth-secret scheme: they are signed with the shared secret and expire,
// so they are useless after the Session.
func (s *Server) iceServers() []map[string]any {
	servers := []map[string]any{}
	if len(s.cfg.STUNURLs) > 0 {
		servers = append(servers, map[string]any{"urls": s.cfg.STUNURLs})
	}
	if len(s.cfg.TURNURLs) > 0 {
		username := fmt.Sprintf("%d:overpass", time.Now().Add(s.cfg.SolveWindow+turnGrace).Unix())
		mac := hmac.New(sha1.New, []byte(s.cfg.TURNSecret))
		mac.Write([]byte(username))
		servers = append(servers, map[string]any{
			"urls":       s.cfg.TURNURLs,
			"username":   username,
			"credential": base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		})
	}
	return servers
}
