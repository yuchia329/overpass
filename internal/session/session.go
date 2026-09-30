// Package session is the Session relay: it pairs a Task's Bridge connection
// with the connections of the Solver who claimed it, forwarding frames from
// the Bridge to the Solver and input from the Solver to the Bridge. It also
// carries the WebRTC signaling that lets the two connect directly, after
// which frames and input bypass it. It does not interpret frames or SDP.
package session

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/yuchia329/overpass/internal/secret"
	"github.com/yuchia329/overpass/internal/task"
)

const (
	// bridgeBuffer is how many messages a Bridge may fall behind before it is dropped.
	bridgeBuffer = 256
	// answerBuffer is how many answers a Solver connection may fall behind
	// before later ones are dropped. A dropped answer only keeps that Solver
	// on the relay.
	answerBuffer = 4
)

var (
	ErrAlreadyJoined = errors.New("a bridge is already connected to this task")
	ErrNoBridge      = errors.New("no bridge is connected to this task")
)

// Frame is one screencast frame, passed through as the Bridge sent it.
type Frame struct {
	Data     string          // base64 JPEG
	Metadata json.RawMessage // screencast metadata
}

// View is the latest look at a Session's page for its Solver.
type View struct {
	TaskID string
	URL    string
	Frame  Frame
}

// Answer is the Bridge's WebRTC answer to its Solver's offer, with the
// credential the Solver's peer must present to the Bridge.
type Answer struct {
	TaskID    string
	SDP       string
	PeerToken string
}

// Viewer is one Solver connection's feed of the Session it holds the Claim
// on. Only the latest View is kept: a Solver who falls behind skips frames.
type Viewer struct {
	Wallet  string
	C       <-chan View
	c       chan View
	Answers <-chan Answer
	answers chan Answer
}

// Relay holds the live Sessions. It is fed the Task lifecycle's Events.
type Relay struct {
	mu       sync.Mutex
	sessions map[string]*session // by Task id, while a Bridge is connected
	viewers  map[*Viewer]struct{}
}

type session struct {
	bridge *Bridge
	solver string // the claimant's wallet, once claimed
	// peerToken is the credential, made at Claim, that the claimant's WebRTC
	// peer presents to the Bridge.
	peerToken string
	ended     bool // Solved, Expired or Failed
	url    string
	frame  *Frame
}

// Input is one input event from the Solver: a pointer down, move or up, or a
// wheel scroll. X and Y are normalized to 0–1 of the displayed frame, DX and
// DY are a wheel's scroll in frame widths and heights, and T is the Solver's
// clock in milliseconds.
type Input struct {
	Type   string // pointer or wheel
	Action string // down, move or up, for a pointer event
	X, Y   float64
	DX, DY float64
	T      float64
}

// ToBridge is one message for the Bridge. Exactly one of Input, Offer and
// Event is set.
type ToBridge struct {
	Input *Input
	Offer string      // the claimant's WebRTC offer SDP
	Event *task.Event // the Task was claimed or ended
	// PeerToken comes with a Claimed Event: the credential the claimant's
	// WebRTC peer must present.
	PeerToken string
}

// Bridge is a Bridge connection's handle on its Task's Session.
type Bridge struct {
	TaskID string
	C      <-chan ToBridge // closed when the Bridge falls too far behind
	c      chan ToBridge
	closed bool
}

func New() *Relay {
	return &Relay{sessions: map[string]*session{}, viewers: map[*Viewer]struct{}{}}
}

// JoinBridge connects a Bridge to its Task's Session. A Task has at most one
// Bridge connected at a time.
func (r *Relay) JoinBridge(taskID string) (*Bridge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[taskID]; ok {
		return nil, ErrAlreadyJoined
	}
	c := make(chan ToBridge, bridgeBuffer)
	b := &Bridge{TaskID: taskID, C: c, c: c}
	r.sessions[taskID] = &session{bridge: b}
	return b, nil
}

// LeaveBridge ends the Session of a Bridge that disconnected.
func (r *Relay) LeaveBridge(b *Bridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[b.TaskID]; ok && s.bridge == b {
		delete(r.sessions, b.TaskID)
	}
}

