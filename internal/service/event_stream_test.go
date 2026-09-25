package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// eventStream reads c's event stream until the test ends, and returns the
// events received and a channel that yields Events' result once it returns.
func eventStream(t *testing.T, c *Client) (<-chan Event, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events, done := make(chan Event, 1024), make(chan error, 1)
	go func() {
		done <- c.Events(ctx, func(e Event) error {
			events <- e
			return nil
		})
	}()
	t.Cleanup(cancel)
	return events, done
}

// nextEvent returns the stream's next event.
func nextEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
		return Event{}
	}
}

// awaitEvents reads the stream until it has seen every one of want, in any
// order, and returns everything it read.
func awaitEvents(t *testing.T, events <-chan Event, want ...Event) []Event {
	t.Helper()
	var seen []Event
	for missing := slices.Clone(want); len(missing) > 0; {
		e := nextEvent(t, events)
		seen = append(seen, e)
		missing = slices.DeleteFunc(missing, func(w Event) bool { return w == e })
	}
	return seen
}

func TestEventStreamStartsEveryConnectionWithAResync(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	events, _ := eventStream(t, c)
	if e := nextEvent(t, events); e != (Event{Kind: EventResync}) {
		t.Fatalf("first event: %+v", e)
	}
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	awaitEvents(t, events, Event{Kind: EventRuntime})

	// Changes made while no stream is open are covered by the next stream's
	// resync, which comes before anything that follows.
	mutation(t, c, "DELETE", "profile", ClearProfileRequest{"mason"})
	again, _ := eventStream(t, c)
	if e := nextEvent(t, again); e != (Event{Kind: EventResync}) {
		t.Fatalf("first event after reconnecting: %+v", e)
	}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft"})
	if e := nextEvent(t, again); e != (Event{Kind: EventRuntime}) {
		t.Fatalf("event after the reconnect's resync: %+v", e)
	}
}

// Every runtime override announces a runtime change; a reload announces the
// configuration and the views that follow it, and a failed reload the
// configuration's last error.
func TestEventStreamAnnouncesRuntimeAndConfigurationChanges(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	events, _ := eventStream(t, c)
	nextEvent(t, events)
	for _, m := range []struct {
		method, kind string
		input        any
	}{
		{"PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft"}},
		{"DELETE", "pause", ClearPauseRequest{Scope: "factory"}},
		{"PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}}},
		{"DELETE", "priority", ClearPriorityRequest{project}},
		{"PUT", "profile", ProfileRequest{"mason", "other"}},
		{"DELETE", "profile", ClearProfileRequest{"mason"}},
	} {
		mutation(t, c, m.method, m.kind, m.input)
		if e := nextEvent(t, events); e != (Event{Kind: EventRuntime}) {
			t.Fatalf("%s %s: %+v", m.method, m.kind, e)
		}
	}
	if _, err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEvents(t, events, Event{Kind: EventConfig}, Event{Kind: EventRuntime}, Event{Kind: EventSpend, Project: project})
	path := filepath.Join(opts.Config.Root, "config.toml")
	must(t, os.WriteFile(path, []byte("version = \n"), 0600))
	if _, err := c.Reload(context.Background()); err == nil {
		t.Fatal("invalid configuration reloaded")
	}
	if e := nextEvent(t, events); e != (Event{Kind: EventConfig}) {
		t.Fatalf("failed reload: %+v", e)
	}
}

