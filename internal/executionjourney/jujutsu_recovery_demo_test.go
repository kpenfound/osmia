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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const driftCarrying = "carrying"

const recoveryPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": ["resume"], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": ["resume"], "footprint": ["internal.audit"]}
]}
`

var (
	recoveryFiles = map[string]string{
		"dedupe": "internal/trace/dedupe.go",
		"audit":  "internal/audit/audit.go",
	}
	recoveryUpstream = map[string]string{
		intakefixture.TrackedFile: "package trace\n\n// upstream changed git\n",
		recoveryFiles["dedupe"]:   "package trace\n\n// upstream dedupe\n",
		recoveryFiles["audit"]:    "package audit\n\n// upstream audit\n",
	}
	recoveryReports = map[string]string{"resume": journey.ResumeReport, "dedupe": dedupeReport, "audit": auditReport}
)

type recoveryDemo struct {
	units   *journey.LandingDemo
	project config.ProjectID

	owner func() *journey.Client

	entered chan struct{}

	ask chan struct{}

	auditWrote, carrying chan struct{}
	wroteOnce            sync.Once

	auditReviewed chan struct{}
	auditOnce     sync.Once

	mu           sync.Mutex
	asking       bool
	asked        *api.ProjectRebaseResponse
	continuation string
	sawEdits     bool

	marked   map[string]string
	problems []string
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
	if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Resumed from the kept edits"}); err != nil || !recorded {
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
			d.wroteOnce.Do(func() { close(d.auditWrote) })
			select {
			case <-d.carrying:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Built " + unit}); err != nil || !recorded {
			return nil, fmt.Errorf("done refused: %q %v", reason, err)
		}
		return recoveryResult(req, "Built"), nil
	}
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
			if recorded, reason, err := intakefixture.Done(ctx, tools, args); err != nil || !recorded {
				return nil, fmt.Errorf("done of %s refused: %q %v", req.Name, reason, err)
			}
		} else if body, err := fixturekit.CallTool(ctx, tools, execution.DoneTool, args); err != nil || !strings.Contains(body, `"recorded":true`) {
			return nil, fmt.Errorf("done of %s: %s %v", req.Name, body, err)
		}
		return recoveryResult(req, "Resolved"), nil
	}
}

func reviewDrift(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
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
			intakefixture.ReportProblem(&d.mu, &d.problems, "the owner's drift rebase request: %v", err)
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
	result, err := d.units.Review(ctx, unit, req, tools)
	if unit == "audit" && err == nil {
		d.auditOnce.Do(func() { close(d.auditReviewed) })
	}
	return result, err
}

func TestJujutsuRecoveryDemonstration(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, _ := journey.NewParallelMasonFixtureOn(t, config.WorkspacesJujutsu, 2, 3, recoveryPlan)
	defer func() { f.Stop(t) }()
	f.Stop(t)

	var stream config.WorkstreamID
	home := filepath.Dir(f.Clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "--bare", fork)
	fixturekit.DemoGit(t, home, "-C", f.Clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(stream)))
	}
	prs := &fixturekit.FakePulls{Fork: forkBranch, SyncHead: true}
	f.Opts.PullRequests = prs
	var passed atomic.Int64
	f.Opts.SchedulePassed = func() { passed.Add(1) }

	demo := &recoveryDemo{units: &journey.LandingDemo{Reviews: map[string][]service.UnitReviewIdentity{}}, project: f.Project, owner: func() *journey.Client { return f.C },
		entered: make(chan struct{}), ask: make(chan struct{}), auditWrote: make(chan struct{}), carrying: make(chan struct{}), auditReviewed: make(chan struct{}), marked: map[string]string{}}

	const (
		resolvedGit    = "package trace\n\n// changed before the interruption, on upstream's change\n"
		resolvedDedupe = "package trace\n\n// dedupe, beside upstream's\n"
		resolvedAudit  = "package audit\n\n// audit, beside upstream's\n"
	)
	resolutions := map[string]string{"dedupe": resolvedDedupe, "audit": resolvedAudit}
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("resume")] = demo.interrupted
	f.Engine.Turns[intake.MasonAgent("resume")+"-recover-1"] = demo.continued
	for unit, resolved := range resolutions {
		f.Engine.Turns[execution.MasonTurnID(unit)] = demo.build(unit)
		f.Engine.Turns[execution.ResolveTurnID(unit, 1)] = demo.resolve(recoveryFiles[unit], resolved, true, map[string]any{"outcome": "Kept upstream's file beside " + unit})
	}
	f.Engine.Turns[lifecycle.DriftResolveTurnID(1, 1)] = demo.resolve(intakefixture.TrackedFile, resolvedGit, false, map[string]any{"outcome": "Kept resume's change on upstream's"})
	f.Engine.Turns[lifecycle.DriftReviewTurnID(1, 1)] = reviewDrift
	manager := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range recoveryReports {
			if strings.HasPrefix(req.Name, intake.ReviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		if strings.HasPrefix(req.Name, "final-") {
			body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				intakefixture.ReportProblem(&demo.mu, &demo.problems, "final report %s: %s: %v", req.Name, body, err)
			}
			return recoveryResult(req, "Reviewed"), nil
		}
		return manager(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()
	f.Start(t)

	s := f.S
	crashing := make(chan struct{})
	var crashed atomic.Bool
	s.Boundary = func(step string) error {
		if step == "land-advanced" && crashed.CompareAndSwap(false, true) {
			close(crashing)
			<-s.Lifetime.Done()
			return errors.New("the service died in the middle of the landing")
		}
		return nil
	}

	stream, _ = f.BuiltAs(t, "jujutsu-recovery")
	if got := journey.BackendOf(t, f, stream); got != config.WorkspacesJujutsu {
		t.Fatalf("the workstream is on %s", got)
	}
	base := journey.FeatureTip(t, f, stream)
	select {
	case <-demo.entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("resume's mason never started")
	}
	target := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	intakefixture.CheckPauseStop(t, awaitCompleted(f, t, stream, intake.MasonAgent("resume"), execution.MasonTurnID("resume")), "workstream", "Stop the mason")
	intakefixture.AwaitFurtherPasses(t, &passed, 2)
	if !holdsEdits(inDirectory(filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume"))) {
		t.Fatal("the stopped turn's edits are not in resume's workspace")
	}
	intakefixture.Mutation(t, f.C, "DELETE", "pause", target)

	select {
	case <-crashing:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("resume never reached its landing")
	}
	f.Stop(t)
	moved := landedCommits(f, t, stream, base)
	if len(moved) != 1 || !holdsEdits(inCommit(f, moved[0])) {
		t.Fatalf("the cut-short landing left the feature branch with %v", moved)
	}

	if left := f.Jujutsu().Checkpoints(t, intake.BranchesDirectory); len(left) != 0 {
		t.Fatalf("cancelled landing left checkpoints %v", left)
	}
	entry, changed := f.Jujutsu().Interrupted(t, intake.BranchesDirectory)
	if !changed {
		t.Fatal("the cut-short landing changed nothing in the feature workspaces' operation log")
	}

	f.Start(t)
	f.AwaitMerged(t, stream, "resume")
	if log := f.Jujutsu().OperationLog(t, intake.BranchesDirectory); !slices.ContainsFunc(log, func(line string) bool { return strings.HasSuffix(line, " restore to operation "+entry) }) {
		t.Fatalf("the feature workspaces were not restored to %s:\n%s", entry, strings.Join(log, "\n"))
	}
	if got := landedCommits(f, t, stream, base); !slices.Equal(got, moved) {
		t.Fatalf("after the restart the feature branch holds %v, not the one landing %v", got, moved)
	}
	var resume api.UnitLanding
	fixturekit.Must(t, json.Unmarshal([]byte(journey.LandingByUnit(t, f.Repository(), stream)["resume"].Content), &resume))
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

	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	select {
	case <-demo.auditWrote:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("audit's mason never wrote its file")
	}
	from := intakefixture.Seals(t, f.Repository(), stream)[0].Base.Commit
	upstream := journey.AdvanceUpstream(t, f, recoveryUpstream)
	close(demo.ask)

	intakefixture.Eventually(t, "the drift rebase never set out to carry the units over", func() bool {
		return slices.ContainsFunc(journey.DriftRecords(t, f.Repository(), stream), func(r service.DriftRebase) bool {
			return r.Outcome == driftCarrying || r.Outcome == lifecycle.DriftRebased
		})
	})
	close(demo.carrying)

	f.AwaitMerged(t, stream, "dedupe")
	f.AwaitMerged(t, stream, "audit")
	intakefixture.CheckProblems(t, &demo.mu, &demo.problems)
	repository := f.Repository()
	demo.mu.Lock()
	asked, marked := demo.asked, demo.marked
	demo.mu.Unlock()
	if asked == nil || len(asked.Covered) != 1 || asked.Covered[0] != (api.DriftCoverage{Workstream: stream, Drift: 1}) {
		t.Fatalf("the owner's drift rebase request was answered %+v", asked)
	}
	if conflicted := intakefixture.TransitionByID(t, repository, stream, "drift-1-conflicted"); !strings.Contains(conflicted.Reason, intakefixture.TrackedFile+" conflicted") {
		t.Fatalf("the feature branch's conflict %+v", conflicted)
	}
	for turn, path := range map[string]string{lifecycle.DriftResolveTurnID(1, 1): intakefixture.TrackedFile, execution.ResolveTurnID("dedupe", 1): recoveryFiles["dedupe"], execution.ResolveTurnID("audit", 1): recoveryFiles["audit"]} {
		if !strings.Contains(marked[turn], "<<<<<<< ") || !strings.Contains(marked[turn], ">>>>>>> ") {
			t.Fatalf("turn %s found %s as %q", turn, path, marked[turn])
		}
	}
	var resolved string
	for _, r := range journey.DriftRecords(t, repository, stream) {
		if r.Outcome == lifecycle.DriftRebased {
			resolved = r.Commit
		}
	}
	if resolved == "" || journey.ParentOf(t, f, resolved) != upstream || journey.FileAt(t, f, resolved, intakefixture.TrackedFile) != resolvedGit {
		t.Fatalf("the drift rebase moved the feature branch to %q, not resume's landing resolved onto %s", resolved, upstream)
	}
	if all := intakefixture.Seals(t, repository, stream); len(all) != 2 || all[0].Base.Commit != from || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}

	g := intakefixture.ProviderOf(t, journey.NewUnitWorkspaces(f.S.Cfg, repository).StreamWorkspaces, stream)
	landings := journey.LandingByUnit(t, repository, stream)
	for unit, state := range map[string]string{"dedupe": intake.UnitApproved, "audit": intake.UnitImplementing} {

		rebases := journey.UnitRebases(t, repository, stream, unit)
		if len(rebases) == 0 || rebases[0].State != state || rebases[0].Base != resume.Commit || rebases[0].Onto != resolved || !slices.Equal(rebases[0].Conflicts, []string{recoveryFiles[unit]}) {
			t.Fatalf("unit %s's rebases %+v", unit, rebases)
		}
		if stored, err := g.StoredConflicts(ctx, rebases[0].Commit); err != nil || !slices.Equal(stored, []string{recoveryFiles[unit]}) {
			t.Fatalf("unit %s's rebased commit %s stores %v: %v", unit, rebases[0].Commit, stored, err)
		}
		th := f.Thread(t, stream, intake.MasonAgent(unit))
		i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == execution.ResolveTurnID(unit, 1) })
		if i < 0 || !strings.Contains(th.Turns[i].Request.Prompt, "- "+recoveryFiles[unit]+"\n") {
			t.Fatalf("unit %s's mason got no turn resolving %s", unit, recoveryFiles[unit])
		}
		if unit == "dedupe" {
			back := intakefixture.TransitionByID(t, repository, stream, trace.UnitSubject(unit)+"-"+intake.UnitImplementing+"-rebase-1")
			if back.From != intake.UnitApproved || back.To != intake.UnitImplementing {
				t.Fatalf("dedupe's return to its mason %+v", back)
			}
		}

		change, err := execution.ReadUnitChange(repository, stream, unit)
		fixturekit.Must(t, err)
		if change == "" {
			t.Fatalf("unit %s records no change", unit)
		}
		reviews := demo.units.Reviewed(unit)
		walk := func(commit, role string) {
			t.Helper()
			walked, apiErr := journey.InspectForTest(f.S).TraceView(ctx, string(stream), "commit", commit)
			if apiErr != nil {
				t.Fatalf("the walk of unit %s's commit %s: %+v", unit, commit, apiErr)
			}
			if !slices.ContainsFunc(walked.(api.CommitTrace).Records, func(r api.CommitRecord) bool { return r.Unit == unit && (role == "" || r.Role == role) }) {
				t.Fatalf("the walk of unit %s's commit %s records %+v", unit, commit, walked.(api.CommitTrace).Records)
			}
		}
		walk(rebases[0].Snapshot, service.CommitChange)
		for _, r := range rebases {
			if got, err := g.ChangeOf(ctx, r.Commit); err != nil || got != change {
				t.Fatalf("unit %s's rebase %d made %s of change %q, %v; want %s", unit, r.Rebase, r.Commit, got, err, change)
			}
			walk(r.Commit, service.CommitChange)
		}
		for _, reviewed := range reviews {
			walk(reviewed.Candidate.Revision, "")
		}

		for _, r := range rebases {
			if !slices.ContainsFunc(reviews, func(id service.UnitReviewIdentity) bool { return id.Candidate.BaseRevision == r.Onto }) {
				t.Fatalf("unit %s was never reviewed on %s after rebase %d; reviews %+v", unit, r.Onto, r.Rebase, reviews)
			}
		}
		var landed api.UnitLanding
		fixturekit.Must(t, json.Unmarshal([]byte(landings[unit].Content), &landed))
		last := reviews[len(reviews)-1].Candidate
		if landed.Candidate != last.Revision || landed.Base != last.BaseRevision || slices.ContainsFunc(rebases, func(r api.UnitRebase) bool { return r.Snapshot == landed.Candidate }) {
			t.Fatalf("unit %s landed %+v; its last review read %+v", unit, landed, last)
		}
		if journey.FileAt(t, f, landed.Commit, recoveryFiles[unit]) != resolutions[unit] {
			t.Fatalf("unit %s's landing lost its mason's resolution", unit)
		}
	}
	if first := demo.units.Reviewed("dedupe")[0].Candidate; first.BaseRevision != resume.Commit {
		t.Fatalf("dedupe was first approved on %s, not resume's landing %s", first.BaseRevision, resume.Commit)
	}

	f.AwaitFeature(t, stream, intake.AssembledState)
	var presented api.DeliveryPresentation
	var err error
	intakefixture.Eventually(t, "final review never presented the delivery", func() bool {
		presented, err = f.C.Delivery(ctx, stream)
		return err == nil
	})
	_, err = f.C.ApproveDelivery(ctx, stream, api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	fixturekit.Must(t, err)
	f.AwaitFeature(t, stream, intake.DeliveredState)
	intakefixture.CheckProblems(t, &demo.mu, &demo.problems)
	prs.Mu.Lock()
	defer prs.Mu.Unlock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft || prs.Prs[0].HeadCommit != forkBranch() {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	for name, content := range map[string]string{intakefixture.TrackedFile: resolvedGit, recoveryFiles["dedupe"]: resolvedDedupe, recoveryFiles["audit"]: resolvedAudit, "internal/trace/kept.go": interruptedEdits["internal/trace/kept.go"]} {
		if got := journey.FileAt(t, f, forkBranch(), name); got != content {
			t.Fatalf("the delivered branch holds %s as %q, want %q", name, got, content)
		}
	}
}
