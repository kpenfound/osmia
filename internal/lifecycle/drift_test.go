package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDriftRebaseMovesTheFeatureBranchAndTheSeal(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	before := journey.MoveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	sealed := intakefixture.Seals(t, repository, stream)
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	if len(sealed) != 1 || sealed[0].Base.Commit == upstream {
		t.Fatalf("seals before the drift rebase %+v", sealed)
	}

	op := journey.RequestDrift(t, d, stream)
	requested := intakefixture.TransitionByID(t, repository, stream, "drift-1")
	if requested.Subject != lifecycle.DriftSubject || requested.To != "requested-1" || requested.Actor != execution.ForemanActor || requested.Cause != "seal-1" {
		t.Fatalf("the request %+v", requested)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectAbsent {
		t.Fatalf("inspection before the drift rebase %+v: %v", seen, err)
	}
	result := journey.SettleOperation(t, f.S, repository, stream, op, d)
	g := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
	fixturekit.Must(t, err)
	if tip == before || journey.ParentOf(t, f, tip) != upstream || journey.FileAt(t, f, tip, "internal/trace/resume.go") != "package trace\n" || journey.FileAt(t, f, tip, "UPSTREAM.md") != "upstream\n" {
		t.Fatalf("the feature branch is at %s, not the feature commit replayed onto %s", tip, upstream)
	}
	wantReason := fmt.Sprintf("feature branch %s is rebased from %s onto upstream/main at %s as %s; seal 1 moves from base %s to %s in seal.json revision 2", intake.FeatureBranch(stream), before, upstream, tip, sealed[0].Base.Commit, upstream)
	if result.Outcome != "succeeded" || result.Evidence != wantReason {
		t.Fatalf("drift rebase result %+v", result)
	}
	after := intakefixture.Seals(t, repository, stream)
	moved := sealed[0]
	moved.Base.Commit = upstream
	if len(after) != 2 || !reflect.DeepEqual(after[1], moved) {
		t.Fatalf("seals after the drift rebase %+v, want the first with base %s", after, upstream)
	}
	if doc := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)[1]; doc.Actor != execution.ForemanActor || doc.Cause != op.ID {
		t.Fatalf("the moved seal's header %+v", doc.Header)
	}
	records := journey.DriftRecords(t, repository, stream)
	want := []lifecycle.DriftRebase{
		{Drift: 1, Operation: op.ID, Outcome: lifecycle.DriftReplayed, Branch: intake.FeatureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, From: sealed[0].Base.Commit, Before: before, Commit: tip},
		{Drift: 1, Operation: op.ID, Outcome: lifecycle.DriftRebased, Branch: intake.FeatureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, From: sealed[0].Base.Commit, Before: before, Commit: tip, Seal: 1, SealRevision: 2},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("drift/rebase.json %+v, want %+v", records, want)
	}
	rebased := intakefixture.TransitionByID(t, repository, stream, "drift-1-rebased")
	if rebased.To != "rebased-1" || rebased.Cause != op.ID || rebased.Reason != wantReason {
		t.Fatalf("the outcome %+v", rebased)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectCompleted || !reflect.DeepEqual(seen.Result, &result) {
		t.Fatalf("inspection after the drift rebase %+v: %v", seen, err)
	}
	if again, err := d.Apply(ctx, op); err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("a recorded drift rebase applied again: %+v %v", again, err)
	}
	if moved := upstreamMovedEvents(t, repository, stream); len(moved) != 0 {
		t.Fatalf("a clean drift rebase with nothing visible told the chief of staff %+v", moved)
	}

	op = journey.RequestDrift(t, d, stream)
	result = journey.SettleOperation(t, f.S, repository, stream, op, d)
	if now, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || now != tip {
		t.Fatalf("a drift rebase with nothing new moved the branch to %s: %v", now, err)
	}
	if n := len(intakefixture.Seals(t, repository, stream)); n != 2 {
		t.Fatalf("a drift rebase with nothing new recorded seal revision %d", n)
	}
	if want := fmt.Sprintf("feature branch %s at %s already descends from upstream/main at %s; seal 1 keeps its base %s", intake.FeatureBranch(stream), tip, upstream, upstream); result.Evidence != want {
		t.Fatalf("drift rebase 2 result %+v", result)
	}
}