// eventTraceFixture is a service whose trace holds stream and quiet, with
// quiet's question escalated to the owner as inbox entry 1.
func eventTraceFixture(t *testing.T) (*Service, *Client) {
	t.Helper()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "evs-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: quiet, At: demoStart, Actor: ownerActor, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	sessions := t.TempDir()
	_, err = repo.Ask(ctx, demoAgent, claimTurn(t, repo, sessions, quiet, demoAgent, demoThread, "build", demoStart), "Where does state live?", demoStart)
	must(t, err)
	_, err = repo.EnsureChiefOfStaff(ctx, quiet, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, claimTurn(t, repo, sessions, quiet, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", demoStart),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Recommendation: "In files."}, demoStart)
	must(t, err)
	must(t, repo.Close())
	return start(t, opts)
}

// The workstream, conversation, inbox and spend views each announce their
// changes with the project and, where one applies, the workstream.
func TestEventStreamAnnouncesTraceChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c := eventTraceFixture(t)
	events, _ := eventStream(t, c)
	nextEvent(t, events)

	if _, err := c.Send(ctx, stream, "How is it going?"); err != nil {
		t.Fatal(err)
	}
	awaitEvents(t, events, Event{Kind: EventConversation, Project: project, Workstream: stream}, Event{Kind: EventWorkstream, Project: project, Workstream: stream})

	if _, err := c.Answer(ctx, 1, "In files."); err != nil {
		t.Fatal(err)
	}
	awaitEvents(t, events, Event{Kind: EventInbox, Project: project, Workstream: quiet}, Event{Kind: EventWorkstream, Project: project, Workstream: quiet})

	appendDayCost(t, s.active.repository, stream, "cost", demoStart, 0.25, true)
	awaitEvents(t, events, Event{Kind: EventSpend, Project: project}, Event{Kind: EventWorkstream, Project: project, Workstream: stream})

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature_handed", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner"}
	_, err := s.active.repository.SetFeatureState(ctx, h, "handed", "Owner handed the feature in")
	must(t, err)
	awaitEvents(t, events, Event{Kind: EventWorkstream, Project: project, Workstream: stream}, Event{Kind: EventInbox, Project: project, Workstream: stream})
}

