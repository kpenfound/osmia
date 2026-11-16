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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/recordspans"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The markers the telemetry journey seeds into content the factory handles.
// None of them may appear in any span.
const (
	journeyReport   = "zq-mason-report-marker"
	journeyReply    = "zq-mason-reply-marker"
	journeyEvidence = "zq-finding-evidence-marker"
	journeyAction   = "zq-finding-action-marker"
	journeyPause    = "zq-pause-reason-marker"
	journeyCheck    = "zq-check-output-marker"
)

// journeyContent is the text the journey's workstream carries that a span must
// never repeat: the plan's unit titles, the final report's summary and a
// prompt and a system prompt of a mason turn, besides the seeded markers.
var journeyContent = []string{
	journeyReport, journeyReply, journeyEvidence, journeyAction, journeyPause, journeyCheck,
	"Resume from the last chunk", "Send chunks", "Record acknowledged chunks", "Resumable uploads", reviewSummary,
	"Build unit resume of this workstream.", "You are a mason of the",
}

const (
	journeyMasons    = 2
	journeyReloaded  = 3
	journeyReviewers = 1
)

// telemetryJourney is a workstream driven from intake to its archive on a
// service that is restarted between the final review and its delivery.
type telemetryJourney struct {
	f      *shedFixture
	stream config.WorkstreamID
	// created is the workstream's recorded creation.
	created time.Time
	// pauseSetAt is when the pause that stays open across the restart was set.
	pauseSetAt time.Time
	// openState is the last workstream transition recorded before the restart:
	// the workstream state it entered is open across the restart.
	openState trace.Transition
	// beforeRestart holds the spans the first run exported by the time it stopped.
	beforeRestart servicetest.Spans
	// stopTook is the longest a stop of the service took.
	stopTook time.Duration
	// delivered reports the workstream reached delivery.
	delivered bool
	// sinkOn and sinkStats are the service's span sink before its last stop.
	sinkOn    bool
	sinkStats telemetry.Stats
}

