package executionjourney

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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func latestReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, execution.UnitReport) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == execution.ReportDocument(unit) {
			latest = d
		}
	}
	var report execution.UnitReport
	if latest.Revision != 0 {
		fixturekit.Must(t, json.Unmarshal([]byte(latest.Content), &report))
	}
	return latest, report
}

func TestRebaseWaitsForAnInterruptedWriterAndKeepsItsWork(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewRebaseFixture(t, "writer")
	defer func() { repository.Close() }()
	ctx := context.Background()
	m := journey.NewMasonController(f.S, repository)
	lands := &journey.Foreman{Masons: m}
	units := journey.NewUnitWorkspaces(f.S.Cfg, repository)
	w, base, found, err := units.Find(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	paths, err := units.Driver().Paths(w)
	fixturekit.Must(t, err)
	viewRoot := filepath.Join(f.S.Cfg.Root.String(), "views", string(f.Project), string(stream), intake.MasonAgent("dedupe"), execution.MasonTurnID("dedupe"))
	fixturekit.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "DEDUPE.md"), []byte("dedupe\n"), 0600))
	_, err = repository.ClaimTurn(ctx, stream, intake.MasonAgent("dedupe"), "crashed", filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.MasonAgent("dedupe"), execution.MasonTurnID("dedupe")), f.Clock.Now())
	fixturekit.Must(t, err)
	landed := journey.MoveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})

	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.RebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("a workspace with an interrupted writer was rebased: %+v", ops)
	}
	if ops := journey.RebaseOperations(t, repository, stream, "resume"); len(ops) != 1 {
		t.Fatalf("resume, whose mason is idle, has rebases %+v", ops)
	}
	if ops := journey.LandOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for while units are behind: %+v", ops)
	}
	if head := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", w.Path, "rev-parse", "HEAD")); head != base {
		t.Fatalf("the writer's workspace moved to %s", head)
	}

	fixturekit.Must(t, repository.Close())
	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	m.Repository = repository
	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.RebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("a workspace with an interrupted writer was rebased: %+v", ops)
	}
	fixturekit.Must(t, m.Pass(ctx))
	fixturekit.Must(t, m.Pass(ctx))
	th, err := repository.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	continuation := th.Turns[len(th.Turns)-1]
	if continuation.Request.TurnID != intake.MasonAgent("dedupe")+"-recover-1" || !continuation.CompletedAt.IsZero() {
		t.Fatalf("the interrupted turn's continuation %+v", continuation)
	}
	admit := journey.InspectForTest(f.S).Admit(f.S.Current(), repository)
	candidate := scheduler.Candidate{Workstream: stream, Thread: th, Turn: continuation}
	if admitted, err := admit(ctx, candidate); err != nil || admitted {
		t.Fatalf("a mason turn of a unit behind its feature branch was admitted: %t %v", admitted, err)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}
	in, err := execution.DecodeRebase(ops[0].Operation)
	fixturekit.Must(t, err)
	if in != (execution.RebaseInput{Unit: "dedupe", Rebase: 1, Onto: landed}) {
		t.Fatalf("rebase input %+v", in)
	}
	r := journey.Rebaser{Foreman: lands}
	result, err := r.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("the rebase %+v", result)
	}
	for name, want := range map[string]string{"DEDUPE.md": "dedupe\n", "LANDED.md": "landed\n"} {
		if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
			t.Fatalf("the rebased workspace's %s holds %q, %v", name, data, err)
		}
	}
	if status := fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", w.Path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("the rebased workspace:\n%s", status)
	}
	head := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", w.Path, "rev-parse", "HEAD"))
	if parent := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "log", "-1", "--format=%P", head)); parent != landed {
		t.Fatalf("the rebased commit's parents %s, want %s", parent, landed)
	}
	rebases := journey.UnitRebases(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("rebase records %+v", rebases)
	}
	got := rebases[0]
	snapshot := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "show", got.Snapshot+":DEDUPE.md"))
	want := api.UnitRebase{Unit: "dedupe", Rebase: 1, Operation: ops[0].Operation.ID, State: intake.UnitImplementing, Branch: execution.UnitBranch(stream, "dedupe"), Base: base, Onto: landed, Snapshot: got.Snapshot, Commit: head, Conflicts: []string{}}
	if !reflect.DeepEqual(got, want) || snapshot != "dedupe" {
		t.Fatalf("rebase.json %+v, want %+v; snapshot holds %q", got, want, snapshot)
	}
	if state, err := repository.Workflow(stream, execution.RebaseSubject("dedupe")); err != nil || state.Value != "rebased-1" {
		t.Fatalf("rebase subject %+v %v", state, err)
	}
	if admitted, err := admit(ctx, candidate); err != nil || !admitted {
		t.Fatalf("the continuation of a rebased unit was held: %t %v", admitted, err)
	}

	again, err := r.Apply(ctx, ops[0].Operation)
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("the rebase applied again: %+v %v", again, err)
	}
	observed, err := r.Inspect(ctx, ops[0].Operation)
	if err != nil || observed.State != coreadapter.EffectCompleted || !reflect.DeepEqual(*observed.Result, result) {
		t.Fatalf("inspection after the rebase %+v %v", observed, err)
	}
	if n := len(journey.UnitRebases(t, repository, stream, "dedupe")); n != 1 {
		t.Fatalf("%d rebase records after the rebase applied again", n)
	}
}