// A commit announces only the views its paths hold, once each, and a
// workflow commit announces the conversation only when the chief of staff's
// thread in it changed.
func TestTraceCommitsAnnounceTheirViews(t *testing.T) {
	t.Parallel()
	h := newHub()
	sub, _ := h.subscribe()
	sub.take()
	observe := h.traceEvents(project)
	prefix := "workstreams/" + string(stream) + "/"
	ws := Event{Kind: EventWorkstream, Project: project, Workstream: stream}
	chat := Event{Kind: EventConversation, Project: project, Workstream: stream}
	inbox := Event{Kind: EventInbox, Project: project, Workstream: stream}
	spend := Event{Kind: EventSpend, Project: project}
	workflow := func(chief string) []byte {
		return []byte(`{"schema":"osmia.workflow","version":1,"transactions":[],"threads":{"` + trace.ChiefOfStaff + `":` + chief + `,"agent_mason":{"turns":[]}}}`)
	}
	librarian := "workstreams/" + string(librarianWorkstream(project)) + "/"
	for _, c := range []struct {
		name   string
		commit trace.Commit
		want   []Event
	}{
		{"status", trace.Commit{Paths: []string{prefix + "status.jsonl"}}, []Event{ws}},
		{"question", trace.Commit{Paths: []string{prefix + "questions/1/question.jsonl", prefix + "questions/1/rulings.jsonl", prefix + "workflow.json"}, Content: map[string][]byte{prefix + "workflow.json": workflow(`{"turns":[]}`)}}, []Event{ws, inbox, chat}},
		{"unchanged chief thread", trace.Commit{Paths: []string{prefix + "workflow.json"}, Content: map[string][]byte{prefix + "workflow.json": workflow(`{"turns":[]}`)}}, []Event{ws}},
		{"changed chief thread", trace.Commit{Paths: []string{prefix + "workflow.json"}, Content: map[string][]byte{prefix + "workflow.json": workflow(`{"turns":[{}]}`)}}, []Event{ws, chat}},
		{"workflow read from disk", trace.Commit{Paths: []string{prefix + "workflow.json"}}, []Event{ws, chat}},
		{"chief log", trace.Commit{Paths: []string{prefix + "agents/" + trace.ChiefOfStaff + "/log.jsonl", prefix + "agents/agent_mason/log.jsonl"}}, []Event{ws, chat}},
		{"feature transition", trace.Commit{Paths: []string{prefix + "events.jsonl"}}, []Event{ws, inbox}},
		{"cost", trace.Commit{Paths: []string{prefix + "ledger.jsonl"}}, []Event{spend, ws}},
		{"librarian", trace.Commit{Paths: []string{librarian + "ledger.jsonl", librarian + "workflow.json", librarian + "questions/1/question.jsonl"}}, []Event{spend}},
		{"project files", trace.Commit{Paths: []string{"charter.md", "documents.jsonl", "kb/entities.json"}}, nil},
	} {
		observe(c.commit)
		if got := sub.take(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Events already waiting are not queued twice, and the event beyond the
// queue's bound replaces what waits with a resync, which later events follow.
func TestSubscriberQueueIsBoundedAndCoalesced(t *testing.T) {
	t.Parallel()
	h := newHub()
	sub, _ := h.subscribe()
	if got := sub.take(); !reflect.DeepEqual(got, []Event{{Kind: EventResync}}) {
		t.Fatalf("first take: %+v", got)
	}
	numbered := func(i int) Event {
		return Event{Kind: EventWorkstream, Project: project, Workstream: config.WorkstreamID(fmt.Sprintf("w_%032x", i))}
	}
	var full []Event
	for i := range eventQueue {
		full = append(full, numbered(i))
	}
	h.publish(full...)
	h.publish(full...)
	if got := sub.take(); !reflect.DeepEqual(got, full) {
		t.Fatalf("a full queue: %d events", len(got))
	}
	h.publish(full...)
	h.publish(numbered(eventQueue), numbered(eventQueue+1))
	if got := sub.take(); !reflect.DeepEqual(got, []Event{{Kind: EventResync}, numbered(eventQueue + 1)}) {
		t.Fatalf("an overflowing queue: %+v", got)
	}
	h.publish(numbered(1), Event{Kind: EventResync}, numbered(2))
	if got := sub.take(); !reflect.DeepEqual(got, []Event{{Kind: EventResync}, numbered(2)}) {
		t.Fatalf("a published resync: %+v", got)
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

// A reader that stops reading neither delays publishers nor another
// subscriber, and its stream ends once a write outlasts the write timeout.
func TestStalledEventReaderIsDroppedWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	s := &Service{hub: newHub(), options: Options{WriteTimeout: 200 * time.Millisecond}}
	other, _ := s.hub.subscribe()
	served, stalled := make(chan struct{}), &stalledWriter{header: http.Header{}, wrote: make(chan struct{})}
	go func() {
		s.streamEvents(stalled, httptest.NewRequest("GET", Prefix+"/events", nil))
		close(served)
	}()
	// The stream has subscribed once its resync is written.
	<-stalled.wrote
	began := time.Now()
	for i := range 10 * eventQueue {
		s.hub.publish(Event{Kind: EventWorkstream, Project: project, Workstream: config.WorkstreamID(fmt.Sprintf("w_%032x", i))})
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("publishing past a stalled reader took %s", elapsed)
	}
	if got := other.take(); got[0] != (Event{Kind: EventResync}) || len(got) > 1+eventQueue {
		t.Fatalf("the other subscriber: %d events, first %+v", len(got), got[0])
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled stream outlasted its write timeout")
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if _, ok := s.hub.subs[other]; !ok || len(s.hub.subs) != 1 {
		t.Fatalf("subscribers after the stalled stream ended: %d", len(s.hub.subs))
	}
}

// A stream outlives the server's read and write timeouts while it is idle.
func TestEventStreamOutlivesTheRequestTimeouts(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	opts.ReadTimeout, opts.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	_, c := start(t, opts)
	events, done := eventStream(t, c)
	nextEvent(t, events)
	time.Sleep(time.Second)
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	if e := nextEvent(t, events); e != (Event{Kind: EventRuntime}) {
		t.Fatalf("event after idling past the timeouts: %+v", e)
	}
	select {
	case err := <-done:
		t.Fatalf("stream ended: %v", err)
	default:
	}
}

// Shutdown ends open streams at once rather than waiting out the drain,
// whether it was asked for or a reconciliation loop failure began it, and the
// client reports the stream's end as unavailable.
func TestShutdownEndsEventStreams(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]func(*Service) error{
		"close": func(s *Service) error { return s.Close() },
		"loop failure": func(s *Service) error {
			s.failures <- errors.New("loop failed")
			if err := s.Wait(); err == nil || err.Error() != "loop failed" {
				return fmt.Errorf("service result: %v", err)
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			opts.ShutdownTimeout = 5 * time.Second
			s, c := start(t, opts)
			events, done := eventStream(t, c)
			nextEvent(t, events)
			began := time.Now()
			must(t, stop(s))
			if elapsed := time.Since(began); elapsed > 2*time.Second {
				t.Fatalf("shutdown with an open stream took %s", elapsed)
			}
			select {
			case err := <-done:
				var api *APIError
				if !errors.As(err, &api) || api.Code != Unavailable {
					t.Fatalf("stream end: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the client's stream outlived the service")
			}
		})
	}
}
