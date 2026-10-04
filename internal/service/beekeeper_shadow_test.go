package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// The service creates the Beekeeper's shadow project on first start and
// reopens it, unchanged, under the same reserved identifier on a later start
// against the same state directory. It has no target repository: nothing is
// cloned or configured for it.
func TestShadowProjectCreatedThenReopenedAcrossRestart(t *testing.T) {
	t.Parallel()
	home, err := os.MkdirTemp("", "bk-restart-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)

	s1, err := Start(context.Background(), opts)
	must(t, err)
	root := s1.cfg.Root
	directory, err := root.ProjectTrace(config.ShadowProjectID)
	must(t, err)
	if _, err := os.Stat(filepath.Join(directory, ".git")); err != nil {
		t.Fatalf("shadow trace not created under the service state directory: %v", err)
	}
	streams, err := s1.Beekeeper().Workstreams()
	must(t, err)
	if !reflect.DeepEqual(streams, []config.WorkstreamID{config.BeekeeperWorkstreamID}) {
		t.Fatalf("shadow workstreams: %v", streams)
	}
	if _, err := os.Stat(filepath.Join(home, "clone")); !os.IsNotExist(err) {
		t.Fatalf("a target repository was cloned for the shadow project: %v", err)
	}
	before := snapshot(t, directory)
	must(t, s1.Close())

	s2, err := Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s2.Close() })
	if !reflect.DeepEqual(snapshot(t, directory), before) {
		t.Fatal("the shadow project's trace changed across a restart that only reopens it")
	}
	streams, err = s2.Beekeeper().Workstreams()
	must(t, err)
	if !reflect.DeepEqual(streams, []config.WorkstreamID{config.BeekeeperWorkstreamID}) {
		t.Fatalf("shadow workstreams after restart: %v", streams)
	}
}

// The shadow project exists and the Beekeeper's thread can be opened even
// with no owner project registered, and with Hearsay not configured.
func TestShadowProjectExistsWithNoOwnerProjectRegistered(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	s, err := Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	if len(s.cfg.Projects) != 0 {
		t.Fatalf("expected no registered project: %+v", s.cfg.Projects)
	}
	if s.cfg.Hearsay.URL != "" {
		t.Fatalf("expected Hearsay not configured: %+v", s.cfg.Hearsay)
	}
	streams, err := s.Beekeeper().Workstreams()
	must(t, err)
	if !reflect.DeepEqual(streams, []config.WorkstreamID{config.BeekeeperWorkstreamID}) {
		t.Fatalf("shadow workstreams: %v", streams)
	}
}

// The shadow project appears in no service-level project or workstream
// listing or status query, and a project-scoped lookup with its identifier
// gets the same not-found result as an unknown project.
func TestShadowProjectHiddenFromListingsAndLookups(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	_, c := start(t, opts)
	ctx := context.Background()

	cfg, err := c.Configuration(ctx)
	must(t, err)
	for _, p := range cfg.Projects {
		if p.ID == config.ShadowProjectID {
			t.Fatalf("shadow project listed in the configuration response: %+v", cfg.Projects)
		}
	}
	rt, err := c.Runtime(ctx)
	must(t, err)
	for _, p := range rt.Projects {
		if p.Project == config.ShadowProjectID {
			t.Fatalf("shadow project listed in the runtime response: %+v", rt.Projects)
		}
	}
	statuses, err := c.Statuses(ctx)
	must(t, err)
	for _, w := range statuses.Workstreams {
		if w.Workstream == config.BeekeeperWorkstreamID {
			t.Fatalf("reserved Beekeeper workstream listed in statuses: %+v", statuses.Workstreams)
		}
	}

	const unknown config.ProjectID = "p_9999999999999999999999999999ffff"
	_, shadowErr := c.HandIn(ctx, HandInRequest{Project: config.ShadowProjectID})
	_, unknownErr := c.HandIn(ctx, HandInRequest{Project: unknown})
	var shadowAPI, unknownAPI *APIError
	if !errors.As(shadowErr, &shadowAPI) || !errors.As(unknownErr, &unknownAPI) {
		t.Fatalf("expected API errors: shadow=%v unknown=%v", shadowErr, unknownErr)
	}
	if shadowAPI.Code != NotFound || shadowAPI.Code != unknownAPI.Code {
		t.Fatalf("shadow lookup did not behave like an unknown project: shadow=%+v unknown=%+v", shadowAPI, unknownAPI)
	}
}

// The shadow project is never part of the registered-project runtime the
// scheduler and reconciliation loops act on, so no shed, architect, mason,
// reviewer or delivery work is ever scheduled for it.
func TestShadowProjectRunsNoWorkflow(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, _ := start(t, opts)
	for _, p := range s.projects {
		if p.id == config.ShadowProjectID {
			t.Fatal("the shadow project is wired into the reconciliation loop")
		}
	}
	for _, repo := range s.traces() {
		if repo.Project() == config.ShadowProjectID {
			t.Fatal("the shadow project's trace shares the scheduler's role capacity pool")
		}
	}
	streams, err := s.Beekeeper().Workstreams()
	must(t, err)
	if len(streams) != 1 || !slices.Contains(streams, config.BeekeeperWorkstreamID) {
		t.Fatalf("shadow project workstreams: %v", streams)
	}
}
