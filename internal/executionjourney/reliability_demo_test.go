package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

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

func completedTurn(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, agent string, i int) trace.QueuedTurn {
	t.Helper()
	var th trace.Thread
	intakefixture.Eventually(t, fmt.Sprintf("turn %d of %s never completed", i, agent), func() bool {
		var err error
		th, err = f.Repository().Thread(stream, agent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil && len(th.Turns) > i && !th.Turns[i].CompletedAt.IsZero()
	})
	return th.Turns[i]
}

func factoryPauseOf(t *testing.T, c *journey.Client) (runtime.Pause, bool) {
	t.Helper()
	return intakefixture.FactoryPauseOf(t, c.Runtime)
}

var resumeLimitResets = fixturekit.DemoStart.Add(1000 * time.Hour)

type reliability struct {
	f          *journey.ShedFixture
	masons     *intakefixture.FakeMasons
	passed     *atomic.Int64
	problems   *demoProblems
	configPath string
}

func reliabilityPlan(t *testing.T, units ...string) string {
	t.Helper()
	type document struct {
		Version int               `json:"version"`
		Units   []json.RawMessage `json:"units"`
	}
	var all document
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.ReliabilityPlan), &all))
	kept := document{Version: all.Version}
	for _, raw := range all.Units {
		var unit struct {
			ID string `json:"id"`
		}
		fixturekit.Must(t, json.Unmarshal(raw, &unit))
		if slices.Contains(units, unit.ID) {
			kept.Units = append(kept.Units, raw)
		}
	}
	out, err := json.Marshal(kept)
	fixturekit.Must(t, err)
	return string(out)
}

func newReliability(t *testing.T, fallback bool, units ...string) *reliability {
	t.Helper()
	f, masons, passed := journey.NewParallelMasonFixtureCounting(t, 1, 4, reliabilityPlan(t, units...))
	r := &reliability{f: f, masons: masons, passed: passed, problems: &demoProblems{}, configPath: filepath.Join(f.Opts.Config.Root, "config.toml")}
	f.Stop(t)
	configText, err := os.ReadFile(r.configPath)
	fixturekit.Must(t, err)
	if !strings.Contains(string(configText), "per_workstream = 4\n") {
		t.Fatalf("config without the fixture's capacity:\n%s", configText)
	}
	text := strings.Replace(string(configText), "per_workstream = 4\n", "per_workstream = 4\nreviewers = 4\n", 1) + "[budget]\nper_day = \"1.00\"\n"
	if fallback {
		text = strings.Replace(text, "model = \"test\"\n", "model = \"test\"\nfallback = \"other\"\n", 1)
	}
	fixturekit.Must(t, os.WriteFile(r.configPath, []byte(text), 0600))
	f.Opts.Location = time.UTC

	enforced := journey.Enforce(f.Opts, service.Enforcement{Engine: f.Engine, Hosts: f.Opts.Committee.Hosts})
	f.Opts.Threads, f.Opts.Controls = enforced.Threads, enforced.Controls
	f.Engine.Mu.Lock()
	f.Engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	chief := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief(ctx, req, verified, tools)
	}
	f.Engine.Mu.Unlock()
	return r
}

func (r *reliability) start(t *testing.T) {
	t.Helper()
	r.f.Start(t)
	t.Cleanup(func() { r.f.Stop(t) })
}

func (r *reliability) play(unit string, play func(context.Context, agent.Request, *mcp.ClientSession) error) {
	r.masons.Mu.Lock()
	defer r.masons.Mu.Unlock()
	r.masons.Play[unit] = play
}

func (r *reliability) turn(name string, run fixturekit.DemoTurn) {
	r.f.Engine.Mu.Lock()
	defer r.f.Engine.Mu.Unlock()
	r.f.Engine.Turns[name] = run
}

func (r *reliability) costing(unit string) {
	r.turn(execution.MasonTurnID(unit), func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result, err := r.masons.Turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	})
}

func (r *reliability) limiting(unit string) (backends func() []string, attempts func() []agent.Request) {
	var mu sync.Mutex
	var seenBackends []string
	var seenAttempts []agent.Request
	r.turn(execution.MasonTurnID(unit), func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		seenAttempts = append(seenAttempts, req)
		seenBackends = append(seenBackends, req.Profile.Agent)
		mu.Unlock()
		if req.Profile.Agent == agent.AgentClaude {
			return &agent.Result{ClaudeID: "session-limited", SessionDir: req.SessionDir, IsError: true, RateLimit: &agent.RateLimit{Status: "rejected", Type: "five_hour", ResetsAt: resumeLimitResets}}, nil
		}
		return r.masons.Turn(ctx, req, verified, tools)
	})
	return func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(seenBackends) },
		func() []agent.Request { mu.Lock(); defer mu.Unlock(); return slices.Clone(seenAttempts) }
}

