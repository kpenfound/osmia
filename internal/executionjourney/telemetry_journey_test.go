package executionjourney

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/recordspans"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	journeyReport   = "zq-mason-report-marker"
	journeyReply    = "zq-mason-reply-marker"
	journeyEvidence = "zq-finding-evidence-marker"
	journeyAction   = "zq-finding-action-marker"
	journeyPause    = "zq-pause-reason-marker"
	journeyCheck    = "zq-check-output-marker"
)

var journeyContent = []string{
	journeyReport, journeyReply, journeyEvidence, journeyAction, journeyPause, journeyCheck,
	"Resume from the last chunk", "Send chunks", "Record acknowledged chunks", "Resumable uploads", fixturekit.ReviewSummary,
	"Build unit resume of this workstream.", "You are a mason of the",
}

const (
	journeyMasons    = 2
	journeyReloaded  = 3
	journeyReviewers = 1
)

type telemetryJourney struct {
	f      *journey.ShedFixture
	stream config.WorkstreamID

	created time.Time

	pauseSetAt time.Time

	openState trace.Transition

	beforeRestart fixturekit.Spans

	stopTook time.Duration

	delivered bool

	sinkOn    bool
	sinkStats telemetry.Stats
}

func driveTelemetryJourney(t *testing.T, exporter sdktrace.SpanExporter) *telemetryJourney {
	t.Helper()
	ctx := context.Background()
	checker := &fixturekit.FakeChecks{
		Links:    []string{"dag+check://release/version-round-trip", "dag+check://go/packages/tests/test?go-package=internal/web"},
		Outcomes: []fixturekit.CheckOutcome{{Result: api.CheckResult{Output: fixturekit.PassedReport + journeyCheck + "\n"}}},
	}

	hosted := &fixturekit.FakePulls{}
	prepare := func(o *journey.Options) {
		o.ReviewChecks = checker
		o.PullRequests = hosted
		if exporter != nil {
			o.TelemetryExporter = exporter
		}
	}
	capacity := fmt.Sprintf("masons = %d\nper_workstream = 3\nreviewers = %d\n", journeyMasons, journeyReviewers)
	f, masons := journey.NewMasonFixtureWith(t, config.WorkspacesGit, capacity, intakefixture.DisjointPlan, prepare)
	intakefixture.ExtendParallelMasons(t, f.Clone, f.Repository(), f.Clock.Now(), f.Engine, masons, execution.MasonTurnID, intake.OwnerActor)
	defer f.Stop(t)
	j := &telemetryJourney{f: f}

	selection := make([]jevtest.Result, 24)
	for i := range selection {
		selection[i] = jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.92), "check-1": jevtest.Noul(.05)}}}
	}
	useJevProvider(f.S, &jevtest.Provider{Results: selection})

	masons.Response = map[string]string{}
	units := []string{"resume", "upload", "audit"}
	for _, unit := range units {
		masons.Play[execution.MasonTurnID(unit)] = intakefixture.ReportDone(journeyReport)
		masons.Response[execution.MasonTurnID(unit)] = journeyReply
	}
	masonGate, reviewGate := make(chan struct{}), make(chan struct{})
	entered := make(chan string, 16)
	gated := func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		select {
		case entered <- req.Name:
		default:
		}
		select {
		case <-masonGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return masons.Turn(ctx, req, turn, tools)
	}
	var reviewing atomic.Int32
	var resumeReviews atomic.Int32
	f.Engine.Mu.Lock()
	chief := f.Engine.Turns["*"]
	f.Engine.Turns[execution.MasonTurnID("resume")] = gated
	f.Engine.Turns[execution.MasonTurnID("upload")] = gated
	f.Engine.Turns[execution.MasonTurnID("audit")] = gated
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, intake.MasonAgent("resume")+"-revise-"):
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/retry.go"), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := intakefixture.ReportDone(journeyReport)(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: journeyReply, SessionDir: req.SessionDir, NumTurns: 1}, nil
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			if _, err := reviewtools.IdentityInPrompt(req.Prompt); err != nil {
				return nil, err
			}

			if reviewing.Add(1) == 1 {
				select {
				case <-reviewGate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			verdict := map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}}
			if strings.Contains(req.Name, "resume") && resumeReviews.Add(1) == 1 {
				verdict = map[string]any{"decision": "material_findings", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{{Severity: "material", Evidence: journeyEvidence, Action: journeyAction}}}
			}
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, verdict)
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict: %s: %v", body, err)
			}
		case strings.HasPrefix(req.Name, "final-1-"):
			body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("final report: %s: %v", body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: journeyReply, SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()

	factory := runtime.Target{Scope: "factory"}

	var stream config.WorkstreamID
	hostFork, host := journey.HostService(t, f, func() config.WorkstreamID { return stream })
	hosted.Mu.Lock()
	hosted.Fork, hosted.Branches, hosted.SyncHead = host.Fork, host.Branches, host.SyncHead
	hosted.Mu.Unlock()
	f.S.Options.PullRequests, f.Opts.PullRequests = hosted, hosted
	stream = builtPaused(f, t, factory, "telemetry")[0]
	j.stream = stream
	var err error
	j.created, err = f.Repository().WorkstreamCreated(stream)
	fixturekit.Must(t, err)

	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: factory, Mode: "soft", Source: "owner", Reason: journeyPause})
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)

	for range journeyMasons {
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("the masons did not both start")
		}
	}
	awaitRole(t, f, intake.MasonRole, journeyMasons, 0)

	rewriteCapacity(t, f.Opts.Config.Root, fmt.Sprintf("masons = %d\n", journeyMasons), fmt.Sprintf("masons = %d\n", journeyReloaded))
	_, err = f.C.Reload(ctx)
	fixturekit.Must(t, err)
	awaitRole(t, f, intake.MasonRole, journeyReloaded, 0)
	close(masonGate)

	awaitRole(t, f, intake.ReviewerRole, journeyReviewers, 1)
	close(reviewGate)

	for _, unit := range units {
		f.AwaitMerged(t, stream, unit)
	}
	f.AwaitFeature(t, stream, intake.AssembledState)
	intakefixture.Soon(t, "the final review to report", func() bool {
		return len(intakefixture.StreamDocuments(t, f.Repository(), stream, delivery.FinalReportDocument)) >= 1
	})
	masons.Check(t)

	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: factory, Mode: "soft", Source: "owner", Reason: journeyPause})
	rt, err := f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	for _, p := range rt.Effective.Pauses {
		if p.Target == factory {
			j.pauseSetAt = p.SetAt
		}
	}
	if j.pauseSetAt.IsZero() {
		t.Fatalf("runtime pauses %+v", rt.Effective.Pauses)
	}
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Subject == trace.FeatureSubject {
			j.openState = tr
		}
	}
	j.stop(t)
	if mem, ok := exporter.(interface{ GetSpans() tracetest.SpanStubs }); ok {
		j.beforeRestart = fixturekit.Spans(mem.GetSpans())
	}

	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p := &journey.PublicationFixture{ShedFixture: f, Stream: stream, Repository: repository, Fork: hostFork, Pulls: hosted}
	p.Approve(t, nil)
	fixturekit.Must(t, repository.Close())
	f.Opts.PullRequests = p.Pulls

	f.Start(t)
	rt, err = f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	var kept bool
	for _, pause := range rt.Effective.Pauses {
		kept = kept || (pause.Target == factory && pause.SetAt.Equal(j.pauseSetAt))
	}
	if !kept {
		t.Fatalf("the pause did not survive the restart: %+v", rt.Effective.Pauses)
	}
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitFeature(t, stream, intake.DeliveredState)
	j.delivered = true
	_, err = f.C.Archive(ctx, stream)
	fixturekit.Must(t, err)

	j.sinkOn, j.sinkStats = service.InspectForTest(f.S.Core()).Telemetry()
	j.stop(t)
	return j
}