// driveTelemetryJourney runs one workstream through intake, build and delivery
// with two mason slots held while the mason limit is raised, a queued review,
// a unit returned to its mason, a pause and a restart. A nil exporter runs it
// with telemetry off.
func driveTelemetryJourney(t *testing.T, exporter sdktrace.SpanExporter) *telemetryJourney {
	t.Helper()
	ctx := context.Background()
	checks := &fakeChecks{
		Links:    []string{"dag+check://release/version-round-trip", "dag+check://go/packages/tests/test?go-package=internal/web"},
		Outcomes: []checkOutcome{{Result: CheckResult{Output: passedReport + journeyCheck + "\n"}}},
	}
	// The service holds a pull request credential from its first start; the
	// host behind it is set up once the fixture has its clone.
	hosted := &fakePulls{}
	prepare := func(o *Options) {
		o.reviewChecks = checks
		o.PullRequests = hosted
		if exporter != nil {
			o.telemetryExporter = exporter
		}
	}
	capacity := fmt.Sprintf("masons = %d\nper_workstream = 3\nreviewers = %d\n", journeyMasons, journeyReviewers)
	f, masons := newMasonFixtureWith(t, config.WorkspacesGit, capacity, disjointPlan, prepare)
	intakefixture.ExtendParallelMasons(t, f.clone, f.repository(), f.clock.Now(), f.engine, masons, masonTurnID, ownerActor)
	defer f.stop(t)
	j := &telemetryJourney{f: f}

	selection := make([]jevtest.Result, 24)
	for i := range selection {
		selection[i] = jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.92), "check-1": jevtest.Noul(.05)}}}
	}
	useJevProvider(f.s, &jevtest.Provider{Results: selection})

	// The masons report with seeded text, and the first mason turns of two units
	// wait at a gate so both mason slots are held while the limit is raised.
	masons.Response = map[string]string{}
	units := []string{"resume", "upload", "audit"}
	for _, unit := range units {
		masons.Play[masonTurnID(unit)] = reportDone(journeyReport)
		masons.Response[masonTurnID(unit)] = journeyReply
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
	f.engine.Mu.Lock()
	chief := f.engine.Turns["*"]
	f.engine.Turns[masonTurnID("resume")] = gated
	f.engine.Turns[masonTurnID("upload")] = gated
	f.engine.Turns[masonTurnID("audit")] = gated
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, masonAgent("resume")+"-revise-"):
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/retry.go"), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := reportDone(journeyReport)(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: journeyReply, SessionDir: req.SessionDir, NumTurns: 1}, nil
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			if _, err := reviewtools.IdentityInPrompt(req.Prompt); err != nil {
				return nil, err
			}
			// The first review waits at a gate holding the only reviewer slot,
			// so the next review queues for one.
			if reviewing.Add(1) == 1 {
				select {
				case <-reviewGate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			verdict := map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}}
			if strings.Contains(req.Name, "resume") && resumeReviews.Add(1) == 1 {
				verdict = map[string]any{"decision": "material_findings", "summary": reviewSummary, "findings": []ReviewFinding{{Severity: "material", Evidence: journeyEvidence, Action: journeyAction}}}
			}
			body, err := callTool(ctx, tools, verdictTool, verdict)
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict: %s: %v", body, err)
			}
		case strings.HasPrefix(req.Name, "final-1-"):
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("final report: %s: %v", body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: journeyReply, SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()

	factory := runtime.Target{Scope: "factory"}
	// The service holds a pull request credential: its final review pushes
	// the feature branch to a local bare fork and opens the draft there.
	var stream config.WorkstreamID
	hostFork, host := hostService(t, f, func() config.WorkstreamID { return stream })
	hosted.Mu.Lock()
	hosted.Fork, hosted.Branches, hosted.SyncHead = host.Fork, host.Branches, host.SyncHead
	hosted.Mu.Unlock()
	f.s.options.PullRequests, f.opts.PullRequests = hosted, hosted
	stream = f.builtPaused(t, factory, "telemetry")[0]
	j.stream = stream
	var err error
	j.created, err = f.repository().WorkstreamCreated(stream)
	must(t, err)

	// A second pause replaces the first, and is released to start the work.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "owner", Reason: journeyPause})
	mutation(t, f.c, "DELETE", "pause", factory)

	// Two units hold both mason slots.
	for range journeyMasons {
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal("the masons did not both start")
		}
	}
	awaitRole(t, f, masonRole, journeyMasons, 0)
	// The mason limit rises while both slots are held, and the third unit takes
	// the slot the reload added.
	rewriteCapacity(t, f.opts.Config.Root, fmt.Sprintf("masons = %d\n", journeyMasons), fmt.Sprintf("masons = %d\n", journeyReloaded))
	_, err = f.c.Reload(ctx)
	must(t, err)
	awaitRole(t, f, masonRole, journeyReloaded, 0)
	close(masonGate)
	// The first units reported reach review: one holds the reviewer slot and
	// another waits for it.
	awaitRole(t, f, reviewerRole, journeyReviewers, 1)
	close(reviewGate)

	for _, unit := range units {
		f.awaitMerged(t, stream, unit)
	}
	f.awaitFeature(t, stream, AssembledState)
	deadline := time.Now().Add(demoTimeout)
	for len(streamDocuments(t, f.repository(), stream, finalReportDocument)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the final review did not report")
		}
		time.Sleep(50 * time.Millisecond)
	}
	masons.Check(t)

	// A pause stays open across the restart that follows.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "owner", Reason: journeyPause})
	rt, err := f.c.Runtime(ctx)
	must(t, err)
	for _, p := range rt.Effective.Pauses {
		if p.Target == factory {
			j.pauseSetAt = p.SetAt
		}
	}
	if j.pauseSetAt.IsZero() {
		t.Fatalf("runtime pauses %+v", rt.Effective.Pauses)
	}
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Subject == trace.FeatureSubject {
			j.openState = tr
		}
	}
	j.stop(t)
	if mem, ok := exporter.(interface{ GetSpans() tracetest.SpanStubs }); ok {
		j.beforeRestart = servicetest.Spans(mem.GetSpans())
	}

	// The owner approves delivery while the service is down.
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.setSole(&activeProject{repository: repository})
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, fork: hostFork, pulls: hosted}
	p.approve(t, nil)
	must(t, repository.Close())
	f.opts.PullRequests = p.pulls

	f.start(t)
	rt, err = f.c.Runtime(ctx)
	must(t, err)
	var kept bool
	for _, pause := range rt.Effective.Pauses {
		kept = kept || (pause.Target == factory && pause.SetAt.Equal(j.pauseSetAt))
	}
	if !kept {
		t.Fatalf("the pause did not survive the restart: %+v", rt.Effective.Pauses)
	}
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitFeature(t, stream, DeliveredState)
	j.delivered = true
	_, err = f.c.Archive(ctx, stream)
	must(t, err)

	j.sinkOn, j.sinkStats = service.InspectForTest(f.s.core()).Telemetry()
	j.stop(t)
	return j
}

// stop stops the service and keeps how long that took.
func (j *telemetryJourney) stop(t *testing.T) {
	t.Helper()
	began := time.Now()
	j.f.stop(t)
	j.stopTook = max(j.stopTook, time.Since(began))
}

