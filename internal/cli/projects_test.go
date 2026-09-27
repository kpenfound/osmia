package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
)

const (
	otherProject = "p_fedcba9876543210fedcba9876543210"
	otherStream  = "w_fedcba9876543210fedcba9876543210"
)

// withTwoProjects lists a second project beside the fixture's, and gives each
// project's trace the inbox entries 1 and 2 that escalate writes.
func withTwoProjects(t *testing.T) service.Options {
	t.Helper()
	opts := fixture(t)
	root := opts.Config.Root
	top, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(strings.Replace(string(top), fmt.Sprintf("active_projects=[%q]", project), fmt.Sprintf("active_projects=[%q, %q]", project, otherProject), 1)), 0600))
	must(t, os.MkdirAll(filepath.Join(root, "projects", otherProject), 0700))
	must(t, os.WriteFile(filepath.Join(root, "projects", otherProject, "config.toml"), []byte(fmt.Sprintf(`version=1
upstream="upstream/other"
fork="owner/other"
clone=%q
`, filepath.Join(filepath.Dir(root), "other"))), 0600))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	escalate(t, cfg.Root, cfg.For(project).Project, stream, "Both fixed.", "No.")
	escalate(t, cfg.Root, cfg.For(otherProject).Project, otherStream, "Other fixed.", "Please force-push this.")
	opts.Workstreams = nil
	opts.Reconciliation.Now = func() time.Time { return written.Add(2 * time.Hour) }
	return opts
}

func TestAnswerNamesTheProjectOfTheEntry(t *testing.T) {
	opts := withTwoProjects(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root

	var list service.InboxResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "inbox", "--json")), &list))
	byProject := map[config.ProjectID][]int{}
	for _, e := range list.Entries {
		byProject[e.Project] = append(byProject[e.Project], e.Number)
		if e.Answer.Body["project"] != string(e.Project) {
			t.Fatalf("entry %d of project %s answers with %v", e.Number, e.Project, e.Answer.Body)
		}
	}
	if len(byProject[project]) != 2 || len(byProject[otherProject]) != 2 {
		t.Fatalf("inbox entries by project %v", byProject)
	}

	// Both projects have an open entry 1: the answer has to name one.
	code, out, diag := invoke(t, root, "answer", "1", "Both are part of the contract.")
	if code != 4 || out != "" || diag != "validation: inbox entry 1 is open in projects "+project+", "+otherProject+"; name one with --project\n" {
		t.Fatalf("ambiguous answer: %d %q %q", code, out, diag)
	}
	if got, want := successful(t, root, "answer", "1", "Neither is.", "--project", otherProject), "Ruling recorded on inbox entry 1 ("+otherStream+", questions 1, 2)\n"; !strings.HasPrefix(got, want) {
		t.Fatalf("answer in the other project: %q", got)
	}
	// Entry 1 is now open only in the first project, which the answer finds.
	if got, want := successful(t, root, "answer", "1", "Both are."), "Ruling recorded on inbox entry 1 ("+stream+", questions 1, 2)\n"; !strings.HasPrefix(got, want) {
		t.Fatalf("answer found by number: %q", got)
	}
	// Entry 2 of the other project has no quick reply; the first project's does.
	code, out, diag = invoke(t, root, "answer", "2", "--accept", "--project", otherProject)
	if code != 4 || out != "" || diag != "validation: inbox entry 2 has no eligible quick reply; give a ruling explicitly\n" {
		t.Fatalf("accept without a quick reply: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "answer", "2", "--accept")
	if code != 4 || !strings.Contains(diag, "inbox entry 2 is open in projects") {
		t.Fatalf("ambiguous accept: %d %q %q", code, out, diag)
	}
	if got := successful(t, root, "answer", "2", "--accept", "--project", project); !strings.HasPrefix(got, "Ruling recorded on inbox entry 2 ("+stream+", questions 3)\n") {
		t.Fatalf("accept in the first project: %q", got)
	}
	for _, args := range [][]string{{"answer", "2", "No.", "--project", "bad"}, {"inbox", "--project", project}, {"answer", "2", "No.", "--project"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestPauseAndPriorityFindTheProjectOfTheWorkstream(t *testing.T) {
	opts := withTwoProjects(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	c := service.NewClient(s.Socket())
	t.Cleanup(c.Close)

	successful(t, root, "pause", otherStream)
	successful(t, root, "priority", "set", otherStream)
	rt, err := c.Runtime(context.Background())
	must(t, err)
	want := runtime.Target{Scope: "workstream", Project: otherProject, Workstream: otherStream}
	if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target != want {
		t.Fatalf("pauses %+v", rt.Effective.Pauses)
	}
	if len(rt.Effective.Priorities) != 1 || rt.Effective.Priorities[0].Project != otherProject {
		t.Fatalf("priorities %+v", rt.Effective.Priorities)
	}
	successful(t, root, "resume", otherStream)

	// Without a workstream there is no project to act on.
	code, out, diag := invoke(t, root, "priority", "clear")
	if code != 4 || out != "" || diag != "validation: several projects are active ("+project+", "+otherProject+"); name a workstream of the project\n" {
		t.Fatalf("priority clear: %d %q %q", code, out, diag)
	}
	unknown := "w_00000000000000000000000000000abc"
	code, out, diag = invoke(t, root, "pause", unknown)
	if code != 4 || out != "" || diag != "not_found: workstream "+unknown+" is not in an active project; list workstreams with osmia status\n" {
		t.Fatalf("pause of an unknown workstream: %d %q %q", code, out, diag)
	}
}
