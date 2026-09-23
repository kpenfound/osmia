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

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// newRebaseFixture builds independentPlan with masons that never report
// done, stops the service once resume's mason ran, and starts dedupe with the
// service stopped, so both units are implementing: resume with its turn
// completed and dedupe with its first turn queued. It returns the workstream
// and the trace, open.
func newRebaseFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, masons := newMasonFixture(t, 1, independentPlan)
	stream, _ := f.builtAs(t, key)
	f.awaitMasonRan(t, stream, "resume")
	masons.check(t)
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	m := newMasonController(f.s, repository)
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream is not building")
	}
	if started, err := m.start(context.Background(), b, "dedupe"); err != nil || !started {
		t.Fatalf("dedupe did not start: %v", err)
	}
	return f, stream, repository
}

// moveFeature commits files onto the workstream's feature branch, as a
// landing does, and returns the new tip.
func moveFeature(t *testing.T, f *shedFixture, stream config.WorkstreamID, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	g := featureWorkspaces(f.s.cfg)
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
	must(t, err)
	w := acquired.(workspace.Worktree)
	tip, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
	for name, content := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(w.Path, name)), 0700))
		must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
	}
	moved, err := g.Snapshot(ctx, w, tip)
	must(t, err)
	return moved
}

// rebaseOperation returns the unit's rebase operations.
func rebaseOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool {
		in, err := decodeRebase(o.Operation)
		return err != nil || in.Unit != unit
	})
}

// unitRebases returns the recorded revisions of the unit's rebase.json.
func unitRebases(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []UnitRebase {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var out []UnitRebase
	for _, d := range docs {
		if d.ID == rebaseDocument(unit) {
			var r UnitRebase
			must(t, json.Unmarshal([]byte(d.Content), &r))
			out = append(out, r)
		}
	}
	return out
}

// latestReport returns the latest revision of the unit's report.json.
func latestReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReport) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reportDocument(unit) {
			latest = d
		}
	}
	var report UnitReport
	if latest.Revision != 0 {
		must(t, json.Unmarshal([]byte(latest.Content), &report))
	}
	return latest, report
}

// completeMasonTurn runs the next queued turn of the unit's mason as a
// finished turn that reported the unit done.
func completeMasonTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, criterion CriterionReport) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	agent := masonAgent(unit)
	th, err := repository.Thread(stream, agent)
	must(t, err)
	token := "done-" + th.Turns[len(th.Turns)-1].Request.TurnID
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.clock.Now())
	must(t, err)
	content, err := json.Marshal(MasonReport{Outcome: "Built", Criteria: []CriterionReport{criterion}})
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: directory, StartedAt: q.Claim.At, Outcome: &coreadapter.Outcome{Status: masonDone, Report: string(content), Card: &exampleCard}}}
	must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
	return q
}