// awaitRole waits until the role's pool reports the slots in use and, when
// waiting is positive, at least that many turns waiting for a slot.
func awaitRole(t *testing.T, f *shedFixture, role string, used, waiting int) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	var last string
	for time.Now().Before(deadline) {
		status, err := f.c.Statuses(context.Background())
		must(t, err)
		if status.Capacity != nil {
			for _, r := range status.Capacity.Roles {
				last = fmt.Sprintf("%s used %d waiting %d", r.Role, r.Used, len(r.Waiting))
				if r.Role == role && r.Used == used && len(r.Waiting) >= waiting {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the %s pool never reached %d in use and %d waiting; last %s", role, used, waiting, last)
}

// rewriteCapacity replaces old with new in the service's configuration file.
func rewriteCapacity(t *testing.T, root, old, new string) {
	t.Helper()
	path := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	if !strings.Contains(string(data), old) {
		t.Fatalf("configuration lacks %q:\n%s", old, data)
	}
	must(t, os.WriteFile(path, []byte(strings.Replace(string(data), old, new, 1)), 0600))
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

var recordspansUnitCauses = recordspans.UnitCauses

func hasAttr(sp tracetest.SpanStub, key string) bool {
	_, ok := servicetest.Attr(sp, key)
	return ok
}

func attrOf(sp tracetest.SpanStub, key string) string {
	v, _ := servicetest.Attr(sp, key)
	return v
}

// The running service sends every span of the contract for a workstream driven
// from intake to its archive: a unit returned to its mason with its cause, two
// held mason slots across a limit change, a queued review, checks with a
// covered count, a pause and a restart in the middle, and no content of the
// factory in any of them.
func TestTelemetryJourneySendsTheSpanContract(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	j := driveTelemetryJourney(t, exporter)
	spans := servicetest.SpansOf(exporter)
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

	// One root in the workstream's trace, from its recorded creation to its archive.
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

	// Workstream states leave no gap from creation to the end of the root, and
	// include an owner gate the owner ended.
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
	for _, want := range []string{AssembledState, DeliveredState, "building", "cause owner"} {
		if !seen[want] {
			fail("no workstream state span for %q: %v", want, seen)
		}
	}

	// The state open at the restart is sent once, after it, with its recorded
	// start and the ID its recorded entry gives.
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

	// Unit states: contiguous per unit, every cause from the bounded set, and
	// exactly one unit returned to its mason, with why.
	unitStates := own.Named("osmia.unit.state")
	causes := map[string]bool{}
	for _, c := range recordspansUnitCauses {
		causes[c] = true
	}
	byUnit := map[string]servicetest.Spans{}
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

	// Turns follow the GenAI conventions and carry no cost the backend did not report.
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

	// Checks carry the count they covered.
	checkRuns := own.Named("osmia.check.run")
	kinds := map[string]int{}
	for _, sp := range checkRuns {
		kinds[attrOf(sp, attrKind)]++
		// A final review's result is read from the pull request's statuses,
		// which select nothing, so only the other runs carry a covered count.
		final := attrOf(sp, attrKind) == "final"
		if attrOf(sp, attrOutcome) != "passed" || !final && (!hasAttr(sp, attrCovered) || attrOf(sp, attrCovered) == "0") || final && hasAttr(sp, attrCovered) {
			fail("check span %v", sp.Attributes)
		}
	}
	if kinds["unit"] < 4 {
		fail("check spans by kind %v", kinds)
	}

	// Slot spans: held and waited for, in every pool that did work.
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

	// Pool limits: one span per interval of constant limit for every configured
	// pool, used or not, and the mason limit changes at the reload while its
	// slots stay held.
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
		for run, list := range map[string]servicetest.Spans{"first": runOne.With(attrPool, pool), "second": runTwo.With(attrPool, pool)} {
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

	// Capacity follows from these spans alone: the mason pool is at capacity
	// while its slots meet the limit and not for the whole run, a pool that held
	// nothing is idle throughout, and the unbounded pools are busy or idle and
	// never at capacity.
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

	// Pauses cover the pause, never its reason; the one open at the restart is
	// sent after it with its recorded start and derived ID.
	pauses := spans.Named("osmia.pause")
	var open servicetest.Spans
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

	// No content, in any span name, attribute, event, status or resource.
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

// With telemetry off the same journey sends no spans, builds no sink and keeps
// no interval ledger.
func TestTelemetryOffJourneySendsNoSpans(t *testing.T) {
	t.Parallel()
	j := driveTelemetryJourney(t, nil)
	if !j.delivered || j.sinkOn || (j.sinkStats != telemetry.Stats{}) {
		t.Fatalf("delivered %v, sink on %v, counted %+v", j.delivered, j.sinkOn, j.sinkStats)
	}
	root := j.f.s.cfg.Root
	for _, ledger := range []func() (string, error){root.SlotIntervals, root.ServiceIntervals} {
		path, err := ledger()
		must(t, err)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("telemetry off wrote %s: %v", path, err)
		}
	}
}

// A collector that refuses every export, or never answers, neither stops the
// workstream nor holds the service past its bounded flush.
func TestTelemetryJourneyContinuesWhenTheCollectorRefusesOrHangs(t *testing.T) {
	t.Parallel()
	refusing := &servicetest.RefusingExporter{}
	hanging := servicetest.NewHangingExporter()
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

// Every configured pool has a pool limit span for the run, whether or not it
// did work: a service that is started and stopped with no work sends no slot
// span, and the idle pools show only as limit spans over the whole run.
func TestTelemetryServiceWithoutWorkSendsAPoolLimitSpanForEveryConfiguredPool(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	opts.telemetryExporter = exporter
	s, err := Start(context.Background(), opts)
	must(t, err)
	must(t, s.Close())
	spans := servicetest.SpansOf(exporter)
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