// Input forwards a Solver's input event to the Bridge, but only from the
// Solver holding the Task's Claim. Events reach the Bridge in the order they
// were forwarded.
func (r *Relay) Input(taskID, wallet string, in Input) error {
	return r.fromSolver(taskID, wallet, ToBridge{Input: &in})
}

// Offer forwards a Solver's WebRTC offer to the Bridge, but only from the
// Solver holding the Task's Claim.
func (r *Relay) Offer(taskID, wallet, sdp string) error {
	return r.fromSolver(taskID, wallet, ToBridge{Offer: sdp})
}

func (r *Relay) fromSolver(taskID, wallet string, m ToBridge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[taskID]
	if !ok {
		return ErrNoBridge
	}
	if s.ended || s.solver == "" || s.solver != wallet {
		return task.ErrNotYourClaim
	}
	s.bridge.send(m)
	return nil
}

// Answer forwards the Bridge's WebRTC answer, with the Session's peer token,
// to every connection of the claiming Solver while the Task is live.
func (r *Relay) Answer(b *Bridge, sdp string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[b.TaskID]
	if !ok || s.bridge != b || s.solver == "" || s.ended {
		return
	}
	a := Answer{TaskID: b.TaskID, SDP: sdp, PeerToken: s.peerToken}
	for v := range r.viewers {
		if v.Wallet != s.solver {
			continue
		}
		select {
		case v.answers <- a:
		default:
		}
	}
}

// Frame shows a new frame of the Agent's page to the claiming Solver.
func (r *Relay) Frame(b *Bridge, f Frame) {
	r.update(b, func(s *session) { s.frame = &f })
}

// URL records the Agent's page URL; the Solver sees it with the next frame.
func (r *Relay) URL(b *Bridge, url string) {
	r.update(b, func(s *session) { s.url = url })
}

func (r *Relay) update(b *Bridge, change func(*session)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[b.TaskID]
	if !ok || s.bridge != b {
		return
	}
	change(s)
	r.show(b.TaskID, s)
}

// Watch registers a Solver connection. If the Solver holds a live Claim, its
// page is shown at once, so a reconnecting Solver picks up where they were.
func (r *Relay) Watch(wallet string) *Viewer {
	c, answers := make(chan View, 1), make(chan Answer, answerBuffer)
	v := &Viewer{Wallet: wallet, C: c, c: c, Answers: answers, answers: answers}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.viewers[v] = struct{}{}
	for id, s := range r.sessions {
		if s.solver == wallet {
			r.show(id, s)
		}
	}
	return v
}

// Unwatch stops a Viewer's feed. It is safe to call more than once.
func (r *Relay) Unwatch(v *Viewer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.viewers, v)
}

// Publish applies a Task Event to the Task's Session and tells its Bridge.
// It is idempotent, so a Bridge that joins late can be caught up with the
// Task's current state.
func (r *Relay) Publish(e task.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[e.TaskID]
	if !ok {
		return
	}
	switch e.State {
	case task.Claimed:
		s.solver = e.SolverWallet
		if s.peerToken == "" {
			s.peerToken = secret.New("pt_")
		}
		r.show(e.TaskID, s)
	case task.Solved, task.Expired, task.Failed:
		s.ended = true
	default:
		return
	}
	m := ToBridge{Event: &e}
	if e.State == task.Claimed {
		m.PeerToken = s.peerToken
	}
	s.bridge.send(m)
}

// send never blocks. Input events a Bridge is too slow for are dropped, so
// a Solver's input can never end the Session; a Bridge too slow to take an
// Event is dropped.
func (b *Bridge) send(m ToBridge) {
	if b.closed {
		return
	}
	select {
	case b.c <- m:
	default:
		if m.Event != nil {
			b.closed = true
			close(b.c)
		}
	}
}

// show sends the Session's latest View to every connection of its Solver,
// replacing any View they have not read yet. It never blocks.
func (r *Relay) show(taskID string, s *session) {
	if s.solver == "" || s.ended || s.frame == nil {
		return
	}
	view := View{TaskID: taskID, URL: s.url, Frame: *s.frame}
	for v := range r.viewers {
		if v.Wallet != s.solver {
			continue
		}
		select {
		case <-v.c: // drop the unread View
		default:
		}
		v.c <- view // cannot block: the buffer is empty and only show sends, under r.mu
	}
}
