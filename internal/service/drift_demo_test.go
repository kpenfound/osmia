package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// driftPlan has resume, and audit, which depends on it, so audit is built and
// reviewed on the feature branch resume landed on.
const driftPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`

// upstreamWrote is what upstream commits as masonWrote: a placeholder that
// conflicts with the file resume's landing adds. The drift mason resolves the
// conflict with the feature's file, resolvedWrote, which supersedes it.
const (
	upstreamWrote = "package trace\n\n// upstream reserves this file for resume\n"
	resolvedWrote = "package trace\n"
)

// cloneGit runs git in the fixture's home, as demoGit does, from a fake
// agent's turn, which must not fail the test itself.
func cloneGit(home string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// awaitDrift waits until the workstream's drift subject is want.
func (f *shedFixture) awaitDrift(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, driftSubject)
		must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drift of %s is %q, never %s", stream, state.Value, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitMasonSettled waits until the unit's mason thread has at least turns
// turns and none of them is unfinished.
func (f *shedFixture) awaitMasonSettled(t *testing.T, stream config.WorkstreamID, unit string, turns int) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, masonAgent(unit))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && len(th.Turns) >= turns && !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mason of unit %s of %s never settled: %+v", unit, stream, th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestM4DriftCadenceDemonstration keeps two building workstreams of one
// project current with upstream on the project's upstream_rebase cadence,
// through the local API with fake masons. See docs/m4-upstream-drift.md.
func TestM4DriftCadenceDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 2, independentPlan)
	defer func() { f.stop(t) }()

	// The owner sets the project's cadence to three hours.
	path, err := f.s.cfg.Root.ProjectConfig(f.project)
	must(t, err)
	configFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("upstream_rebase = \"3h\"\n")
	must(t, errors.Join(err, configFile.Close()))
	f.stop(t)
	f.start(t)
	if f.s.cfg.Project.RebaseInterval() != 3*time.Hour {
		t.Fatalf("upstream_rebase is %q", f.s.cfg.Project.UpstreamRebase)
	}

	// Each workstream starts resume, whose mason leaves its work in the
	// unit's workspace without reporting done, so the unit stays
	// implementing.
	streams := []config.WorkstreamID{}
	for _, key := range []string{"cadence-uploads", "cadence-audits"} {
		stream, _ := f.builtAs(t, key)
		streams = append(streams, stream)
	}
	sealed := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		f.awaitMasonSettled(t, stream, "resume", 2)
		sealed[stream] = seals(t, f.repository(), stream)[0].Base.Commit
		st, err := f.c.Status(ctx, stream)
		must(t, err)
		if st.Drift != nil {
			t.Fatalf("status of %s before any drift rebase: %+v", stream, st.Drift)
		}
	}
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})

	// Three hours of service time after the later sealing, both workstreams
	// are due. Each gets one drift rebase on the project's lander.
	latest := slices.MaxFunc([]time.Time{sealedAt(t, f.repository(), streams[0]), sealedAt(t, f.repository(), streams[1])}, time.Time.Compare)
	setClock(f, latest.Add(3*time.Hour))
	for _, stream := range streams {
		f.awaitDrift(t, stream, "rebased-1")
	}
	settle()
	masons.check(t)
	for _, stream := range streams {
		repository := f.repository()
		ops := checkDrifts(t, repository, stream, 1, "once the interval elapsed")
		if ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
			t.Fatalf("drift rebase of %s: %+v", stream, ops[0].Result)
		}
		asked := transitionByID(t, repository, stream, "drift-1")
		if want := "upstream_rebase 3h0m0s has elapsed since the sealing at " + sealedAt(t, repository, stream).Format(time.RFC3339) + ": drift rebase 1 fetches"; !strings.HasPrefix(asked.Reason, want) {
			t.Fatalf("drift rebase of %s asked for with %q", stream, asked.Reason)
		}

		// The feature branch is current with upstream and the seal's base
		// moved with it, keeping the seal number.
		tip := featureTip(t, f, stream)
		demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "merge-base", "--is-ancestor", upstream, tip)
		all := seals(t, repository, stream)
		if len(all) != 2 || all[0].Base.Commit != sealed[stream] || all[1].Base.Commit != upstream || all[1].Seal != all[0].Seal {
			t.Fatalf("seals of %s: %+v", stream, all)
		}

		// resume's workspace, with its mason's unfinished work, was carried
		// onto the rebased branch while the unit kept implementing.
		rebases := unitRebases(t, repository, stream, "resume")
		if len(rebases) != 1 || rebases[0].Onto != tip || rebases[0].State != UnitImplementing || len(rebases[0].Conflicts) != 0 {
			t.Fatalf("resume's rebases in %s: %+v", stream, rebases)
		}
		workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
		for name, want := range map[string]string{masonWrote: "package trace\n", "UPSTREAM.md": "upstream\n"} {
			if data, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || string(data) != want {
				t.Fatalf("resume's workspace of %s holds %s %q: %v", stream, name, data, err)
			}
		}
		if state, err := f.unitState(stream, "resume"); err != nil || state != UnitImplementing {
			t.Fatalf("resume of %s is %s: %v", stream, state, err)
		}
		records := driftRecords(t, repository, stream)
		var outcomes []string
		for _, r := range records {
			outcomes = append(outcomes, r.Outcome)
		}
		if want := []string{driftReplayed, driftCarrying, driftRebased}; !slices.Equal(outcomes, want) {
			t.Fatalf("drift/rebase.json of %s records %q, want %q", stream, outcomes, want)
		}

		// A clean drift rebase is quiet: the chief of staff hears nothing,
		// and status shows the drift rebase.
		if moved := upstreamMovedEvents(t, repository, stream); len(moved) != 0 {
			t.Fatalf("a clean drift rebase of %s raised %+v", stream, moved)
		}
		rebased := transitionByID(t, repository, stream, "drift-1-rebased")
		st, err := f.c.Status(ctx, stream)
		must(t, err)
		if want := (&DriftStatus{Drift: 1, Outcome: driftRebased, At: rebased.At, Reason: rebased.Reason, Moved: []string{}}); !reflect.DeepEqual(st.Drift, want) {
			t.Fatalf("status drift of %s %+v, want %+v", stream, st.Drift, want)
		}
	}
}

// driftDemo plays the unit reviewers, the drift mason and the drift reviewer
// of TestM4DriftConflictDemonstration, and records what they saw.
type driftDemo struct {
	units   *landingDemo
	owner   *Client
	project config.ProjectID
	clone   string

	mu       sync.Mutex
	stream   config.WorkstreamID
	asked    *ProjectRebaseResponse
	resolves int
	// resolving closes when the first resolve turn has written its
	// resolution.
	resolving chan struct{}
	marked    string
	recovered string
	// reviewedTip is the feature branch's tip while the drift reviewer read
	// the candidate, and reviewPrompt its prompt.
	reviewedTip, reviewPrompt string
	problems                  []string
}

func (d *driftDemo) problem(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.problems = append(d.problems, fmt.Sprintf(format, args...))
}

func (d *driftDemo) check(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.problems) != 0 {
		t.Fatal(strings.Join(d.problems, "\n"))
	}
}

// review plays unit review turns. audit's first reviewer asks for a drift
// rebase of the project before it approves, as the owner would through
// osmia project rebase.
func (d *driftDemo) review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	ask := unit == "audit" && d.asked == nil
	d.mu.Unlock()
	if ask {
		asked, err := d.owner.RebaseProject(ctx, d.project)
		if err != nil {
			d.problem("the owner's drift rebase request: %v", err)
		} else {
			d.mu.Lock()
			d.asked = &asked
			d.mu.Unlock()
		}
	}
	return d.units.review(ctx, unit, req, tools)
}

// resolve plays the drift mason's resolve turn: it replaces the conflict in
// its view with the resolution and is still working when the service stops.
func (d *driftDemo) resolve(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	path := filepath.Join(req.Workspace.Directory(), masonWrote)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.resolves++
	first := d.resolves == 1
	if first {
		d.marked = string(data)
	}
	d.mu.Unlock()
	if !first {
		d.problem("the resolve turn ran again")
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Ran again", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	if err := os.WriteFile(path, []byte(resolvedWrote), 0644); err != nil {
		return nil, err
	}
	close(d.resolving)
	<-ctx.Done()
	return nil, ctx.Err()
}

// recover plays the continuation of the interrupted resolve turn: it finds
// the resolution in its view and reports done.
func (d *driftDemo) recover(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	data, err := os.ReadFile(filepath.Join(req.Workspace.Directory(), masonWrote))
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.recovered = string(data)
	d.mu.Unlock()
	body, err := callTool(ctx, tools, doneTool, map[string]any{"outcome": "Kept the feature's file, which supersedes upstream's placeholder"})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift done %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Resolved", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// reviewDrift plays the drift reviewer: it notes where the feature branch
// is while it reads the candidate, and approves it.
func (d *driftDemo) reviewDrift(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	stream := d.stream
	d.mu.Unlock()
	tip, err := cloneGit(filepath.Dir(d.clone), "-C", d.clone, "rev-parse", featureBranch(stream))
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.reviewedTip, d.reviewPrompt = tip, req.Prompt
	d.mu.Unlock()
	evidence := []ReviewEvidence{{Criterion: "spec#1", Evidence: "The feature's file supersedes upstream's placeholder"}, {Criterion: "spec#2", Evidence: "Nothing of the feature branch's change was lost"}}
	body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": evidence, "findings": []ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift verdict %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// TestM4DriftConflictDemonstration asks for a drift rebase through the local
// API while an approved unit waits to land, resolves the feature branch's
// conflict with upstream through a drift mason across a restart and a drift
// reviewer, and sends the approval back to review before it lands. See
// docs/m4-upstream-drift.md.
func TestM4DriftConflictDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, driftPlan)
	defer func() { f.stop(t) }()
	auditReport := CriterionReport{Criterion: "spec#2", Done: "built audit", Evidence: "the planned proof holds", Proof: "reviewer judgement"}
	masons.play[masonTurnID("resume")] = reportDone("Uploads resume")
	masons.play[masonTurnID("audit")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal", "trace", "audit.go"), []byte("package trace\n// audit\n"), 0644); err != nil {
			return err
		}
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built audit", "criteria": []any{criterionArgs(auditReport)}})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	demo := &driftDemo{units: &landingDemo{reviews: map[string][]UnitReviewIdentity{}}, owner: f.c, project: f.project, clone: f.clone, resolving: make(chan struct{})}
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("audit")] = masons.turn
	f.engine.turns[driftResolveTurnID(1, 1)] = demo.resolve
	f.engine.turns[driftMasonAgent+"-recover-1"] = demo.recover
	f.engine.turns[driftReviewTurnID(1, 1)] = demo.reviewDrift
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, unit := range []string{"resume", "audit"} {
			if strings.HasPrefix(req.Name, reviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()

	stream, _ := f.builtAs(t, "drift-demo")
	demo.mu.Lock()
	demo.stream = stream
	demo.mu.Unlock()
	from := seals(t, f.repository(), stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, map[string]string{masonWrote: upstreamWrote})

	// resume lands the file upstream now holds too. audit is built on it,
	// and its reviewer asks for a drift rebase before approving it.
	f.awaitMerged(t, stream, "resume")
	var resume UnitLanding
	must(t, json.Unmarshal([]byte(landingByUnit(t, f.repository(), stream)["resume"].Content), &resume))
	select {
	case <-demo.resolving:
	case <-time.After(demoTimeout):
		t.Fatal("the drift mason never started resolving")
	}
	f.awaitUnit(t, stream, "audit", UnitApproved)
	demo.mu.Lock()
	asked := demo.asked
	demo.mu.Unlock()
	if want := (ProjectRebaseResponse{Project: f.project, Covered: []DriftCoverage{{Workstream: stream, Drift: 1}}, Skipped: []DriftSkip{}}); asked == nil || !reflect.DeepEqual(*asked, want) {
		t.Fatalf("the owner's request was answered %+v, want %+v", asked, want)
	}

	// The replay conflicts: the feature branch and the seal stay, the
	// operation holds the lander so approved audit does not land, and the
	// drift mason works on the conflict markers.
	repository := f.repository()
	if requested := transitionByID(t, repository, stream, "drift-1"); !strings.HasPrefix(requested.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("drift rebase 1 asked for with %q", requested.Reason)
	}
	if conflicted := transitionByID(t, repository, stream, "drift-1-conflicted"); conflicted.To != "conflicted-1" || !strings.Contains(conflicted.Reason, masonWrote+" conflicted") {
		t.Fatalf("the conflict %+v", conflicted)
	}
	if tip := featureTip(t, f, stream); tip != resume.Commit {
		t.Fatalf("the feature branch moved to %s while its conflict is open", tip)
	}
	if n := len(seals(t, repository, stream)); n != 1 {
		t.Fatalf("the seal has %d revisions while the conflict is open", n)
	}
	if ops := landOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("landings while the conflict is open %+v", ops)
	}
	demo.mu.Lock()
	marked := demo.marked
	demo.mu.Unlock()
	if !strings.Contains(marked, "<<<<<<< ") || !strings.Contains(marked, "// upstream reserves") {
		t.Fatalf("the drift mason's view holds %q", marked)
	}
	th := f.thread(t, stream, driftMasonAgent)
	for _, want := range []string{"- " + masonWrote + "\n", "conflict markers", "## Sealed spec: spec.md"} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}
	st, err := f.c.Status(ctx, stream)
	must(t, err)
	if st.Drift == nil || st.Drift.Drift != 1 || st.Drift.Outcome != driftConflicted || len(st.Drift.Moved) != 1 {
		t.Fatalf("status while the conflict is open %+v", st.Drift)
	}

	// The service stops while the drift mason is still working.
	f.stop(t)
	f.start(t)
	f.awaitMerged(t, stream, "audit")
	masons.check(t)
	demo.check(t)
	repository = f.repository()

	// The restart neither replayed the conflict again nor queued a second
	// resolve turn: the interrupted turn's view was copied back, and one
	// continuation reported done.
	demo.mu.Lock()
	resolves, recovered, reviewedTip, reviewPrompt := demo.resolves, demo.recovered, demo.reviewedTip, demo.reviewPrompt
	demo.mu.Unlock()
	if resolves != 1 || recovered != resolvedWrote {
		t.Fatalf("the resolve turn ran %d times; its continuation found %q", resolves, recovered)
	}
	th = f.thread(t, stream, driftMasonAgent)
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1), driftMasonAgent + "-recover-1"}) || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("drift mason turns %v, the first %s", ids, th.Turns[0].Status())
	}
	if ids := turnIDs(t, repository, stream, driftReviewerAgent); !slices.Equal(ids, []string{driftReviewTurnID(1, 1)}) {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	records := driftRecords(t, repository, stream)
	var outcomes []string
	for _, r := range records {
		outcomes = append(outcomes, r.Outcome)
	}
	if want := []string{driftConflicted, driftConflicted, driftResolved, driftReplayed, driftCarrying, driftRebased}; !slices.Equal(outcomes, want) {
		t.Fatalf("drift/rebase.json records %q, want %q", outcomes, want)
	}

	// The drift reviewer read the resolved candidate while the feature
	// branch still held resume's landing; only its approval moved the
	// branch and the seal's base.
	resolved := records[len(records)-1].Commit
	if reviewedTip != resume.Commit || !strings.Contains(reviewPrompt, resolved) || !strings.Contains(reviewPrompt, "## Sealed spec: spec.md") {
		t.Fatalf("the drift reviewer read %s with the feature branch at %s", resolved, reviewedTip)
	}
	if parentOf(t, f, resolved) != upstream || fileAt(t, f, resolved, masonWrote) != resolvedWrote {
		t.Fatalf("the approved candidate %s is not resume's landing resolved onto %s", resolved, upstream)
	}
	if all := seals(t, repository, stream); len(all) != 2 || all[0].Base.Commit != from || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}
	if _, found, err := driftWorkspaces(f.s.cfg).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}

	// audit's approval was of a candidate on resume's landing. The drift
	// rebase carried its workspace onto the resolved branch and returned it
	// to review; the rebased candidate was reviewed again and landed.
	rebases := unitRebases(t, repository, stream, "audit")
	if len(rebases) != 1 || rebases[0].State != UnitApproved || rebases[0].Onto != resolved || len(rebases[0].Conflicts) != 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	back := transitionByID(t, repository, stream, trace.UnitSubject("audit")+"-reviewing-rebase-1")
	if back.From != UnitApproved || !strings.HasPrefix(back.Reason, "the approval no longer holds: ") {
		t.Fatalf("audit's return to review %+v", back)
	}
	reviews := demo.units.reviewed("audit")
	if len(reviews) != 2 || reviews[0].Candidate.BaseRevision != resume.Commit || reviews[1].Candidate.BaseRevision != resolved || reviews[1].Candidate.Revision != rebases[0].Commit {
		t.Fatalf("audit's reviews %+v after rebase %+v", reviews, rebases[0])
	}
	var audit UnitLanding
	must(t, json.Unmarshal([]byte(landingByUnit(t, repository, stream)["audit"].Content), &audit))
	if audit.Base != resolved || audit.Candidate != reviews[1].Candidate.Revision || featureTip(t, f, stream) != audit.Commit {
		t.Fatalf("audit landed %+v on the branch at %s", audit, featureTip(t, f, stream))
	}
	rebased := transitionByID(t, repository, stream, "drift-1-rebased")
	for _, o := range landOperations(t, repository, stream) {
		if in, err := decodeLand(o.Operation); err == nil && in.Unit == "audit" && !o.Transition.At.After(rebased.At) {
			t.Fatalf("audit's landing was asked for at %s, before drift rebase 1 finished at %s", o.Transition.At, rebased.At)
		}
	}

	// The chief of staff heard about the conflict and the returned
	// approval, and status lists both.
	moved := upstreamMovedEvents(t, repository, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	if len(moved) != 2 || moved[0].TransitionID != "drift-1-conflicted" || moved[1].TransitionID != back.ID {
		t.Fatalf("upstream moved events %+v", moved)
	}
	checkMoved(t, moved[0], stream, move, "feature branch "+featureBranch(stream)+" conflicts with upstream in "+masonWrote)
	checkMoved(t, moved[1], stream, move, "unit audit's approval no longer holds")
	f.awaitEventTurns(t, stream, moved[0].Event.Body, moved[1].Event.Body)
	st, err = f.c.Status(ctx, stream)
	must(t, err)
	if want := (&DriftStatus{Drift: 1, Outcome: driftRebased, At: rebased.At, Reason: rebased.Reason, Moved: []string{moved[0].Event.Body, moved[1].Event.Body}}); !reflect.DeepEqual(st.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", st.Drift, want)
	}
}
