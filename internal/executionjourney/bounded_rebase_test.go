package executionjourney

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

type boundedRebase struct {
	f          *journey.ShedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	retryPasses
	landed string
}

func newBoundedRebase(t *testing.T, key string) *boundedRebase {
	t.Helper()
	f, stream, repository := journey.NewRebaseFixture(t, key)
	t.Cleanup(func() { repository.Close() })
	ctx := context.Background()
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	w, _, _, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "resume")
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "RESUME.md"), []byte("resume\n"), 0600))
	landed := journey.MoveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})
	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.RebaseOperations(t, repository, stream, "resume"); len(ops) != 1 {
		t.Fatalf("resume's rebases %+v", ops)
	}
	passes := newRetryPasses(t, repository, "rebase-test", service.RebaseAction, execution.NewRebaser(lands.Driver()))
	return &boundedRebase{f, stream, repository, passes, landed}
}

func (b *boundedRebase) operation(t *testing.T) trace.OperationRecord {
	t.Helper()
	ops := journey.RebaseOperations(t, b.repository, b.stream, "resume")
	if len(ops) != 1 {
		t.Fatalf("resume's rebases %+v", ops)
	}
	return ops[0]
}

func (b *boundedRebase) notices(t *testing.T, words string) []string {
	t.Helper()
	entries, err := b.repository.Outbox(b.stream)
	fixturekit.Must(t, err)
	var bodies []string
	for _, e := range entries {
		if e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, words) {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

func (b *boundedRebase) outcomes(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, tr := range intakefixture.AllTransitions(t, b.f.S.Cfg.Root, b.f.Project, b.stream) {
		if tr.Subject == execution.RebaseSubject("resume") && tr.From != "" {
			ids = append(ids, tr.ID)
		}
	}
	return ids
}

func TestRebaseThatKeepsFailingIsRefusedOnceItsCommitIsConfirmedAbsent(t *testing.T) {
	t.Parallel()
	b := newBoundedRebase(t, "give-up-rebase-absent")
	tip := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(b.f.Clone), "-C", b.f.Clone, "rev-parse", execution.UnitBranch(b.stream, "resume")))
	b.f.S.Boundary = func(name string) error {
		if name == "rebase-snapshotting" {
			return errors.New("the clone is unreachable")
		}
		return nil
	}
	b.pass(t, reconcile.GiveUpFailures+1)
	if op := b.operation(t); op.Result != nil || reconcile.ConsecutiveFailures(op) != reconcile.GiveUpFailures+1 {
		t.Fatalf("inside the window: %+v", op)
	}
	if got := b.notices(t, "The rebase of unit resume has failed 3 times in a row"); len(got) != 1 {
		t.Fatalf("notices at the third failure: %q", got)
	}
	b.clock.Advance(10 * time.Minute)
	b.pass(t, 1)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "failed" || !op.Acknowledged {
		t.Fatalf("the rebase ended as %+v", op)
	}
	for _, words := range []string{execution.RefusedAfterRetries, "the rebase is confirmed absent", "holds no rebase of this operation", "the clone is unreachable"} {
		if !strings.Contains(op.Result.Evidence, words) {
			t.Fatalf("the refusal lacks %q: %s", words, op.Result.Evidence)
		}
	}
	if !slices.Equal(b.outcomes(t), []string{execution.RebaseSubject("resume") + "-1-refused"}) {
		t.Fatalf("rebase outcomes %v", b.outcomes(t))
	}
	if after := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(b.f.Clone), "-C", b.f.Clone, "rev-parse", execution.UnitBranch(b.stream, "resume"))); after != tip {
		t.Fatalf("the unit branch moved from %s to %s", tip, after)
	}
	if got := b.notices(t, "Unit resume was not rebased onto"); len(got) != 1 {
		t.Fatalf("notices of the refusal: %q", got)
	}
	if got := b.notices(t, "The rebase of unit resume has failed"); len(got) != 1 {
		t.Fatalf("the streak notified the chief %d times: %q", len(got), got)
	}
}

func TestRebaseWhoseCommitIsOnItsTargetIsRecordedWhenGivenUp(t *testing.T) {
	t.Parallel()
	b := newBoundedRebase(t, "give-up-rebase-completed")
	failing := true
	b.f.S.Boundary = func(name string) error {
		if failing && name == "rebase-moved" {
			return errors.New("the service stopped")
		}
		return nil
	}
	b.pass(t, reconcile.GiveUpFailures+1)
	home := filepath.Dir(b.f.Clone)
	if count := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", b.f.Clone, "rev-list", "--count", b.landed+".."+execution.UnitBranch(b.stream, "resume"))); count != "1" || b.operation(t).Result != nil {
		t.Fatalf("before the window: %s commits on the unit branch, %+v", count, b.operation(t))
	}
	failing = false
	b.clock.Advance(10 * time.Minute)
	b.pass(t, 1)
	op := b.operation(t)
	if op.Result == nil || op.Result.Outcome != "succeeded" || !op.Acknowledged {
		t.Fatalf("the rebase ended as %+v", op)
	}
	if count := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", b.f.Clone, "rev-list", "--count", b.landed+".."+execution.UnitBranch(b.stream, "resume"))); count != "1" {
		t.Fatalf("the unit branch gained %s commits on %s", count, b.landed)
	}
	if !slices.Equal(b.outcomes(t), []string{execution.RebaseSubject("resume") + "-1-rebased"}) {
		t.Fatalf("rebase outcomes %v", b.outcomes(t))
	}
	if rebases := journey.UnitRebases(t, b.repository, b.stream, "resume"); len(rebases) != 1 || rebases[0].Onto != b.landed {
		t.Fatalf("rebase records %+v", rebases)
	}
	if got := b.notices(t, "The rebase of unit resume failed repeatedly"); len(got) != 1 {
		t.Fatalf("notices of the recorded success: %q", got)
	}
}
