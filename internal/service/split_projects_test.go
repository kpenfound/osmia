package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func splitFixture(t *testing.T) (*twoProjectFixture, map[config.ProjectID]string, trace.Document) {
	t.Helper()
	f := newTwoProjectFixture(t)
	r, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
	must(t, err)
	charter, err := r.EditCharter(context.Background(), 1, "# Group rules\n\n1. Keep identities.\n\n7. Preserve numbered citations.\n", demoStart)
	must(t, err)
	must(t, r.Close())
	before := map[config.ProjectID]string{}
	for _, p := range f.cfg.Projects {
		path, err := f.cfg.Root.ProjectConfig(p.ID)
		must(t, err)
		data, err := os.ReadFile(path)
		must(t, err)
		text := "# project comment\ngroup_id = \"\" # own charter\n" + string(data) + "\n# last comment\n"
		if p.ID != project {
			text = "# project comment\n  group_id = " + fmt.Sprintf("%q", project) + " # source charter\n" + string(data) + "\n# last comment\n"
		}
		must(t, os.WriteFile(path, []byte(text), 0600))
		before[p.ID] = text
	}
	return f, before, charter
}

func assertSplit(t *testing.T, f *twoProjectFixture, before map[config.ProjectID]string, source trace.Document, currentSource ...trace.Document) []trace.Document {
	t.Helper()
	cfg, err := config.Load(f.opts.Config)
	must(t, err)
	if len(cfg.Projects) != 2 || !cfg.Active(project) || !cfg.Active(otherProject) {
		t.Fatalf("projects: %+v", cfg.Projects)
	}
	var copied []trace.Document
	for _, p := range cfg.Projects {
		path, err := cfg.Root.ProjectConfig(p.ID)
		must(t, err)
		data, err := os.ReadFile(path)
		must(t, err)
		want := before[p.ID]
		if p.ID != project {
			want = strings.Replace(want, "group_id = "+fmt.Sprintf("%q", project), "", 1)
		} else {
			want = strings.Replace(want, `group_id = ""`, "", 1)
		}
		if string(data) != want {
			t.Fatalf("config bytes changed beyond group_id:\ngot %q\nwant %q", data, want)
		}
		r, err := trace.Open(cfg.Root, p)
		must(t, err)
		doc, err := r.Charter(context.Background())
		must(t, err)
		docs, err := trace.Read[trace.Document](r, "")
		must(t, err)
		must(t, r.Close())
		expected := source.Content
		if p.ID == project && len(currentSource) > 0 {
			expected = currentSource[0].Content
		}
		if doc.Content != expected {
			t.Fatalf("project %s charter: %+v", p.ID, doc)
		}
		if p.ID != project {
			cause := fmt.Sprintf("split-project:%s:charter#%d", project, source.Revision)
			if doc.Cause != cause {
				t.Fatalf("provenance: %+v", doc)
			}
			count := 0
			for _, d := range docs {
				if d.Cause == cause {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("copied %d revisions", count)
			}
			copied = append(copied, doc)
		}
	}
	return copied
}

func TestStartupSplitsProjectsAndPreservesConfigAndChartersAcrossRestarts(t *testing.T) {
	t.Parallel()
	f, before, source := splitFixture(t)
	// No runner executes while this test examines startup's durable effects.
	f.opts.Threads = nil
	var first []trace.Document
	for restart := range 2 {
		s, err := Start(context.Background(), f.opts)
		must(t, err)
		if got := s.configuration().Projects; len(got) != 2 || got[0].ID != project || got[1].ID != otherProject {
			t.Fatalf("active projects: %+v", got)
		}
		must(t, s.Close())
		copied := assertSplit(t, f, before, source)
		if restart == 0 {
			first = copied
		} else if !reflect.DeepEqual(first, copied) {
			t.Fatal("second startup changed copied charter history")
		}
	}
}

func TestProjectSplitResumesAfterEveryDurableStep(t *testing.T) {
	steps := []string{"split-intent-written", "split-config-rewritten:" + string(project), "split-charter-copied:" + string(otherProject), "split-config-rewritten:" + string(otherProject), "split-intent-removed"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, before, source := splitFixture(t)
			interrupted := errors.New("power loss")
			f.opts.boundary = func(_ context.Context, at string) error {
				if at == step {
					return interrupted
				}
				return nil
			}
			s, err := Start(context.Background(), f.opts)
			if !errors.Is(err, interrupted) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("interrupt %s: %v", step, err)
			}
			f.opts.boundary = nil
			f.opts.Threads = nil
			s, err = Start(context.Background(), f.opts)
			must(t, err)
			must(t, s.Close())
			first := assertSplit(t, f, before, source)
			s, err = Start(context.Background(), f.opts)
			must(t, err)
			must(t, s.Close())
			second := assertSplit(t, f, before, source)
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("restart duplicated or changed charter: %+v %+v", first, second)
			}
			if _, err := os.Stat(filepath.Join(f.cfg.Root.String(), "project-splits.json")); !os.IsNotExist(err) {
				t.Fatalf("unfinished journal: %v", err)
			}
		})
	}
}