func (j *telemetryJourney) stop(t *testing.T) {
	t.Helper()
	began := time.Now()
	j.f.Stop(t)
	j.stopTook = max(j.stopTook, time.Since(began))
}

func awaitRole(t *testing.T, f *journey.ShedFixture, role string, used, waiting int) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("the %s pool to reach %d in use and %d waiting", role, used, waiting), func() bool {
		status, err := f.C.Statuses(context.Background())
		fixturekit.Must(t, err)
		if status.Capacity == nil {
			return false
		}
		for _, r := range status.Capacity.Roles {
			if r.Role == role && r.Used == used && len(r.Waiting) >= waiting {
				return true
			}
		}
		return false
	})
}

func rewriteCapacity(t *testing.T, root, old, new string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), old) {
		t.Fatalf("configuration lacks %q:\n%s", old, data)
	}
	fixturekit.Must(t, os.WriteFile(path, []byte(strings.Replace(string(data), old, new, 1)), 0600))
}

const (
	attrWorkstream = "osmia.workstream.id"
	attrUnit       = "osmia.unit.id"
	attrState      = "osmia.state"
	attrNext       = "osmia.next_state"
	attrCause      = "osmia.cause"
	attrOutcome    = "osmia.outcome"
	attrRole       = "osmia.role"
	attrPool       = "osmia.pool.id"
	attrLimit      = "osmia.pool.limit"
	attrUnbounded  = "osmia.pool.unbounded"
	attrCovered    = "osmia.covered"
	attrKind       = "osmia.kind"
)