func (r *reliability) awaitBudgetPause(t *testing.T) runtime.Pause {
	t.Helper()
	var pause runtime.Pause
	intakefixture.Eventually(t, "the daily budget never paused the factory", func() bool {
		var ok bool
		pause, ok = factoryPauseOf(t, r.f.C)
		return ok
	})
	return pause
}

func (r *reliability) awaitProviderFallback(t *testing.T) api.StatusResponse {
	t.Helper()
	var st api.StatusResponse
	intakefixture.Eventually(t, "status never showed the mason's fallback profile under the usage limit", func() bool {
		var err error
		st, err = r.f.C.Statuses(context.Background())
		fixturekit.Must(t, err)
		return st.Profiles[intake.MasonRole] == (api.EffectiveProfile{Name: "other", Source: "provider_fallback", Reason: "Provider claude usage limit (rejected)"})
	})
	return st
}

func TestDailyBudgetPausesDispatchUntilTheOwnerResumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReliability(t, false, "upload", "audit")
	f := r.f
	auditEntered := make(chan struct{})
	var once sync.Once
	r.play(execution.MasonTurnID("upload"), intakefixture.ReportDone("Chunks are sent"))
	r.play(execution.MasonTurnID("audit"), intakefixture.ReportDone("Acknowledgements are recorded"))
	r.costing("upload")
	r.turn(execution.MasonTurnID("audit"), func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() { close(auditEntered) })
		return r.masons.Turn(ctx, req, verified, tools)
	})
	r.start(t)
	c := f.C

	stream, _ := f.BuiltAs(t, "reliability")
	f.AwaitUnit(t, stream, "upload", execution.UnitChecking)
	budgetPause := r.awaitBudgetPause(t)
	if budgetPause.Mode != "soft" || budgetPause.Source != runtime.PauseDailyBudget || !strings.HasPrefix(budgetPause.Reason, "Daily budget reached: known spend on 2026-09-16 is USD 1.25 of the USD 1.00 per-day budget; dispatch resumes at the next local day; ") || !strings.HasSuffix(budgetPause.Reason, " attempt(s) have unknown cost, so actual spend may be higher") {
		t.Fatalf("budget pause %+v", budgetPause)
	}
	intakefixture.Eventually(t, "status never showed the daily budget", func() bool {
		st, err := c.Statuses(ctx)
		fixturekit.Must(t, err)
		b := st.DailyBudget
		return b != nil && b.Day == "2026-09-16" && b.SpendUSD == "1.25" && b.LimitUSD == "1.00" && b.LowerBound && b.UnknownCosts != 0
	})
	budgetWait := api.UnitDispatch{Reason: execution.DeferPaused, Pause: &runtime.Pause{Target: factoryTarget}, Message: "Waits while a factory pause is in force, set by daily-budget."}
	intakefixture.Eventually(t, "audit never waited for the budget pause", func() bool {
		return reflect.DeepEqual(journey.UnitStatus(t, f, stream, "audit"), deferred(f, t, stream, "audit", budgetWait))
	})
	intakefixture.AwaitPasses(t, r.passed, 3)
	intakefixture.Settled(t, func() int { return len(journey.Starts(t, f, stream)) })
	if got := journey.Starts(t, f, stream); !slices.Equal(got, []string{trace.UnitSubject("upload")}) {
		t.Fatalf("units started under the budget pause: %v", got)
	}

	intakefixture.Mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(factoryTarget))
	select {
	case <-auditEntered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("audit's mason never started after the owner resumed")
	}
	f.AwaitUnit(t, stream, "audit", intake.UnitReviewing)
	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again the same day: %+v", p)
	}
}

