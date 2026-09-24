package service

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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// driftOperations returns the workstream's drift rebase operations in the
// order they were asked for.
func driftOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != DriftAction })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return ops
}

// driftRecords returns every recorded revision of drift/rebase.json.
func driftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []DriftRebase {
	t.Helper()
	var out []DriftRebase
	for _, d := range streamDocuments(t, repository, stream, driftDocument) {
		var r DriftRebase
		must(t, json.Unmarshal([]byte(d.Content), &r))
		out = append(out, r)
	}
	return out
}

// seals returns every recorded revision of seal.json.
func seals(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []seal.Seal {
	t.Helper()
	var out []seal.Seal
	for _, d := range streamDocuments(t, repository, stream, seal.DocumentID) {
		s, err := seal.Parse([]byte(d.Content))
		must(t, err)
		out = append(out, s)
	}
	return out
}

// requestDrift asks for drift rebases and returns the one operation it
// asked for, for the workstream.
func requestDrift(t *testing.T, d drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	asked := len(driftOperations(t, d.repository, stream))
	requested, err := d.requestDrifts(context.Background())
	must(t, err)
	if !slices.Equal(requested, []config.WorkstreamID{stream}) {
		t.Fatalf("drift rebases asked for %v, want %s", requested, stream)
	}
	ops := driftOperations(t, d.repository, stream)
	if len(ops) != asked+1 {
		t.Fatalf("drift operations %+v", ops)
	}
	return ops[asked].Operation
}

// settleOperation reconciles one pending operation as the reconciliation
// controller does: inspection first, the effect only when it is absent, and
// the result recorded on the operation, at the time of the service's clock.
func settleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	ctx := context.Background()
	ops, err := repository.Operations(stream)
	must(t, err)
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == op.ID })
	if i < 0 {
		t.Fatalf("operation %s is not recorded", op.ID)
	}
	var result coreadapter.OperationResult
	now := s.now()
	must(t, repository.WithOperation(ctx, stream, ops[i].EventID, foremanActor, func() time.Time { return now }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		seen, err := r.Inspect(ctx, op)
		if err != nil {
			return err
		}
		observe := a.Action("observe", now)
		observe.Observation = &seen
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if seen.State == coreadapter.EffectCompleted {
			result = *seen.Result
		} else {
			if err := a.Record(ctx, a.Action("effect", now)); err != nil {
				return err
			}
			if result, err = r.Apply(ctx, op); err != nil {
				return err
			}
		}
		done := a.Action("result", now)
		done.Result = &result
		return a.Record(ctx, done)
	}))
	return result
}

// parentOf returns the first parent of a commit of the fixture's clone.
func parentOf(t *testing.T, f *shedFixture, commit string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", commit+"^"))
}

// fileAt returns a file of a commit of the fixture's clone.
func fileAt(t *testing.T, f *shedFixture, commit, path string) string {
	t.Helper()
	return demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "show", commit+":"+path)
}

