package api_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yuchia329/unstuck/internal/api"
)

// proxyIdleTimeout stands in for the idle timeout of a load balancer,
// reverse proxy or tunnel between the Bridge or Solver and the backend.
const proxyIdleTimeout = 300 * time.Millisecond

func withPingsAndLongWindows(c *api.Config) {
	c.ClaimWindow = 3 * time.Second
	c.SolveWindow = 3 * time.Second
	c.PingInterval = 50 * time.Millisecond
}

func TestClaimedTaskWhoseBridgeSendsNothingStaysClaimedBehindAnIdleTimeoutProxy(t *testing.T) {
	h := newHarness(t, withPingsAndLongWindows)
	proxied := h.through(idleProxy(t, h.url, proxyIdleTimeout))
	key := h.register()
	h.credit(key, testPrice)
	created := h.createTask(key)
	taskID := created.body["task_id"].(string)
	b := proxied.connectBridge(created)
	s := proxied.connectSolver()
	s.mustClaim(taskID)
	b.next("claimed")

	// Neither the Bridge nor the Solver sends anything for 3 idle timeouts.
	b.never("failed", 3*proxyIdleTimeout)
	if got := h.taskState(key, taskID); got != "claimed" {
		t.Fatalf("task state = %q, want claimed", got)
	}

	// Both sockets are still open: the Bridge's Solved reaches the Solver.
	b.send(map[string]any{"type": "solved"})
	s.next("task_solved", taskID)
}

func TestIdleQueueSocketStaysOpenBehindAnIdleTimeoutProxy(t *testing.T) {
	h := newHarness(t, withPingsAndLongWindows)
	s := h.through(idleProxy(t, h.url, proxyIdleTimeout)).connectSolver()

	time.Sleep(3 * proxyIdleTimeout)

	key := h.register()
	h.credit(key, testPrice)
	created := h.createTask(key)
	h.connectBridge(created)
	s.next("task_added", created.body["task_id"].(string))
}

// through returns a view of h whose requests go to baseURL instead.
func (h *harness) through(baseURL string) *harness {
	c := *h
	c.url = baseURL
	return &c
}

// idleProxy forwards TCP connections to the server at targetURL and cuts
// any connection that carries no bytes either way for idle, as proxies do.
// It returns the proxy's base URL.
func idleProxy(t *testing.T, targetURL string, idle time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	target := strings.TrimPrefix(targetURL, "http://")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyConn(c, target, idle)
		}
	}()
	return "http://" + ln.Addr().String()
}

func proxyConn(client net.Conn, target string, idle time.Duration) {
	defer client.Close()
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer upstream.Close()
	activity := make(chan struct{}, 1)
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				select {
				case activity <- struct{}{}:
				default:
				}
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			timer.Reset(idle)
		case <-timer.C:
			return
		case <-done:
			return
		}
	}
}