func TestHardPauseStopsAMasonAndResumingContinuesItsSession(t *testing.T) {
	t.Parallel()
	r := newReliability(t, false, "audit", "resume")
	f, masons := r.f, r.masons
	auditRecovered := intake.MasonAgent("audit") + "-recover-1"
	const auditFile = "internal/audit/audit.go"
	var mu sync.Mutex
	var auditContinuation agent.Request
	auditEntered := make(chan struct{})
	r.play(auditRecovered, func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), auditFile)); err != nil {
			return fmt.Errorf("the continuation lost the stopped turn's file: %v", err)
		}
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledgements are recorded"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	})
	r.play(execution.MasonTurnID("resume"), intakefixture.ReportDone("Uploads resume"))
	r.turn(execution.MasonTurnID("audit"), func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		path := filepath.Join(req.Workspace.Directory(), auditFile)
		if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0755), os.WriteFile(path, []byte("package audit\n"), 0644)); err != nil {
			return nil, err
		}
		close(auditEntered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-audit", ResultText: "Half of audit is built", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, Signal: 15}, nil
	})
	r.turn(auditRecovered, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		auditContinuation = req
		mu.Unlock()
		return masons.Turn(ctx, req, verified, tools)
	})
	r.start(t)
	c := f.C

	stream, _ := f.BuiltAs(t, "reliability")
	select {
	case <-auditEntered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("audit's mason never started")
	}
	workstream := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}
	intakefixture.Mutation(t, c, "PUT", "pause", api.PauseRequest{Target: workstream, Mode: "hard", Reason: "Stop the mason", Source: "owner"})
	stopped := completedTurn(t, f, stream, intake.MasonAgent("audit"), 0)
	intakefixture.CheckPauseStop(t, stopped, "workstream", "Stop the mason")
	attempts := func() int {
		n := 0
		for _, q := range f.Thread(t, stream, intake.MasonAgent("audit")).Turns[1:] {
			n += len(q.Attempts)
		}
		return n
	}
	intakefixture.AwaitPasses(t, r.passed, 3)
	intakefixture.Settled(t, attempts)
	for _, q := range f.Thread(t, stream, intake.MasonAgent("audit")).Turns[1:] {
		if len(q.Attempts) != 0 {
			t.Fatalf("turn %s of audit's mason ran under the hard pause: %+v", q.Request.TurnID, q.Attempts)
		}
	}
	if u := journey.UnitStatus(t, f, stream, "audit"); u.State != intake.UnitImplementing {
		t.Fatalf("stopped audit is %+v", u)
	}

	intakefixture.Mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(workstream))
	f.AwaitUnit(t, stream, "audit", intake.UnitReviewing)
	recovered := completedTurn(t, f, stream, intake.MasonAgent("audit"), 1)
	if recovered.Request.TurnID != auditRecovered || recovered.Request.ThreadID != stopped.Request.ThreadID || !strings.Contains(recovered.Request.Prompt, "A hard pause stopped your last turn.") || recovered.Status() != "idle" {
		t.Fatalf("audit's continuation %+v", recovered)
	}
	if a := recovered.Attempts; len(a) != 1 || a[0].Path != "resume" || a[0].SourceSession.ID != "session-audit" || a[0].SourceSequence != stopped.Sequence {
		t.Fatalf("audit's continuation did not resume the stopped session: %+v", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if auditContinuation.ResumeID != "session-audit" {
		t.Fatalf("audit's continuation ran as %+v", auditContinuation)
	}
}

func TestOwnerProfileSwitchDuringATurnAppliesToTheNextTurnWithAReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReliability(t, false, "dedupe", "resume")
	f, masons := r.f, r.masons
	dedupeClarified := intake.MasonAgent("dedupe") + "-clarify-1"
	var mu sync.Mutex
	var dedupeContinuation agent.Request
	var dedupeBackends []string
	clarifyEntered, releaseClarify := make(chan struct{}), make(chan struct{})
	masons.Mu.Lock()
	masons.Response = map[string]string{execution.MasonTurnID("dedupe"): "Half of dedupe is built"}
	masons.Mu.Unlock()
	r.play(dedupeClarified, func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	})
	r.play(execution.MasonTurnID("resume"), intakefixture.ReportDone("Uploads resume"))
	r.turn(execution.MasonTurnID("dedupe"), func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		dedupeBackends = append(dedupeBackends, req.Profile.Agent)
		mu.Unlock()
		var ack api.MutationResponse
		if err := f.C.Do(ctx, "PUT", service.Prefix+"/runtime/profile", api.ProfileRequest{Role: intake.MasonRole, Profile: "other"}, &ack); err != nil || !ack.Applied {
			r.problems.report("profile switch during dedupe's turn: %v %+v", err, ack)
		}
		return masons.Turn(ctx, req, verified, tools)
	})
	r.turn(dedupeClarified, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
	})
	r.start(t)
	c := f.C

	stream, _ := f.BuiltAs(t, "reliability")
	select {
	case <-clarifyEntered:
	case <-time.After(fixturekit.DemoTimeout):
		r.problems.check(t)
		t.Fatal("dedupe's follow-up never started")
	}
	r.problems.check(t)
	rt, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if rt.Profiles[intake.MasonRole] != (api.EffectiveProfile{Name: "other", Source: "owner_override"}) {
		t.Fatalf("mason profile after the switch: %+v", rt.Profiles[intake.MasonRole])
	}
	th := f.Thread(t, stream, intake.MasonAgent("dedupe"))
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

	intakefixture.Mutation(t, c, "DELETE", "profile", api.ClearProfileRequest{Role: intake.MasonRole})
	close(releaseClarify)
	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
}