func TestPausedWorkstreamIsSkippedByDriftRebases(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "paused-drift", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	g := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	before, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
	fixturekit.Must(t, err)
	journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	journey.RequestDrift(t, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	fixturekit.Must(t, repository.Close())

	state, err := json.Marshal(runtime.State{Version: runtime.Version, Pauses: []runtime.Pause{{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "test", SetAt: time.Now().UTC()}}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(f.S.Cfg.Root.String(), "runtime.json"), state, 0600))
	f.Start(t)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return journey.DriftOperations(t, f.Repository(), stream) })
	f.Stop(t)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Evidence != "drift rebase 1 changed nothing: the workstream is paused" {
		t.Fatalf("drift operations of a paused workstream %+v", ops)
	}

	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	d = journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	if requested, err := d.RequestDrifts(ctx, intakefixture.TestDrift); err != nil || len(requested) != 0 {
		t.Fatalf("drift rebases asked for a paused workstream: %v %v", requested, err)
	}
	if ops := journey.DriftOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("drift operations %+v", ops)
	}
	if tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != before {
		t.Fatalf("a paused workstream's branch moved to %s: %v", tip, err)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a paused workstream's seal changed: %+v", after)
	}
	if skipped := intakefixture.TransitionByID(t, repository, stream, "drift-1-skipped"); skipped.To != "skipped-1" {
		t.Fatalf("the outcome %+v", skipped)
	}
	if records := journey.DriftRecords(t, repository, stream); len(records) != 0 {
		t.Fatalf("a skipped drift rebase recorded %+v", records)
	}
}

func TestInterruptedDriftRebaseRecoversWithoutReplayingAgain(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "interrupted-drift", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	g := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	journey.MoveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	replays := 0
	check := func(repository *trace.Repository, k int, point, before, upstream string, result coreadapter.OperationResult) {
		t.Helper()
		if replays != k {
			t.Fatalf("drift rebase %d interrupted at %s replayed %d times in all", k, point, replays)
		}
		tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
		fixturekit.Must(t, err)
		if tip == before || journey.ParentOf(t, f, tip) != upstream {
			t.Fatalf("drift rebase %d interrupted at %s left the branch at %s, not on %s", k, point, tip, upstream)
		}
		if result.Outcome != "succeeded" || !strings.Contains(result.Evidence, fmt.Sprintf("as %s; seal 1 moves", tip)) {
			t.Fatalf("drift rebase %d result %+v", k, result)
		}
		if all := intakefixture.Seals(t, repository, stream); len(all) != k+1 || all[k].Base.Commit != upstream {
			t.Fatalf("seals after drift rebase %d interrupted at %s: %+v", k, point, all)
		}
		var outcomes []string
		for _, r := range journey.DriftRecords(t, repository, stream) {
			if r.Drift == k {
				outcomes = append(outcomes, r.Outcome)
			}
		}
		if !slices.Equal(outcomes, []string{lifecycle.DriftReplayed, lifecycle.DriftRebased}) {
			t.Fatalf("drift rebase %d interrupted at %s recorded %q", k, point, outcomes)
		}
		transitions, err := trace.Read[trace.Transition](repository, stream)
		fixturekit.Must(t, err)
		if n := len(slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.ID != fmt.Sprintf("drift-%d-rebased", k) })); n != 1 {
			t.Fatalf("drift rebase %d recorded its outcome %d times", k, n)
		}
	}
	interrupt := func(k int, point string) (before, upstream string, op coreadapter.Operation) {
		t.Helper()
		before, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
		fixturekit.Must(t, err)
		upstream = journey.AdvanceUpstream(t, f, map[string]string{fmt.Sprintf("UPSTREAM-%d.md", k): "upstream\n"})
		op = journey.RequestDrift(t, d, stream)
		stop := true
		f.S.Boundary = func(name string) error {
			if name == point && stop {
				stop = false
				return errors.New("the service stopped")
			}
			if name == "drift-replaying" {
				replays++
			}
			return nil
		}
		if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("drift rebase %d interrupted at %s: %v", k, point, err)
		}
		return before, upstream, op
	}

	observes := func(op coreadapter.Operation, point, evidence string) {
		t.Helper()
		observed, err := d.Inspect(ctx, op)
		if err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, evidence) {
			t.Fatalf("inspection after an interruption at %s: %+v %v", point, observed, err)
		}
	}

	before, upstream, op := interrupt(1, "drift-replaying")
	if tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != before || len(journey.DriftRecords(t, repository, stream)) != 0 {
		t.Fatalf("a drift rebase interrupted before its replay moved the branch to %s or recorded %+v: %v", tip, journey.DriftRecords(t, repository, stream), err)
	}
	observes(op, "drift-replaying", "the drift rebase has not replayed it")
	check(repository, 1, "drift-replaying", before, upstream, journey.SettleOperation(t, f.S, repository, stream, op, d))

	before, upstream, op = interrupt(2, "drift-replayed")
	if tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != before {
		t.Fatalf("the branch moved to %s before the recorded replay was applied: %v", tip, err)
	}
	observes(op, "drift-replayed", "the branch moves to it")
	journey.AdvanceUpstream(t, f, map[string]string{"LATER.md": "later\n"})
	check(repository, 2, "drift-replayed", before, upstream, journey.SettleOperation(t, f.S, repository, stream, op, d))

	before, upstream, op = interrupt(3, "drift-moved")
	observes(op, "drift-moved", "the seal and outcome are recorded without replaying again")
	check(repository, 3, "drift-moved", before, upstream, journey.SettleOperation(t, f.S, repository, stream, op, d))

	before, upstream, _ = interrupt(4, "drift-recorded")
	f.S.ClearBoundary()
	fixturekit.Must(t, repository.Close())
	f.Start(t)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return journey.DriftOperations(t, f.Repository(), stream) })
	f.Stop(t)
	if len(ops) != 4 || ops[3].Result == nil {
		t.Fatalf("drift operations after a restart %+v", ops)
	}
	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	check(repository, 4, "drift-recorded", before, upstream, *ops[3].Result)
}

