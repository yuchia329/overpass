package api_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuchia329/overpass/internal/api"
)

// claimedSession starts a Task, joins its Bridge and has a fresh Solver claim it.
func claimedSession(t *testing.T, mutate ...func(*api.Config)) (h *harness, id string, b *bridge, s *solver) {
	t.Helper()
	h = newHarness(t, append([]func(*api.Config){func(c *api.Config) { c.ClaimWindow, c.SolveWindow = time.Hour, time.Hour }}, mutate...)...)
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id = created.body["task_id"].(string)
	b = h.connectBridge(created)
	s = h.connectSolver()
	s.mustClaim(id)
	return h, id, b, s
}

func TestClaimingSolversOfferReachesTheBridgeAndItsAnswerComesBackWithThePeerToken(t *testing.T) {
	_, id, b, s := claimedSession(t)
	claimed := b.next("claimed")
	token, _ := claimed["peer_token"].(string)
	if !strings.HasPrefix(token, "pt_") {
		t.Fatalf("claimed = %v, want a peer_token", claimed)
	}

	s.send(map[string]any{"type": "rtc_offer", "task_id": id, "sdp": "v=0 offer"})
	if m := b.next("rtc_offer"); m["sdp"] != "v=0 offer" {
		t.Errorf("rtc_offer = %v, want the Solver's SDP", m)
	}
	b.send(map[string]any{"type": "rtc_answer", "sdp": "v=0 answer"})

	m := s.next("rtc_answer", id)
	if m["sdp"] != "v=0 answer" || m["peer_token"] != token {
		t.Errorf("rtc_answer = %v, want the Bridge's SDP and peer token %s", m, token)
	}
}

func TestOfferFromASolverWithoutTheClaimIsNotForwarded(t *testing.T) {
	h, id, b, _ := claimedSession(t)
	other := h.connectSolver()

	other.send(map[string]any{"type": "rtc_offer", "task_id": id, "sdp": "v=0 offer"})

	if m := other.next("error", id); m["error"] != "not_your_claim" {
		t.Errorf("error = %v, want not_your_claim", m["error"])
	}
	b.never("rtc_offer", 100*time.Millisecond)
}

func TestTheBridgesAnswerReachesOnlyTheClaimingSolver(t *testing.T) {
	h, id, b, s := claimedSession(t)
	other := h.connectSolver()
	s.send(map[string]any{"type": "rtc_offer", "task_id": id, "sdp": "v=0 offer"})
	b.next("rtc_offer")

	b.send(map[string]any{"type": "rtc_answer", "sdp": "v=0 answer"})

	s.next("rtc_answer", id)
	other.never("rtc_answer", id, 100*time.Millisecond)
}

func TestOversizedOfferIsRefused(t *testing.T) {
	_, id, b, s := claimedSession(t)

	s.send(map[string]any{"type": "rtc_offer", "task_id": id, "sdp": strings.Repeat("a", 20_000)})

	if m := s.next("error", id); m["error"] != "invalid_offer" {
		t.Errorf("error = %v, want invalid_offer", m["error"])
	}
	b.never("rtc_offer", 100*time.Millisecond)
}

func TestNoAnswerIsForwardedAfterTheTaskEnds(t *testing.T) {
	_, id, b, s := claimedSession(t)
	s.send(map[string]any{"type": "give_up", "task_id": id})
	s.next("task_failed", id)
	b.next("failed")

	b.send(map[string]any{"type": "rtc_answer", "sdp": "v=0 answer"})

	s.never("rtc_answer", id, 100*time.Millisecond)
}

func TestClaimedCarriesICEServersWithShortLivedTURNCredentials(t *testing.T) {
	const secret = "turn-secret"
	h := newHarness(t, func(c *api.Config) {
		c.ClaimWindow, c.SolveWindow = time.Hour, time.Minute
		c.STUNURLs = []string{"stun:stun.example.com:3478"}
		c.TURNURLs = []string{"turn:turn.example.com:3478"}
		c.TURNSecret = secret
	})
	key := h.register()
	h.credit(key, 10_000)
	created := h.createTask(key)
	id := created.body["task_id"].(string)
	b := h.connectBridge(created)
	s := h.connectSolver()

	s.send(map[string]any{"type": "claim", "task_id": id})
	for who, m := range map[string]map[string]any{"solver": s.claimReply(id), "bridge": b.next("claimed")} {
		servers, _ := m["ice_servers"].([]any)
		if len(servers) != 2 {
			t.Fatalf("%s: ice_servers = %v, want STUN and TURN", who, m["ice_servers"])
		}
		stun, turn := servers[0].(map[string]any), servers[1].(map[string]any)
		if urls, _ := stun["urls"].([]any); len(urls) != 1 || urls[0] != "stun:stun.example.com:3478" {
			t.Errorf("%s: STUN = %v", who, stun)
		}
		if urls, _ := turn["urls"].([]any); len(urls) != 1 || urls[0] != "turn:turn.example.com:3478" {
			t.Errorf("%s: TURN urls = %v", who, turn["urls"])
		}
		// coturn's use-auth-secret: username is "<expiry>:<name>", credential
		// is base64(HMAC-SHA1(secret, username)).
		username, _ := turn["username"].(string)
		mac := hmac.New(sha1.New, []byte(secret))
		mac.Write([]byte(username))
		if turn["credential"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Errorf("%s: credential does not match username %q", who, username)
		}
		expiry, err := strconv.ParseInt(strings.SplitN(username, ":", 2)[0], 10, 64)
		if left := time.Until(time.Unix(expiry, 0)); err != nil || left < time.Minute || left > time.Hour {
			t.Errorf("%s: username %q, want an expiry past the solve window", who, username)
		}
	}
}

func TestResumedClaimCarriesICEServers(t *testing.T) {
	h, id, _, s := claimedSession(t, func(c *api.Config) { c.STUNURLs = []string{"stun:stun.example.com:3478"} })

	again := h.connectSolverAs(s.wallet)

	if servers, _ := again.next("claimed", id)["ice_servers"].([]any); len(servers) != 1 {
		t.Errorf("ice_servers = %v, want the STUN server", servers)
	}
}