// A drift rebase fetches upstream's moved main and replays the building
// workstream's feature branch onto it. The branch moves to the rebased
// commit, and one trace commit records drift/rebase.json, the next seal.json
// revision with the fetched commit as its base, and the rebase's outcome.
// A later drift rebase with nothing new upstream leaves the branch and the
// seal as they are.
func TestDriftRebaseMovesTheFeatureBranchAndTheSeal(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before := moveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	sealed := seals(t, repository, stream)
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	if len(sealed) != 1 || sealed[0].Base.Commit == upstream {
		t.Fatalf("seals before the drift rebase %+v", sealed)
	}

	op := requestDrift(t, d, stream)
	requested := transitionByID(t, repository, stream, "drift-1")
	if requested.Subject != driftSubject || requested.To != "requested-1" || requested.Actor != foremanActor || requested.Cause != "seal-1" {
		t.Fatalf("the request %+v", requested)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectAbsent {
		t.Fatalf("inspection before the drift rebase %+v: %v", seen, err)
	}
	result := settleOperation(t, f.s, repository, stream, op, d)
	g := featureWorkspaces(f.s.cfg)
	tip, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
	if tip == before || parentOf(t, f, tip) != upstream || fileAt(t, f, tip, "internal/trace/resume.go") != "package trace\n" || fileAt(t, f, tip, "UPSTREAM.md") != "upstream\n" {
		t.Fatalf("the feature branch is at %s, not the feature commit replayed onto %s", tip, upstream)
	}
	wantReason := fmt.Sprintf("feature branch %s is rebased from %s onto upstream/main at %s as %s; seal 1 moves from base %s to %s in seal.json revision 2", featureBranch(stream), before, upstream, tip, sealed[0].Base.Commit, upstream)
	if result.Outcome != "succeeded" || result.Evidence != wantReason {
		t.Fatalf("drift rebase result %+v", result)
	}
	after := seals(t, repository, stream)
	moved := sealed[0]
	moved.Base.Commit = upstream
	if len(after) != 2 || !reflect.DeepEqual(after[1], moved) {
		t.Fatalf("seals after the drift rebase %+v, want the first with base %s", after, upstream)
	}
	if doc := streamDocuments(t, repository, stream, seal.DocumentID)[1]; doc.Actor != foremanActor || doc.Cause != op.ID {
		t.Fatalf("the moved seal's header %+v", doc.Header)
	}
	records := driftRecords(t, repository, stream)
	want := []DriftRebase{
		{Drift: 1, Operation: op.ID, Outcome: driftReplayed, Branch: featureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, Before: before, Commit: tip},
		{Drift: 1, Operation: op.ID, Outcome: driftRebased, Branch: featureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, Before: before, Commit: tip, Seal: 1, SealRevision: 2},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("drift/rebase.json %+v, want %+v", records, want)
	}
	rebased := transitionByID(t, repository, stream, "drift-1-rebased")
	if rebased.To != "rebased-1" || rebased.Cause != op.ID || rebased.Reason != wantReason {
		t.Fatalf("the outcome %+v", rebased)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectCompleted || !reflect.DeepEqual(seen.Result, &result) {
		t.Fatalf("inspection after the drift rebase %+v: %v", seen, err)
	}
	if again, err := d.Apply(ctx, op); err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("a recorded drift rebase applied again: %+v %v", again, err)
	}

	// Nothing new upstream: the branch and the seal stay.
	op = requestDrift(t, d, stream)
	result = settleOperation(t, f.s, repository, stream, op, d)
	if now, _, err := g.Branch(ctx, featureBranch(stream)); err != nil || now != tip {
		t.Fatalf("a drift rebase with nothing new moved the branch to %s: %v", now, err)
	}
	if n := len(seals(t, repository, stream)); n != 2 {
		t.Fatalf("a drift rebase with nothing new recorded seal revision %d", n)
	}
	if want := fmt.Sprintf("feature branch %s at %s already descends from upstream/main at %s; seal 1 keeps its base %s", featureBranch(stream), tip, upstream, upstream); result.Evidence != want {
		t.Fatalf("drift rebase 2 result %+v", result)
	}
}

// A drift rebase whose replay conflicts leaves the feature branch and the
// seal where they were and records the upstream commit and the conflicted
// paths.
func TestConflictingDriftRebaseLeavesTheBranchAndTheSeal(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "conflicted-drift")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	sealed := streamDocuments(t, repository, stream, seal.DocumentID)

	op := requestDrift(t, d, stream)
	result := settleOperation(t, f.s, repository, stream, op, d)
	wantReason := fmt.Sprintf("feature branch %s does not rebase cleanly onto upstream/main at %s: CODEOWNERS conflicted; the branch stays at %s and the seal is unchanged", featureBranch(stream), upstream, before)
	if result.Outcome != "succeeded" || result.Evidence != wantReason {
		t.Fatalf("drift rebase result %+v", result)
	}
	if tip, _, err := featureWorkspaces(f.s.cfg).Branch(ctx, featureBranch(stream)); err != nil || tip != before {
		t.Fatalf("a conflicted drift rebase moved the feature branch to %s: %v", tip, err)
	}
	if after := streamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a conflicted drift rebase changed the seal: %+v", after)
	}
	want := []DriftRebase{{Drift: 1, Operation: op.ID, Outcome: driftConflicted, Branch: featureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, Before: before, Conflicts: []string{"CODEOWNERS"}}}
	if records := driftRecords(t, repository, stream); !reflect.DeepEqual(records, want) {
		t.Fatalf("drift/rebase.json %+v, want %+v", records, want)
	}
	if conflicted := transitionByID(t, repository, stream, "drift-1-conflicted"); conflicted.To != "conflicted-1" || conflicted.Reason != wantReason {
		t.Fatalf("the outcome %+v", conflicted)
	}
}

