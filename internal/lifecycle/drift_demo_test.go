package lifecycle_test

import (
	"context"
	"encoding/json"
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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const driftPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`

const (
	upstreamWrote = "package trace\n\n// upstream reserves this file for resume\n"
	resolvedWrote = "package trace\n\n// resume uses the upstream reservation\n"
)

func cloneGit(home string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *shedFixture) awaitDrift(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.Eventually(t, "drift of "+string(stream)+" never reached "+want, func() bool {
		state, err := f.repository().Workflow(stream, lifecycle.DriftSubject)
		intakefixture.Must(t, err)
		return state.Value == want
	})
}

func (f *shedFixture) awaitMasonSettled(t *testing.T, stream config.WorkstreamID, unit string, turns int) {
	t.Helper()
	intakefixture.AwaitMasonSettled(t, f.repository(), stream, unit, turns)
}

func appendMasonConfig(t *testing.T, opts *Options, extra string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(path, append(data, []byte(extra)...), 0600))
}

type continuationGate struct {
	mu      sync.Mutex
	running map[string]chan struct{}
	release chan struct{}
}

func newContinuationGate() *continuationGate {
	return &continuationGate{running: map[string]chan struct{}{}, release: make(chan struct{})}
}

func (g *continuationGate) started(stream string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[stream] == nil {
		g.running[stream] = make(chan struct{})
	}
	return g.running[stream]
}

func (g *continuationGate) turn(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	started := g.started(intakefixture.SessionStream(req))
	g.mu.Lock()
	select {
	case <-started:
	default:
		close(started)
	}
	g.mu.Unlock()
	select {
	case <-g.release:
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Released", SessionDir: req.SessionDir, NumTurns: 1}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *continuationGate) awaitRunning(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.Await(t, "a continuation of the mason of "+string(stream)+" to run", g.started(string(stream)))
}

func (f *shedFixture) awaitQueuedFollowUp(t *testing.T, stream config.WorkstreamID, unit string) trace.Thread {
	t.Helper()
	var th trace.Thread
	intakefixture.Eventually(t, "the mason of "+unit+" in "+string(stream)+" never had a follow-up queued", func() bool {
		var err error
		th, err = f.repository().Thread(stream, intakefixture.MasonAgent(unit))
		if err != nil || len(th.Turns) == 0 {
			return false
		}
		last := th.Turns[len(th.Turns)-1]
		return last.CompletedAt.IsZero() && last.Claim == nil && strings.HasPrefix(last.Request.TurnID, intakefixture.MasonAgent(unit)+"-clarify-")
	})
	return th
}

func TestDriftCadenceDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 2\n", independentPlan, func(opts *Options) {
		appendMasonConfig(t, opts, "\n[mason]\nmax_clean_turns = 5\n")
	})
	defer func() { f.stop(t) }()
	continuations := newContinuationGate()
	f.engine.Mu.Lock()
	for i := 1; i <= 6; i++ {
		f.engine.Turns[fmt.Sprintf("%s-continue-%d", intakefixture.MasonAgent("resume"), i)] = continuations.turn
	}
	f.engine.Mu.Unlock()

	fixturekit.EditProjectSettings(t, f.opts.Config.Root, f.project, func(text string) string { return text + "upstream_rebase = \"3h\"\n" })
	f.stop(t)
	f.start(t)
	if f.s.cfg.Project.RebaseInterval() != 3*time.Hour {
		t.Fatalf("upstream_rebase is %q", f.s.cfg.Project.UpstreamRebase)
	}

	streams := []config.WorkstreamID{}
	for _, key := range []string{"cadence-uploads", "cadence-audits"} {
		stream, _ := f.builtAs(t, key)
		streams = append(streams, stream)
	}
	sealed := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		f.awaitMasonSettled(t, stream, "resume", 2)
		continuations.awaitRunning(t, stream)
		sealed[stream] = intakefixture.Seals(t, f.repository(), stream)[0].Base.Commit
		st, err := f.c.Status(ctx, stream)
		intakefixture.Must(t, err)
		if st.Drift != nil {
			t.Fatalf("status of %s before any drift rebase: %+v", stream, st.Drift)
		}
	}
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})

	intakefixture.Must(t, f.c.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: runtime.Target{Scope: "role", Role: intakefixture.MasonRole}, Mode: "soft", Reason: "hold mason turns", Source: "owner"}, nil))
	close(continuations.release)
	for _, stream := range streams {
		th := f.awaitQueuedFollowUp(t, stream, "resume")
		for _, q := range th.Turns[:len(th.Turns)-1] {
			if q.CompletedAt.IsZero() {
				t.Fatalf("a mason turn of resume in %s still runs when its drift rebase becomes due: %+v", stream, q.Request.TurnID)
			}
		}
		if state, err := f.unitState(stream, "resume"); err != nil || state != intakefixture.UnitImplementing {
			t.Fatalf("resume of %s is %s before its drift rebase: %v", stream, state, err)
		}
	}

	latest := slices.MaxFunc([]time.Time{sealedAt(t, f.repository(), streams[0]), sealedAt(t, f.repository(), streams[1])}, time.Time.Compare)
	setClock(f, latest.Add(3*time.Hour))
	for _, stream := range streams {
		f.awaitDrift(t, stream, "rebased-1")
	}
	intakefixture.Settle()
	masons.Check(t)
	for _, stream := range streams {
		repository := f.repository()
		ops := checkDrifts(t, repository, stream, 1, "once the interval elapsed")
		if ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
			t.Fatalf("drift rebase of %s: %+v", stream, ops[0].Result)
		}
		asked := intakefixture.TransitionByID(t, repository, stream, "drift-1")
		if want := "upstream_rebase 3h0m0s has elapsed since the sealing at " + sealedAt(t, repository, stream).Format(time.RFC3339) + ": drift rebase 1 fetches"; !strings.HasPrefix(asked.Reason, want) {
			t.Fatalf("drift rebase of %s asked for with %q", stream, asked.Reason)
		}

		tip := featureTip(t, f, stream)
		fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "merge-base", "--is-ancestor", upstream, tip)
		all := intakefixture.Seals(t, repository, stream)
		if len(all) != 2 || all[0].Base.Commit != sealed[stream] || all[1].Base.Commit != upstream || all[1].Seal != all[0].Seal {
			t.Fatalf("seals of %s: %+v", stream, all)
		}

		rebases := unitRebases(t, repository, stream, "resume")
		if len(rebases) != 1 || rebases[0].Onto != tip || rebases[0].State != intakefixture.UnitImplementing || len(rebases[0].Conflicts) != 0 {
			t.Fatalf("resume's rebases in %s: %+v", stream, rebases)
		}
		workspace := filepath.Join(f.opts.Config.Root, intake.UnitsDirectory, string(f.project), string(stream), "resume")
		for name, want := range map[string]string{intakefixture.MasonWrote: "package trace\n", "UPSTREAM.md": "upstream\n"} {
			if data, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || string(data) != want {
				t.Fatalf("resume's workspace of %s holds %s %q: %v", stream, name, data, err)
			}
		}
		if state, err := f.unitState(stream, "resume"); err != nil || state != intakefixture.UnitImplementing {
			t.Fatalf("resume of %s is %s: %v", stream, state, err)
		}
		records := driftRecords(t, repository, stream)
		var outcomes []string
		for _, r := range records {
			outcomes = append(outcomes, r.Outcome)
		}
		if want := []string{lifecycle.DriftReplayed, lifecycle.DriftCarrying, lifecycle.DriftRebased}; !slices.Equal(outcomes, want) {
			t.Fatalf("drift/rebase.json of %s records %q, want %q", stream, outcomes, want)
		}

		if moved := upstreamMovedEvents(t, repository, stream); len(moved) != 0 {
			t.Fatalf("a clean drift rebase of %s raised %+v", stream, moved)
		}
		rebased := intakefixture.TransitionByID(t, repository, stream, "drift-1-rebased")
		st, err := f.c.Status(ctx, stream)
		intakefixture.Must(t, err)
		if want := (&api.DriftStatus{Drift: 1, Outcome: lifecycle.DriftRebased, At: rebased.At, Reason: rebased.Reason, Moved: []string{}}); !reflect.DeepEqual(st.Drift, want) {
			t.Fatalf("status drift of %s %+v, want %+v", stream, st.Drift, want)
		}
	}
}

type driftDemo struct {
	units   *landingDemo
	owner   *Client
	project config.ProjectID
	clone   string

	mu                        sync.Mutex
	stream                    config.WorkstreamID
	asked                     *api.ProjectRebaseResponse
	resolves                  int
	resolving                 chan struct{}
	marked                    string
	recovered                 string
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
	intakefixture.CheckProblems(t, &d.mu, &d.problems)
}

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

func (d *driftDemo) resolve(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
	path := filepath.Join(req.Workspace.Directory(), intakefixture.MasonWrote)
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

func (d *driftDemo) recover(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	data, err := os.ReadFile(filepath.Join(req.Workspace.Directory(), intakefixture.MasonWrote))
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.recovered = string(data)
	d.mu.Unlock()
	body, err := intakefixture.CallTool(ctx, tools, execution.DoneTool, map[string]any{"outcome": "Combined the feature's file with the upstream reservation"})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift done %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Resolved", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func (d *driftDemo) reviewDrift(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	d.mu.Lock()
	stream := d.stream
	d.mu.Unlock()
	tip, err := cloneGit(filepath.Dir(d.clone), "-C", d.clone, "rev-parse", intake.FeatureBranch(stream))
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.reviewedTip, d.reviewPrompt = tip, req.Prompt
	d.mu.Unlock()
	if body, err := intakefixture.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{"change": "resolution", "paths": []string{intakefixture.MasonWrote}}); err != nil || !strings.Contains(body, "+// resume uses the upstream reservation") {
		return nil, fmt.Errorf("resolution diff %s: %v", body, err)
	}
	if body, err := intakefixture.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{"change": "feature", "files_only": true}); err != nil || !strings.Contains(body, `"path":"`+intakefixture.MasonWrote+`"`) {
		return nil, fmt.Errorf("feature files %s: %v", body, err)
	}
	body, err := intakefixture.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("drift verdict %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func TestDriftConflictDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, driftPlan)
	defer func() { f.stop(t) }()
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Uploads resume")
	masons.Play[service.TestMasonTurnID("audit")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal", "trace", "audit.go"), []byte("package trace\n// audit\n"), 0644); err != nil {
			return err
		}
		recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Built audit"})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	demo := &driftDemo{units: &landingDemo{reviews: map[string][]service.UnitReviewIdentity{}}, owner: f.c, project: f.project, clone: f.clone, resolving: make(chan struct{})}
	f.engine.Mu.Lock()
	f.engine.Turns[service.TestMasonTurnID("audit")] = masons.Turn
	f.engine.Turns[lifecycle.DriftResolveTurnID(1, 1)] = demo.resolve
	f.engine.Turns[service.TestDriftMasonAgent+"-recover-1"] = demo.recover
	f.engine.Turns[lifecycle.DriftReviewTurnID(1, 1)] = demo.reviewDrift
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, unit := range []string{"resume", "audit"} {
			if strings.HasPrefix(req.Name, intake.ReviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()

	stream, _ := f.builtAs(t, "drift-demo")
	demo.mu.Lock()
	demo.stream = stream
	demo.mu.Unlock()
	from := intakefixture.Seals(t, f.repository(), stream)[0].Base.Commit
	upstream := advanceUpstream(t, f, map[string]string{intakefixture.MasonWrote: upstreamWrote})

	f.awaitMerged(t, stream, "resume")
	var resume api.UnitLanding
	intakefixture.Must(t, json.Unmarshal([]byte(landingByUnit(t, f.repository(), stream)["resume"].Content), &resume))
	select {
	case <-demo.resolving:
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("the drift mason never started resolving")
	}
	f.awaitUnit(t, stream, "audit", intake.UnitApproved)
	demo.mu.Lock()
	asked := demo.asked
	demo.mu.Unlock()
	if want := (api.ProjectRebaseResponse{Project: f.project, Covered: []api.DriftCoverage{{Workstream: stream, Drift: 1}}, Skipped: []api.DriftSkip{}}); asked == nil || !reflect.DeepEqual(*asked, want) {
		t.Fatalf("the owner's request was answered %+v, want %+v", asked, want)
	}

	repository := f.repository()
	if requested := intakefixture.TransitionByID(t, repository, stream, "drift-1"); !strings.HasPrefix(requested.Reason, "the owner asked for a drift rebase at ") {
		t.Fatalf("drift rebase 1 asked for with %q", requested.Reason)
	}
	if conflicted := intakefixture.TransitionByID(t, repository, stream, "drift-1-conflicted"); conflicted.To != "conflicted-1" || !strings.Contains(conflicted.Reason, intakefixture.MasonWrote+" conflicted") {
		t.Fatalf("the conflict %+v", conflicted)
	}
	if tip := featureTip(t, f, stream); tip != resume.Commit {
		t.Fatalf("the feature branch moved to %s while its conflict is open", tip)
	}
	if n := len(intakefixture.Seals(t, repository, stream)); n != 1 {
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
	th := f.thread(t, stream, service.TestDriftMasonAgent)
	for _, want := range []string{"- " + intakefixture.MasonWrote + "\n", "conflict markers", "## Sealed spec: spec.md"} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}
	st, err := f.c.Status(ctx, stream)
	intakefixture.Must(t, err)
	if st.Drift == nil || st.Drift.Drift != 1 || st.Drift.Outcome != lifecycle.DriftConflicted || len(st.Drift.Moved) != 1 {
		t.Fatalf("status while the conflict is open %+v", st.Drift)
	}

	f.stop(t)
	f.start(t)
	f.awaitMerged(t, stream, "audit")
	masons.Check(t)
	demo.check(t)
	repository = f.repository()

	demo.mu.Lock()
	resolves, recovered, reviewedTip, reviewPrompt := demo.resolves, demo.recovered, demo.reviewedTip, demo.reviewPrompt
	demo.mu.Unlock()
	if resolves != 1 || recovered != resolvedWrote {
		t.Fatalf("the resolve turn ran %d times; its continuation found %q", resolves, recovered)
	}
	th = f.thread(t, stream, service.TestDriftMasonAgent)
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), service.TestDriftMasonAgent + "-recover-1"}) || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("drift mason turns %v, the first %s", ids, th.Turns[0].Status())
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftReviewerAgent); !slices.Equal(ids, []string{lifecycle.DriftReviewTurnID(1, 1)}) {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	records := driftRecords(t, repository, stream)
	var outcomes []string
	for _, r := range records {
		outcomes = append(outcomes, r.Outcome)
	}
	if want := []string{lifecycle.DriftConflicted, lifecycle.DriftConflicted, intake.DriftResolved, lifecycle.DriftReplayed, lifecycle.DriftCarrying, lifecycle.DriftRebased}; !slices.Equal(outcomes, want) {
		t.Fatalf("drift/rebase.json records %q, want %q", outcomes, want)
	}

	resolved := records[len(records)-1].Commit
	if reviewedTip != resume.Commit || !strings.Contains(reviewPrompt, resolved) || !strings.Contains(reviewPrompt, "## Sealed spec: spec.md") || !strings.Contains(reviewPrompt, `Change "resolution"`) || strings.Contains(reviewPrompt, "+// resume uses the upstream reservation") {
		t.Fatalf("the drift reviewer read %s with the feature branch at %s", resolved, reviewedTip)
	}
	if parentOf(t, f, resolved) != upstream || fileAt(t, f, resolved, intakefixture.MasonWrote) != resolvedWrote {
		t.Fatalf("the approved candidate %s is not resume's landing resolved onto %s", resolved, upstream)
	}
	if all := intakefixture.Seals(t, repository, stream); len(all) != 2 || all[0].Base.Commit != from || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}
	if _, found, err := intake.Workspaces(f.s.cfg, service.TestDriftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}

	rebases := unitRebases(t, repository, stream, "audit")
	if len(rebases) != 1 || rebases[0].State != intake.UnitApproved || rebases[0].Base != resume.Commit || rebases[0].Onto != resolved || len(rebases[0].Conflicts) != 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	if got := fileAt(t, f, rebases[0].Commit, intakefixture.MasonWrote); got != resolvedWrote {
		t.Fatalf("audit's carried candidate replaced the reviewed drift resolution with %q", got)
	}
	if got := fileAt(t, f, rebases[0].Commit, "internal/trace/audit.go"); got != "package trace\n// audit\n" {
		t.Fatalf("audit's carried candidate lost its own change: %q", got)
	}
	back := intakefixture.TransitionByID(t, repository, stream, trace.UnitSubject("audit")+"-checking-rebase-1")
	if back.From != intake.UnitApproved || !strings.HasPrefix(back.Reason, "the approval no longer holds: ") {
		t.Fatalf("audit's return to review %+v", back)
	}
	reviews := demo.units.reviewed("audit")
	if len(reviews) != 2 || reviews[0].Candidate.BaseRevision != resume.Commit || reviews[1].Candidate.BaseRevision != resolved || reviews[1].Candidate.Revision != rebases[0].Commit {
		t.Fatalf("audit's reviews %+v after rebase %+v", reviews, rebases[0])
	}
	var audit api.UnitLanding
	intakefixture.Must(t, json.Unmarshal([]byte(landingByUnit(t, repository, stream)["audit"].Content), &audit))
	if audit.Base != resolved || audit.Candidate != reviews[1].Candidate.Revision || featureTip(t, f, stream) != audit.Commit {
		t.Fatalf("audit landed %+v on the branch at %s", audit, featureTip(t, f, stream))
	}
	rebased := intakefixture.TransitionByID(t, repository, stream, "drift-1-rebased")
	for _, o := range landOperations(t, repository, stream) {
		if in, err := execution.DecodeLand(o.Operation); err == nil && in.Unit == "audit" && !o.Transition.At.After(rebased.At) {
			t.Fatalf("audit's landing was asked for at %s, before drift rebase 1 finished at %s", o.Transition.At, rebased.At)
		}
	}

	moved := upstreamMovedEvents(t, repository, stream)
	move := trace.UpstreamMove{Drift: 1, From: from, To: upstream}
	if len(moved) != 2 || moved[0].TransitionID != "drift-1-conflicted" || moved[1].TransitionID != back.ID {
		t.Fatalf("upstream moved events %+v", moved)
	}
	checkMoved(t, moved[0], stream, move, "feature branch "+intake.FeatureBranch(stream)+" conflicts with upstream in "+intakefixture.MasonWrote)
	checkMoved(t, moved[1], stream, move, "unit audit's approval no longer holds")
	f.awaitEventTurns(t, stream, moved[0].Event.Body, moved[1].Event.Body)
	st, err = f.c.Status(ctx, stream)
	intakefixture.Must(t, err)
	if want := (&api.DriftStatus{Drift: 1, Outcome: lifecycle.DriftRebased, At: rebased.At, Reason: rebased.Reason, Moved: []string{moved[0].Event.Body, moved[1].Event.Body}}); !reflect.DeepEqual(st.Drift, want) {
		t.Fatalf("status drift %+v, want %+v", st.Drift, want)
	}
}
