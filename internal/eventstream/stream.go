package eventstream

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// eventQueue is how many events one subscriber may have waiting. The next
// event beyond it replaces them all with a resync.
const eventQueue = 64

// eventHeartbeat is how long a stream may stay silent before the service
// writes a comment, which finds readers that went away.
const eventHeartbeat = 15 * time.Second

// Hub fans change notifications out to the subscribers of the event stream.
// Publishing never blocks on a subscriber. A nil hub drops everything.
type Hub struct {
	mu     sync.Mutex
	subs   map[*Subscriber]struct{}
	closed bool
	done   chan struct{}
}

func New() *Hub {
	return &Hub{subs: map[*Subscriber]struct{}{}, done: make(chan struct{})}
}

// Subscriber is one stream's queue. It starts with a resync pending, so the
// stream's first event asks the client to read everything.
type Subscriber struct {
	mu      sync.Mutex
	pending []api.Event
	resync  bool
	wake    chan struct{}
}

// Subscribe registers a new subscriber, or reports false once the hub closed.
func (h *Hub) Subscribe() (*Subscriber, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	s := &Subscriber{resync: true, wake: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return s, true
}

func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

// Publish queues events on every subscriber.
func (h *Hub) Publish(events ...api.Event) {
	if h == nil || len(events) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.push(events)
	}
}

// Close ends every stream; later subscriptions are refused.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

// push queues events, skipping any already waiting. A resync drops what is
// waiting, which it supersedes, and so does an event that would overflow the
// queue, which becomes a resync.
func (s *Subscriber) push(events []api.Event) {
	s.mu.Lock()
	for _, e := range events {
		switch {
		case e.Kind == api.EventResync || len(s.pending) >= eventQueue && !slices.Contains(s.pending, e):
			s.resync, s.pending = true, s.pending[:0]
		case !slices.Contains(s.pending, e):
			s.pending = append(s.pending, e)
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Take empties the queue, a pending resync first.
func (s *Subscriber) Take() []api.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.Event
	if s.resync {
		out = append(out, api.Event{Kind: api.EventResync})
	}
	out = append(out, s.pending...)
	s.resync, s.pending = false, nil
	return out
}

// TraceEvents returns the observer of a project's trace, which announces the
// views each commit changes. The librarian's workstream has no status,
// conversation or inbox entry; its changes refresh extraction in configuration.
func (h *Hub) TraceEvents(project config.ProjectID, librarian config.WorkstreamID) func(trace.Commit) {
	return func(c trace.Commit) {
		var out []api.Event
		add := func(kind api.EventKind, stream config.WorkstreamID) {
			if e := (api.Event{Kind: kind, Project: project, Workstream: stream}); !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		for _, name := range c.Paths {
			parts := strings.Split(name, "/")
			if name == "charter.md" || name == "documents.jsonl" {
				add(api.EventConfig, "")
			}
			if len(parts) == 1 && name == "ledger.jsonl" {
				add(api.EventSpend, "")
			}
			if len(parts) < 3 || parts[0] != "workstreams" {
				continue
			}
			stream := config.WorkstreamID(parts[1])
			if parts[2] == "ledger.jsonl" {
				add(api.EventSpend, "")
			}
			if stream == librarian {
				add(api.EventConfig, "")
				continue
			}
			add(api.EventWorkstream, stream)
			switch {
			case parts[2] == "questions" || parts[2] == "events.jsonl" || parts[2] == "documents.jsonl":
				// Questions carry the inbox's escalations and rulings,
				// transitions open and close the other decisions and abandon
				// workstreams, whose entries the inbox leaves out, and
				// documents carry packets, final reports and the owner's
				// decisions.
				add(api.EventInbox, stream)
			case parts[2] == "agents" && len(parts) > 3 && parts[3] == trace.ChiefOfStaff:
				add(api.EventConversation, stream)
			case parts[2] == "workflow.json" && c.Workflow != nil:
				if slices.Contains(c.Workflow.Threads, trace.ChiefOfStaff) {
					add(api.EventConversation, stream)
				}
				if c.Workflow.Deliveries {
					// The chief of staff seeing a contest can raise it to
					// the owner's inbox.
					add(api.EventInbox, stream)
				}
			}
		}
		h.Publish(out...)
	}
}

// Serve serves the event stream until the client leaves, a write
// stalls past the write timeout, or the service shuts down. It returns false
// if the hub refuses the subscription before a response is written.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, writeTimeout time.Duration) bool {
	sub, ok := h.Subscribe()
	if !ok {
		return false
	}
	defer h.Unsubscribe(sub)
	control := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Each write gets the write timeout of its own, so an idle stream
	// outlives the server's timeouts and a stalled reader does not.
	write := func(frame string) bool {
		if control.SetWriteDeadline(time.Now().Add(writeTimeout)) != nil {
			return false
		}
		if _, err := io.WriteString(w, frame); err != nil {
			return false
		}
		return control.Flush() == nil
	}
	heartbeat := time.NewTimer(eventHeartbeat)
	defer heartbeat.Stop()
	for {
		for _, e := range sub.Take() {
			data, _ := json.Marshal(e)
			if !write("event: " + string(e.Kind) + "\ndata: " + string(data) + "\n\n") {
				return true
			}
		}
		heartbeat.Reset(eventHeartbeat)
		select {
		case <-sub.wake:
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return true
			}
		case <-r.Context().Done():
			return true
		case <-h.done:
			return true
		}
	}
}

// Done is closed when the hub shuts down.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Wake signals that a subscriber has queued events.
func (s *Subscriber) Wake() <-chan struct{} { return s.wake }

// Idle reports whether there is at least one subscriber and all queues are empty.
func (h *Hub) Idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return false
	}
	for s := range h.subs {
		s.mu.Lock()
		idle := !s.resync && len(s.pending) == 0
		s.mu.Unlock()
		if !idle {
			return false
		}
	}
	return true
}