// A paused workstream takes no drift rebase: none is asked for while the
// pause is in force, and one asked for before the pause is recorded skipped
// with nothing changed.
func TestPausedWorkstreamIsSkippedByDriftRebases(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "paused-drift")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	g := featureWorkspaces(f.s.cfg)
	before, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	requestDrift(t, d, stream)
	sealed := streamDocuments(t, repository, stream, seal.DocumentID)
	must(t, repository.Close())

	state, err := json.Marshal(runtime.State{Version: runtime.Version, Pauses: []runtime.Pause{{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "test", SetAt: time.Now().UTC()}}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.s.cfg.Root.String(), "runtime.json"), state, 0600))
	f.start(t)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return driftOperations(t, f.repository(), stream) })
	f.stop(t)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Evidence != "drift rebase 1 changed nothing: the workstream is paused" {
		t.Fatalf("drift operations of a paused workstream %+v", ops)
	}

	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	if requested, err := d.requestDrifts(ctx); err != nil || len(requested) != 0 {
		t.Fatalf("drift rebases asked for a paused workstream: %v %v", requested, err)
	}
	if ops := driftOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("drift operations %+v", ops)
	}
	if tip, _, err := g.Branch(ctx, featureBranch(stream)); err != nil || tip != before {
		t.Fatalf("a paused workstream's branch moved to %s: %v", tip, err)
	}
	if after := streamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a paused workstream's seal changed: %+v", after)
	}
	if skipped := transitionByID(t, repository, stream, "drift-1-skipped"); skipped.To != "skipped-1" {
		t.Fatalf("the outcome %+v", skipped)
	}
	if records := driftRecords(t, repository, stream); len(records) != 0 {
		t.Fatalf("a skipped drift rebase recorded %+v", records)
	}
}

