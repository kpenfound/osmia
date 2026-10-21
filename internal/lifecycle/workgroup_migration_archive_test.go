package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStartupWrapsCleanedArchivesWithoutRecreatingHistory(t *testing.T) {
	// Serial: the interruption hook is shared with startup migration tests.
	for _, interrupted := range []bool{false, true} {
		name := "restart"
		if interrupted {
			name = "interrupted migration"
		}
		t.Run(name, func(t *testing.T) {
			opts, clone := projectFixture(t)
			ctx := context.Background()
			s, c := start(t, opts)
			first, err := c.AddProject(ctx, request(clone))
			must(t, err)
			otherClone := filepath.Join(filepath.Dir(clone), "other")
			demoGit(t, filepath.Dir(clone), "init", "--quiet", otherClone)
			second, err := c.AddProject(ctx, ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: otherClone})
			must(t, err)
			projects := []config.ProjectID{first.Project.ID, second.Project.ID}
			archives := []runtime.Archive{
				{Project: projects[0], Workstream: "w_00000000000000000000000000000071", State: DeliveredState, Title: "Published request", DeliveryLink: "https://github.com/upstream/repo/pull/71"},
				{Project: projects[0], Workstream: "w_00000000000000000000000000000072", State: AbandonedState, Title: "Cancelled request"},
				{Project: projects[1], Workstream: "w_00000000000000000000000000000073", State: AbandonedState, Title: "Other cancelled request"},
			}
			// One mapping is already durable, as after an earlier partial migration.
			existing, err := trace.EnsureWorkgroups(ctx, s.projectViews()[0].repository, []config.WorkstreamID{archives[0].Workstream}, demoStart, migrationActor, "migrate-workgroups")
			must(t, err)
			must(t, s.Close())
			// This is an upgrade root with completed archive cleanup and no remaining
			// member histories or newer lifecycle evidence. Runtime is the only record
			// of these workstreams; migration must not manufacture their deleted traces.
			for i := range archives {
				archives[i].ArchivedAt = demoStart
				archives[i].CleanedAt = demoStart.Add(time.Minute)
			}
			before, err := json.Marshal(runtime.State{Version: runtime.Version, Archived: archives})
			must(t, err)
			runtimePath := filepath.Join(opts.Config.Root, "runtime.json")
			must(t, os.WriteFile(runtimePath, before, 0600))

			var partial []trace.Workgroup
			if interrupted {
				crash := errors.New("interrupted archive migration")
				lifecycle.EnsureWorkgroupsInterrupt = func(id config.ProjectID) error {
					if id == projects[1] {
						return crash
					}
					return nil
				}
				t.Cleanup(func() { lifecycle.EnsureWorkgroupsInterrupt = nil })
				failed, err := Start(ctx, opts)
				lifecycle.EnsureWorkgroupsInterrupt = nil
				if failed != nil {
					must(t, failed.Close())
				}
				if !errors.Is(err, crash) {
					t.Fatalf("startup interruption: got %v, want %v", err, crash)
				}
				cfg, err := config.Load(opts.Config)
				must(t, err)
				repo, err := trace.Open(cfg.Root, cfg.For(projects[0]).Project)
				must(t, err)
				var revision int
				partial, revision, err = trace.Workgroups(repo)
				repo.Close()
				must(t, err)
				if revision != 2 || len(partial) != 2 {
					t.Fatalf("first project was not migrated before interruption: revision=%d groups=%+v", revision, partial)
				}
			}

			var previous map[config.ProjectID][]trace.Workgroup
			for range 2 {
				s, c = start(t, opts)
				current := map[config.ProjectID][]trace.Workgroup{}
				for _, p := range s.projectViews() {
					groups, revision, err := trace.Workgroups(p.repository)
					must(t, err)
					wantRevision, wantCount := 1, 1
					if p.id == projects[0] {
						wantRevision, wantCount = 2, 2
						if g, ok := trace.WorkgroupOf(groups, archives[0].Workstream); !ok || !reflect.DeepEqual(g, existing[0]) {
							t.Fatalf("existing workgroup changed: %+v", groups)
						}
						if partial != nil && !reflect.DeepEqual(groups, partial) {
							t.Fatalf("resumption changed committed groups: before=%+v after=%+v", partial, groups)
						}
					}
					if revision != wantRevision || len(groups) != wantCount {
						t.Fatalf("archive migration: project=%s revision=%d groups=%+v", p.id, revision, groups)
					}
					current[p.id] = groups
					resolved, resolutionRevision, err := trace.ResolvedWorkgroups(p.repository)
					must(t, err)
					if len(resolved) != 0 || resolutionRevision != 0 {
						t.Fatalf("archive migration invented resolution evidence: revision=%d resolved=%v", resolutionRevision, resolved)
					}
				}
				if previous != nil && !reflect.DeepEqual(previous, current) {
					t.Fatalf("restart changed workgroups: before=%+v after=%+v", previous, current)
				}
				previous = current
				for _, a := range archives {
					g, ok := trace.WorkgroupOf(current[a.Project], a.Workstream)
					if !ok || !reflect.DeepEqual(g.Members, []config.WorkstreamID{a.Workstream}) {
						t.Fatalf("cleaned archive has no solitary workgroup: %+v", a)
					}
					status, err := c.Status(ctx, a.Workstream)
					must(t, err)
					if status.Project != a.Project || !status.Archived || !status.TraceDeleted || status.Workgroup == nil || status.Workgroup.ID != g.ID {
						t.Fatalf("cleaned archive is not visible in its workgroup: %+v", status)
					}
					if !strings.Contains(status.Wait, "no pinned merge or cancel evidence") {
						t.Fatalf("cleaned archive lost its resolution diagnostic: %+v", status)
					}
					if _, err := os.Lstat(filepath.Join(opts.Config.Root, "workstreams", string(a.Project), string(a.Workstream))); !os.IsNotExist(err) {
						t.Fatalf("deleted archive trace was recreated: %v", err)
					}
				}
				must(t, s.Close())
				after, err := os.ReadFile(runtimePath)
				must(t, err)
				if !bytes.Equal(before, after) {
					t.Fatalf("migration rewrote archive records: before=%s after=%s", before, after)
				}
			}
		})
	}
}
func TestWorkgroupMigrationExcludesPinnedArchivesAndRetainedSessions(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	ctx := context.Background()
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	p := s.sole()
	feature := config.WorkstreamID("w_00000000000000000000000000000081")
	session := config.WorkstreamID("w_00000000000000000000000000000082")
	must(t, p.repository.CreateWorkstream(ctx, feature, demoStart, registrationActor))
	must(t, p.repository.CreateSessionWorkstream(ctx, session, "default", "deadbeef", "osmia/session/"+string(session), "exploration", demoStart, registrationActor))
	archives := []runtime.Archive{
		{Project: added.Project.ID, Workstream: feature},
		{Project: added.Project.ID, Workstream: session},
		{Project: added.Project.ID, Workstream: "w_00000000000000000000000000000083", Workgroup: "g_00000000000000000000000000000083"},
		{Project: "p_00000000000000000000000000000084", Workstream: "w_00000000000000000000000000000084"},
		{Project: added.Project.ID, Workstream: librarianWorkstream(added.Project.ID)},
	}
	must(t, s.ensureWorkgroups(ctx, s.current(), []*activeProject{p}, archives))
	groups, revision, err := trace.Workgroups(p.repository)
	must(t, err)
	if revision != 1 || len(groups) != 1 || !reflect.DeepEqual(groups[0].Members, []config.WorkstreamID{feature}) {
		t.Fatalf("migration duplicated a retained feature or wrapped an excluded identity: revision=%d groups=%+v", revision, groups)
	}
}
func TestCleanedArchiveWorkgroupResolutionRequiresExactEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"merged", "cancelled", "legacy resolved flag", "no archive", "no cleanup", "different project", "different workstream"} {
		t.Run(name, func(t *testing.T) {
			opts, cfg := conversationFixture(t, "archive-resolution-")
			ctx := context.Background()
			missing := config.WorkstreamID("w_00000000000000000000000000000091")
			repo, err := trace.Open(cfg.Root, cfg.Project)
			must(t, err)
			t.Cleanup(func() { repo.Close() })
			groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{missing}, demoStart, migrationActor, "migrate-workgroups")
			must(t, err)
			a := runtime.Archive{Project: cfg.Project.ID, Workstream: missing, ArchivedAt: demoStart, CleanedAt: demoStart.Add(time.Minute), State: DeliveredState, Title: "Archived request", Resolved: true}
			wantResolved, wantError := false, false
			switch name {
			case "merged":
				a.ResolutionEvidence, wantResolved = "merged", true
			case "cancelled":
				a.State, a.ResolutionEvidence, wantResolved = AbandonedState, "cancelled", true
			case "no archive":
				wantError = true
			case "no cleanup":
				a.CleanedAt, wantError = time.Time{}, true
			case "different project":
				a.Project, wantError = otherProject, true
			case "different workstream":
				a.Workstream, wantError = quiet, true
			}
			state := runtime.State{Version: runtime.Version}
			if name != "no archive" {
				state.Archived = []runtime.Archive{a}
			}
			data, err := json.Marshal(state)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(opts.Config.Root, "runtime.json"), data, 0600))
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg})
			must(t, err)
			t.Cleanup(func() { store.Close() })
			s := &Service{cfg: cfg, store: store, options: opts}
			resolver := &workgroupResolver{s: s, repository: repo}
			for range 2 {
				err := resolver.Pass(ctx)
				if wantError {
					if err == nil || !strings.Contains(err.Error(), "is missing without verified resolution and cleanup evidence") {
						t.Fatalf("unexplained missing member: %v", err)
					}
				} else {
					must(t, err)
				}
				resolved, revision, err := trace.ResolvedWorkgroups(repo)
				must(t, err)
				if wantResolved {
					if revision != 1 || !reflect.DeepEqual(resolved, []config.WorkgroupID{groups[0].ID}) {
						t.Fatalf("pinned archive did not resolve exactly once: revision=%d resolved=%v", revision, resolved)
					}
				} else if revision != 0 || len(resolved) != 0 {
					t.Fatalf("missing evidence granted resolution: revision=%d resolved=%v", revision, resolved)
				}
			}
			if _, err := os.Lstat(filepath.Join(opts.Config.Root, "workstreams", string(cfg.Project.ID), string(missing))); !os.IsNotExist(err) {
				t.Fatalf("resolver recreated deleted history: %v", err)
			}
		})
	}
}