func TestProjectSplitEscapedGroupKeyRecoversAfterCharterCopy(t *testing.T) {
	for _, key := range []string{`"group\u005fid"`, `"group\U0000005fid"`} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			f, before, source := splitFixture(t)
			for id, text := range before {
				path, err := f.cfg.Root.ProjectConfig(id)
				must(t, err)
				must(t, os.WriteFile(path, []byte(strings.Replace(text, "group_id =", key+" =", 1)), 0600))
				got, present, err := config.ProjectGroupSource(path)
				must(t, err)
				if !present || (id == otherProject && got != project) {
					t.Fatalf("escaped migration source: %s, present=%t", got, present)
				}
			}
			f.opts.Threads = nil
			interrupted := errors.New("power loss after charter copy")
			f.opts.boundary = func(_ context.Context, at string) error {
				if at == "split-charter-copied:"+string(otherProject) {
					return interrupted
				}
				return nil
			}
			s, err := Start(context.Background(), f.opts)
			if s != nil {
				must(t, s.Close())
			}
			if !errors.Is(err, interrupted) {
				t.Fatalf("interrupt after charter copy: %v", err)
			}
			path, err := f.cfg.Root.ProjectConfig(otherProject)
			must(t, err)
			data, err := os.ReadFile(path)
			must(t, err)
			if !strings.Contains(string(data), key+" =") {
				t.Fatal("interruption did not leave escaped group key pending removal")
			}
			r, err := trace.Open(f.cfg.Root, f.cfg.For(otherProject).Project)
			must(t, err)
			copied, err := r.Charter(context.Background())
			must(t, err)
			must(t, r.Close())
			if copied.Content != source.Content || copied.Cause != fmt.Sprintf("split-project:%s:charter#%d", project, source.Revision) {
				t.Fatalf("charter was not durably copied before interruption: %+v", copied)
			}
			f.opts.boundary = nil
			for range 2 {
				s, err := Start(context.Background(), f.opts)
				must(t, err)
				must(t, s.Close())
				// The expected bytes are identical for bare and escaped keys.
				got := assertSplit(t, f, before, source)
				if len(got) != 1 || !reflect.DeepEqual(got[0], copied) {
					t.Fatalf("restart duplicated or changed copied charter: %+v", got)
				}
				if _, err := os.Stat(filepath.Join(f.cfg.Root.String(), "project-splits.json")); !os.IsNotExist(err) {
					t.Fatalf("unfinished journal: %v", err)
				}
			}
		})
	}
}

func TestProjectSplitPreservesWorkgroupApprovalPinsAndChief(t *testing.T) {
	t.Parallel()
	f, before, source := splitFixture(t)
	ctx := context.Background()
	var members []trace.CrossRepoMember
	for _, p := range f.cfg.Projects {
		r, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		members = append(members, trace.CrossRepoMember{Project: r, Workstream: f.streams[p.ID]})
	}
	chief, group, err := trace.CreateWorkgroupTrace(ctx, f.cfg.Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	for _, m := range members {
		must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, group.ID, demoStart, ownerActor, "fixture"))
	}
	_, err = chief.EnsureChiefOfStaff(ctx, group.Members[0], demoStart, serviceActor)
	must(t, err)
	approval := trace.GroupApproval{Workgroup: group.ID, ProjectSpecRevision: 3, Complete: true}
	for _, m := range members {
		approval.Members = append(approval.Members, trace.GroupMemberApproval{Project: m.Project.Project(), Workstream: m.Workstream, Spec: 4, Plan: 5, Round: 2, Record: []byte(`{"owner":"approved"}`)})
	}
	must(t, trace.RecordGroupApproval(ctx, chief, group.Members[0], approval, demoStart, ownerActor))
	beforeDocs, err := trace.Read[trace.Document](chief, group.Members[0])
	must(t, err)
	beforeChief, err := chief.ChiefOfStaffThread(group.Members[0])
	must(t, err)
	must(t, chief.Close())
	for _, m := range members {
		must(t, m.Project.Close())
	}
	f.opts.Threads = nil
	for range 2 {
		s, err := Start(ctx, f.opts)
		must(t, err)
		must(t, s.Close())
		assertSplit(t, f, before, source)
		authority, err := trace.OpenWorkgroupAuthority(f.cfg.Root, group.ID)
		must(t, err)
		groups, _, err := trace.Workgroups(authority)
		must(t, err)
		gotApproval, _, found, err := trace.ReadGroupApproval(authority, group.Members[0])
		must(t, err)
		gotDocs, err := trace.Read[trace.Document](authority, group.Members[0])
		must(t, err)
		gotChief, err := authority.ChiefOfStaffThread(group.Members[0])
		must(t, err)
		must(t, authority.Close())
		if len(groups) != 1 || !reflect.DeepEqual(groups[0], group) || !found || !reflect.DeepEqual(gotApproval, approval) || !reflect.DeepEqual(gotDocs, beforeDocs) || !reflect.DeepEqual(gotChief.Identity, beforeChief.Identity) {
			t.Fatal("migration changed shared identity, approval, pins, history or chief")
		}
		for _, p := range f.cfg.Projects {
			r, err := trace.Open(f.cfg.Root, p)
			must(t, err)
			id, err := r.CrossRepoWorkgroupOf(f.streams[p.ID])
			must(t, err)
			must(t, r.Close())
			if id != group.ID {
				t.Fatal("migration replaced membership")
			}
		}
	}
}