// A drift rebase interrupted after its replay is recorded, after the branch
// moved, or after its outcome is recorded recovers to one outcome and one
// seal revision without replaying again, onto the upstream commit it
// recorded even when upstream moved again. A restarted service completes an
// interrupted drift rebase from its record.
func TestInterruptedDriftRebaseRecoversWithoutReplayingAgain(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "interrupted-drift")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	g := featureWorkspaces(f.s.cfg)
	moveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	replays := 0
	// check checks drift rebase k, interrupted at point, after its recovery.
	check := func(repository *trace.Repository, k int, point, before, upstream string, result coreadapter.OperationResult) {
		t.Helper()
		if replays != k {
			t.Fatalf("drift rebase %d interrupted at %s replayed %d times in all", k, point, replays)
		}
		tip, _, err := g.Branch(ctx, featureBranch(stream))
		must(t, err)
		if tip == before || parentOf(t, f, tip) != upstream {
			t.Fatalf("drift rebase %d interrupted at %s left the branch at %s, not on %s", k, point, tip, upstream)
		}
		if result.Outcome != "succeeded" || !strings.Contains(result.Evidence, fmt.Sprintf("as %s; seal 1 moves", tip)) {
			t.Fatalf("drift rebase %d result %+v", k, result)
		}
		if all := seals(t, repository, stream); len(all) != k+1 || all[k].Base.Commit != upstream {
			t.Fatalf("seals after drift rebase %d interrupted at %s: %+v", k, point, all)
		}
		var outcomes []string
		for _, r := range driftRecords(t, repository, stream) {
			if r.Drift == k {
				outcomes = append(outcomes, r.Outcome)
			}
		}
		if !slices.Equal(outcomes, []string{driftReplayed, driftRebased}) {
			t.Fatalf("drift rebase %d interrupted at %s recorded %q", k, point, outcomes)
		}
		transitions, err := trace.Read[trace.Transition](repository, stream)
		must(t, err)
		if n := len(slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.ID != fmt.Sprintf("drift-%d-rebased", k) })); n != 1 {
			t.Fatalf("drift rebase %d recorded its outcome %d times", k, n)
		}
	}
	// interrupt asks for drift rebase k and stops it at point.
	interrupt := func(k int, point string) (before, upstream string, op coreadapter.Operation) {
		t.Helper()
		before, _, err := g.Branch(ctx, featureBranch(stream))
		must(t, err)
		upstream = advanceUpstream(t, f, map[string]string{fmt.Sprintf("UPSTREAM-%d.md", k): "upstream\n"})
		op = requestDrift(t, d, stream)
		stop := true
		f.s.boundary = func(name string) error {
			if name == "drift-replaying" {
				replays++
			}
			if name == point && stop {
				stop = false
				return errors.New("the service stopped")
			}
			return nil
		}
		if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("drift rebase %d interrupted at %s: %v", k, point, err)
		}
		return before, upstream, op
	}

	before, upstream, op := interrupt(1, "drift-replayed")
	if tip, _, err := g.Branch(ctx, featureBranch(stream)); err != nil || tip != before {
		t.Fatalf("the branch moved to %s before the recorded replay was applied: %v", tip, err)
	}
	advanceUpstream(t, f, map[string]string{"LATER.md": "later\n"})
	check(repository, 1, "drift-replayed", before, upstream, settleOperation(t, f.s, repository, stream, op, d))

	before, upstream, op = interrupt(2, "drift-moved")
	check(repository, 2, "drift-moved", before, upstream, settleOperation(t, f.s, repository, stream, op, d))

	before, upstream, _ = interrupt(3, "drift-recorded")
	f.s.boundary = nil
	must(t, repository.Close())
	f.start(t)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return driftOperations(t, f.repository(), stream) })
	f.stop(t)
	if len(ops) != 3 || ops[2].Result == nil {
		t.Fatalf("drift operations after a restart %+v", ops)
	}
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	check(repository, 3, "drift-recorded", before, upstream, *ops[2].Result)
}

