package eventstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const project = fixturekit.Project
const stream = fixturekit.Stream
const librarian = config.WorkstreamID("w_00000000000000000000000000000001")

// A commit announces only the views its paths hold, once each, and a
// workflow commit announces the conversation only when the chief of staff's
// thread in it changed.
func TestTraceCommitsAnnounceTheirViews(t *testing.T) {
	t.Parallel()
	h := New()
	sub, _ := h.Subscribe()
	sub.Take()
	observe := h.TraceEvents(project, librarian)
	prefix := "workstreams/" + string(stream) + "/"
	ws := api.Event{Kind: api.EventWorkstream, Project: project, Workstream: stream}
	chat := api.Event{Kind: api.EventConversation, Project: project, Workstream: stream}
	inbox := api.Event{Kind: api.EventInbox, Project: project, Workstream: stream}
	spend := api.Event{Kind: api.EventSpend, Project: project}
	wf := prefix + "workflow.json"
	librarian := "workstreams/" + string(librarian) + "/"
	for _, c := range []struct {
		name   string
		commit trace.Commit
		want   []api.Event
	}{
		{"status", trace.Commit{Paths: []string{prefix + "status.jsonl"}}, []api.Event{ws}},
		{"question", trace.Commit{Paths: []string{prefix + "questions/1/question.jsonl", prefix + "questions/1/rulings.jsonl", wf}, Workflow: &trace.WorkflowChange{Threads: []string{trace.ChiefOfStaff}}}, []api.Event{ws, inbox, chat}},
		{"another thread", trace.Commit{Paths: []string{wf}, Workflow: &trace.WorkflowChange{Threads: []string{"agent_mason"}}}, []api.Event{ws}},
		{"chief thread", trace.Commit{Paths: []string{wf}, Workflow: &trace.WorkflowChange{Threads: []string{"agent_mason", trace.ChiefOfStaff}}}, []api.Event{ws, chat}},
		// An acknowledged event can raise a contest to the owner's inbox.
		{"deliveries", trace.Commit{Paths: []string{wf}, Workflow: &trace.WorkflowChange{Deliveries: true}}, []api.Event{ws, inbox}},
		{"transactions only", trace.Commit{Paths: []string{wf}, Workflow: &trace.WorkflowChange{}}, []api.Event{ws}},
		{"chief log", trace.Commit{Paths: []string{prefix + "agents/" + trace.ChiefOfStaff + "/log.jsonl", prefix + "agents/agent_mason/log.jsonl"}}, []api.Event{ws, chat}},
		{"feature transition", trace.Commit{Paths: []string{prefix + "events.jsonl"}}, []api.Event{ws, inbox}},
		{"packet", trace.Commit{Paths: []string{prefix + "documents.jsonl"}}, []api.Event{ws, inbox}},
		{"cost", trace.Commit{Paths: []string{prefix + "ledger.jsonl"}}, []api.Event{spend, ws}},
		{"librarian", trace.Commit{Paths: []string{librarian + "ledger.jsonl", librarian + "workflow.json", librarian + "questions/1/question.jsonl"}, Workflow: &trace.WorkflowChange{Threads: []string{trace.ChiefOfStaff}, Deliveries: true}}, []api.Event{spend, {Kind: api.EventConfig, Project: project}}},
		{"project files", trace.Commit{Paths: []string{"charter.md", "documents.jsonl", "kb/entities.json"}}, []api.Event{{Kind: api.EventConfig, Project: project}}},
		{"charter", trace.Commit{Paths: []string{"charter.md"}}, []api.Event{{Kind: api.EventConfig, Project: project}}},
		{"extraction result", trace.Commit{Paths: []string{"documents.jsonl"}}, []api.Event{{Kind: api.EventConfig, Project: project}}},
		{"knowledge files", trace.Commit{Paths: []string{"kb/entities.json"}}, nil},
	} {
		observe(c.commit)
		if got := sub.Take(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Events already waiting are not queued twice, and the event beyond the
// queue's bound replaces what waits with a resync, which later events follow.
func TestSubscriberQueueIsBoundedAndCoalesced(t *testing.T) {
	t.Parallel()
	h := New()
	sub, _ := h.Subscribe()
	if got := sub.Take(); !reflect.DeepEqual(got, []api.Event{{Kind: api.EventResync}}) {
		t.Fatalf("first take: %+v", got)
	}
	numbered := func(i int) api.Event {
		return api.Event{Kind: api.EventWorkstream, Project: project, Workstream: config.WorkstreamID(fmt.Sprintf("w_%032x", i))}
	}
	var full []api.Event
	for i := range eventQueue {
		full = append(full, numbered(i))
	}
	h.Publish(full...)
	h.Publish(full...)
	if got := sub.Take(); !reflect.DeepEqual(got, full) {
		t.Fatalf("a full queue: %d events", len(got))
	}
	h.Publish(full...)
	h.Publish(numbered(eventQueue), numbered(eventQueue+1))
	if got := sub.Take(); !reflect.DeepEqual(got, []api.Event{{Kind: api.EventResync}, numbered(eventQueue + 1)}) {
		t.Fatalf("an overflowing queue: %+v", got)
	}
	h.Publish(numbered(1), api.Event{Kind: api.EventResync}, numbered(2))
	if got := sub.Take(); !reflect.DeepEqual(got, []api.Event{{Kind: api.EventResync}, numbered(2)}) {
		t.Fatalf("a published resync: %+v", got)
	}
}

// A reader that stops reading neither delays publishers nor another
// subscriber, and its stream ends once a write outlasts the write timeout.
func TestStalledEventReaderIsDroppedWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	s := New()
	other, _ := s.Subscribe()
	served, stalled := make(chan struct{}), &stalledWriter{header: http.Header{}, wrote: make(chan struct{})}
	go func() {
		s.Serve(stalled, httptest.NewRequest("GET", api.Prefix+"/events", nil), 200*time.Millisecond)
		close(served)
	}()
	// The stream has subscribed once its resync is written.
	<-stalled.wrote
	began := time.Now()
	for i := range 10 * eventQueue {
		s.Publish(api.Event{Kind: api.EventWorkstream, Project: project, Workstream: config.WorkstreamID(fmt.Sprintf("w_%032x", i))})
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("publishing past a stalled reader took %s", elapsed)
	}
	if got := other.Take(); got[0] != (api.Event{Kind: api.EventResync}) || len(got) > 1+eventQueue {
		t.Fatalf("the other subscriber: %d events, first %+v", len(got), got[0])
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled stream outlasted its write timeout")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[other]; !ok || len(s.subs) != 1 {
		t.Fatalf("subscribers after the stalled stream ended: %d", len(s.subs))
	}
}

// stalledWriter is a stream response whose reader stopped reading: every write
// after the first blocks until its write deadline passes. It closes wrote once
// the first write returns.
type stalledWriter struct {
	header   http.Header
	wrote    chan struct{}
	mu       sync.Mutex
	deadline time.Time
	writes   int
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	first, deadline := w.writes == 1, w.deadline
	w.mu.Unlock()
	if first {
		close(w.wrote)
		return len(b), nil
	}
	if deadline.IsZero() {
		select {}
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}
func (w *stalledWriter) FlushError() error { return nil }
func (w *stalledWriter) SetReadDeadline(time.Time) error {
	return nil
}
func (w *stalledWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	return nil
}
