package service

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// awaitDrained waits until no removed project is still draining.
func awaitDrained(t *testing.T, s *Service) {
	t.Helper()
	soon(t, "the removed projects to drain", func() bool { return len(s.drainingProjects()) == 0 })
}

// list rewrites active_projects in the fixture's top-level configuration.
func (f *twoProjectFixture) list(t *testing.T, ids ...config.ProjectID) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + string(id) + `"`
	}
	edited := regexp.MustCompile(`(?m)^active_projects = \[.*\]$`).ReplaceAllString(string(text), "active_projects = ["+strings.Join(quoted, ", ")+"]")
	must(t, os.WriteFile(path, []byte(edited), 0600))
}

// withMasons sets the fixture's shared mason capacity.
func (f *twoProjectFixture) withMasons(t *testing.T, n string) {
	t.Helper()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(text), "masons = 1\n", "masons = "+n+"\n", 1)), 0600))
}

// operations waits until the project's workstream has want operations, every
// one acknowledged.
func (f *twoProjectFixture) operations(t *testing.T, repo *trace.Repository, id config.ProjectID, want int) {
	t.Helper()
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := repo.Operations(f.streams[id])
		must(t, err)
		if len(ops) != want {
			return nil
		}
		return ops
	})
}

func drainingIDs(t *testing.T, c *Client) []config.ProjectID {
	t.Helper()
	st, err := c.Statuses(context.Background())
	must(t, err)
	var out []config.ProjectID
	for _, p := range st.Draining {
		out = append(out, p.ID)
	}
	return out
}

func TestRemovedProjectFinishesItsTurnInFlightThenStops(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	f.withMasons(t, "2")
	entered, release := f.block(project, "first")
	s, c := start(t, f.opts)
	await(t, "the removed project's turn", entered)
	f.operations(t, f.repository(otherProject), otherProject, 1)
	removing := f.repository(project)
	// The thread is busy, so its next turn waits in the queue.
	f.queue(t, removing, project, "second")

	removed, err := c.RemoveProject(ctx, project)
	must(t, err)
	if removed.Project.ID != project || len(removed.Unfinished) != 1 || removed.Unfinished[0] != (UnfinishedWorkstream{Workstream: stream}) || !strings.Contains(removed.NextStep, "The project drains") {
		t.Fatalf("remove response: %+v", removed)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject {
		t.Fatalf("configuration after removal: %+v", cfg.Projects)
	}
	// Several passes run while the turn is in flight: the project drains,
	// its turn keeps its slot, and nothing new is dispatched.
	time.Sleep(2500 * time.Millisecond)
	if got := drainingIDs(t, c); !slices.Equal(got, []config.ProjectID{project}) {
		t.Fatalf("draining %v", got)
	}
	if got := f.dispatchedTurns(t, removing, project); !slices.Equal(got, []string{turn(project, "first")}) {
		t.Fatalf("draining project dispatched %v", got)
	}
	if traces := s.traces(); len(traces) != 2 || traces[1] != removing {
		t.Fatalf("capacity pool: %v", traces)
	}
	st, d := s.capacityStatus(nil)
	if d != nil || st.Roles[0].Used != 1 {
		t.Fatalf("capacity while draining: %+v %+v", st, d)
	}

	close(release)
	awaitDrained(t, s)
	if got := drainingIDs(t, c); len(got) != 0 {
		t.Fatalf("still draining %v", got)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("capacity pool after the drain: %v", traces)
	}
	// The other project keeps running.
	other := f.repository(otherProject)
	f.queue(t, other, otherProject, "second")
	f.operations(t, other, otherProject, 2)

	// The drained trace is closed and kept: its turn finished and the queued
	// one never ran.
	repo, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
	must(t, err)
	if got := f.dispatchedTurns(t, repo, project); !slices.Equal(got, []string{turn(project, "first")}) {
		t.Fatalf("drained project dispatched %v", got)
	}
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	for _, q := range th.Turns {
		finished := !q.CompletedAt.IsZero()
		if first := q.Request.TurnID == turn(project, "first"); finished != first || q.Claim != nil && !first {
			t.Fatalf("turn %s: %+v", q.Request.TurnID, q)
		}
	}
	must(t, repo.Close())

	// A restart does not bring the removed project back.
	must(t, s.Close())
	s, c = start(t, f.opts)
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{otherProject}) {
		t.Fatalf("active projects after restart %v", got)
	}
	if traces := s.traces(); len(traces) != 1 || traces[0].Project() != otherProject {
		t.Fatalf("running after restart: %v", traces)
	}
	if got := drainingIDs(t, c); len(got) != 0 {
		t.Fatalf("draining after restart %v", got)
	}
}

func TestReloadStartsListedProjectsAndDrainsUnlistedOnes(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	f.list(t, project)
	s, c := start(t, f.opts)
	first := f.repository(project)
	f.operations(t, first, project, 1)
	if f.repository(otherProject) != nil {
		t.Fatal("an unlisted project opened")
	}
	entered, release := f.block(project, "second")
	f.queue(t, first, project, "second")
	await(t, "the second turn", entered)

	// The reload starts the listed project and drains the other.
	f.list(t, otherProject)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	if len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload: %+v", reloaded)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != otherProject || hasCode(cfg.Diagnostics, "configuration", RestartRequired) || hasCode(cfg.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("configuration after the reload: %+v", cfg)
	}
	other := f.repository(otherProject)
	if other == nil {
		t.Fatal("the listed project did not open")
	}
	f.operations(t, other, otherProject, 1)
	if got := drainingIDs(t, c); !slices.Equal(got, []config.ProjectID{project}) {
		t.Fatalf("draining %v", got)
	}

	// Listing the project again while it drains is refused.
	f.list(t, otherProject, project)
	reloadFails(t, c, filepath.Join(f.opts.Config.Root, "config.toml"), "active_projects")
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.LastError == nil || !strings.Contains(now.LastError.Message, "project "+string(project)+" is still draining") || len(now.Projects) != 1 {
		t.Fatalf("after the refused reload: %+v", now)
	}

	close(release)
	awaitDrained(t, s)
	// Once drained, listing it again reopens its trace and it dispatches.
	_, err = c.Reload(ctx)
	must(t, err)
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{otherProject, project}) {
		t.Fatalf("active projects %v", got)
	}
	again := f.repository(project)
	if again == nil || again == first {
		t.Fatal("the relisted project did not reopen its trace")
	}
	f.operations(t, again, project, 2)
	f.queue(t, again, project, "third")
	f.operations(t, again, project, 3)
	if got := f.dispatchedTurns(t, again, project); !slices.Equal(got, []string{turn(project, "first"), turn(project, "second"), turn(project, "third")}) {
		t.Fatalf("relisted project dispatched %v", got)
	}
}
