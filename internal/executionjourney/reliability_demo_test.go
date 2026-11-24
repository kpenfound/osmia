package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newParallelMasonFixtureCounting(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *intakefixture.FakeMasons, *atomic.Int64) {
	t.Helper()
	passed, prepare := countingSchedule()
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, prepare)
	intakefixture.ExtendParallelMasons(t, f.clone, f.repository(), f.clock.Now(), f.engine, fake, service.TestMasonTurnID, intake.OwnerActor)
	return f, fake, passed
}

type demoProblems struct {
	mu   sync.Mutex
	list []string
}

func (p *demoProblems) report(format string, args ...any) {
	intakefixture.ReportProblem(&p.mu, &p.list, format, args...)
}

func (p *demoProblems) check(t *testing.T) {
	t.Helper()
	intakefixture.CheckProblems(t, &p.mu, &p.list)
}

func completedTurn(t *testing.T, f *shedFixture, stream config.WorkstreamID, agent string, i int) trace.QueuedTurn {
	t.Helper()
	var th trace.Thread
	intakefixture.Eventually(t, fmt.Sprintf("turn %d of %s never completed", i, agent), func() bool {
		var err error
		th, err = f.repository().Thread(stream, agent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil && len(th.Turns) > i && !th.Turns[i].CompletedAt.IsZero()
	})
	return th.Turns[i]
}

func unitStatus(t *testing.T, f *shedFixture, stream config.WorkstreamID, unit string) api.UnitStatus {
	t.Helper()
	return intakefixture.ReadUnitStatus(t, f.c, stream, unit)
}

func factoryPauseOf(t *testing.T, c *Client) (runtime.Pause, bool) {
	t.Helper()
	return intakefixture.FactoryPauseOf(t, c.Runtime)
}

var resumeLimitResets = fixturekit.DemoStart.Add(1000 * time.Hour)

func TestReliabilityDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons, passed := newParallelMasonFixtureCounting(t, 1, 4, intakefixture.ReliabilityPlan)
	defer func() { f.stop(t) }()
	configPath := filepath.Join(f.opts.Config.Root, "config.toml")

	f.stop(t)
	configText, err := os.ReadFile(configPath)
	fixturekit.Must(t, err)
	if !strings.Contains(string(configText), "per_workstream = 4\n") {
		t.Fatalf("config without the fixture's capacity:\n%s", configText)
	}
	fixturekit.Must(t, os.WriteFile(configPath, []byte(strings.Replace(string(configText), "per_workstream = 4\n", "per_workstream = 4\nreviewers = 4\n", 1)), 0600))
	configFile, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[budget]\nper_day = \"1.00\"\n")
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
	f.opts.Location = time.UTC

	enforced := Enforce(f.opts, service.Enforcement{Engine: f.engine, Hosts: f.opts.Committee.Hosts})
	f.opts.Threads, f.opts.controls = enforced.Threads, enforced.controls
	const crash = "crash: the feature branch moved"
	crashed := make(chan struct{})
	f.opts.boundary = func(lifetime context.Context, name string) error {
		if name == "land-advanced" {
			close(crashed)
			<-lifetime.Done()
			return errors.New(crash)
		}
		return nil
	}
	f.start(t)

	problems := &demoProblems{}
	c := f.c
	auditRecovered := intake.MasonAgent("audit") + "-recover-1"
	dedupeClarified := intake.MasonAgent("dedupe") + "-clarify-1"
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
	masons.Mu.Lock()
	masons.Response = map[string]string{service.TestMasonTurnID("dedupe"): "Half of dedupe is built"}
	masons.Play[service.TestMasonTurnID("upload")] = intakefixture.ReportDone("Chunks are sent")
	masons.Play[auditRecovered] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), auditFile)); err != nil {
			return fmt.Errorf("the continuation lost the stopped turn's file: %v", err)
		}
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledgements are recorded"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.Play[dedupeClarified] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Uploads resume")
	masons.Mu.Unlock()

	f.engine.Mu.Lock()
	f.engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	f.engine.Turns[service.TestMasonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result, err := masons.Turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	}

	f.engine.Turns[service.TestMasonTurnID("audit")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		path := filepath.Join(req.Workspace.Directory(), auditFile)
		if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0755), os.WriteFile(path, []byte("package audit\n"), 0644)); err != nil {
			return nil, err
		}
		close(auditEntered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-audit", ResultText: "Half of audit is built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	}
	f.engine.Turns[auditRecovered] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		auditContinuation = req
		mu.Unlock()
		return masons.Turn(ctx, req, verified, tools)
	}

	f.engine.Turns[service.TestMasonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		dedupeBackends = append(dedupeBackends, req.Profile.Agent)
		mu.Unlock()
		var ack api.MutationResponse
		if err := c.Do(ctx, "PUT", service.Prefix+"/runtime/profile", api.ProfileRequest{Role: intake.MasonRole, Profile: "other"}, &ack); err != nil || !ack.Applied {
			problems.report("profile switch during dedupe's turn: %v %+v", err, ack)
		}
		return masons.Turn(ctx, req, verified, tools)
	}

	f.engine.Turns[dedupeClarified] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
		return masons.Turn(ctx, req, verified, tools)
	}

	f.engine.Turns[service.TestMasonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		backend := req.Profile.Agent
		mu.Lock()
		resumeAttempts = append(resumeAttempts, req)
		resumeBackends = append(resumeBackends, backend)
		mu.Unlock()
		if backend == agent.AgentClaude {
			return &agent.Result{ClaudeID: "session-limited", SessionDir: req.SessionDir, IsError: true, RateLimit: &agent.RateLimit{Status: "rejected", Type: "five_hour", ResetsAt: resumeLimitResets}}, nil
		}
		return masons.Turn(ctx, req, verified, tools)
	}
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()

	stream, _ := f.builtAs(t, "reliability")

	f.awaitUnit(t, stream, "upload", execution.UnitChecking)
	var budgetPause runtime.Pause
	intakefixture.Eventually(t, "the daily budget never paused the factory", func() bool {
		var ok bool
		budgetPause, ok = factoryPauseOf(t, c)
		return ok
	})
	if budgetPause.Mode != "soft" || budgetPause.Source != runtime.PauseDailyBudget || !strings.HasPrefix(budgetPause.Reason, "Daily budget reached: known spend on 2026-09-16 is USD 1.25 of the USD 1.00 per-day budget; dispatch resumes at the next local day; ") || !strings.HasSuffix(budgetPause.Reason, " attempt(s) have unknown cost, so actual spend may be higher") {
		t.Fatalf("budget pause %+v", budgetPause)
	}

	var st api.StatusResponse
	intakefixture.Eventually(t, "status never showed the daily budget", func() bool {
		var err error
		st, err = c.Statuses(ctx)
		fixturekit.Must(t, err)
		b := st.DailyBudget
		return b != nil && b.Day == "2026-09-16" && b.SpendUSD == "1.25" && b.LimitUSD == "1.00" && b.LowerBound && b.UnknownCosts != 0
	})
	budgetWait := api.UnitDispatch{Reason: execution.DeferPaused, Pause: &runtime.Pause{Target: factoryTarget}, Message: "Waits while a factory pause is in force, set by daily-budget."}
	intakefixture.Eventually(t, "audit never waited for the budget pause", func() bool {
		return reflect.DeepEqual(unitStatus(t, f, stream, "audit"), f.deferred(t, stream, "audit", budgetWait))
	})
	intakefixture.AwaitPasses(t, passed, 3)
	intakefixture.Settled(t, func() int { return len(starts(t, f, stream)) })
	if got := starts(t, f, stream); !slices.Equal(got, []string{trace.UnitSubject("upload")}) {
		t.Fatalf("units started under the budget pause: %v", got)
	}

	mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(factoryTarget))
	select {
	case <-auditEntered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("audit's mason never started after the owner resumed")
	}

	workstream := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	mutation(t, c, "PUT", "pause", api.PauseRequest{Target: workstream, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	stopped := completedTurn(t, f, stream, intake.MasonAgent("audit"), 0)
	intakefixture.CheckPauseStop(t, stopped, "workstream", "Stop the mason")
	attempts := func() int {
		n := 0
		for _, q := range f.thread(t, stream, intake.MasonAgent("audit")).Turns[1:] {
			n += len(q.Attempts)
		}
		return n
	}
	intakefixture.AwaitPasses(t, passed, 3)
	intakefixture.Settled(t, attempts)
	for _, q := range f.thread(t, stream, intake.MasonAgent("audit")).Turns[1:] {
		if len(q.Attempts) != 0 {
			t.Fatalf("turn %s of audit's mason ran under the hard pause: %+v", q.Request.TurnID, q.Attempts)
		}
	}
	if u := unitStatus(t, f, stream, "audit"); u.State != intake.UnitImplementing {
		t.Fatalf("stopped audit is %+v", u)
	}
	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again the same day: %+v", p)
	}

	mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(workstream))
	f.awaitUnit(t, stream, "audit", intake.UnitReviewing)
	recovered := completedTurn(t, f, stream, intake.MasonAgent("audit"), 1)
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

	select {
	case <-clarifyEntered:
	case <-time.After(fixturekit.DemoTimeout):
		problems.check(t)
		t.Fatal("dedupe's follow-up never started")
	}
	problems.check(t)
	rt, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if rt.Profiles[intake.MasonRole] != (api.EffectiveProfile{Name: "other", Source: "owner_override"}) {
		t.Fatalf("mason profile after the switch: %+v", rt.Profiles[intake.MasonRole])
	}
	th := f.thread(t, stream, intake.MasonAgent("dedupe"))
	if len(th.Turns) != 2 || th.Turns[0].Request.Profile.Name != "default" || th.Turns[0].Attempts[0].Profile.Backend != agent.AgentClaude || th.Turns[0].Status() != "idle" {
		t.Fatalf("dedupe's first turn %+v", th.Turns)
	}
	first, next := th.Turns[0], th.Turns[1]
	if next.Request.TurnID != dedupeClarified || next.Request.Profile.Name != "other" || next.Request.Profile.Backend != agent.AgentCodex {
		t.Fatalf("dedupe's follow-up request %+v", next.Request)
	}
	if a := next.Attempts; len(a) != 1 || a[0].Path != "replay" || a[0].ReplayFrom != first.Sequence || a[0].Profile.Name != "other" {
		t.Fatalf("dedupe's follow-up attempts %+v", a)
	}
	mu.Lock()
	if req := dedupeContinuation; !slices.Equal(dedupeBackends, []string{agent.AgentClaude, agent.AgentCodex}) || req.ResumeID != "" || !strings.Contains(req.Prompt, "osmia-owned-log") || !strings.Contains(req.Prompt, "Half of dedupe is built") {
		t.Fatalf("dedupe's sessions ran on %v; the follow-up as %+v", dedupeBackends, req)
	}
	mu.Unlock()

	mutation(t, c, "DELETE", "profile", api.ClearProfileRequest{Role: intake.MasonRole})
	if u := unitStatus(t, f, stream, "resume"); !reflect.DeepEqual(u, f.deferred(t, stream, "resume", slotless(1))) {
		t.Fatalf("resume while dedupe holds the slot: %+v", u)
	}

	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	data, err := os.ReadFile(configPath)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), "model = \"test\"\n") {
		t.Fatalf("configuration without the default profile's model:\n%s", data)
	}
	withFallback := func(profile string) []byte {
		return []byte(strings.Replace(string(data), "model = \"test\"\n", "model = \"test\"\nfallback = \""+profile+"\"\n", 1))
	}
	fixturekit.Must(t, os.WriteFile(configPath, withFallback("missing"), 0600))
	reloadFails(t, c, configPath, "profiles.default.fallback")
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Digest != before.Digest || now.Effective.Profiles["default"].Fallback != "" || now.LastError == nil || now.LastError.Path != configPath || now.LastError.Field != "profiles.default.fallback" {
		t.Fatalf("after the failed reload: digest %s (was %s), last error %+v", now.Digest, before.Digest, now.LastError)
	}

	fixturekit.Must(t, os.WriteFile(configPath, withFallback("other"), 0600))
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if reloaded.Digest == before.Digest || now.Digest != reloaded.Digest || now.LastError != nil || now.Effective.Profiles["default"].Fallback != "other" || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload %+v; configuration digest %s, last error %+v", reloaded, now.Digest, now.LastError)
	}
	close(releaseClarify)
	f.awaitUnit(t, stream, "dedupe", intake.UnitReviewing)

	f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
	limited := completedTurn(t, f, stream, intake.MasonAgent("resume"), 0)
	if a := limited.Attempts; len(a) != 2 || a[0].Profile.Name != "default" || a[0].FailureClass != coreadapter.Infrastructure ||
		a[1].Profile.Name != "other" || a[1].Path != "replay" || !strings.HasPrefix(a[1].Reason, "fallback from profile default to other after 1 infrastructure failures: provider usage limit") || limited.Status() != "idle" {
		t.Fatalf("resume's attempts %+v", limited.Attempts)
	}
	mu.Lock()
	if !slices.Equal(resumeBackends, []string{agent.AgentClaude, agent.AgentCodex}) || resumeAttempts[1].ResumeID != "" || !strings.Contains(resumeAttempts[1].Prompt, "osmia-owned-log") {
		t.Fatalf("resume's sessions ran on %v: %+v", resumeBackends, resumeAttempts)
	}
	mu.Unlock()
	intakefixture.Eventually(t, "status never showed the mason's fallback profile under the usage limit", func() bool {
		st, err = c.Statuses(ctx)
		fixturekit.Must(t, err)
		return st.Profiles[intake.MasonRole] == (api.EffectiveProfile{Name: "other", Source: "provider_fallback", Reason: "Provider claude usage limit (rejected)"})
	})
	if l := st.ProviderLimits; len(l) != 1 || l[0].Backend != agent.AgentClaude || l[0].Status != "rejected" || l[0].Kind != "five_hour" || !l[0].ResetsAt.Equal(resumeLimitResets) {
		t.Fatalf("provider limits %+v", st.ProviderLimits)
	}

	f.awaitUnit(t, stream, "resume", intake.UnitApproved)
	_, approval := approvedReview(t, f.repository(), stream, "resume")
	base := approval.Identity.Candidate.BaseRevision
	select {
	case <-crashed:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the landing never reached its interruption boundary")
	}
	f.stop(t)
	moved := f.landedCommits(t, stream, base)
	if len(moved) != 1 {
		t.Fatalf("the interrupted landing left %v on the feature branch", moved)
	}
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	landings := landingByUnit(t, repository, stream)
	fixturekit.Must(t, repository.Close())
	if state.Value != intake.UnitApproved || len(landings) != 0 {
		t.Fatalf("after the crash resume is %s with landings %+v", state.Value, landings)
	}

	f.opts.boundary = nil
	f.start(t)
	c = f.c
	f.awaitMerged(t, stream, "resume")
	masons.Check(t)
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
	var landing api.UnitLanding
	fixturekit.Must(t, json.Unmarshal([]byte(landingByUnit(t, f.repository(), stream)["resume"].Content), &landing))
	if landing.Commit != moved[0] || landing.Operation != ops[0].Operation.ID || landing.Base != base {
		t.Fatalf("landing %+v; the feature branch holds %s", landing, moved[0])
	}
	merged := 0
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Subject == trace.UnitSubject("resume") && tr.To == intake.UnitMerged {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("resume merged %d times", merged)
	}
	if u := unitStatus(t, f, stream, "resume"); u.State != intake.UnitMerged || u.Landing == nil || u.Landing.Commit != moved[0] {
		t.Fatalf("status of resume %+v", u)
	}

	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again after the restart: %+v", p)
	}
	intakefixture.Eventually(t, "status after the restart lost the fallback profile or the daily budget", func() bool {
		st, err = c.Statuses(ctx)
		fixturekit.Must(t, err)
		return st.Profiles[intake.MasonRole].Source == "provider_fallback" && st.DailyBudget != nil && st.DailyBudget.SpendUSD == "1.25"
	})
	if cfg, err := c.Configuration(ctx); err != nil || cfg.Digest != reloaded.Digest {
		t.Fatalf("configuration after the restart: %v, digest %s, want %s", err, cfg.Digest, reloaded.Digest)
	}
	var ran []string
	for _, req := range masons.Requests(stream) {
		ran = append(ran, req.Name)
	}
	if want := []string{service.TestMasonTurnID("upload"), auditRecovered, service.TestMasonTurnID("dedupe"), dedupeClarified, service.TestMasonTurnID("resume")}; !slices.Equal(ran, want) {
		t.Fatalf("fake mason sessions %v, want %v", ran, want)
	}
}