// A mason turn a stop interrupted holds its unit's workspace: after a landing
// moves the feature branch, the unit is not rebased and nothing lands until
// the mason controller copies the turn's view back. The continuation it
// queues is held while the workspace is behind. The rebase then keeps the
// recovered work and the landed commit, and the continuation is admitted. A
// rebase applied again changes nothing.
func TestRebaseWaitsForAnInterruptedWriterAndKeepsItsWork(t *testing.T) {
	t.Parallel()
	f, stream, repository := newRebaseFixture(t, "writer")
	defer func() { repository.Close() }()
	ctx := context.Background()
	m := newMasonController(f.s, repository)
	lands := &foreman{masons: m}
	units := newUnitWorkspaces(f.s.cfg)
	w, base, found, err := units.find(ctx, stream, "dedupe")
	must(t, err)
	if !found {
		t.Fatal("dedupe has no workspace")
	}
	paths, err := units.paths(w)
	must(t, err)
	viewRoot := filepath.Join(f.s.cfg.Root.String(), "views", string(f.project), string(stream), masonAgent("dedupe"), masonTurnID("dedupe"))
	must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "DEDUPE.md"), []byte("dedupe\n"), 0600))
	_, err = repository.ClaimTurn(ctx, stream, masonAgent("dedupe"), "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), masonAgent("dedupe"), masonTurnID("dedupe")), f.clock.Now())
	must(t, err)
	landed := moveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})

	must(t, lands.Pass(ctx))
	if ops := rebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("a workspace with an interrupted writer was rebased: %+v", ops)
	}
	if ops := rebaseOperations(t, repository, stream, "resume"); len(ops) != 1 {
		t.Fatalf("resume, whose mason is idle, has rebases %+v", ops)
	}
	if ops := landOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for while units are behind: %+v", ops)
	}
	if head := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", w.Path, "rev-parse", "HEAD")); head != base {
		t.Fatalf("the writer's workspace moved to %s", head)
	}

	// A restart finds the claimed turn interrupted with its view.
	must(t, repository.Close())
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	m.repository = repository
	must(t, lands.Pass(ctx))
	if ops := rebaseOperations(t, repository, stream, "dedupe"); len(ops) != 0 {
		t.Fatalf("a workspace with an interrupted writer was rebased: %+v", ops)
	}
	must(t, m.Pass(ctx))
	must(t, m.Pass(ctx))
	th, err := repository.Thread(stream, masonAgent("dedupe"))
	must(t, err)
	continuation := th.Turns[len(th.Turns)-1]
	if continuation.Request.TurnID != masonAgent("dedupe")+"-recover-1" || !continuation.CompletedAt.IsZero() {
		t.Fatalf("the interrupted turn's continuation %+v", continuation)
	}
	admit := f.s.admit(f.s.current(), repository)
	candidate := scheduler.Candidate{Workstream: stream, Thread: th, Turn: continuation}
	if admitted, err := admit(ctx, candidate); err != nil || admitted {
		t.Fatalf("a mason turn of a unit behind its feature branch was admitted: %t %v", admitted, err)
	}

	must(t, lands.Pass(ctx))
	ops := rebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}
	in, err := decodeRebase(ops[0].Operation)
	must(t, err)
	if in != (rebaseInput{Unit: "dedupe", Rebase: 1, Onto: landed}) {
		t.Fatalf("rebase input %+v", in)
	}
	r := rebaser{lands}
	result, err := r.Apply(ctx, ops[0].Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("the rebase %+v", result)
	}
	for name, want := range map[string]string{"DEDUPE.md": "dedupe\n", "LANDED.md": "landed\n"} {
		if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
			t.Fatalf("the rebased workspace's %s holds %q, %v", name, data, err)
		}
	}
	if status := demoGit(t, filepath.Dir(f.clone), "-C", w.Path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("the rebased workspace:\n%s", status)
	}
	head := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", w.Path, "rev-parse", "HEAD"))
	if parent := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%P", head)); parent != landed {
		t.Fatalf("the rebased commit's parents %s, want %s", parent, landed)
	}
	rebases := unitRebases(t, repository, stream, "dedupe")
	if len(rebases) != 1 {
		t.Fatalf("rebase records %+v", rebases)
	}
	got := rebases[0]
	snapshot := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "show", got.Snapshot+":DEDUPE.md"))
	want := UnitRebase{Unit: "dedupe", Rebase: 1, Operation: ops[0].Operation.ID, State: UnitImplementing, Branch: unitBranch(stream, "dedupe"), Base: base, Onto: landed, Snapshot: got.Snapshot, Commit: head, Conflicts: []string{}}
	if !reflect.DeepEqual(got, want) || snapshot != "dedupe" {
		t.Fatalf("rebase.json %+v, want %+v; snapshot holds %q", got, want, snapshot)
	}
	if state, err := repository.Workflow(stream, rebaseSubject("dedupe")); err != nil || state.Value != "rebased-1" {
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
	if n := len(unitRebases(t, repository, stream, "dedupe")); n != 1 {
		t.Fatalf("%d rebase records after the rebase applied again", n)
	}
}