func hasAttr(sp tracetest.SpanStub, key string) bool {
	_, ok := fixturekit.Attr(sp, key)
	return ok
}

func attrOf(sp tracetest.SpanStub, key string) string {
	v, _ := fixturekit.Attr(sp, key)
	return v
}

func TestTelemetryJourneySendsTheSpanContract(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	j := driveTelemetryJourney(t, exporter)
	spans := fixturekit.SpansOf(exporter)
	stream := string(j.stream)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
	}
	defer func() {
		if t.Failed() {
			t.Logf("spans:\n%s", spans.Summary())
		}
	}()

	roots := spans.Named("osmia.workstream").With(attrWorkstream, stream)
	if len(roots) != 1 {
		t.Fatalf("%d root spans", len(roots))
	}
	root := roots[0]
	rootID := telemetry.WorkstreamRoot(j.stream)
	if root.Parent.IsValid() || root.SpanContext.SpanID() != rootID || root.SpanContext.TraceID() != telemetry.WorkstreamTrace(j.stream) || !root.StartTime.Equal(j.created) || attrOf(root, attrCause) != "archived" {
		fail("root span %+v, created %v", root, j.created)
	}
	own := spans.With(attrWorkstream, stream)
	ids := map[string]bool{}
	for _, sp := range spans {
		key := sp.SpanContext.TraceID().String() + "/" + sp.SpanContext.SpanID().String()
		if ids[key] {
			fail("span %s was sent twice", sp.Name)
		}
		ids[key] = true
	}
	for _, sp := range own {
		if sp.SpanContext.TraceID() != telemetry.WorkstreamTrace(j.stream) {
			fail("%s is not in the workstream's trace", sp.Name)
		}
		if sp.SpanContext.SpanID() != rootID && sp.Parent.SpanID() != rootID {
			fail("%s is not a child of the root", sp.Name)
		}
	}

	states := own.Named("osmia.workstream.state").SortedByStart()
	if len(states) < 4 || !states[0].StartTime.Equal(j.created) || !states[len(states)-1].EndTime.Equal(root.EndTime) || attrOf(states[len(states)-1], attrNext) != "archived" {
		t.Fatalf("workstream states: %d", len(states))
	}
	seen := map[string]bool{}
	for i, sp := range states {
		seen[attrOf(sp, attrState)], seen["cause "+attrOf(sp, attrCause)] = true, true
		if i > 0 && (!sp.StartTime.Equal(states[i-1].EndTime) || attrOf(sp, attrState) != attrOf(states[i-1], attrNext)) {
			fail("state span %d (%s) does not follow %s", i, attrOf(sp, attrState), attrOf(states[i-1], attrNext))
		}
	}
	for _, want := range []string{intake.AssembledState, intake.DeliveredState, "building", "cause owner"} {
		if !seen[want] {
			fail("no workstream state span for %q: %v", want, seen)
		}
	}

	openID := telemetry.WorkstreamSpan(j.stream, "workstream-state", j.openState.ID)
	if got := j.beforeRestart.Where(func(sp tracetest.SpanStub) bool { return sp.SpanContext.SpanID() == openID }); len(got) != 0 {
		fail("the state open at the restart was sent before it ended")
	}
	if got := states.Where(func(sp tracetest.SpanStub) bool { return sp.SpanContext.SpanID() == openID }); len(got) != 1 || !got[0].StartTime.Equal(j.openState.At) || attrOf(got[0], attrState) != j.openState.To {
		fail("the state entered by %s was sent as %v", j.openState.ID, got)
	}
	if len(j.beforeRestart) == 0 || len(j.beforeRestart) >= len(spans) {
		fail("%d spans before the restart of %d", len(j.beforeRestart), len(spans))
	}
	for _, sp := range j.beforeRestart.With(attrWorkstream, stream) {
		if sp.SpanContext.TraceID() != telemetry.WorkstreamTrace(j.stream) {
			fail("%s sent before the restart is in another trace", sp.Name)
		}
	}

	unitStates := own.Named("osmia.unit.state")
	causes := map[string]bool{}
	for _, c := range recordspans.UnitCauses {
		causes[c] = true
	}
	byUnit := map[string]fixturekit.Spans{}
	for _, sp := range unitStates {
		byUnit[attrOf(sp, attrUnit)] = append(byUnit[attrOf(sp, attrUnit)], sp)
		if !causes[attrOf(sp, attrCause)] {
			fail("unit state span with cause %q", attrOf(sp, attrCause))
		}
	}
	if len(byUnit) != 3 {
		fail("unit state spans for %d units", len(byUnit))
	}
	for unit, list := range byUnit {
		list = list.SortedByStart()
		for i := 1; i < len(list); i++ {
			if !list[i].StartTime.Equal(list[i-1].EndTime) || attrOf(list[i], attrState) != attrOf(list[i-1], attrNext) {
				fail("unit %s: state span %d does not follow the one before", unit, i)
			}
		}
		if last := list[len(list)-1]; attrOf(last, attrNext) != "merged" || attrOf(last, attrCause) != "landed" {
			fail("unit %s ends %s because %s", unit, attrOf(last, attrNext), attrOf(last, attrCause))
		}
	}
	bounces := unitStates.With(attrNext, "implementing").With(attrCause, "review-rejected")
	if len(bounces) != 1 || attrOf(bounces[0], attrUnit) != "resume" || attrOf(bounces[0], attrState) != "reviewing" {
		fail("units returned to the mason: %v", bounces)
	}

	turns := own.Where(func(sp tracetest.SpanStub) bool { return strings.HasPrefix(sp.Name, "invoke_agent ") })
	roles := map[string]int{}
	for _, sp := range turns {
		role := attrOf(sp, attrRole)
		roles[role]++
		if sp.Name != "invoke_agent "+role || attrOf(sp, "gen_ai.operation.name") != "invoke_agent" || attrOf(sp, "gen_ai.agent.name") != role || !hasAttr(sp, "gen_ai.provider.name") || !hasAttr(sp, "gen_ai.request.model") ||
			!hasAttr(sp, "osmia.slot_wait_ms") || !hasAttr(sp, attrOutcome) || attrOf(sp, "osmia.cost_known") != "false" || hasAttr(sp, "osmia.cost_usd") {
			fail("turn span %s: %v", sp.Name, sp.Attributes)
		}
	}
	if roles["mason"] < 4 || roles["reviewer"] < 3 {
		fail("turn spans by role %v", roles)
	}

	checkRuns := own.Named("osmia.check.run")
	kinds := map[string]int{}
	for _, sp := range checkRuns {
		kinds[attrOf(sp, attrKind)]++

		final := attrOf(sp, attrKind) == "final"
		if attrOf(sp, attrOutcome) != "passed" || !final && (!hasAttr(sp, attrCovered) || attrOf(sp, attrCovered) == "0") || final && hasAttr(sp, attrCovered) {
			fail("check span %v", sp.Attributes)
		}
	}
	if kinds["unit"] < 4 {
		fail("check spans by kind %v", kinds)
	}

	slots := spans.Where(func(sp tracetest.SpanStub) bool { return strings.HasPrefix(sp.Name, "osmia.slot.") })
	slotRoles := map[string]int{}
	for _, sp := range slots {
		role := attrOf(sp, attrRole)
		slotRoles[role]++
		unbounded := attrOf(sp, attrUnbounded) == "true"
		if attrOf(sp, attrPool) != role || !hasAttr(sp, attrOutcome) || unbounded == hasAttr(sp, attrLimit) || attrOf(sp, attrLimit) == "0" || role == "architect" != unbounded {
			fail("slot span %s: %v", sp.Name, sp.Attributes)
		}
	}
	for _, role := range []string{"mason", "reviewer", "checks", "architect", "committee"} {
		if slotRoles[role] == 0 {
			fail("no slot span for %s: %v", role, slotRoles)
		}
	}
	held := slots.Named("osmia.slot.held")
	waits := slots.Named("osmia.slot.wait")
	reviewWaits := waits.With(attrRole, "reviewer").With(attrOutcome, "granted").With("osmia.wait_reason", "capacity").With(attrLimit, fmt.Sprint(journeyReviewers))
	if len(reviewWaits) == 0 {
		fail("no review waited for the reviewer slot until it was granted: %v", waits)
	}
	for _, sp := range held {
		if attrOf(sp, attrOutcome) != "released" {
			fail("held span %v", sp.Attributes)
		}
	}

	uptimes := spans.Named("osmia.service.uptime").SortedByStart()
	if len(uptimes) != 2 || len(spans.Named("osmia.service.start")) != 2 || !uptimes[0].EndTime.Before(uptimes[1].StartTime) && !uptimes[0].EndTime.Equal(uptimes[1].StartTime) {
		t.Fatalf("%d uptime spans", len(uptimes))
	}
	for _, u := range uptimes {
		if attrOf(u, attrOutcome) != "shutdown" || u.SpanContext.TraceID() != telemetry.FactoryTrace("service") {
			fail("uptime span %v", u.Attributes)
		}
	}
	limits := spans.Named("osmia.pool.limit")
	runOne := limits.Where(func(sp tracetest.SpanStub) bool { return sp.Parent.SpanID() == uptimes[0].SpanContext.SpanID() })
	runTwo := limits.Where(func(sp tracetest.SpanStub) bool { return sp.Parent.SpanID() == uptimes[1].SpanContext.SpanID() })
	for _, pool := range []string{"mason", "reviewer", "committee", "architect", "checks"} {
		for run, list := range map[string]fixturekit.Spans{"first": runOne.With(attrPool, pool), "second": runTwo.With(attrPool, pool)} {
			if len(list) == 0 {
				fail("no pool limit span for %s in the %s run", pool, run)
			}
			for _, sp := range list {
				if (pool == "architect") != (attrOf(sp, attrUnbounded) == "true") || hasAttr(sp, attrLimit) == (attrOf(sp, attrUnbounded) == "true") || attrOf(sp, attrLimit) == "0" {
					fail("pool limit span %v", sp.Attributes)
				}
			}
		}
	}
	masonLimits := runOne.With(attrPool, "mason").SortedByStart()
	if len(masonLimits) != 2 || attrOf(masonLimits[0], attrLimit) != fmt.Sprint(journeyMasons) || attrOf(masonLimits[1], attrLimit) != fmt.Sprint(journeyReloaded) ||
		!masonLimits[0].EndTime.Equal(masonLimits[1].StartTime) || attrOf(masonLimits[0], attrOutcome) != "reloaded" || attrOf(masonLimits[1], attrOutcome) != "shutdown" {
		t.Fatalf("mason limit spans of the first run: %v", masonLimits)
	}
	if two := runTwo.With(attrPool, "mason"); len(two) != 1 || attrOf(two[0], attrLimit) != fmt.Sprint(journeyReloaded) {
		fail("mason limit spans of the second run: %v", two)
	}
	reload := masonLimits[0].EndTime
	across := held.With(attrRole, "mason").Where(func(sp tracetest.SpanStub) bool {
		return attrOf(sp, attrLimit) == fmt.Sprint(journeyMasons) && sp.StartTime.Before(reload) && sp.EndTime.After(reload)
	})
	if len(across) != journeyMasons {
		fail("%d mason slots held across the reload", len(across))
	}
	if len(held.With(attrRole, "mason").With(attrLimit, fmt.Sprint(journeyReloaded))) == 0 {
		fail("no mason slot was granted under the raised limit")
	}

	times := spans.PoolTimes(uptimes[0].StartTime, uptimes[1].EndTime)
	for _, pool := range []string{"mason", "reviewer", "committee", "architect", "checks"} {
		pt := times[pool]
		unbounded := pool == "architect"
		if pt.Running <= 0 || pt.Busy+pt.Idle != pt.Running || unbounded && pt.AtCapacity != 0 || pt.AtCapacity > pt.Busy {
			fail("pool %s time %+v", pool, pt)
		}
		if len(held.With(attrPool, pool)) == 0 && pt.Idle != pt.Running {
			fail("pool %s held nothing and was idle %v of %v", pool, pt.Idle, pt.Running)
		}
	}
	if mason := times["mason"]; mason.AtCapacity <= 0 || mason.AtCapacity >= mason.Running {
		fail("mason pool at capacity %v of the %v it ran", mason.AtCapacity, mason.Running)
	}

	pauses := spans.Named("osmia.pause")
	var open fixturekit.Spans
	for _, sp := range pauses {
		if sp.StartTime.Equal(j.pauseSetAt) {
			open = append(open, sp)
		}
		if attrOf(sp, "osmia.pause.scope") != "factory" || attrOf(sp, "osmia.pause.source") != "owner" {
			fail("pause span %v", sp.Attributes)
		}
	}
	wantPause := telemetry.FactorySpan("service", "pause", "factory", "", "", "", "owner", strconv.FormatInt(j.pauseSetAt.UnixNano(), 10))
	if len(pauses) < 3 || len(open) != 1 || open[0].SpanContext.SpanID() != wantPause || open[0].EndTime.Before(uptimes[1].StartTime) || attrOf(open[0], attrOutcome) != "released" {
		fail("%d pause spans; the one open at the restart: %v", len(pauses), open)
	}

	if leaks := spans.Leaks(journeyContent...); len(leaks) != 0 {
		fail("content in spans:\n%s", strings.Join(leaks, "\n"))
	}
	for _, sp := range spans {
		if sp.Status.Description != "" || sp.Resource == nil || len(sp.Resource.Attributes()) != 2 {
			fail("span %s status %q resource %v", sp.Name, sp.Status.Description, sp.Resource)
		}
		for _, kv := range sp.Attributes {
			key, value := string(kv.Key), kv.Value.String()
			if !strings.HasPrefix(key, "osmia.") && !strings.HasPrefix(key, "gen_ai.") || strings.ContainsAny(value, " \t\n") {
				fail("span %s attribute %s=%q", sp.Name, key, value)
			}
		}
		for _, e := range sp.Events {
			if !strings.HasPrefix(e.Name, "osmia.") {
				fail("span %s event %s", sp.Name, e.Name)
			}
		}
	}
}

