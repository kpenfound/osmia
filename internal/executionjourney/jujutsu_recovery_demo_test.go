package executionjourney

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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const recoveryPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": ["resume"], "footprint": ["internal.audit"]}
]}
`

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
	recoveryReports = map[string]string{"resume": resumeReport, "dedupe": dedupeReport, "audit": auditReport}
)

type recoveryDemo struct {
	units   *landingDemo
	project config.ProjectID

	owner func() *Client

	entered chan struct{}

	ask chan struct{}
	// auditReviewed holds dedupe until audit has been reviewed on the drift base.
	auditReviewed chan struct{}
	auditOnce     sync.Once

	mu           sync.Mutex
	asking       bool
	asked        *ProjectRebaseResponse
	continuation string
	sawEdits     bool

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

func (d *recoveryDemo) interrupted(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	if err := writeEdits(req.Workspace.Directory()); err != nil {
		return nil, err
	}
	close(d.entered)
	<-ctx.Done()
	return &agent.Result{ClaudeID: "session-stopped", ResultText: "Half built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
}

func (d *recoveryDemo) continued(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	d.continuation, d.sawEdits = req.Prompt, holdsEdits(inDirectory(req.Workspace.Directory()))
	d.mu.Unlock()
	if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Resumed from the kept edits"}); err != nil || !recorded {
		return nil, fmt.Errorf("done refused: %q %v", reason, err)
	}
	return recoveryResult(req, "Continued"), nil
}

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
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built " + unit}); err != nil || !recorded {
			return nil, fmt.Errorf("done refused: %q %v", reason, err)
		}
		return recoveryResult(req, "Built"), nil
	}
}

func clarified(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reported", SessionDir: req.SessionDir, NumTurns: 1, HasOutcome: true, Outcome: agent.Outcome{Status: "shipped"}}, nil
}

func (d *recoveryDemo) resolve(path, resolved string, unit bool, args map[string]any) func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
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
		if unit {
			if recorded, reason, err := done(ctx, tools, args); err != nil || !recorded {
				return nil, fmt.Errorf("done of %s refused: %q %v", req.Name, reason, err)
			}
		} else if body, err := callTool(ctx, tools, doneTool, args); err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("done of %s: %s %v", req.Name, body, err)
		}
		return recoveryResult(req, "Resolved"), nil
	}
}

func reviewDrift(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift verdict %s: %v", body, err)
	}
	return recoveryResult(req, "Reviewed"), nil
}

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
	if unit == "dedupe" && !first {
		select {
		case <-d.auditReviewed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	result, err := d.units.review(ctx, unit, req, tools)
	if unit == "audit" && err == nil {
		d.auditOnce.Do(func() { close(d.auditReviewed) })
	}
	return result, err
}

func TestJujutsuRecoveryDemonstration(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, _ := newParallelMasonFixtureOn(t, config.WorkspacesJujutsu, 2, 3, recoveryPlan)
	defer func() { f.stop(t) }()
	f.stop(t)

	var stream config.WorkstreamID
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}
	prs := &fakePulls{Fork: forkBranch}
	f.opts.PullRequests = prs
	var passed atomic.Int64
	f.opts.schedulePassed = func() { passed.Add(1) }

	demo := &recoveryDemo{units: &landingDemo{reviews: map[string][]UnitReviewIdentity{}}, project: f.project, owner: func() *Client { return f.c },
		entered: make(chan struct{}), ask: make(chan struct{}), auditReviewed: make(chan struct{}), marked: map[string]string{}}

	const (
		resolvedGit    = "package trace\n\n// changed before the interruption, on upstream's change\n"
		resolvedDedupe = "package trace\n\n// dedupe, beside upstream's\n"
		resolvedAudit  = "package audit\n\n// audit, beside upstream's\n"
	)
	resolutions := map[string]string{"dedupe": resolvedDedupe, "audit": resolvedAudit}
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("resume")] = demo.interrupted
	f.engine.Turns[masonAgent("resume")+"-recover-1"] = demo.continued
	for unit, resolved := range resolutions {
		f.engine.Turns[masonTurnID(unit)] = demo.build(unit)
		f.engine.Turns[resolveTurnID(unit, 1)] = demo.resolve(recoveryFiles[unit], resolved, true, map[string]any{"outcome": "Kept upstream's file beside " + unit})
	}
	for i := 1; i <= 3; i++ {
		f.engine.Turns[fmt.Sprintf("%s-clarify-%d", masonAgent("audit"), i)] = clarified
	}
	f.engine.Turns[driftResolveTurnID(1, 1)] = demo.resolve(trackedFile, resolvedGit, false, map[string]any{"outcome": "Kept resume's change on upstream's"})
	f.engine.Turns[driftReviewTurnID(1, 1)] = reviewDrift
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range recoveryReports {
			if strings.HasPrefix(req.Name, reviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		if strings.HasPrefix(req.Name, "final-") {
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				demo.problem("final report %s: %s: %v", req.Name, body, err)
			}
			return recoveryResult(req, "Reviewed"), nil
		}
		return chief(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	f.start(t)

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
	awaitFurtherPasses(t, &passed, 2)
	if !holdsEdits(inDirectory(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume"))) {
		t.Fatal("the stopped turn's edits are not in resume's workspace")
	}
	mutation(t, f.c, "DELETE", "pause", target)

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

	// Cancellation restores the local checkpoint before relinquishing clone
	// ownership, so another workstream can safely complete before restart.
	if left := checkpoints(t, f, branchesDirectory); len(left) != 0 {
		t.Fatalf("cancelled landing left checkpoints %v", left)
	}
	entry, changed := interrupted(t, f, branchesDirectory)
	if !changed {
		t.Fatal("the cut-short landing changed nothing in the feature workspaces' operation log")
	}

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

	f.awaitUnit(t, stream, "dedupe", UnitReviewing)
	f.awaitMasonSettled(t, stream, "audit", 2)
	from := seals(t, f.repository(), stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, recoveryUpstream)
	close(demo.ask)

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
	prs.Mu.Lock()
	defer prs.Mu.Unlock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft || prs.Prs[0].HeadCommit != forkBranch() {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	for name, content := range map[string]string{trackedFile: resolvedGit, recoveryFiles["dedupe"]: resolvedDedupe, recoveryFiles["audit"]: resolvedAudit, "internal/trace/kept.go": interruptedEdits["internal/trace/kept.go"]} {
		if got := fileAt(t, f, forkBranch(), name); got != content {
			t.Fatalf("the delivered branch holds %s as %q, want %q", name, got, content)
		}
	}
}