func TestInterruptedRebaseIsReconciled(t *testing.T) {
	t.Parallel()
	evidence := map[string]string{
		"rebase-snapshotting": "holds no rebase of this operation",
		"rebase-snapshotted":  "holds no rebase of this operation",
		"rebase-moved":        "this operation's rebase of snapshot",
		"rebase-recorded":     "rebase succeeded",
	}
	for _, step := range []string{"rebase-snapshotting", "rebase-snapshotted", "rebase-moved", "rebase-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := journey.NewRebaseFixture(t, "interrupted-rebase")
			defer repository.Close()
			ctx := context.Background()
			lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
			w, _, _, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "resume")
			fixturekit.Must(t, err)
			fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "RESUME.md"), []byte("resume\n"), 0600))
			landed := journey.MoveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})
			fixturekit.Must(t, lands.Pass(ctx))
			ops := journey.RebaseOperations(t, repository, stream, "resume")
			if len(ops) != 1 {
				t.Fatalf("resume's rebases %+v", ops)
			}
			crashed := false
			f.S.Boundary = func(name string) error {
				if name == step && !crashed {
					crashed = true
					return errors.New("crash")
				}
				return nil
			}
			r := journey.Rebaser{Foreman: lands}
			if _, err := r.Apply(ctx, ops[0].Operation); err == nil || !strings.Contains(err.Error(), "crash") {
				t.Fatalf("the rebase never reached %s: %v", step, err)
			}
			if observed, err := r.Inspect(ctx, ops[0].Operation); err != nil || (step == "rebase-recorded") != (observed.State == coreadapter.EffectCompleted) || !strings.Contains(observed.Evidence, evidence[step]) {
				t.Fatalf("inspection after the crash %+v %v", observed, err)
			}
			result, err := r.Apply(ctx, ops[0].Operation)
			if err != nil || result.Outcome != "succeeded" {
				t.Fatalf("the retried rebase %+v %v", result, err)
			}
			home := filepath.Dir(f.Clone)
			if count := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-list", "--count", landed+".."+execution.UnitBranch(stream, "resume"))); count != "1" {
				t.Fatalf("the unit branch gained %s commits on %s", count, landed)
			}
			for name, want := range map[string]string{"RESUME.md": "resume\n", "LANDED.md": "landed\n"} {
				if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
					t.Fatalf("the rebased workspace's %s holds %q, %v", name, data, err)
				}
			}
			if status := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", w.Path, "status", "--porcelain")); status != "" {
				t.Fatalf("the rebased workspace:\n%s", status)
			}
			rebases := journey.UnitRebases(t, repository, stream, "resume")
			head := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", w.Path, "rev-parse", "HEAD"))
			if len(rebases) != 1 || rebases[0].Commit != head || rebases[0].Onto != landed {
				t.Fatalf("rebase records %+v, head %s", rebases, head)
			}
			var outcomes []string
			for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
				if tr.Subject == execution.RebaseSubject("resume") && tr.From != "" {
					outcomes = append(outcomes, tr.ID)
				}
			}
			if !slices.Equal(outcomes, []string{execution.RebaseSubject("resume") + "-1-rebased"}) {
				t.Fatalf("rebase outcomes %v", outcomes)
			}
		})
	}
}