func TestDriftRebaseIsSerializedWithLandings(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "drift-lander")
	defer func() { repository.Close() }()
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	d := journey.Drifter{Foreman: lands}
	ctx := context.Background()
	fixturekit.Must(t, lands.Pass(ctx))
	landings := journey.LandOperations(t, repository, stream)
	if len(landings) != 1 || landings[0].Result != nil {
		t.Fatalf("landing operations %+v", landings)
	}
	journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	if requested, err := d.RequestDrifts(ctx, intakefixture.TestDrift); err != nil || len(requested) != 0 {
		t.Fatalf("drift rebases asked for while a landing has no result: %v %v", requested, err)
	}
	if ops := journey.DriftOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("drift operations %+v", ops)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, landings[0].Operation, lands); result.Outcome != "succeeded" {
		t.Fatalf("landing result %+v", result)
	}

	op := journey.RequestDrift(t, d, stream)
	for range 2 {
		fixturekit.Must(t, lands.Pass(ctx))
	}
	if ops := journey.LandOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("landings asked for while a drift rebase has no result: %+v", ops)
	}
	if ops := journey.RebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("unit rebases asked for while a drift rebase has no result: %+v", ops)
	}
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for unit carryover: %v", err)
	}
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift retry did not wait for the recorded unit rebase: %v", err)
	}
	tip, _, err := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit).Branch(ctx, intake.FeatureBranch(stream))
	fixturekit.Must(t, err)
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("unit rebases during the drift rebase %+v", ops)
	}
	if drift := journey.DriftOperations(t, repository, stream); len(drift) != 1 || drift[0].Result != nil {
		t.Fatalf("drift completed before unit carryover: %+v", drift)
	}
	fixturekit.Must(t, repository.Close())
	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	lands.Repository = repository
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("restarted drift did not resume its recorded unit rebase: %v", err)
	}
	ops = journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("restart duplicated the unit rebase: %+v", ops)
	}
	if in, err := execution.DecodeRebase(ops[0].Operation); err != nil || in.Onto != tip {
		t.Fatalf("unit rebase %+v onto the drifted tip %s: %v", in, tip, err)
	}
	journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Rebaser{Foreman: lands})
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if state.Value != execution.UnitChecking {
		t.Fatalf("rebased approved unit remained %s", state.Value)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("drift rebase result %+v", result)
	}
	fixturekit.Must(t, lands.Pass(ctx))
	if got := journey.LandOperations(t, repository, stream); len(got) != 1 {
		t.Fatalf("rebased unit landed under its old approval: %+v", got)
	}
}

