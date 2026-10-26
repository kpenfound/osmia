package delivery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFinalChecksRecordsLifecycleReportFailure(t *testing.T) {
	t.Parallel()
	cfg, repository := deliveryTrace(t)
	home := filepath.Dir(cfg.Project.Clone)
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(fixturekit.DemoGit(t, home, append([]string{"-C", cfg.Project.Clone}, args...)...))
	}
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.test")
	git("commit", "--allow-empty", "-qm", "base")
	base := git("rev-parse", "HEAD")
	file := filepath.Join(cfg.Project.Clone, fixturekit.MasonWrote)
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(file), 0700))
	fixturekit.Must(t, os.WriteFile(file, []byte("package trace\n"), 0600))
	git("add", ".")
	git("commit", "-qm", "candidate")
	commit := git("rev-parse", "HEAD")

	raw := fixturekit.LifecycleFailureOutput()
	checks := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: CheckResult{ExitCode: 1, Output: raw}}}}
	controller := New(Dependencies{
		Now:    func() time.Time { return fixturekit.DemoStart },
		Step:   func(string) error { return nil },
		Checks: checks,
		AcquireReviewCheckSlot: func(context.Context, config.ProjectID, config.WorkstreamID, int, string) (func(), error) {
			return func() {}, nil
		},
	})
	reviewer := &FinalReviewer{S: controller, Repository: repository}
	report := FinalReport{Review: 1, Operation: "review", Commit: commit, Upstream: &seal.Base{Commit: base}}
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if run.Status != checkrun.ChecksFailed || run.Error != "" || run.ExitCode != 1 ||
		!slices.Equal(run.Failed, []string{"dag://?check=test&go-package=internal/lifecycle"}) ||
		!strings.Contains(run.Output, "lifecycle_test.go:42: expected pending change to apply") {
		t.Fatalf("final checks: %+v", run)
	}
	wantCommand := []string{"dagger", "check", "--progress=report", "--fail-fast", "--parallel", "4"}
	if !slices.Equal(run.Command, wantCommand) {
		t.Fatalf("command: %q", run.Command)
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var recorded, captured bool
	for _, doc := range docs {
		switch doc.Path {
		case FinalChecksPath(1):
			var saved FinalCheckRun
			fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &saved))
			if !reflect.DeepEqual(saved, run) {
				t.Fatalf("recorded result differs: %+v", saved)
			}
			recorded = true
		case run.OutputPath:
			if doc.Content != raw || doc.Revision != run.OutputRevision {
				t.Fatal("complete captured output or its revision was lost")
			}
			captured = true
		}
	}
	if !recorded || !captured {
		t.Fatalf("recorded result=%t captured output=%t", recorded, captured)
	}
	count, _, err := reviewer.IncompleteChecks(stream, 1, commit)
	fixturekit.Must(t, err)
	if count != 0 {
		t.Fatalf("failed check entered incomplete retry: %d", count)
	}
	cached, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(cached, run) || len(checks.Runs()) != 1 {
		t.Fatal("completed failure was rerun")
	}
}
