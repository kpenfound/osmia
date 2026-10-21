package execution

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestInterruptedLandingIsReconciled(t *testing.T) {
	t.Parallel()
	observed := map[string]string{
		"land-committed": "holds no commit of this landing",
		"land-advanced":  "this operation's landing commit on",
		"land-recorded":  "landing succeeded",
	}
	for _, step := range []string{"land-committed", "land-advanced", "land-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newReviewFixture(t, "interrupted-landing")
			approveDirectly(t, f.s, repository, stream, "resume", "spec#1")
			lands := &foreman{masons: newMasonController(f.s, repository)}
			ctx := context.Background()
			must(t, lands.Pass(ctx))
			crashed := false
			f.s.Step = func(name string) error {
				if name == step && !crashed {
					crashed = true
					return errors.New("crash")
				}
				return nil
			}
			controller, err := reconcile.New(repository, reconcile.Options{
				Worker: "landing-test", Now: f.clock.Now, RetryDelay: time.Second,
				Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: lands},
				Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != LandAction },
			})
			must(t, err)
			must(t, controller.Pass(ctx))
			if !crashed {
				t.Fatal("the landing never reached " + step)
			}
			jump(f.clock, f.clock.Now().Add(time.Second))
			must(t, controller.Pass(ctx))
			ops := landOperations(t, repository, stream)
			if len(ops) != 1 || ops[0].Result == nil {
				t.Fatalf("landing operations %+v", ops)
			}
			retries := 0
			var observations []coreadapter.Observation
			for _, a := range ops[0].History {
				if a.Kind == "observe" && a.Observation != nil {
					observations = append(observations, *a.Observation)
				}
				if a.Kind == "retry" {
					retries++
					if !strings.Contains(a.Failure, "crash") {
						t.Fatalf("retry %+v", a)
					}
				}
			}
			if retries != 1 || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("%d retries, result %+v", retries, ops[0].Result)
			}
			if len(observations) != 2 || (observations[1].State == coreadapter.EffectCompleted) != (step == "land-recorded") || !strings.Contains(observations[1].Evidence, observed[step]) {
				t.Fatalf("the retry observed %+v", observations)
			}
			_, result := approvedReview(t, repository, stream, "resume")
			commits := f.landedCommits(t, stream, result.Identity.Candidate.BaseRevision)
			if len(commits) != 1 {
				t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
			}
			var merged, landed []string
			for _, tr := range allTransitions(t, f.cfg.Root, f.cfg.Project.ID, stream) {
				if tr.Subject == trace.UnitSubject("resume") && tr.To == UnitMerged {
					merged = append(merged, tr.ID)
				}
				if tr.Subject == landingSubject("resume") && strings.HasPrefix(tr.To, "landed-") {
					landed = append(landed, tr.ID)
				}
			}
			if len(merged) != 1 || len(landed) != 1 {
				t.Fatalf("merged %v, landed %v", merged, landed)
			}
			docs, err := trace.Read[trace.Document](repository, stream)
			must(t, err)
			var landing UnitLanding
			for _, d := range docs {
				if d.ID == landingDocument("resume") {
					must(t, json.Unmarshal([]byte(d.Content), &landing))
				}
			}
			if landing.Commit != commits[0] {
				t.Fatalf("landing.json records %s, the branch holds %s", landing.Commit, commits[0])
			}
			if status := strings.TrimSpace(demoGit(t, filepath.Dir(f.cfg.Project.Clone), "-C", filepath.Join(f.cfg.Root.String(), intake.BranchesDirectory, string(f.cfg.Project.ID), string(stream)), "status", "--porcelain")); status != "" {
				t.Fatalf("the feature workspace after the landing:\n%s", status)
			}
		})
	}
}