func TestProjectGroupingRoutesAreNotFound(t *testing.T) {
	opts, _ := projectFixture(t)
	s, err := Start(context.Background(), opts)
	must(t, err)
	defer s.Close()
	for _, method := range []string{"GET", "POST"} {
		request := httptest.NewRequest(method, Prefix+"/projects/repos", strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		s.handle(response, request)
		if response.Code != 404 {
			t.Fatalf("removed route %s: %d %s", method, response.Code, response.Body.String())
		}
	}
}

func TestProjectSplitPinsCharterBeforeAnInterruptedStartup(t *testing.T) {
	t.Parallel()
	f, _, source := splitFixture(t)
	ctx := context.Background()
	stop := errors.New("interrupted after intent")
	f.opts.boundary = func(_ context.Context, name string) error {
		if name == "split-intent-written" {
			return stop
		}
		return nil
	}
	_, err := Start(ctx, f.opts)
	if !errors.Is(err, stop) {
		t.Fatalf("interruption: %v", err)
	}
	r, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
	must(t, err)
	_, err = r.EditCharter(ctx, source.Revision, "1. Owner edited the source after the migration started.\n", demoStart)
	must(t, err)
	must(t, r.Close())
	f.opts.boundary = nil
	f.opts.Threads = nil
	s, err := Start(ctx, f.opts)
	must(t, err)
	must(t, s.Close())
	r, err = trace.Open(f.cfg.Root, f.cfg.For(otherProject).Project)
	must(t, err)
	defer r.Close()
	copied, err := r.Charter(ctx)
	must(t, err)
	if copied.Content != source.Content || copied.Cause != fmt.Sprintf("split-project:%s:charter#%d", project, source.Revision) {
		t.Fatalf("restart replaced the pinned charter: %+v", copied)
	}
}

func TestProjectSplitRecoversPendingRegistrationWithPinnedCharter(t *testing.T) {
	for _, published := range []bool{false, true} {
		steps := []string{"project-charter-pinned", "trace-created", "project-charter-copied", "project-group-removed", "active-project-listed", "journal-removed"}
		if !published {
			steps = append(steps, "project-config-written")
		}
		for _, step := range steps {
			t.Run(fmt.Sprintf("published=%t/%s", published, step), func(t *testing.T) {
				t.Parallel()
				f, before, source := splitFixture(t)
				p := f.cfg.For(otherProject).Project
				if !published {
					directory, err := f.cfg.Root.ProjectTrace(p.ID)
					must(t, err)
					must(t, os.RemoveAll(directory))
					must(t, os.MkdirAll(directory, 0700))
					path, err := f.cfg.Root.ProjectConfig(p.ID)
					must(t, err)
					must(t, os.WriteFile(path, []byte(before[p.ID]), 0600))
					fixturekit.DemoGit(t, filepath.Dir(p.Clone), "init", "-q", p.Clone)
				}
				data, err := json.Marshal(p)
				must(t, err)
				var legacy map[string]any
				must(t, json.Unmarshal(data, &legacy))
				legacy["group_id"] = project
				data, err = json.Marshal(map[string]any{"version": 1, "project": legacy})
				must(t, err)
				path, err := f.cfg.Root.PendingProject()
				must(t, err)
				must(t, os.WriteFile(path, data, 0600))
				interrupted := errors.New("interrupted registration recovery")
				var reached bool
				f.opts.Threads = nil
				f.opts.boundary = func(_ context.Context, at string) error {
					if at == step {
						reached = true
						return interrupted
					}
					if strings.HasPrefix(at, "split-") {
						t.Fatal("config split began before registration recovery completed")
					}
					return nil
				}
				s, err := Start(context.Background(), f.opts)
				must(t, err)
				if !reached || !errors.Is(s.pending, interrupted) {
					t.Fatalf("registration interruption at %s: reached=%t pending=%v", step, reached, s.pending)
				}
				must(t, s.Close())
				r, err := trace.Open(f.cfg.Root, f.cfg.For(project).Project)
				must(t, err)
				edited, err := r.EditCharter(context.Background(), source.Revision, "1. Owner edited the source while registration recovery was interrupted.\n", demoStart)
				must(t, err)
				must(t, r.Close())
				f.opts.boundary = nil
				var first []trace.Document
				for restart := range 2 {
					s, err := Start(context.Background(), f.opts)
					must(t, err)
					if s.pending != nil {
						t.Fatal(s.pending)
					}
					must(t, s.Close())
					copied := assertSplit(t, f, before, source, edited)
					if restart == 0 {
						first = copied
					} else if !reflect.DeepEqual(first, copied) {
						t.Fatal("restart changed the pinned charter revision")
					}
				}
			})
		}
	}
}
