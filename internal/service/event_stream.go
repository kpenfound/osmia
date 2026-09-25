package service

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// eventQueue is how many events one subscriber may have waiting. The next
// event beyond it replaces them all with a resync.
const eventQueue = 64

// eventHeartbeat is how long a stream may stay silent before the service
// writes a comment, which finds readers that went away.
const eventHeartbeat = 15 * time.Second

// hub fans change notifications out to the subscribers of the event stream.
// Publishing never blocks on a subscriber. A nil hub drops everything.
type hub struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
	done   chan struct{}
	// chief holds, by workstream, a hash of the chief-of-staff thread as the
	// last commit of the workstream's workflow wrote it.
	chief map[config.WorkstreamID][32]byte
}

func newHub() *hub {
	return &hub{subs: map[*subscriber]struct{}{}, done: make(chan struct{}), chief: map[config.WorkstreamID][32]byte{}}
}

// subscriber is one stream's queue. It starts with a resync pending, so the
// stream's first event asks the client to read everything.
type subscriber struct {
	mu      sync.Mutex
	pending []Event
	resync  bool
	wake    chan struct{}
}

// subscribe registers a new subscriber, or reports false once the hub closed.
func (h *hub) subscribe() (*subscriber, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	s := &subscriber{resync: true, wake: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return s, true
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

// publish queues events on every subscriber.
func (h *hub) publish(events ...Event) {
	if h == nil || len(events) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.push(events)
	}
}

// close ends every stream; later subscriptions are refused.
func (h *hub) close() {
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
func (s *subscriber) push(events []Event) {
	s.mu.Lock()
	for _, e := range events {
		switch {
		case e.Kind == EventResync || len(s.pending) >= eventQueue && !slices.Contains(s.pending, e):
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

// take empties the queue, a pending resync first.
func (s *subscriber) take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	if s.resync {
		out = append(out, Event{Kind: EventResync})
	}
	out = append(out, s.pending...)
	s.resync, s.pending = false, nil
	return out
}

// traceEvents returns the observer of a project's trace, which announces the
// views each commit changes. The librarian's workstream has no status,
// conversation or inbox entry, so only its spend is announced.
func (h *hub) traceEvents(project config.ProjectID) func(trace.Commit) {
	librarian := librarianWorkstream(project)
	return func(c trace.Commit) {
		var out []Event
		add := func(kind EventKind, stream config.WorkstreamID) {
			if e := (Event{Kind: kind, Project: project, Workstream: stream}); !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		for _, name := range c.Paths {
			parts := strings.Split(name, "/")
			if len(parts) == 1 && name == "ledger.jsonl" {
				add(EventSpend, "")
			}
			if len(parts) < 3 || parts[0] != "workstreams" {
				continue
			}
			stream := config.WorkstreamID(parts[1])
			if parts[2] == "ledger.jsonl" {
				add(EventSpend, "")
			}
			if stream == librarian {
				continue
			}
			add(EventWorkstream, stream)
			switch {
			case parts[2] == "questions" || parts[2] == "events.jsonl" || parts[2] == "documents.jsonl":
				// Questions carry the inbox's escalations and rulings,
				// transitions open and close the other decisions and abandon
				// workstreams, whose entries the inbox leaves out, and
				// documents carry packets, final reports and the owner's
				// decisions.
				add(EventInbox, stream)
			case parts[2] == "agents" && len(parts) > 3 && parts[3] == trace.ChiefOfStaff:
				add(EventConversation, stream)
			case parts[2] == "workflow.json" && h.chiefChanged(stream, c.Content[name]):
				add(EventConversation, stream)
			}
		}
		h.publish(out...)
	}
}

// chiefChanged reports whether a workstream's workflow, as a commit wrote it,
// holds a chief-of-staff thread other than the one last seen. A workflow the
// commit read from the work tree, or one that cannot be decoded, counts as a
// change.
func (h *hub) chiefChanged(stream config.WorkstreamID, workflow []byte) bool {
	if h == nil {
		return false
	}
	var log struct {
		Threads map[string]json.RawMessage `json:"threads"`
	}
	if workflow == nil || json.Unmarshal(workflow, &log) != nil {
		return true
	}
	sum := sha256.Sum256(log.Threads[trace.ChiefOfStaff])
	h.mu.Lock()
	defer h.mu.Unlock()
	last, seen := h.chief[stream]
	h.chief[stream] = sum
	return !seen || last != sum
}

// streamEvents serves the event stream until the client leaves, a write
// stalls past the write timeout, or the service shuts down.
func (s *Service) streamEvents(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.hub.subscribe()
	if !ok {
		fail(w, Unavailable)
		return
	}
	defer s.hub.unsubscribe(sub)
	control := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Each write gets the write timeout of its own, so an idle stream
	// outlives the server's timeouts and a stalled reader does not.
	write := func(frame string) bool {
		if control.SetWriteDeadline(time.Now().Add(s.options.WriteTimeout)) != nil {
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
		for _, e := range sub.take() {
			data, _ := json.Marshal(e)
			if !write("event: " + string(e.Kind) + "\ndata: " + string(data) + "\n\n") {
				return
			}
		}
		heartbeat.Reset(eventHeartbeat)
		select {
		case <-sub.wake:
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.hub.done:
			return
		}
	}
}