// A rebase interrupted after its snapshot, after the workspace moved or after
// its record is reconciled on retry: the unit branch holds one rebased commit
// on the new tip with the workspace's work, the workspace is clean and the
// rebase is recorded once.
func TestInterruptedRebaseIsReconciled(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"rebase-snapshotted", "rebase-moved", "rebase-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newRebaseFixture(t, "interrupted-rebase")
			defer repository.Close()
			ctx := context.Background()
			lands := &foreman{masons: newMasonController(f.s, repository)}
			w, _, _, err := newUnitWorkspaces(f.s.cfg).find(ctx, stream, "resume")
			must(t, err)
			must(t, os.WriteFile(filepath.Join(w.Path, "RESUME.md"), []byte("resume\n"), 0600))
			landed := moveFeature(t, f, stream, map[string]string{"LANDED.md": "landed\n"})
			must(t, lands.Pass(ctx))
			ops := rebaseOperations(t, repository, stream, "resume")
			if len(ops) != 1 {
				t.Fatalf("resume's rebases %+v", ops)
			}
			crashed := false
			f.s.boundary = func(name string) error {
				if name == step && !crashed {
					crashed = true
					return errors.New("crash")
				}
				return nil
			}
			r := rebaser{lands}
			if _, err := r.Apply(ctx, ops[0].Operation); err == nil || !strings.Contains(err.Error(), "crash") {
				t.Fatalf("the rebase never reached %s: %v", step, err)
			}
			if observed, err := r.Inspect(ctx, ops[0].Operation); err != nil || (step == "rebase-recorded") != (observed.State == coreadapter.EffectCompleted) {
				t.Fatalf("inspection after the crash %+v %v", observed, err)
			}
			result, err := r.Apply(ctx, ops[0].Operation)
			if err != nil || result.Outcome != "succeeded" {
				t.Fatalf("the retried rebase %+v %v", result, err)
			}
			home := filepath.Dir(f.clone)
			if count := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-list", "--count", landed+".."+unitBranch(stream, "resume"))); count != "1" {
				t.Fatalf("the unit branch gained %s commits on %s", count, landed)
			}
			for name, want := range map[string]string{"RESUME.md": "resume\n", "LANDED.md": "landed\n"} {
				if data, err := os.ReadFile(filepath.Join(w.Path, name)); err != nil || string(data) != want {
					t.Fatalf("the rebased workspace's %s holds %q, %v", name, data, err)
				}
			}
			if status := strings.TrimSpace(demoGit(t, home, "-C", w.Path, "status", "--porcelain")); status != "" {
				t.Fatalf("the rebased workspace:\n%s", status)
			}
			rebases := unitRebases(t, repository, stream, "resume")
			head := strings.TrimSpace(demoGit(t, home, "-C", w.Path, "rev-parse", "HEAD"))
			if len(rebases) != 1 || rebases[0].Commit != head || rebases[0].Onto != landed {
				t.Fatalf("rebase records %+v, head %s", rebases, head)
			}
			var outcomes []string
			for _, tr := range allTransitions(t, f.trace, stream) {
				if tr.Subject == rebaseSubject("resume") && tr.From != "" {
					outcomes = append(outcomes, tr.ID)
				}
			}
			if !slices.Equal(outcomes, []string{rebaseSubject("resume") + "-1-rebased"}) {
				t.Fatalf("rebase outcomes %v", outcomes)
			}
		})
	}
}

