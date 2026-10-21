package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// Adding and removing a project each ask open streams to read everything.
func TestEventStreamResyncsWhenTheProjectChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := projectFixture(t)
	_, c := start(t, opts)
	events, _ := eventStream(t, c)
	nextEvent(t, events)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	awaitEvents(t, events, Event{Kind: EventResync})
	_, err = c.RemoveProject(ctx, added.Project.ID)
	must(t, err)
	awaitEvents(t, events, Event{Kind: EventResync})
}

// A stream starts with a resync. After it, every runtime override announces a
// runtime change; a reload announces the configuration and the views that
// follow it, and a failed reload the configuration's last error.
func TestEventStreamAnnouncesRuntimeAndConfigurationChanges(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	events, _ := eventStream(t, c)
	if e := nextEvent(t, events); e != (Event{Kind: EventResync}) {
		t.Fatalf("first event: %+v", e)
	}
	for _, m := range []struct {
		method, kind string
		input        any
	}{
		{"PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft"}},
		{"DELETE", "pause", ClearPauseRequest{Scope: "factory"}},
		{"PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}}},
		{"DELETE", "priority", ClearPriorityRequest{Project: project}},
		{"PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"}},
		{"DELETE", "profile", ClearProfileRequest{Role: "mason"}},
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
	return start(t, eventTraceOptions(t))
}

// eventTraceOptions plants a workstream with one escalated question in the
// trace and returns the options of a service that has not started.
func eventTraceOptions(t *testing.T) Options {
	t.Helper()
	opts, cfg := conversationFixture(t, "evs-")
	fixturekit.SeedEventTrace(t, cfg)
	return opts
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

	if _, err := c.Answer(ctx, 1, "In files.", ""); err != nil {
		t.Fatal(err)
	}
	awaitEvents(t, events, Event{Kind: EventInbox, Project: project, Workstream: quiet}, Event{Kind: EventWorkstream, Project: project, Workstream: quiet})

	appendDayCost(t, s.sole().repository, stream, "cost", demoStart, 0.25, true)
	awaitEvents(t, events, Event{Kind: EventSpend, Project: project}, Event{Kind: EventWorkstream, Project: project, Workstream: stream})

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature_handed", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner"}
	_, err := s.sole().repository.SetFeatureState(ctx, h, "handed", "Owner handed the feature in")
	must(t, err)
	awaitEvents(t, events, Event{Kind: EventWorkstream, Project: project, Workstream: stream}, Event{Kind: EventInbox, Project: project, Workstream: stream})
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
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
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