// Drift rebases share the project's one lander with landings: none is asked
// for while a landing has no result, and no landing or unit rebase is asked
// for while a drift rebase has none. The drift operation holds the lander
// until its unit rebases are recorded and the units are current.
func TestDriftRebaseIsSerializedWithLandings(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "drift-lander")
	defer func() { repository.Close() }()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	d := drifter{lands}
	ctx := context.Background()
	must(t, lands.Pass(ctx))
	landings := landOperations(t, repository, stream)
	if len(landings) != 1 || landings[0].Result != nil {
		t.Fatalf("landing operations %+v", landings)
	}
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	if requested, err := d.requestDrifts(ctx); err != nil || len(requested) != 0 {
		t.Fatalf("drift rebases asked for while a landing has no result: %v %v", requested, err)
	}
	if ops := driftOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("drift operations %+v", ops)
	}
	if result := settleOperation(t, f.s, repository, stream, landings[0].Operation, lands); result.Outcome != "succeeded" {
		t.Fatalf("landing result %+v", result)
	}

	op := requestDrift(t, d, stream)
	for range 2 {
		must(t, lands.Pass(ctx))
	}
	if ops := landOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("landings asked for while a drift rebase has no result: %+v", ops)
	}
	if ops := rebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("unit rebases asked for while a drift rebase has no result: %+v", ops)
	}
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for unit carryover: %v", err)
	}
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift retry did not wait for the recorded unit rebase: %v", err)
	}
	tip, _, err := featureWorkspaces(f.s.cfg).Branch(ctx, featureBranch(stream))
	must(t, err)
	ops := rebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("unit rebases during the drift rebase %+v", ops)
	}
	if drift := driftOperations(t, repository, stream); len(drift) != 1 || drift[0].Result != nil {
		t.Fatalf("drift completed before unit carryover: %+v", drift)
	}
	must(t, repository.Close())
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	lands.repository = repository
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("restarted drift did not resume its recorded unit rebase: %v", err)
	}
	ops = rebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("restart duplicated the unit rebase: %+v", ops)
	}
	if in, err := decodeRebase(ops[0].Operation); err != nil || in.Onto != tip {
		t.Fatalf("unit rebase %+v onto the drifted tip %s: %v", in, tip, err)
	}
	settleOperation(t, f.s, repository, stream, ops[0].Operation, rebaser{lands})
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	if state.Value != UnitReviewing {
		t.Fatalf("rebased approved unit remained %s", state.Value)
	}
	if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("drift rebase result %+v", result)
	}
	must(t, lands.Pass(ctx))
	if got := landOperations(t, repository, stream); len(got) != 1 {
		t.Fatalf("rebased unit landed under its old approval: %+v", got)
	}
}

func TestDriftWaitsForActiveMasonBeforeRebasingItsWorkspace(t *testing.T) {
	t.Parallel()
	f, stream, repository := newRebaseFixture(t, "drift-writer")
	defer repository.Close()
	ctx := context.Background()
	units := newUnitWorkspaces(f.s.cfg)
	w, before, found, err := units.find(ctx, stream, "dedupe")
	must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	_, err = repository.ClaimTurn(ctx, stream, masonAgent("dedupe"), "active-mason", f.s.cfg.Root.String(), f.clock.Now())
	must(t, err)
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	op := requestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for the active mason: %v", err)
	}
	if ops := rebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("active mason's workspace was scheduled for rebase: %+v", ops)
	}
	if head, _, err := units.git.Branch(ctx, w.Branch); err != nil || head != before {
		t.Fatalf("active mason's workspace moved from %s to %s: %v", before, head, err)
	}
}

func TestDriftQueuesUnitConflictOnce(t *testing.T) {
	t.Parallel()
	f, stream, repository := newRebaseFixture(t, "drift-unit-conflict")
	defer func() { repository.Close() }()
	ctx := context.Background()
	units := newUnitWorkspaces(f.s.cfg)
	w, base, found, err := units.find(ctx, stream, "dedupe")
	must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	must(t, os.WriteFile(filepath.Join(w.Path, "CONFLICT.md"), []byte("unit\n"), 0600))
	_, err = units.git.Snapshot(ctx, w, base)
	must(t, err)
	advanceUpstream(t, f, map[string]string{"CONFLICT.md": "upstream\n"})
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	op := requestDrift(t, d, stream)
	if _, err := d.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "awaits unit carryover") {
		t.Fatalf("drift did not wait for conflicted unit: %v", err)
	}
	rebases := rebaseOperations(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("unit rebase requests %+v", rebases)
	}
	settleOperation(t, f.s, repository, stream, rebases[0].Operation, rebaser{d.foreman})
	for _, other := range rebaseOperations(t, repository, stream, "resume") {
		settleOperation(t, f.s, repository, stream, other.Operation, rebaser{d.foreman})
	}
	if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("drift result %+v", result)
	}
	check := func() {
		th, err := repository.Thread(stream, masonAgent("dedupe"))
		must(t, err)
		count := 0
		for _, turn := range th.Turns {
			if turn.Request.TurnID == resolveTurnID("dedupe", 1) {
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
	must(t, repository.Close())
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	d.repository = repository
	if _, err := d.Apply(ctx, op); err != nil {
		t.Fatalf("drift after restart: %v", err)
	}
	check()
}