// A rebase that conflicts with the landed commit leaves conflict markers in
// the unit's workspace and records the conflicted paths; the approval no
// longer holds and nothing lands. The unit returns to implementing, and its
// mason gets a turn with the conflicted files and the sealed spec. A done
// that leaves the markers keeps the unit implementing and names them again;
// once they are resolved the unit returns to review with a candidate on the
// new tip whose diff carries no marker.
func TestRebaseConflictGoesToTheMasonAndNeverToTheReviewer(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "conflict")
	defer repository.Close()
	ctx := context.Background()
	m := newMasonController(f.s, repository)
	lands := &foreman{masons: m}
	landed := moveFeature(t, f, stream, map[string]string{masonWrote: "package trace\n\n// landed\n"})
	_, before := latestReport(t, repository, stream, "dedupe")

	must(t, lands.Pass(ctx))
	ops := rebaseOperations(t, repository, stream, "dedupe")
	if len(ops) != 1 {
		t.Fatalf("dedupe's rebases %+v", ops)
	}
	result, err := rebaser{lands}.Apply(ctx, ops[0].Operation)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the conflicted rebase %+v %v", result, err)
	}
	rebases := unitRebases(t, repository, stream, "dedupe")
	if len(rebases) != 1 || !slices.Equal(rebases[0].Conflicts, []string{masonWrote}) || rebases[0].State != UnitApproved || rebases[0].Snapshot != before.Candidate {
		t.Fatalf("rebase records %+v", rebases)
	}
	w, _, _, err := newUnitWorkspaces(f.s.cfg).find(ctx, stream, "dedupe")
	must(t, err)
	data, err := os.ReadFile(filepath.Join(w.Path, masonWrote))
	must(t, err)
	if !strings.Contains(string(data), "\n<<<<<<< "+landed+"\n") || !strings.Contains(string(data), "\n>>>>>>> "+before.Candidate+"\n") {
		t.Fatalf("the conflicted file holds %q", data)
	}
	if _, report := latestReport(t, repository, stream, "dedupe"); !reflect.DeepEqual(report, before) {
		t.Fatalf("a conflicted rebase changed the report to %+v", report)
	}
	transition, _ := rebaseIDs("dedupe", 1)
	outbox, err := repository.Outbox(stream)
	must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == transition+"-conflicted" && e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, "Unit dedupe conflicts with feature branch") && strings.Contains(e.Event.Body, masonWrote)
	}) {
		t.Fatal("the chief of staff was not told of the conflict")
	}

	must(t, lands.Pass(ctx))
	if ops := landOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("a landing was asked for with a conflicted unit: %+v", ops)
	}
	state, err := repository.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	if state.Value != UnitImplementing {
		t.Fatalf("the conflicted unit is %s", state.Value)
	}
	moved := slices.IndexFunc(allTransitions(t, f.trace, stream), func(tr trace.Transition) bool {
		return tr.ID == trace.UnitSubject("dedupe")+"-implementing-rebase-1" && tr.From == UnitApproved && tr.Cause == transition+"-conflicted" && tr.Actor == foremanActor
	})
	if moved < 0 {
		t.Fatal("the unit's return to implementing is not recorded")
	}
	th, err := repository.Thread(stream, masonAgent("dedupe"))
	must(t, err)
	resolve := th.Turns[len(th.Turns)-1]
	for _, want := range []string{"- " + masonWrote + "\n", `"<<<<<<< ` + landed + `"`, `">>>>>>> ` + before.Candidate + `"`, "2. Acknowledged chunks are never sent again."} {
		if resolve.Request.TurnID != resolveTurnID("dedupe", 1) || !strings.Contains(resolve.Request.Prompt, want) {
			t.Fatalf("the resolve turn %s lacks %q:\n%s", resolve.Request.TurnID, want, resolve.Request.Prompt)
		}
	}
	must(t, lands.Pass(ctx))
	if again, err := repository.Thread(stream, masonAgent("dedupe")); err != nil || len(again.Turns) != len(th.Turns) {
		t.Fatalf("another pass queued more turns: %+v %v", again, err)
	}

	done := completeMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != UnitImplementing {
		t.Fatalf("a candidate with conflict markers left implementing: %+v %v", state, err)
	}
	if doc, _ := latestReport(t, repository, stream, "dedupe"); doc.Revision != rebases[0].Report {
		t.Fatalf("a candidate with conflict markers was reported as revision %d", doc.Revision)
	}
	th, err = repository.Thread(stream, masonAgent("dedupe"))
	must(t, err)
	remind := th.Turns[len(th.Turns)-1]
	if remind.Request.TurnID != fmt.Sprintf("%s-markers-%d", masonAgent("dedupe"), done.Sequence) || !strings.Contains(remind.Request.Prompt, "still carry conflict markers") || !strings.Contains(remind.Request.Prompt, masonWrote) {
		t.Fatalf("the reminder %s:\n%s", remind.Request.TurnID, remind.Request.Prompt)
	}

	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n\n// landed and deduplicated\n"), 0600))
	completeMasonTurn(t, f, repository, stream, "dedupe", dedupeReport)
	must(t, m.Pass(ctx))
	if state, err := repository.Workflow(stream, trace.UnitSubject("dedupe")); err != nil || state.Value != UnitReviewing {
		t.Fatalf("the resolved unit is %+v %v", state, err)
	}
	_, report := latestReport(t, repository, stream, "dedupe")
	if report.Base != landed || report.Candidate == before.Candidate {
		t.Fatalf("the resolved report %+v, want a new candidate from %s", report, landed)
	}
	req, _, err := m.candidateEvidence(ctx, stream, "dedupe")
	must(t, err)
	if strings.Contains(req.Diff, "<<<<<<<") || strings.Contains(req.Diff, ">>>>>>>") || !strings.Contains(req.Diff, "+// landed and deduplicated") {
		t.Fatalf("the reviewer's diff:\n%s", req.Diff)
	}
	if paths, _, err := m.conflicts(stream, "dedupe"); err != nil || len(paths) != 0 {
		t.Fatalf("resolved conflicts %v %v", paths, err)
	}
}
