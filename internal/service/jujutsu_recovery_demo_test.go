package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// recoveryPlan has resume, and dedupe and audit, which depend on it and are
// built side by side in internal.trace and internal.audit.
const recoveryPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": ["resume"], "footprint": ["internal.audit"]}
]}
`

// recoveryFiles are the files dedupe's and audit's masons add, which
// upstream adds too, and recoveryUpstream what upstream commits: its own
// version of each and a change of trackedFile, which resume's landing
// changes as well.
var (
	recoveryFiles = map[string]string{
		"dedupe": "internal/trace/dedupe.go",
		"audit":  "internal/audit/audit.go",
	}
	recoveryUpstream = map[string]string{
		trackedFile:             "package trace\n\n// upstream changed git\n",
		recoveryFiles["dedupe"]: "package trace\n\n// upstream dedupe\n",
		recoveryFiles["audit"]:  "package audit\n\n// upstream audit\n",
	}
	recoveryReports = map[string]CriterionReport{"resume": resumeReport, "dedupe": dedupeReport, "audit": auditReport}
)

// recoveryDemo plays the agents of TestM6JujutsuRecoveryDemonstration and
// records what they saw.
type recoveryDemo struct {
	units   *landingDemo
	project config.ProjectID
	// owner returns the client of the running service.
	owner func() *Client
	// entered closes once resume's first mason turn wrote its edits.
	entered chan struct{}
	// ask closes when dedupe's first reviewer may ask for a drift rebase.
	ask chan struct{}

	mu           sync.Mutex
	asking       bool
	asked        *ProjectRebaseResponse
	continuation string
	sawEdits     bool
	// marked holds what each conflict resolving turn found in its view.
	marked   map[string]string
	problems []string
}

func (d *recoveryDemo) problem(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.problems = append(d.problems, fmt.Sprintf(format, args...))
}

func (d *recoveryDemo) check(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.problems) != 0 {
		t.Fatal(strings.Join(d.problems, "\n"))
	}
}

func recoveryResult(req agent.Request, text string) *agent.Result {
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}
}

// interrupted plays resume's first mason turn: it writes interruptedEdits
// into its view and works until it is stopped.
func (d *recoveryDemo) interrupted(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	if err := writeEdits(req.Workspace.Directory()); err != nil {
		return nil, err
	}
	close(d.entered)
	<-ctx.Done()
	return &agent.Result{ClaudeID: "session-stopped", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
}

// continued plays the continuation of resume's stopped turn: it notes its
// prompt and whether its view holds the stopped turn's edits, and reports
// done.
func (d *recoveryDemo) continued(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	d.continuation, d.sawEdits = req.Prompt, holdsEdits(inDirectory(req.Workspace.Directory()))
	d.mu.Unlock()
	if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Resumed from the kept edits", "criteria": []any{criterionArgs(resumeReport)}}); err != nil || !recorded {
		return nil, fmt.Errorf("done refused: %q %v", reason, err)
	}
	return recoveryResult(req, "Continued"), nil
}

// build plays the first mason turn of dedupe or audit: it adds the unit's
// file. dedupe's mason reports done; audit's leaves its work unfinished.
func (d *recoveryDemo) build(unit string) func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		path := filepath.Join(req.Workspace.Directory(), recoveryFiles[unit])
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf("package %s\n\n// %s\n", filepath.Base(filepath.Dir(path)), unit)), 0644); err != nil {
			return nil, err
		}
		if unit == "audit" {
			return recoveryResult(req, "Half built"), nil
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built " + unit, "criteria": []any{criterionArgs(recoveryReports[unit])}}); err != nil || !recorded {
			return nil, fmt.Errorf("done refused: %q %v", reason, err)
		}
		return recoveryResult(req, "Built"), nil
	}
}

// clarified plays a follow-up of audit's unfinished turn, which fails, so
// audit stays implementing with its work in its workspace.
func clarified(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reported", SessionDir: req.SessionDir, NumTurns: 1, HasOutcome: true, Outcome: agent.Outcome{Status: "shipped"}}, nil
}

// resolve plays a turn that resolves the conflict in path: it notes what its
// view holds there, writes resolved and reports done with args, through the
// unit mason's report when args carry criteria.
func (d *recoveryDemo) resolve(path, resolved string, args map[string]any) func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		file := filepath.Join(req.Workspace.Directory(), path)
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.marked[req.Name] = string(data)
		d.mu.Unlock()
		if err := os.WriteFile(file, []byte(resolved), 0644); err != nil {
			return nil, err
		}
		if _, unit := args["criteria"]; unit {
			if recorded, reason, err := done(ctx, tools, args); err != nil || !recorded {
				return nil, fmt.Errorf("done of %s refused: %q %v", req.Name, reason, err)
			}
		} else if body, err := callTool(ctx, tools, doneTool, args); err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("done of %s: %s %v", req.Name, body, err)
		}
		return recoveryResult(req, "Resolved"), nil
	}
}

// reviewDrift plays the drift reviewer, which approves the resolution.
func reviewDrift(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	evidence := []ReviewEvidence{{Criterion: "spec#1", Evidence: "The resolution keeps resume's change on upstream's"}, {Criterion: "spec#2", Evidence: "Nothing of the feature branch's change was lost"}}
	body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": evidence, "findings": []ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift verdict %s: %v", body, err)
	}
	return recoveryResult(req, "Reviewed"), nil
}

// review plays unit review turns, which approve the candidate they name.
// dedupe's first reviewer waits for ask and asks for a drift rebase of the
// project before it approves, as the owner would through osmia project
// rebase.
func (d *recoveryDemo) review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	first := unit == "dedupe" && !d.asking
	d.asking = d.asking || first
	d.mu.Unlock()
	if first {
		select {
		case <-d.ask:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		asked, err := d.owner().RebaseProject(ctx, d.project)
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

// TestM6JujutsuRecoveryDemonstration takes a workstream on Jujutsu workspaces
// through a mason turn a hard pause stops, a service restart in the middle
// of a landing, and upstream drift that conflicts with the feature branch
// and with both units in flight, to an owner-approved pull request on the
// fork. See docs/m6-jujutsu-recovery.md.
func TestM6JujutsuRecoveryDemonstration(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, _ := newParallelMasonFixtureOn(t, config.WorkspacesJujutsu, 2, 3, recoveryPlan)
	defer func() { f.stop(t) }()
	f.stop(t)

	// Delivery goes to a local bare fork and a fake pull request host.
	var stream config.WorkstreamID
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}
	prs := &fakePulls{fork: forkBranch}
	f.opts.PullRequests = prs

	demo := &recoveryDemo{units: &landingDemo{reviews: map[string][]UnitReviewIdentity{}}, project: f.project, owner: func() *Client { return f.c },
		entered: make(chan struct{}), ask: make(chan struct{}), marked: map[string]string{}}
	const (
		resolvedGit    = "package trace\n\n// changed before the interruption, on upstream's change\n"
		resolvedDedupe = "package trace\n\n// dedupe, beside upstream's\n"
		resolvedAudit  = "package audit\n\n// audit, beside upstream's\n"
	)
	resolutions := map[string]string{"dedupe": resolvedDedupe, "audit": resolvedAudit}
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = demo.interrupted
	f.engine.turns[masonAgent("resume")+"-recover-1"] = demo.continued
	for unit, resolved := range resolutions {
		f.engine.turns[masonTurnID(unit)] = demo.build(unit)
		f.engine.turns[resolveTurnID(unit, 1)] = demo.resolve(recoveryFiles[unit], resolved, map[string]any{"outcome": "Kept upstream's file beside " + unit, "criteria": []any{criterionArgs(recoveryReports[unit])}})
	}
	for i := 1; i <= 3; i++ {
		f.engine.turns[fmt.Sprintf("%s-clarify-%d", masonAgent("audit"), i)] = clarified
	}
	f.engine.turns[driftResolveTurnID(1, 1)] = demo.resolve(trackedFile, resolvedGit, map[string]any{"outcome": "Kept resume's change on upstream's"})
	f.engine.turns[driftReviewTurnID(1, 1)] = reviewDrift
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range recoveryReports {
			if strings.HasPrefix(req.Name, reviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		if strings.HasPrefix(req.Name, "final-") {
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "criteria": []any{
				map[string]any{"criterion": "spec#1", "evidence": "resume landing and TestResume"},
				map[string]any{"criterion": "spec#2", "evidence": "dedupe and audit landings"},
			}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				demo.problem("final report %s: %s: %v", req.Name, body, err)
			}
			return recoveryResult(req, "Reviewed"), nil
		}
		return chief(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	f.start(t)

	// The first landing's service dies once the feature branch moved,
	// before the landing is recorded.
	s := f.s
	crashing := make(chan struct{})
	var crashed atomic.Bool
	s.boundary = func(step string) error {
		if step == "land-advanced" && crashed.CompareAndSwap(false, true) {
			close(crashing)
			<-s.lifetime.Done()
			return errors.New("the service died in the middle of the landing")
		}
		return nil
	}

	// 1. The workstream is built on Jujutsu workspaces. A hard pause stops
	// resume's first mason turn after it changed trackedFile and added a
	// file; its edits stay in the unit's workspace.
	stream, _ = f.builtAs(t, "jujutsu-recovery")
	if got := backendOf(t, f, stream); got != config.WorkspacesJujutsu {
		t.Fatalf("the workstream is on %s", got)
	}
	base := featureTip(t, f, stream)
	select {
	case <-demo.entered:
	case <-time.After(demoTimeout):
		t.Fatal("resume's mason never started")
	}
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	checkPauseStop(t, f.awaitCompleted(t, stream, masonAgent("resume"), masonTurnID("resume")), "workstream", "Stop the mason")
	settle()
	if !holdsEdits(inDirectory(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume"))) {
		t.Fatal("the stopped turn's edits are not in resume's workspace")
	}
	mutation(t, f.c, "DELETE", "pause", target)

	// 2. The continuation is told which files the stopped turn changed and
	// finds them. resume is reviewed and approved, and its landing is cut
	// short with the feature branch moved and the landing not recorded.
	select {
	case <-crashing:
	case <-time.After(demoTimeout):
		t.Fatal("resume never reached its landing")
	}
	f.stop(t)
	moved := f.landedCommits(t, stream, base)
	if len(moved) != 1 || !holdsEdits(inCommit(f, moved[0])) {
		t.Fatalf("the cut-short landing left the feature branch with %v", moved)
	}
	entry, changed := interrupted(t, f, branchesDirectory)
	if !changed {
		t.Fatal("the cut-short landing changed nothing in the feature workspaces' operation log")
	}

	// 3. The restarted service restores the feature workspaces to the
	// checkpoint taken before the landing and retries it, which finds the
	// commit it made: resume lands once.
	f.start(t)
	f.awaitMerged(t, stream, "resume")
	if log := operationLog(t, f, branchesDirectory); !slices.ContainsFunc(log, func(line string) bool { return strings.HasSuffix(line, " restore to operation "+entry) }) {
		t.Fatalf("the feature workspaces were not restored to %s:\n%s", entry, strings.Join(log, "\n"))
	}
	if got := f.landedCommits(t, stream, base); !slices.Equal(got, moved) {
		t.Fatalf("after the restart the feature branch holds %v, not the one landing %v", got, moved)
	}
	var resume UnitLanding
	must(t, json.Unmarshal([]byte(landingByUnit(t, f.repository(), stream)["resume"].Content), &resume))
	if resume.Commit != moved[0] || resume.Base != base {
		t.Fatalf("resume's landing records %+v, want commit %s on %s", resume, moved[0], base)
	}
	demo.mu.Lock()
	continuation, sawEdits := demo.continuation, demo.sawEdits
	demo.mu.Unlock()
	want := "A hard pause stopped your last turn. " + strings.Replace(keptOnJujutsu, "%s", "that turn", 1) + " Continue from those files"
	if !strings.Contains(continuation, want) || !sawEdits {
		t.Fatalf("the continuation saw the edits %t with prompt %q, want %q", sawEdits, continuation, want)
	}

	// 4. dedupe is built on resume's landing and waits for review; audit's
	// mason left its work unfinished. Upstream adds each of their files and
	// changes trackedFile, and dedupe's reviewer asks for a drift rebase
	// before it approves dedupe.
	f.awaitUnit(t, stream, "dedupe", UnitReviewing)
	f.awaitMasonSettled(t, stream, "audit", 2)
	from := seals(t, f.repository(), stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, recoveryUpstream)
	close(demo.ask)

	// 5. The drift rebase replays the feature branch onto upstream with a
	// conflict in trackedFile, which the drift mason resolves and the drift
	// reviewer approves. Carrying the units onto the resolved branch stores
	// a conflict in each, which its mason resolves; each is reviewed again
	// and lands.
	f.awaitMerged(t, stream, "dedupe")
	f.awaitMerged(t, stream, "audit")
	demo.check(t)
	repository := f.repository()
	demo.mu.Lock()
	asked, marked := demo.asked, demo.marked
	demo.mu.Unlock()
	if asked == nil || len(asked.Covered) != 1 || asked.Covered[0] != (DriftCoverage{Workstream: stream, Drift: 1}) {
		t.Fatalf("the owner's drift rebase request was answered %+v", asked)
	}
	if conflicted := transitionByID(t, repository, stream, "drift-1-conflicted"); !strings.Contains(conflicted.Reason, trackedFile+" conflicted") {
		t.Fatalf("the feature branch's conflict %+v", conflicted)
	}
	for turn, path := range map[string]string{driftResolveTurnID(1, 1): trackedFile, resolveTurnID("dedupe", 1): recoveryFiles["dedupe"], resolveTurnID("audit", 1): recoveryFiles["audit"]} {
		if !strings.Contains(marked[turn], "<<<<<<< ") || !strings.Contains(marked[turn], ">>>>>>> ") {
			t.Fatalf("turn %s found %s as %q", turn, path, marked[turn])
		}
	}
	var resolved string
	for _, r := range driftRecords(t, repository, stream) {
		if r.Outcome == driftRebased {
			resolved = r.Commit
		}
	}
	if resolved == "" || parentOf(t, f, resolved) != upstream || fileAt(t, f, resolved, trackedFile) != resolvedGit {
		t.Fatalf("the drift rebase moved the feature branch to %q, not resume's landing resolved onto %s", resolved, upstream)
	}
	if all := seals(t, repository, stream); len(all) != 2 || all[0].Base.Commit != from || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}

	g := providerOf(t, newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces, stream)
	landings := landingByUnit(t, repository, stream)
	for unit, state := range map[string]string{"dedupe": UnitApproved, "audit": UnitImplementing} {
		// The carry stored the unit's conflict in its rebased commit: dedupe
		// was approved on resume's landing, audit still implementing.
		rebases := unitRebases(t, repository, stream, unit)
		if len(rebases) == 0 || rebases[0].State != state || rebases[0].Base != resume.Commit || rebases[0].Onto != resolved || !slices.Equal(rebases[0].Conflicts, []string{recoveryFiles[unit]}) {
			t.Fatalf("unit %s's rebases %+v", unit, rebases)
		}
		if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || !slices.Equal(stored, []string{recoveryFiles[unit]}) {
			t.Fatalf("unit %s's rebased commit %s stores %v: %v", unit, rebases[0].Commit, stored, err)
		}
		th := f.thread(t, stream, masonAgent(unit))
		i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == resolveTurnID(unit, 1) })
		if i < 0 || !strings.Contains(th.Turns[i].Request.Prompt, "- "+recoveryFiles[unit]+"\n") {
			t.Fatalf("unit %s's mason got no turn resolving %s", unit, recoveryFiles[unit])
		}
		if unit == "dedupe" {
			back := transitionByID(t, repository, stream, trace.UnitSubject(unit)+"-"+UnitImplementing+"-rebase-1")
			if back.From != UnitApproved || back.To != UnitImplementing {
				t.Fatalf("dedupe's return to its mason %+v", back)
			}
		}

		// The trace follows the unit by its change from its first snapshot
		// through every rebase, and names it for each candidate reviewed.
		change, err := unitChange(repository, stream, unit)
		must(t, err)
		if change == "" {
			t.Fatalf("unit %s records no change", unit)
		}
		reviews := demo.units.reviewed(unit)
		walk := func(commit, role string) {
			t.Helper()
			walked, api := f.s.traceView(ctx, string(stream), "commit", commit)
			if api != nil {
				t.Fatalf("the walk of unit %s's commit %s: %+v", unit, commit, api)
			}
			if !slices.ContainsFunc(walked.(CommitTrace).Records, func(r CommitRecord) bool { return r.Unit == unit && (role == "" || r.Role == role) }) {
				t.Fatalf("the walk of unit %s's commit %s records %+v", unit, commit, walked.(CommitTrace).Records)
			}
		}
		walk(rebases[0].Snapshot, commitChange)
		for _, r := range rebases {
			if got, err := g.ChangeOf(ctx, r.Commit); err != nil || got != change {
				t.Fatalf("unit %s's rebase %d made %s of change %q, %v; want %s", unit, r.Rebase, r.Commit, got, err, change)
			}
			walk(r.Commit, commitChange)
		}
		for _, reviewed := range reviews {
			walk(reviewed.Candidate.Revision, "")
		}

		// No approval crossed a rebase: every rebase was reviewed again on
		// its new base, and the unit landed the exact candidate and base its
		// last review approved.
		for _, r := range rebases {
			if !slices.ContainsFunc(reviews, func(id UnitReviewIdentity) bool { return id.Candidate.BaseRevision == r.Onto }) {
				t.Fatalf("unit %s was never reviewed on %s after rebase %d; reviews %+v", unit, r.Onto, r.Rebase, reviews)
			}
		}
		var landed UnitLanding
		must(t, json.Unmarshal([]byte(landings[unit].Content), &landed))
		last := reviews[len(reviews)-1].Candidate
		if landed.Candidate != last.Revision || landed.Base != last.BaseRevision || slices.ContainsFunc(rebases, func(r UnitRebase) bool { return r.Snapshot == landed.Candidate }) {
			t.Fatalf("unit %s landed %+v; its last review read %+v", unit, landed, last)
		}
		if fileAt(t, f, landed.Commit, recoveryFiles[unit]) != resolutions[unit] {
			t.Fatalf("unit %s's landing lost its mason's resolution", unit)
		}
	}
	if first := demo.units.reviewed("dedupe")[0].Candidate; first.BaseRevision != resume.Commit {
		t.Fatalf("dedupe was first approved on %s, not resume's landing %s", first.BaseRevision, resume.Commit)
	}

	// 6. Final review presents the delivery; the owner approves it and the
	// service opens one pull request on the fork.
	f.awaitFeature(t, stream, AssembledState)
	var presented DeliveryPresentation
	var err error
	eventually(t, "final review never presented the delivery", func() bool {
		presented, err = f.c.Delivery(ctx, stream)
		return err == nil
	})
	_, err = f.c.ApproveDelivery(ctx, stream, DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	must(t, err)
	f.awaitFeature(t, stream, DeliveredState)
	demo.check(t)
	prs.mu.Lock()
	defer prs.mu.Unlock()
	if len(prs.prs) != 1 || prs.creates != 1 || prs.prs[0].Body != presented.Draft || prs.prs[0].HeadCommit != forkBranch() {
		t.Fatalf("pull requests %+v", prs.prs)
	}
	for name, content := range map[string]string{trackedFile: resolvedGit, recoveryFiles["dedupe"]: resolvedDedupe, recoveryFiles["audit"]: resolvedAudit, "internal/trace/kept.go": interruptedEdits["internal/trace/kept.go"]} {
		if got := fileAt(t, f, forkBranch(), name); got != content {
			t.Fatalf("the delivered branch holds %s as %q, want %q", name, got, content)
		}
	}
}