func TestRebaseConflictGoesToTheMasonAndNeverToTheReviewer(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "conflict")
	defer repository.Close()
	ctx := context.Background()
	m := journey.NewMasonController(f.S, repository)
	lands := &journey.Foreman{Masons: m}
	landed := journey.MoveFeature(t, f, stream, map[string]string{fixturekit.MasonWrote: "package trace\n\n// landed\n"})
	_, before := latestReport(t, repository, stream, "dedupe")

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}

	result := journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Rebaser{Foreman: lands})
	if result.Outcome != "succeeded" {
		t.Fatalf("the conflicted rebase %+v", result)
	}
	rebases := journey.UnitRebases(t, repository, stream, "dedupe")
	if len(rebases) != 1 || !slices.Equal(rebases[0].Conflicts, []string{fixturekit.MasonWrote}) || rebases[0].State != intake.UnitApproved || rebases[0].Snapshot != before.Candidate {
		t.Fatalf("rebase records %+v", rebases)
	}
	w, _, _, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	data, err := os.ReadFile(filepath.Join(w.Path, fixturekit.MasonWrote))
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), "\n<<<<<<< "+landed+"\n") || !strings.Contains(string(data), "\n>>>>>>> "+before.Candidate+"\n") {
		t.Fatalf("the conflicted file holds %q", data)
	}
	if _, report := latestReport(t, repository, stream, "dedupe"); !reflect.DeepEqual(report, before) {
		t.Fatalf("a conflicted rebase changed the report to %+v", report)
	}
	transition, _ := execution.RebaseIDs("dedupe", 1)
	outbox, err := repository.Outbox(stream)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == transition+"-conflicted" && e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, "Unit dedupe conflicts with feature branch") && strings.Contains(e.Event.Body, fixturekit.MasonWrote)
	}) {
		t.Fatal("the manager was not told of the conflict")
	}

	fixturekit.Must(t, lands.Pass(ctx))
	if ops := journey.LandOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for with a conflicted unit: %+v", ops)
	}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitImplementing {
		t.Fatalf("the conflicted unit is %s", state.Value)
	}
	moved := slices.IndexFunc(intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream), func(tr trace.Transition) bool {
		return tr.ID == trace.UnitSubject("dedupe")+"-implementing-rebase-1" && tr.From == intake.UnitApproved && tr.Cause == transition+"-conflicted" && tr.Actor == execution.ForemanActor
	})
	if moved < 0 {
		t.Fatal("the unit's return to implementing is not recorded")
	}
	th, err := repository.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	resolve := th.Turns[len(th.Turns)-1]
	for _, want := range []string{"- " + fixturekit.MasonWrote + "\n", `"<<<<<<< ` + landed + `"`, `">>>>>>> ` + before.Candidate + `"`, "2. Acknowledged chunks are never sent again."} {
		if resolve.Request.TurnID != execution.ResolveTurnID("dedupe", 1) || !strings.Contains(resolve.Request.Prompt, want) {
			t.Fatalf("the resolve turn %s lacks %q:\n%s", resolve.Request.TurnID, want, resolve.Request.Prompt)
		}
	}
	fixturekit.Must(t, lands.Pass(ctx))
	if again, err := repository.Thread(stream, intake.MasonAgent("dedupe")); err != nil || len(again.Turns) != len(th.Turns) {
		t.Fatalf("another pass queued more turns: %+v %v", again, err)
	}

	done := journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	fixturekit.Must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != intake.UnitImplementing {
		t.Fatalf("a candidate with conflict markers left implementing: %+v %v", state, err)
	}
	if doc, _ := latestReport(t, repository, stream, "dedupe"); doc.Revision != rebases[0].Report {
		t.Fatalf("a candidate with conflict markers was reported as revision %d", doc.Revision)
	}
	th, err = repository.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	remind := th.Turns[len(th.Turns)-1]
	if remind.Request.TurnID != fmt.Sprintf("%s-markers-%d", intake.MasonAgent("dedupe"), done.Sequence) || !strings.Contains(remind.Request.Prompt, "still carry conflict markers") || !strings.Contains(remind.Request.Prompt, fixturekit.MasonWrote) {
		t.Fatalf("the reminder %s:\n%s", remind.Request.TurnID, remind.Request.Prompt)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("package trace\n\n// landed and deduplicated\n"), 0600))
	journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	fixturekit.Must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != execution.UnitChecking {
		t.Fatalf("the resolved unit is %+v %v", state, err)
	}
	_, report := latestReport(t, repository, stream, "dedupe")
	if report.Base != landed || report.Candidate == before.Candidate {
		t.Fatalf("the resolved report %+v, want a new candidate from %s", report, landed)
	}
	req, _, err := m.CandidateEvidence(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if strings.Contains(req.Diff, "<<<<<<<") || strings.Contains(req.Diff, ">>>>>>>") || !strings.Contains(req.Diff, "+// landed and deduplicated") {
		t.Fatalf("the reviewer's diff:\n%s", req.Diff)
	}
	if paths, _, err := m.Driver().Conflicts(stream, "dedupe"); err != nil || len(paths) != 0 {
		t.Fatalf("resolved conflicts %v %v", paths, err)
	}
}

