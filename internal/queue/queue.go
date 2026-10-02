// Package queue keeps the live Queue of Pending Tasks and fans its changes
// out to every connected Solver.
package queue

import (
	"slices"
	"sync"
	"time"

	"github.com/yuchia329/overpass/internal/task"
)

// subBuffer is how many messages a Solver may fall behind before it is dropped.
const subBuffer = 64

// Task is a queued Task as shown to Solvers.
type Task struct {
	ID        string
	PageURL   string
	Obstacle  string // what the Solver is to clear, as the Agent described it; may be empty
	CreatedAt time.Time
}

// Message is one Queue change for a Solver. Exactly one field is set.
type Message struct {
	Added   *Task
	Removed string // Task id: claimed or Expired
	Failed  *Failed
	Solved  *Solved
}

// Failed tells the Solver holding a Claim that its Task Failed.
type Failed struct {
	TaskID string
	Reason task.Reason
}

// Solved tells the Solver holding a Claim that its Task was Solved and what
// the captured Hold was split into.
type Solved struct {
	TaskID       string
	Earning, Fee int64
}

// Sub is one Solver connection's feed of Queue changes.
type Sub struct {
	Wallet string
	C      <-chan Message // closed when the Sub is dropped or unsubscribed
	c      chan Message
}

// Hub is the in-memory Queue. It is fed by the Task lifecycle's Events.
type Hub struct {
	mu      sync.Mutex
	pending map[string]Task
	subs    map[*Sub]struct{}
}

func New() *Hub {
	return &Hub{pending: map[string]Task{}, subs: map[*Sub]struct{}{}}
}

// Publish applies a Task Event to the Queue.
func (h *Hub) Publish(e task.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e.State == task.Pending {
		t := Task{ID: e.TaskID, PageURL: e.PageURL, Obstacle: e.Obstacle, CreatedAt: e.CreatedAt}
		h.pending[t.ID] = t
		h.broadcast(Message{Added: &t})
		return
	}
	if _, ok := h.pending[e.TaskID]; ok {
		delete(h.pending, e.TaskID)
		h.broadcast(Message{Removed: e.TaskID})
	}
	switch e.State {
	case task.Failed:
		h.sendTo(e.SolverWallet, Message{Failed: &Failed{TaskID: e.TaskID, Reason: e.Reason}})
	case task.Solved:
		h.sendTo(e.SolverWallet, Message{Solved: &Solved{TaskID: e.TaskID, Earning: e.Earning, Fee: e.Fee}})
	}
}

// Subscribe registers a Solver and returns the Tasks queued right now; every
// later change arrives on the Sub.
func (h *Hub) Subscribe(wallet string) (*Sub, []Task) {
	c := make(chan Message, subBuffer)
	s := &Sub{Wallet: wallet, C: c, c: c}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = struct{}{}
	snapshot := make([]Task, 0, len(h.pending))
	for _, t := range h.pending {
		snapshot = append(snapshot, t)
	}
	slices.SortFunc(snapshot, func(a, b Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return s, snapshot
}

// Unsubscribe stops a Sub's feed. It is safe to call more than once.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(s)
}

func (h *Hub) broadcast(m Message) {
	for s := range h.subs {
		h.send(s, m)
	}
}

func (h *Hub) sendTo(wallet string, m Message) {
	for s := range h.subs {
		if s.Wallet == wallet {
			h.send(s, m)
		}
	}
}

// send never blocks: a Solver too slow to keep up is dropped.
func (h *Hub) send(s *Sub, m Message) {
	select {
	case s.c <- m:
	default:
		h.drop(s)
	}
}

func (h *Hub) drop(s *Sub) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.c)
	}
}