func TestTelemetryOffJourneySendsNoSpans(t *testing.T) {
	t.Parallel()
	j := driveTelemetryJourney(t, nil)
	if !j.delivered || j.sinkOn || (j.sinkStats != telemetry.Stats{}) {
		t.Fatalf("delivered %v, sink on %v, counted %+v", j.delivered, j.sinkOn, j.sinkStats)
	}
	root := j.f.S.Cfg.Root
	for _, ledger := range []func() (string, error){root.SlotIntervals, root.ServiceIntervals} {
		path, err := ledger()
		fixturekit.Must(t, err)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("telemetry off wrote %s: %v", path, err)
		}
	}
}

func TestTelemetryJourneyContinuesWhenTheCollectorRefusesOrHangs(t *testing.T) {
	t.Parallel()
	refusing := &fixturekit.RefusingExporter{}
	hanging := fixturekit.NewHangingExporter()
	t.Cleanup(hanging.Release)
	for name, exporter := range map[string]interface {
		sdktrace.SpanExporter
		Calls() int64
	}{"refusing": refusing, "hanging": hanging} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := driveTelemetryJourney(t, exporter)
			if !j.delivered || exporter.Calls() == 0 {
				t.Fatalf("delivered %v after %d export attempts", j.delivered, exporter.Calls())
			}
			if bound := telemetry.ShutdownWait + 3*time.Second; j.stopTook > bound {
				t.Fatalf("a stop took %v, over the bound of %v", j.stopTook, bound)
			}
		})
	}
}

