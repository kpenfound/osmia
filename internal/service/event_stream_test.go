package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// Adding and removing a project each ask open streams to read everything.
func TestEventStreamResyncsWhenTheProjectChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone := projectFixture(t)
	_, c := start(t, opts)
	events, _ := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	fixturekit.AwaitEvents(t, events, Event{Kind: EventResync})
	_, err = c.RemoveProject(ctx, added.Project.ID)
	fixturekit.Must(t, err)
	fixturekit.AwaitEvents(t, events, Event{Kind: EventResync})
}

// A stream starts with a resync. After it, every runtime override announces a
// runtime change; a reload announces the configuration and the views that
// follow it, and a failed reload the configuration's last error.
func TestEventStreamAnnouncesRuntimeAndConfigurationChanges(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	events, _ := fixturekit.EventStream(t, c)
	if e := fixturekit.NextEvent(t, events); e != (Event{Kind: EventResync}) {
		t.Fatalf("first event: %+v", e)
	}
	for _, m := range []struct {
		method, kind string
		input        any
	}{
		{"PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft"}},
		{"DELETE", "pause", ClearPauseRequest{Scope: "factory"}},
		{"PUT", "priority", PriorityRequest{Project: fixturekit.Project, Workstreams: []config.WorkstreamID{fixturekit.Stream}}},
		{"DELETE", "priority", ClearPriorityRequest{Project: fixturekit.Project}},
		{"PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"}},
		{"DELETE", "profile", ClearProfileRequest{Role: "mason"}},
	} {
		mutation(t, c, m.method, m.kind, m.input)
		if e := fixturekit.NextEvent(t, events); e != (Event{Kind: EventRuntime}) {
			t.Fatalf("%s %s: %+v", m.method, m.kind, e)
		}
	}
	if _, err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixturekit.AwaitEvents(t, events, Event{Kind: EventConfig}, Event{Kind: EventRuntime}, Event{Kind: EventSpend, Project: fixturekit.Project})
	path := filepath.Join(opts.Config.Root, "config.toml")
	fixturekit.Must(t, os.WriteFile(path, []byte("version = \n"), 0600))
	if _, err := c.Reload(context.Background()); err == nil {
		t.Fatal("invalid configuration reloaded")
	}
	if e := fixturekit.NextEvent(t, events); e != (Event{Kind: EventConfig}) {
		t.Fatalf("failed reload: %+v", e)
	}
}

func eventTraceFixture(t *testing.T) (*Service, *Client) {
	t.Helper()
	return start(t, eventTraceOptions(t))
}

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
	events, _ := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)

	if _, err := c.Send(ctx, fixturekit.Stream, "How is it going?"); err != nil {
		t.Fatal(err)
	}
	fixturekit.AwaitEvents(t, events, Event{Kind: api.EventConversation, Project: fixturekit.Project, Workstream: fixturekit.Stream}, Event{Kind: EventWorkstream, Project: fixturekit.Project, Workstream: fixturekit.Stream})

	if _, err := c.Answer(ctx, 1, "In files.", ""); err != nil {
		t.Fatal(err)
	}
	fixturekit.AwaitEvents(t, events, Event{Kind: EventInbox, Project: fixturekit.Project, Workstream: intakefixture.Quiet}, Event{Kind: EventWorkstream, Project: fixturekit.Project, Workstream: intakefixture.Quiet})

	appendDayCost(t, s.sole().repository, fixturekit.Stream, "cost", fixturekit.DemoStart, 0.25, true)
	fixturekit.AwaitEvents(t, events, Event{Kind: EventSpend, Project: fixturekit.Project}, Event{Kind: EventWorkstream, Project: fixturekit.Project, Workstream: fixturekit.Stream})

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature_handed", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner"}
	_, err := s.sole().repository.SetFeatureState(ctx, h, "handed", "Owner handed the feature in")
	fixturekit.Must(t, err)
	fixturekit.AwaitEvents(t, events, Event{Kind: EventWorkstream, Project: fixturekit.Project, Workstream: fixturekit.Stream}, Event{Kind: EventInbox, Project: fixturekit.Project, Workstream: fixturekit.Stream})
}

// A stream outlives the server's read and write timeouts while it is idle.
func TestEventStreamOutlivesTheRequestTimeouts(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	opts.ReadTimeout, opts.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	_, c := start(t, opts)
	events, done := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)
	time.Sleep(time.Second)
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
	if e := fixturekit.NextEvent(t, events); e != (Event{Kind: EventRuntime}) {
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
			events, done := fixturekit.EventStream(t, c)
			fixturekit.NextEvent(t, events)
			began := time.Now()
			fixturekit.Must(t, stop(s))
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
