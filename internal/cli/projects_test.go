package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
)

const (
	otherProject = "p_fedcba9876543210fedcba9876543210"
	otherStream  = "w_fedcba9876543210fedcba9876543210"
)

func withTwoProjects(t *testing.T) service.Options {
	t.Helper()
	opts := fixture(t)
	root := opts.Config.Root
	register(t, root, otherProject, fmt.Sprintf("version=1\nupstream=\"upstream/other\"\nfork=\"owner/other\"\nclone=%q\n", filepath.Join(filepath.Dir(root), "other")))
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	escalate(t, cfg.Root, cfg.For(project).Project, stream, "Both fixed.", "No.")
	escalate(t, cfg.Root, cfg.For(otherProject).Project, otherStream, "Other fixed.", "Please force-push this.")
	opts.Workstreams = nil
	opts.Reconciliation.Now = func() time.Time { return written.Add(2 * time.Hour) }
	return opts
}

func startTwoProjects(t *testing.T) (root string, s *service.Service) {
	t.Helper()
	opts := withTwoProjects(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	return opts.Config.Root, s
}

func TestInboxEntriesNameTheProjectThatAnswersThem(t *testing.T) {
	root, _ := startTwoProjects(t)
	var list wire.InboxResponse
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
}

func TestAnswerRefusesAnEntryNumberOpenInSeveralProjects(t *testing.T) {
	root, _ := startTwoProjects(t)
	code, out, diag := invoke(t, root, "answer", "1", "Both are part of the contract.")
	if code != 4 || out != "" || diag != "validation: inbox entry 1 is open in projects "+project+", "+otherProject+"; name one with --project\n" {
		t.Fatalf("ambiguous answer: %d %q %q", code, out, diag)
	}
}

func TestAnswerWithProjectRulesOnThatProjectsEntry(t *testing.T) {
	root, _ := startTwoProjects(t)
	if got, want := successful(t, root, "answer", "1", "Neither is.", "--project", otherProject), "Ruling recorded on inbox entry 1 ("+otherStream+", questions 1, 2)\n"; !strings.HasPrefix(got, want) {
		t.Fatalf("answer in the other project: %q", got)
	}
}

func TestAnswerFindsTheOnlyProjectThatStillHoldsTheEntryNumber(t *testing.T) {
	root, _ := startTwoProjects(t)
	successful(t, root, "answer", "1", "Neither is.", "--project", otherProject)
	if got, want := successful(t, root, "answer", "1", "Both are."), "Ruling recorded on inbox entry 1 ("+stream+", questions 1, 2)\n"; !strings.HasPrefix(got, want) {
		t.Fatalf("answer found by number: %q", got)
	}
}

func TestAnswerAcceptRefusesAnEntryWithoutAQuickReply(t *testing.T) {
	root, _ := startTwoProjects(t)
	code, out, diag := invoke(t, root, "answer", "2", "--accept", "--project", otherProject)
	if code != 4 || out != "" || diag != "validation: inbox entry 2 has no eligible quick reply; give a ruling explicitly\n" {
		t.Fatalf("accept without a quick reply: %d %q %q", code, out, diag)
	}
}

func TestAnswerAcceptRefusesAnEntryNumberOpenInSeveralProjects(t *testing.T) {
	root, _ := startTwoProjects(t)
	code, out, diag := invoke(t, root, "answer", "2", "--accept")
	if code != 4 || !strings.Contains(diag, "inbox entry 2 is open in projects") {
		t.Fatalf("ambiguous accept: %d %q %q", code, out, diag)
	}
}

func TestAnswerAcceptWithProjectRulesWithThatProjectsRecommendation(t *testing.T) {
	root, _ := startTwoProjects(t)
	if got := successful(t, root, "answer", "2", "--accept", "--project", project); !strings.HasPrefix(got, "Ruling recorded on inbox entry 2 ("+stream+", questions 3)\n") {
		t.Fatalf("accept in the first project: %q", got)
	}
}

func TestProjectFlagRefusesInvalidArguments(t *testing.T) {
	root, _ := startTwoProjects(t)
	for _, args := range [][]string{{"answer", "2", "No.", "--project", "bad"}, {"inbox", "--project", project}, {"answer", "2", "No.", "--project"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestPauseFindsTheProjectOfTheWorkstream(t *testing.T) {
	root, s := startTwoProjects(t)
	c := service.NewClient(s.Socket())
	t.Cleanup(c.Close)
	successful(t, root, "pause", otherStream)
	rt, err := c.Runtime(context.Background())
	must(t, err)
	want := runtime.Target{Scope: "workstream", Project: otherProject, Workstream: otherStream}
	if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target != want {
		t.Fatalf("pauses %+v", rt.Effective.Pauses)
	}
}

func TestPriorityFindsTheProjectOfTheWorkstream(t *testing.T) {
	root, s := startTwoProjects(t)
	c := service.NewClient(s.Socket())
	t.Cleanup(c.Close)
	successful(t, root, "priority", "set", otherStream)
	rt, err := c.Runtime(context.Background())
	must(t, err)
	if len(rt.Effective.Priorities) != 1 || rt.Effective.Priorities[0].Project != otherProject {
		t.Fatalf("priorities %+v", rt.Effective.Priorities)
	}
}

func TestPriorityClearNeedsAWorkstreamWhenSeveralProjectsAreActive(t *testing.T) {
	root, _ := startTwoProjects(t)
	code, out, diag := invoke(t, root, "priority", "clear")
	if code != 4 || out != "" || diag != "validation: several projects are active ("+project+", "+otherProject+"); name a workstream of the project\n" {
		t.Fatalf("priority clear: %d %q %q", code, out, diag)
	}
}

func TestPauseRefusesAWorkstreamOutsideTheActiveProjects(t *testing.T) {
	root, _ := startTwoProjects(t)
	unknown := "w_00000000000000000000000000000abc"
	code, out, diag := invoke(t, root, "pause", unknown)
	if code != 4 || out != "" || diag != "not_found: workstream "+unknown+" is not in an active project; list workstreams with osmia status\n" {
		t.Fatalf("pause of an unknown workstream: %d %q %q", code, out, diag)
	}
}

func TestProjectAddWithoutFork(t *testing.T) {
	for _, fork := range []string{"", "DAGGER/dagger"} {
		t.Run(fork, func(t *testing.T) {
			opts, clone := emptyFixture(t)
			s, err := service.Start(context.Background(), opts)
			must(t, err)
			t.Cleanup(func() { s.Close() })
			args := []string{"project", "add", "dagger", "--upstream", "dagger/dagger", "--clone", clone, "--json"}
			if fork != "" {
				args = append(args, "--fork", fork)
			}
			var added wire.ProjectResponse
			must(t, json.Unmarshal([]byte(successful(t, opts.Config.Root, args...)), &added))
			if added.Project.Fork != "" || added.Project.Upstream != "dagger/dagger" {
				t.Fatal(added)
			}
			var again wire.ProjectResponse
			must(t, json.Unmarshal([]byte(successful(t, opts.Config.Root, args...)), &again))
			if again.Project.ID != added.Project.ID {
				t.Fatal("registration was not idempotent")
			}
		})
	}
}