func TestTelemetryServiceWithoutWorkSendsAPoolLimitSpanForEveryConfiguredPool(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	opts := journey.Fixture(t)
	opts.TelemetryExporter = exporter
	s, err := journey.Start(context.Background(), opts)
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.Close())
	spans := fixturekit.SpansOf(exporter)
	uptime := spans.Named("osmia.service.uptime")
	limits := spans.Named("osmia.pool.limit")
	if len(uptime) != 1 || len(limits) != 5 {
		t.Fatalf("%d uptime spans, %d pool limit spans:\n%s", len(uptime), len(limits), spans.Summary())
	}
	for _, pool := range []string{"mason", "reviewer", "committee", "architect", "checks"} {
		list := limits.With(attrPool, pool)
		if len(list) != 1 || !list[0].StartTime.Equal(uptime[0].StartTime) || !list[0].EndTime.Equal(uptime[0].EndTime) || list[0].Parent.SpanID() != uptime[0].SpanContext.SpanID() {
			t.Fatalf("pool %s limit spans %v, uptime %v", pool, list, uptime)
		}
		unbounded := pool == "architect"
		if unbounded != (attrOf(list[0], attrUnbounded) == "true") || unbounded == hasAttr(list[0], attrLimit) || attrOf(list[0], attrLimit) == "0" {
			t.Fatalf("pool %s limit span %v", pool, list[0].Attributes)
		}
	}
	for _, sp := range spans {
		if strings.HasPrefix(sp.Name, "osmia.slot.") && attrOf(sp, attrRole) != "architect" {
			t.Fatalf("a service with no work sent %s:\n%s", sp.Name, spans.Summary())
		}
	}
}