func TestAUnitWaitsWhileAnotherHoldsTheOnlyMasonSlot(t *testing.T) {
	t.Parallel()
	r := newReliability(t, false, "dedupe", "resume")
	f, masons := r.f, r.masons
	holding, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	r.play(execution.MasonTurnID("dedupe"), intakefixture.ReportDone("Acknowledged chunks are skipped"))
	r.play(execution.MasonTurnID("resume"), intakefixture.ReportDone("Uploads resume"))
	r.turn(execution.MasonTurnID("dedupe"), func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		close(holding)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return masons.Turn(ctx, req, verified, tools)
	})
	r.start(t)

	stream, _ := f.BuiltAs(t, "reliability")
	select {
	case <-holding:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("dedupe's mason never started")
	}
	if u := journey.UnitStatus(t, f, stream, "resume"); !reflect.DeepEqual(u, deferred(f, t, stream, "resume", slotless(1))) {
		t.Fatalf("resume while dedupe holds the slot: %+v", u)
	}
	close(release)
	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
}

func TestReloadRefusesAnUnknownFallbackProfileAndAppliesAKnownOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReliability(t, false, "upload", "audit", "dedupe", "resume")
	r.start(t)
	c := r.f.C

	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	data, err := os.ReadFile(r.configPath)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), "model = \"test\"\n") {
		t.Fatalf("configuration without the default profile's model:\n%s", data)
	}
	withFallback := func(profile string) []byte {
		return []byte(strings.Replace(string(data), "model = \"test\"\n", "model = \"test\"\nfallback = \""+profile+"\"\n", 1))
	}
	fixturekit.Must(t, os.WriteFile(r.configPath, withFallback("missing"), 0600))
	intakefixture.ReloadFails(t, c.Reload, r.configPath, "profiles.default.fallback")
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Digest != before.Digest || now.Effective.Profiles["default"].Fallback != "" || now.LastError == nil || now.LastError.Path != r.configPath || now.LastError.Field != "profiles.default.fallback" {
		t.Fatalf("after the failed reload: digest %s (was %s), last error %+v", now.Digest, before.Digest, now.LastError)
	}

	fixturekit.Must(t, os.WriteFile(r.configPath, withFallback("other"), 0600))
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if reloaded.Digest == before.Digest || now.Digest != reloaded.Digest || now.LastError != nil || now.Effective.Profiles["default"].Fallback != "other" || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload %+v; configuration digest %s, last error %+v", reloaded, now.Digest, now.LastError)
	}
}

func TestProviderUsageLimitFallsBackToTheConfiguredProfileWithAReplay(t *testing.T) {
	t.Parallel()
	r := newReliability(t, true, "audit", "resume")
	f := r.f
	backends, attempts := r.limiting("resume")
	r.play(execution.MasonTurnID("audit"), intakefixture.ReportDone("Acknowledgements are recorded"))
	r.play(execution.MasonTurnID("resume"), intakefixture.ReportDone("Uploads resume"))
	r.start(t)

	stream, _ := f.BuiltAs(t, "reliability")
	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	limited := completedTurn(t, f, stream, intake.MasonAgent("resume"), 0)
	if a := limited.Attempts; len(a) != 2 || a[0].Profile.Name != "default" || a[0].FailureClass != coreadapter.Infrastructure ||
		a[1].Profile.Name != "other" || a[1].Path != "replay" || !strings.HasPrefix(a[1].Reason, "fallback from profile default to other after 1 infrastructure failures: provider usage limit") || limited.Status() != "idle" {
		t.Fatalf("resume's attempts %+v", limited.Attempts)
	}
	if got := attempts(); !slices.Equal(backends(), []string{agent.AgentClaude, agent.AgentCodex}) || got[1].ResumeID != "" || !strings.Contains(got[1].Prompt, "osmia-owned-log") {
		t.Fatalf("resume's sessions ran on %v: %+v", backends(), got)
	}
	st := r.awaitProviderFallback(t)
	if l := st.ProviderLimits; len(l) != 1 || l[0].Backend != agent.AgentClaude || l[0].Status != "rejected" || l[0].Kind != "five_hour" || !l[0].ResetsAt.Equal(resumeLimitResets) {
		t.Fatalf("provider limits %+v", st.ProviderLimits)
	}
}

func TestReliabilityDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newReliability(t, false, "upload", "audit", "resume")
	f, masons := r.f, r.masons
	const crash = "crash: the feature branch moved"
	crashed := make(chan struct{})
	f.Opts.Boundary = func(lifetime context.Context, name string) error {
		if name == "land-advanced" {
			close(crashed)
			<-lifetime.Done()
			return errors.New(crash)
		}
		return nil
	}
	r.play(execution.MasonTurnID("upload"), intakefixture.ReportDone("Chunks are sent"))
	r.play(execution.MasonTurnID("audit"), intakefixture.ReportDone("Acknowledgements are recorded"))
	r.play(execution.MasonTurnID("resume"), intakefixture.ReportDone("Uploads resume"))
	r.costing("upload")
	r.limiting("resume")
	r.start(t)
	c := f.C

	stream, _ := f.BuiltAs(t, "reliability")
	f.AwaitUnit(t, stream, "upload", execution.UnitChecking)
	r.awaitBudgetPause(t)
	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	data, err := os.ReadFile(r.configPath)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(r.configPath, []byte(strings.Replace(string(data), "model = \"test\"\n", "model = \"test\"\nfallback = \"other\"\n", 1)), 0600))
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	if reloaded.Digest == before.Digest || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload %+v, digest before %s", reloaded, before.Digest)
	}
	intakefixture.Mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(factoryTarget))
	f.AwaitUnit(t, stream, "resume", intake.UnitApproved)
	r.awaitProviderFallback(t)
	_, approval := journey.ApprovedReview(t, f.Repository(), stream, "resume")
	base := approval.Identity.Candidate.BaseRevision
	select {
	case <-crashed:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the landing never reached its interruption boundary")
	}
	f.Stop(t)
	moved := landedCommits(f, t, stream, base)
	if len(moved) != 1 {
		t.Fatalf("the interrupted landing left %v on the feature branch", moved)
	}
	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	landings := journey.LandingByUnit(t, repository, stream)
	fixturekit.Must(t, repository.Close())
	if state.Value != intake.UnitApproved || len(landings) != 0 {
		t.Fatalf("after the crash resume is %s with landings %+v", state.Value, landings)
	}

	f.Opts.Boundary = nil
	f.Start(t)
	c = f.C
	f.AwaitMerged(t, stream, "resume")
	masons.Check(t)
	r.problems.check(t)
	if commits := landedCommits(f, t, stream, base); !slices.Equal(commits, moved) {
		t.Fatalf("the feature branch holds %v; the interrupted landing made %v", commits, moved)
	}
	ops := journey.LandOperations(t, f.Repository(), stream)
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
	fixturekit.Must(t, json.Unmarshal([]byte(journey.LandingByUnit(t, f.Repository(), stream)["resume"].Content), &landing))
	if landing.Commit != moved[0] || landing.Operation != ops[0].Operation.ID || landing.Base != base {
		t.Fatalf("landing %+v; the feature branch holds %s", landing, moved[0])
	}
	merged := 0
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Subject == trace.UnitSubject("resume") && tr.To == intake.UnitMerged {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("resume merged %d times", merged)
	}
	if u := journey.UnitStatus(t, f, stream, "resume"); u.State != intake.UnitMerged || u.Landing == nil || u.Landing.Commit != moved[0] {
		t.Fatalf("status of resume %+v", u)
	}

	if p, ok := factoryPauseOf(t, c); ok {
		t.Fatalf("the budget paused the factory again after the restart: %+v", p)
	}
	intakefixture.Eventually(t, "status after the restart lost the fallback profile or the daily budget", func() bool {
		st, err := c.Statuses(ctx)
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
	if want := []string{execution.MasonTurnID("upload"), execution.MasonTurnID("audit"), execution.MasonTurnID("resume")}; !slices.Equal(ran, want) {
		t.Fatalf("fake mason sessions %v, want %v", ran, want)
	}
}
