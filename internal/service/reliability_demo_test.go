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
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// reliabilityPlan has four independent units. upload and dedupe share
// internal.upload; they are never implementing together on the one mason
// slot.
const reliabilityPlan = `{"version": 1, "units": [
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

// demoProblems collects what fake turns found wrong; a fake turn runs on the
// service's goroutine and cannot fail the test itself.
type demoProblems struct {
	mu   sync.Mutex
	list []string
}

func (p *demoProblems) report(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *demoProblems) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) != 0 {
		t.Fatal(strings.Join(p.list, "\n"))
	}
}

// eventually polls cond until it holds, failing with what after demoTimeout.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// completedTurn waits until the i-th turn of the agent's thread completed.
func completedTurn(t *testing.T, f *shedFixture, stream config.WorkstreamID, agent string, i int) trace.QueuedTurn {
	t.Helper()
	var th trace.Thread
	eventually(t, fmt.Sprintf("turn %d of %s never completed", i, agent), func() bool {
		var err error
		th, err = f.repository().Thread(stream, agent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil && len(th.Turns) > i && !th.Turns[i].CompletedAt.IsZero()
	})
	return th.Turns[i]
}

// unitStatus returns the unit's status as the status API lists it.
func unitStatus(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) UnitStatus {
	t.Helper()
	st, err := f.c.Status(context.Background(), stream)
	must(t, err)
	for _, u := range st.Units {
		if u.Unit == unit {
			return u
		}
	}
	t.Fatalf("status of %s lists no unit %s: %+v", stream, unit, st.Units)
	return UnitStatus{}
}

// factoryPauseOf returns the factory pause the runtime API reports, if any.
func factoryPauseOf(t *testing.T, c *Client) (runtime.Pause, bool) {
	t.Helper()
	rt, err := c.Runtime(context.Background())
	must(t, err)
	for _, p := range rt.Effective.Pauses {
		if p.Target == factoryTarget {
			return p, true
		}
	}
	return runtime.Pause{}, false
}

// TestM4ReliabilityDemonstration drives one workstream through the local API
// with fake masons, reviewer and chief of staff while the daily budget, a
// hard pause, a live profile switch, a reload, a provider usage limit and a
// crash during landing interrupt it. See docs/m4-reliability.md.
func TestM4ReliabilityDemonstration(t *testing.T) {
	t.Parallel()
	reliabilityDemonstration(t, agent.AgentCodex)
}

func TestM4ReliabilityDemonstrationOpenCode(t *testing.T) {
	t.Parallel()
	reliabilityDemonstration(t, agent.AgentOpenCode)
}

func reliabilityDemonstration(t *testing.T, fallbackBackend string) {
	ctx := context.Background()
	f, masons := newParallelMasonFixture(t, 1, 4, reliabilityPlan)
	defer func() { f.stop(t) }()
	configPath := filepath.Join(f.opts.Config.Root, "config.toml")

	// The owner allows USD 1.00 a day; days follow UTC.
	f.stop(t)
	if fallbackBackend != agent.AgentCodex {
		data, err := os.ReadFile(configPath)
		must(t, err)
		must(t, os.WriteFile(configPath, []byte(strings.Replace(string(data), "agent = \"codex\"", "agent = \""+fallbackBackend+"\"", 1)), 0600))
	}
	configFile, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[budget]\nper_day = \"1.00\"\n")
	must(t, errors.Join(err, configFile.Close()))
	f.opts.Location = time.UTC
	// The thread runner reports provider limits to the service through the
	// runtime controls Enforce binds.
	enforced := Enforce(f.opts, Enforcement{Engine: f.engine, Hosts: f.opts.Committee.Hosts})
	f.opts.Threads, f.opts.controls = enforced.Threads, enforced.controls
	f.start(t)
	const crash = "crash: the feature branch moved"
	f.s.boundary = func(name string) error {
		if name == "land-advanced" {
			return errors.New(crash)
		}
		return nil
	}

	problems := &demoProblems{}
	c := f.c
	auditRecovered := masonAgent("audit") + "-recover-1"
	dedupeClarified := masonAgent("dedupe") + "-clarify-1"
	const auditFile = "internal/audit/audit.go"
	var (
		mu                 sync.Mutex
		auditContinuation  agent.Request
		dedupeContinuation agent.Request
		dedupeBackends     []string
		resumeBackends     []string
		resumeAttempts     []agent.Request
	)
	auditEntered, clarifyEntered, releaseClarify := make(chan struct{}), make(chan struct{}), make(chan struct{})
	masons.mu.Lock()
	masons.response = map[string]string{masonTurnID("dedupe"): "Half of dedupe is built"}
	masons.play[masonTurnID("upload")] = reportDone("Chunks are sent")
	masons.play[auditRecovered] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), auditFile)); err != nil {
			return fmt.Errorf("the continuation lost the stopped turn's file: %v", err)
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledgements are recorded", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.play[dedupeClarified] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.play[masonTurnID("resume")] = reportDone("Uploads resume")
	masons.mu.Unlock()

	f.engine.mu.Lock()
	f.engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	// upload's mason reports what its session cost.
	f.engine.turns[masonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result, err := masons.turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	}
	// audit's mason saves a file and is still working when it is stopped.
	f.engine.turns[masonTurnID("audit")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		path := filepath.Join(req.Workspace.Directory(), auditFile)
		if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0755), os.WriteFile(path, []byte("package audit\n"), 0644)); err != nil {
			return nil, err
		}
		close(auditEntered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-audit", ResultText: "Half of audit is built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	}
	f.engine.turns[auditRecovered] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		auditContinuation = req
		mu.Unlock()
		return masons.turn(ctx, req, verified, tools)
	}
	// The owner switches masons to the other agent binary while dedupe's
	// first turn runs.
	f.engine.turns[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		dedupeBackends = append(dedupeBackends, req.Profile.Agent)
		mu.Unlock()
		var ack MutationResponse
		if err := c.Do(ctx, "PUT", Prefix+"/runtime/profile", ProfileRequest{masonRole, "other"}, &ack); err != nil || !ack.Applied {
			problems.report("profile switch during dedupe's turn: %v %+v", err, ack)
		}
		return masons.turn(ctx, req, verified, tools)
	}
	// dedupe's continuation holds the loop until the owner has reloaded.
	f.engine.turns[dedupeClarified] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		dedupeContinuation = req
		dedupeBackends = append(dedupeBackends, req.Profile.Agent)
		mu.Unlock()
		close(clarifyEntered)
		select {
		case <-releaseClarify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return masons.turn(ctx, req, verified, tools)
	}
	// resume's first session on claude hits the provider's usage limit.
	f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		backend := req.Profile.Agent
		mu.Lock()
		resumeAttempts = append(resumeAttempts, req)
		resumeBackends = append(resumeBackends, backend)
		mu.Unlock()
		if backend == agent.AgentClaude {
			return &agent.Result{ClaudeID: "session-limited", SessionDir: req.SessionDir, IsError: true, RateLimit: &agent.RateLimit{Status: "rejected", Type: "five_hour", ResetsAt: demoStart.Add(5 * time.Hour)}}, nil
		}
		return masons.turn(ctx, req, verified, tools)
	}
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": reviewEvidence(), "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()

	// 1. Daily budget. upload's session spends USD 1.25. The next pass
	// pauses the factory softly, attributed to the daily budget, and audit
	// waits for it; status reports today's spend.
	stream, _ := f.builtAs(t, "reliability")
	f.awaitUnit(t, stream, "upload", UnitReviewing)
	var budgetPause runtime.Pause
	eventually(t, "the daily budget never paused the factory", func() bool {
		var ok bool
		budgetPause, ok = factoryPauseOf(t, c)
		return ok
	})
	if budgetPause.Mode != "soft" || budgetPause.Source != runtime.PauseDailyBudget || !strings.HasPrefix(budgetPause.Reason, "Daily budget reached: known spend on 2026-09-16 is USD 1.25 of the USD 1.00 per-day budget; dispatch resumes at the next local day; ") || !strings.HasSuffix(budgetPause.Reason, " attempt(s) have unknown cost, so actual spend may be higher") {
		t.Fatalf("budget pause %+v", budgetPause)
	}
	st, err := c.Statuses(ctx)
	must(t, err)
	if b := st.DailyBudget; b == nil || b.Day != "2026-09-16" || b.SpendUSD != "1.25" || b.LimitUSD != "1.00" || !b.LowerBound || b.UnknownCosts == 0 {
		t.Fatalf("daily budget status %+v", st.DailyBudget)
	}
	budgetWait := UnitDispatch{Reason: DeferPaused, Pause: &runtime.Pause{Target: factoryTarget}, Message: "Waits while a factory pause is in force, set by daily-budget."}
	eventually(t, "audit never waited for the budget pause", func() bool {
		return reflect.DeepEqual(unitStatus(t, f, stream, "audit"), f.deferred(t, stream, "audit", budgetWait))
	})
	settle()
	if got := starts(t, f, stream); !slices.Equal(got, []string{trace.UnitSubject("upload")}) {
		t.Fatalf("units started under the budget pause: %v", got)
	}

	// The owner resumes. The budget pauses at most once a day, so audit
	// starts and the factory stays running although spend is over the limit.
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(factoryTarget))
	select {
	case <-auditEntered:
	case <-time.After(demoTimeout):
		t.Fatal("audit's mason never started after the owner resumed")
	}

	// 2. Hard pause. The owner hard-pauses the workstream while audit's
	// mason works: its turn is stopped and no continuation runs.
	workstream := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: workstream, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	stopped := completedTurn(t, f, stream, masonAgent("audit"), 0)
	checkPauseStop(t, stopped, "workstream", "Stop the mason")
	settle()
	for _, q := range f.thread(t, stream, masonAgent("audit")).Turns[1:] {
		if len(q.Attempts) != 0 {
			t.Fatalf("turn %s of audit's mason ran under the hard pause: %+v", q.Request.TurnID, q.Attempts)
		}
	}
	if u := unitStatus(t, f, stream, "audit"); u.State != UnitImplementing {
		t.Fatalf("stopped audit is %+v", u)
	}
	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again the same day: %+v", p)
	}

	// Resuming continues the same thread from the stopped session, with the
	// stopped turn's file in the workspace, and audit's mason reports done.
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(workstream))
	f.awaitUnit(t, stream, "audit", UnitReviewing)
	recovered := completedTurn(t, f, stream, masonAgent("audit"), 1)
	if recovered.Request.TurnID != auditRecovered || recovered.Request.ThreadID != stopped.Request.ThreadID || !strings.Contains(recovered.Request.Prompt, "A hard pause stopped your last turn.") || recovered.Status() != "idle" {
		t.Fatalf("audit's continuation %+v", recovered)
	}
	if a := recovered.Attempts; len(a) != 1 || a[0].Path != "resume" || a[0].SourceSession.ID != "session-audit" || a[0].SourceSequence != stopped.Sequence {
		t.Fatalf("audit's continuation did not resume the stopped session: %+v", a)
	}
	mu.Lock()
	if auditContinuation.ResumeID != "session-audit" {
		t.Fatalf("audit's continuation ran as %+v", auditContinuation)
	}
	mu.Unlock()

	// 3. Live profile switch. dedupe's first turn started on claude and
	// finishes on it; its mason ends without an outcome, and the follow-up
	// on the same thread runs on codex, fresh from the owned log.
	select {
	case <-clarifyEntered:
	case <-time.After(demoTimeout):
		problems.check(t)
		t.Fatal("dedupe's follow-up never started")
	}
	problems.check(t)
	rt, err := c.Runtime(ctx)
	must(t, err)
	if rt.Profiles[masonRole] != (EffectiveProfile{Name: "other", Source: "owner_override"}) {
		t.Fatalf("mason profile after the switch: %+v", rt.Profiles[masonRole])
	}
	th := f.thread(t, stream, masonAgent("dedupe"))
	if len(th.Turns) != 2 || th.Turns[0].Request.Profile.Name != "default" || th.Turns[0].Attempts[0].Profile.Backend != agent.AgentClaude || th.Turns[0].Status() != "idle" {
		t.Fatalf("dedupe's first turn %+v", th.Turns)
	}
	first, next := th.Turns[0], th.Turns[1]
	if next.Request.TurnID != dedupeClarified || next.Request.Profile.Name != "other" || next.Request.Profile.Backend != fallbackBackend {
		t.Fatalf("dedupe's follow-up request %+v", next.Request)
	}
	if a := next.Attempts; len(a) != 1 || a[0].Path != "replay" || a[0].ReplayFrom != first.Sequence || a[0].Profile.Name != "other" {
		t.Fatalf("dedupe's follow-up attempts %+v", a)
	}
	mu.Lock()
	if req := dedupeContinuation; !slices.Equal(dedupeBackends, []string{agent.AgentClaude, fallbackBackend}) || req.ResumeID != "" || !strings.Contains(req.Prompt, "osmia-owned-log") || !strings.Contains(req.Prompt, "Half of dedupe is built") {
		t.Fatalf("dedupe's sessions ran on %v; the follow-up as %+v", dedupeBackends, req)
	}
	mu.Unlock()

	// The owner clears the switch while the follow-up runs on codex; the
	// next mason turn is back on the configured profile.
	mutation(t, c, "DELETE", "profile", ClearProfileRequest{masonRole})
	if u := unitStatus(t, f, stream, "resume"); !reflect.DeepEqual(u, f.deferred(t, stream, "resume", slotless(1))) {
		t.Fatalf("resume while dedupe holds the slot: %+v", u)
	}

	// 4. Reload. A fallback to a profile that does not exist fails
	// validation and leaves the loaded configuration and its digest.
	before, err := c.Configuration(ctx)
	must(t, err)
	data, err := os.ReadFile(configPath)
	must(t, err)
	if !strings.Contains(string(data), "model = \"test\"\n") {
		t.Fatalf("configuration without the default profile's model:\n%s", data)
	}
	withFallback := func(profile string) []byte {
		return []byte(strings.Replace(string(data), "model = \"test\"\n", "model = \"test\"\nfallback = \""+profile+"\"\n", 1))
	}
	must(t, os.WriteFile(configPath, withFallback("missing"), 0600))
	reloadFails(t, c, configPath, "profiles.default.fallback")
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Digest != before.Digest || now.Effective.Profiles["default"].Fallback != "" || now.LastError == nil || now.LastError.Path != configPath || now.LastError.Field != "profiles.default.fallback" {
		t.Fatalf("after the failed reload: digest %s (was %s), last error %+v", now.Digest, before.Digest, now.LastError)
	}

	// The fixed file reloads: a new digest, and the default profile falls
	// back to the other binary from the next turn on.
	must(t, os.WriteFile(configPath, withFallback("other"), 0600))
	reloaded, err := c.Reload(ctx)
	must(t, err)
	now, err = c.Configuration(ctx)
	must(t, err)
	if reloaded.Digest == before.Digest || now.Digest != reloaded.Digest || now.LastError != nil || now.Effective.Profiles["default"].Fallback != "other" || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload %+v; configuration digest %s, last error %+v", reloaded, now.Digest, now.LastError)
	}
	close(releaseClarify)
	f.awaitUnit(t, stream, "dedupe", UnitReviewing)

	// 5. Provider usage limit. resume's first session on claude reports the
	// provider's usage limit; the same turn continues on the reloaded
	// fallback, replaying the owned log, and its mason reports done.
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	limited := completedTurn(t, f, stream, masonAgent("resume"), 0)
	if a := limited.Attempts; len(a) != 2 || a[0].Profile.Name != "default" || a[0].FailureClass != coreadapter.Infrastructure ||
		a[1].Profile.Name != "other" || a[1].Path != "replay" || !strings.HasPrefix(a[1].Reason, "fallback from profile default to other after 1 infrastructure failures: provider usage limit") || limited.Status() != "idle" {
		t.Fatalf("resume's attempts %+v", limited.Attempts)
	}
	mu.Lock()
	if !slices.Equal(resumeBackends, []string{agent.AgentClaude, fallbackBackend}) || resumeAttempts[1].ResumeID != "" || !strings.Contains(resumeAttempts[1].Prompt, "osmia-owned-log") {
		t.Fatalf("resume's sessions ran on %v: %+v", resumeBackends, resumeAttempts)
	}
	mu.Unlock()
	st, err = c.Statuses(ctx)
	must(t, err)
	if st.Profiles[masonRole] != (EffectiveProfile{Name: "other", Source: "provider_fallback", Reason: "Provider claude usage limit (rejected)"}) {
		t.Fatalf("mason profile under the usage limit: %+v", st.Profiles[masonRole])
	}
	if l := st.ProviderLimits; len(l) != 1 || l[0].Backend != agent.AgentClaude || l[0].Status != "rejected" || l[0].Kind != "five_hour" || !l[0].ResetsAt.Equal(demoStart.Add(5*time.Hour)) {
		t.Fatalf("provider limits %+v", st.ProviderLimits)
	}

	// 6. Crash during landing. The reviewer approves resume; its landing
	// commits and moves the feature branch, then fails before it records
	// anything, on every attempt, until the service stops.
	f.awaitUnit(t, stream, "resume", UnitApproved)
	_, approval := approvedReview(t, f.repository(), stream, "resume")
	base := approval.Identity.Candidate.BaseRevision
	eventually(t, "the landing was never interrupted", func() bool {
		ops := landOperations(t, f.repository(), stream)
		return len(ops) == 1 && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" })
	})
	f.stop(t)
	moved := f.landedCommits(t, stream, base)
	if len(moved) != 1 {
		t.Fatalf("the interrupted landing left %v on the feature branch", moved)
	}
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	landings := landingByUnit(t, repository, stream)
	must(t, repository.Close())
	if state.Value != UnitApproved || len(landings) != 0 {
		t.Fatalf("after the crash resume is %s with landings %+v", state.Value, landings)
	}

	// The restarted service observes the landing commit its interrupted
	// operation made, records it and merges resume without committing again.
	f.start(t)
	c = f.c
	f.awaitMerged(t, stream, "resume")
	masons.check(t)
	problems.check(t)
	if commits := f.landedCommits(t, stream, base); !slices.Equal(commits, moved) {
		t.Fatalf("the feature branch holds %v; the interrupted landing made %v", commits, moved)
	}
	ops := landOperations(t, f.repository(), stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("landing operations %+v", ops)
	}
	var observed []coreadapter.Observation
	for _, a := range ops[0].History {
		if a.Kind == "retry" && !strings.Contains(a.Failure, crash) {
			t.Fatalf("landing retry %+v", a)
		}
		if a.Kind == "observe" && a.Observation != nil {
			observed = append(observed, *a.Observation)
		}
	}
	if len(observed) < 2 || !strings.Contains(observed[len(observed)-1].Evidence, "this operation's landing commit on") {
		t.Fatalf("the restarted landing observed %+v", observed)
	}
	var landing UnitLanding
	must(t, json.Unmarshal([]byte(landingByUnit(t, f.repository(), stream)["resume"].Content), &landing))
	if landing.Commit != moved[0] || landing.Operation != ops[0].Operation.ID || landing.Base != base {
		t.Fatalf("landing %+v; the feature branch holds %s", landing, moved[0])
	}
	merged := 0
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.Subject == trace.UnitSubject("resume") && tr.To == UnitMerged {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("resume merged %d times", merged)
	}
	if u := unitStatus(t, f, stream, "resume"); u.State != UnitMerged || u.Landing == nil || u.Landing.Commit != moved[0] {
		t.Fatalf("status of resume %+v", u)
	}

	// The restart kept the owner's and the service's decisions: the budget
	// does not pause again today, and masons still fall back from the
	// limited provider under the reloaded configuration.
	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again after the restart: %+v", p)
	}
	st, err = c.Statuses(ctx)
	must(t, err)
	if st.Profiles[masonRole].Source != "provider_fallback" || st.DailyBudget == nil || st.DailyBudget.SpendUSD != "1.25" {
		t.Fatalf("status after the restart: profiles %+v, daily budget %+v", st.Profiles, st.DailyBudget)
	}
	if cfg, err := c.Configuration(ctx); err != nil || cfg.Digest != reloaded.Digest {
		t.Fatalf("configuration after the restart: %v, digest %s, want %s", err, cfg.Digest, reloaded.Digest)
	}
	var ran []string
	for _, req := range masons.requests(stream) {
		ran = append(ran, req.Name)
	}
	if want := []string{masonTurnID("upload"), auditRecovered, masonTurnID("dedupe"), dedupeClarified, masonTurnID("resume")}; !slices.Equal(ran, want) {
		t.Fatalf("fake mason sessions %v, want %v", ran, want)
	}
}