func TestUnitDoneBehindItsFeatureBranchFinishesAfterItsRebase(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewRebaseFixture(t, "done-behind")
	defer repository.Close()
	ctx := context.Background()
	m := journey.NewMasonController(f.S, repository)
	lands := &journey.Foreman{Masons: m}
	journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	landed := journey.MoveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})

	fixturekit.Must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != intake.UnitImplementing {
		t.Fatalf("a unit behind its feature branch is %+v %v", state, err)
	}
	if blocked, err := repository.Workflow(stream, execution.BlockedSubject("dedupe")); err != nil || blocked.Value != "" {
		t.Fatalf("a unit behind its feature branch was blocked: %+v %v", blocked, err)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Rebaser{Foreman: lands}); result.Outcome != "succeeded" {
		t.Fatalf("the rebase %+v", result)
	}
	fixturekit.Must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != execution.UnitChecking {
		t.Fatalf("the rebased unit is %+v %v", state, err)
	}
	if _, report := latestReport(t, repository, stream, "dedupe"); report.Base != landed {
		t.Fatalf("the report %+v is not on %s", report, landed)
	}
}

func TestRebaseConflictRemindersAreBoundedByCleanTurns(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "conflict-bound")
	defer repository.Close()
	f.S.Cfg.Mason.MaxCleanTurns = 1
	ctx := context.Background()
	m := journey.NewMasonController(f.S, repository)
	lands := &journey.Foreman{Masons: m}
	journey.MoveFeature(t, f, stream, map[string]string{fixturekit.MasonWrote: "package trace\n\n// landed\n"})
	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Rebaser{Foreman: lands}); result.Outcome != "succeeded" {
		t.Fatalf("the conflicted rebase %+v", result)
	}
	fixturekit.Must(t, lands.Pass(ctx))

	done := journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	fixturekit.Must(t, m.Pass(ctx))
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.MasonAgent("dedupe")); ids[len(ids)-1] != fmt.Sprintf("%s-markers-%d", intake.MasonAgent("dedupe"), done.Sequence) {
		t.Fatalf("the mason's turns after a done with markers %v", ids)
	}
	again := journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	fixturekit.Must(t, m.Pass(ctx))
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitContested {
		t.Fatalf("the unit is %s after a second done with markers", state.Value)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.MasonAgent("dedupe")); slices.Contains(ids, fmt.Sprintf("%s-markers-%d", intake.MasonAgent("dedupe"), again.Sequence)) {
		t.Fatalf("the mason was reminded again: %v", ids)
	}
	contest := intakefixture.TransitionByID(t, repository, stream, trace.EventID(trace.EventID(again.Request.ID, "response"), "contested"))
	if contest.From != intake.UnitImplementing || contest.Actor != intake.MasonActor || !strings.Contains(contest.Reason, "exhausting mason.max_clean_turns (1)") {
		t.Fatalf("the contest %+v", contest)
	}
}

func TestConcurrentRebaseWaitsForItsResolutionBeforeRemindingTheMason(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewRebaseFixture(t, "resolution-before-reminder")
	defer func() { repository.Close() }()
	ctx := context.Background()
	m := journey.NewMasonController(f.S, repository)
	lands := &journey.Foreman{Masons: m}
	w, _, found, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "dedupe")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("package trace\n\n// dedupe\n"), 0600))
	journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	journey.MoveFeature(t, f, stream, map[string]string{fixturekit.MasonWrote: "package trace\n\n// landed\n"})
	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.RebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("rebases: %+v", ops)
	}
	result := journey.SettleOperation(t, f.S, repository, stream, ops[0].Operation, journey.Rebaser{Foreman: lands})
	if result.Outcome != "succeeded" {
		t.Fatalf("rebase: %+v", result)
	}

	fixturekit.Must(t, m.Pass(ctx))
	th, err := repository.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("the pre-rebase done turn received a reminder: %+v", th.Turns)
	}

	fixturekit.Must(t, repository.Close())
	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	m = journey.NewMasonController(f.S, repository)
	lands = &journey.Foreman{Masons: m}
	fixturekit.Must(t, m.Pass(ctx))
	fixturekit.Must(t, lands.Pass(ctx))
	th, err = repository.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Request.TurnID != execution.ResolveTurnID("dedupe", 1) {
		t.Fatalf("conflict resolution: %+v", th.Turns)
	}

	fixturekit.Must(t, m.Pass(ctx))
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.MasonAgent("dedupe")); len(ids) != 2 {
		t.Fatalf("a queued resolution received a premature reminder: %v", ids)
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, fixturekit.MasonWrote), []byte("package trace\n\n// landed and deduplicated\n"), 0600))
	journey.CompleteMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	fixturekit.Must(t, m.Pass(ctx))
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if state.Value != execution.UnitChecking {
		t.Fatalf("the resolved unit did not finish after restart: %+v", state)
	}
}