func TestDriftRebaseHoldsBackOnlyItsOwnWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "drift-elsewhere")
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	drifting := journey.RecordBuild(t, f, repository, "drift-elsewhere-other", intakefixture.IndependentPlan, config.WorkspacesGit, base)
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	fixturekit.Must(t, lands.DriftDriver().RequestDrift(ctx, drifting, ("the test asks for a drift rebase")))
	if ops := journey.DriftOperations(t, repository, drifting); len(ops) != 1 || ops[0].Result != nil {
		t.Fatalf("drift operations %+v", ops)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	landings := journey.LandOperations(t, repository, stream)
	if len(landings) != 1 {
		t.Fatalf("landings asked for while another workstream's drift rebase has no result: %+v", landings)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, landings[0].Operation, lands); result.Outcome != "succeeded" {
		t.Fatalf("landing result %+v", result)
	}
	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.RebaseOperations(t, repository, stream, "dedupe"); len(ops) != 1 {
		t.Fatalf("unit rebases asked for while another workstream's drift rebase has no result: %+v", ops)
	}
	if ops := journey.LandOperations(t, repository, drifting); len(ops) != 0 {
		t.Fatalf("the drifting workstream was asked to land: %+v", ops)
	}
	if ops := journey.DriftOperations(t, repository, drifting); len(ops) != 1 || ops[0].Result != nil {
		t.Fatalf("drift operations %+v", ops)
	}
}
func TestDriftWaitsForActiveMasonBeforeRebasingItsWorkspace(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewRebaseFixture(t, "drift-writer")
	defer repository.Close()
	ctx := context.Background()
	units := journey.NewUnitWorkspaces(f.S.Cfg, repository)
	w, before, found, err := units.Find(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	_, err = repository.ClaimTurn(ctx, stream, intake.MasonAgent("dedupe"), "active-mason", f.S.Cfg.Root.String(), f.Clock.Now())
	fixturekit.Must(t, err)
	journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	op := journey.RequestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for the active mason: %v", err)
	}
	if ops := journey.RebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("active mason's workspace was scheduled for rebase: %+v", ops)
	}
	if head, _, err := intakefixture.ProviderOf(t, units.StreamWorkspaces, stream).Branch(ctx, w.Branch); err != nil || head != before {
		t.Fatalf("active mason's workspace moved from %s to %s: %v", before, head, err)
	}
}
func TestDriftQueuesUnitConflictOnce(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewRebaseFixture(t, "drift-unit-conflict")
	defer func() { repository.Close() }()
	ctx := context.Background()
	units := journey.NewUnitWorkspaces(f.S.Cfg, repository)
	w, base, found, err := units.Find(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CONFLICT.md"), []byte("unit\n"), 0600))
	_, err = intakefixture.ProviderOf(t, units.StreamWorkspaces, stream).Snapshot(ctx, w, base)
	fixturekit.Must(t, err)
	from := intakefixture.Seals(t, repository, stream)[0].Base.Commit
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"CONFLICT.md": "upstream\n"})
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	op := journey.RequestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for conflicted unit: %v", err)
	}
	if observed, err := d.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, "carries the unfinished units onto it") {
		t.Fatalf("inspection of a drift rebase carrying its units %+v %v", observed, err)
	}
	rebases := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("unit rebase requests %+v", rebases)
	}
	journey.SettleOperation(t, f.S, repository, stream, rebases[0].Operation, journey.Rebaser(d))
	carried := journey.UnitRebases(t, repository, stream, "dedupe")
	if len(carried) != 1 || carried[0].Base != base || !slices.Equal(carried[0].Conflicts, []string{"CONFLICT.md"}) {
		t.Fatalf("the unit's drift conflict %+v", carried)
	}
	for _, other := range journey.RebaseOperations(t, repository, stream, "resume") {
		journey.SettleOperation(t, f.S, repository, stream, other.Operation, journey.Rebaser(d))
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("drift result %+v", result)
	}
	check := func() {
		moved := upstreamMovedEvents(t, repository, stream)
		transition, _ := execution.RebaseIDs("dedupe", 1)
		if len(moved) != 1 || moved[0].TransitionID != transition+"-conflicted" {
			t.Fatalf("upstream moved events of a unit conflict %+v", moved)
		}
		checkMoved(t, moved[0], stream, trace.UpstreamMove{Drift: 1, From: from, To: upstream}, "unit dedupe conflicts with the feature branch", "CONFLICT.md")
		th, err := repository.Thread(stream, intake.MasonAgent("dedupe"))
		fixturekit.Must(t, err)
		count := 0
		for _, turn := range th.Turns {
			if turn.Request.TurnID == execution.ResolveTurnID("dedupe", 1) {
				count++
				if !strings.Contains(turn.Request.Prompt, "sealed spec") || !strings.Contains(turn.Request.Prompt, "CONFLICT.md") {
					t.Fatalf("conflict turn has no sealed spec or path: %q", turn.Request.Prompt)
				}
			}
		}
		if count != 1 {
			t.Fatalf("conflict turn queued %d times", count)
		}
	}
	check()
	fixturekit.Must(t, repository.Close())
	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	d.Repository = repository
	if _, err := d.Apply(ctx, op); err != nil {
		t.Fatalf("drift after restart: %v", err)
	}
	check()
}
