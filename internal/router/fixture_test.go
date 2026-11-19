package router_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	projectID config.ProjectID    = "p_00000000000000000000000000000001"
	streamID  config.WorkstreamID = "w_00000000000000000000000000000001"
	unitID                        = "u1"
)

var seeds atomic.Int64

var (
	epoch    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	owner    = trace.Actor{Kind: "owner", ID: "local"}
	checkers = trace.Actor{Kind: "service", ID: "checks"}
)

// fx is a workstream's trace with a router whose Jev is the fixture provider.
type fx struct {
	t        *testing.T
	root     config.Root
	project  config.Project
	repo     *trace.Repository
	exporter *tracetest.InMemoryExporter
	sink     *telemetry.Sink
	provider *jevfixture.Transitions
	judge    *jev.Judge
	effects  *router.Effects
	router   *router.Router
	// applied counts the effects that ran, by target.
	applied map[string]*atomic.Int32
}

func newFx(t *testing.T, provider *jevfixture.Transitions) *fx {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "target")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	f := &fx{t: t, root: root, project: config.Project{ID: projectID, Clone: clone}, exporter: tracetest.NewInMemoryExporter(), provider: provider, applied: map[string]*atomic.Int32{}}
	if f.sink, err = telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: f.exporter}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.sink.Close() })
	if f.repo, err = trace.Create(context.Background(), root, f.project, epoch, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.repo.Close() })
	if err := f.repo.CreateWorkstream(context.Background(), streamID, epoch, owner); err != nil {
		t.Fatal(err)
	}
	f.effects = &router.Effects{}
	f.build()
	return f
}

// build makes a router and a Jev over the current trace and provider, as a
// restarted service has.
func (f *fx) build() {
	f.judge = jevfixture.TransitionJudge(nil, f.provider)
	f.router = &router.Router{Judge: f.judge, Effects: f.effects, Spans: func(r router.SpanRoute) {
		recordspans.Recorder{Sink: f.sink}.Route(recordspans.Route{Project: r.Project, Workstream: r.Workstream, Unit: r.Unit, Occurrence: r.Occurrence, Trigger: r.Trigger,
			Default: r.Default, Chosen: r.Chosen, Outcome: r.Outcome, Confidence: r.Confidence, Judgment: r.Judgment, Start: r.Start, End: r.End})
	}}
}

// restart closes the trace and opens it again with a new router and Jev.
func (f *fx) restart() {
	f.t.Helper()
	if err := f.repo.Close(); err != nil {
		f.t.Fatal(err)
	}
	repo, err := trace.Open(f.root, f.project)
	if err != nil {
		f.t.Fatal(err)
	}
	f.repo = repo
	f.t.Cleanup(func() { repo.Close() })
	f.build()
}

// seed moves the unit to state, as the machine did before the exit.
func (f *fx) seed(to string) {
	f.t.Helper()
	subject := trace.UnitSubject(unitID)
	state, err := f.repo.Workflow(streamID, subject)
	if err != nil {
		f.t.Fatal(err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("seed-%s-%d", to, seeds.Add(1)), Revision: 1, Project: projectID, Workstream: streamID, Unit: unitID, At: epoch, Actor: checkers, Cause: "seed"}
	if _, err := f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "seeded"}}); err != nil {
		f.t.Fatal(err)
	}
}

// exit is the exit of a unit whose checks failed: today's default sends the
// unit back to its mason; it can instead run the checks again or raise a
// contest, and a redraft-like target needs the strict bar.
func (f *fx) exit() router.Exit {
	state, err := f.repo.Workflow(streamID, trace.UnitSubject(unitID))
	if err != nil {
		f.t.Fatal(err)
	}
	return router.Exit{
		Repo: f.repo, Project: projectID, Workstream: streamID, Unit: unitID, Machine: "unit", Subject: trace.UnitSubject(unitID),
		From: state.Value, Version: state.Version, Trigger: "unit.checks_failed", Cause: "checks-1", Actor: checkers, At: epoch,
		Default: "implementing",
		Targets: []router.Target{
			{Name: "implementing", Description: "Send the unit back to its mason with the failures.", Bar: router.BarModest},
			{Name: "checking", Description: "Run the checks again on the same candidate.", Bar: router.BarModest, NeedsCandidate: true,
				Corroborate: []router.Condition{{Question: "environment", AtLeast: router.LevelModest}}},
			{Name: "contested", Description: "Hold the unit for the owner.", Bar: router.BarModest},
			{Name: "redrafted", Description: "Redraft the unit from the plan.", Bar: router.BarStrict},
			{Name: "redrafted", Description: "Redraft the unit from the plan.", Bar: router.BarStrict},
			{Name: "approved", Description: "Approve the candidate.", Approval: true, NeedsCandidate: true},
			{Name: "abandoned", Description: "Abandon the unit.", Gate: true},
			{Name: "redrafted", Description: "Redraft the unit from the plan.", Bar: router.BarStrict},
		},
		Questions: []router.Question{{Name: "environment", Instructions: "The failure is infrastructure or flaky, not the change."}},
		Evidence: router.Evidence{Intent: "The unit fixes the parser.", Facts: map[string]any{"bounces": 1, "max_bounces": 3},
			Records: []router.Entry{{Kind: "check-run", ID: "checks-1", Text: "TestParse failed: want 3, got 2"}}},
		Prereq: router.Prerequisites{Candidate: func(context.Context) (string, error) { return "", nil }},
	}
}

// track registers an effect for every unit target that records a notice, and
// counts how often each ran.
func (f *fx) track(targets ...string) {
	for _, target := range targets {
		n := &atomic.Int32{}
		f.applied[target] = n
		f.effects.Register("unit", target, func(_ context.Context, p router.Plan) (router.Effect, error) {
			n.Add(1)
			return router.Effect{Events: []trace.Event{trace.Notice(p.TransitionID, "unit", "Unit u1 entered "+p.Target.Name+".")}}, nil
		})
	}
}

func (f *fx) ran(target string) int32 {
	if n, ok := f.applied[target]; ok {
		return n.Load()
	}
	return 0
}

func (f *fx) state() trace.WorkflowState {
	f.t.Helper()
	s, err := f.repo.Workflow(streamID, trace.UnitSubject(unitID))
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// moves returns the unit's transitions that the router chose.
func (f *fx) moves() []trace.Transition {
	f.t.Helper()
	all, err := trace.Read[trace.Transition](f.repo, streamID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []trace.Transition
	for _, tr := range all {
		if tr.Subject == trace.UnitSubject(unitID) && tr.DecidedBy != "" {
			out = append(out, tr)
		}
	}
	return out
}

// notices returns the chief-of-staff notices the route journals sent.
func (f *fx) notices() []string {
	f.t.Helper()
	entries, err := f.repo.Outbox(streamID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Event.Kind != trace.NoticeKind || !strings.HasPrefix(e.TransitionID, "route-") {
			continue
		}
		for _, suffix := range []string{"-held", "-owner", "-refused"} {
			if strings.HasSuffix(e.TransitionID, suffix) {
				out = append(out, e.Event.Body)
			}
		}
	}
	return out
}

// routeSpans returns the osmia.transition.route spans sent so far.
func (f *fx) routeSpans() []tracetest.SpanStub {
	f.t.Helper()
	if err := f.sink.Flush(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	var out []tracetest.SpanStub
	for _, s := range f.exporter.GetSpans() {
		if s.Name == "osmia.transition.route" {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.String()
		}
	}
	return ""
}

// request returns the state of a transition request as a map, and its option
// names in the order of the first pick question.
func request(t *testing.T, p *jevfixture.Transitions, n int) (state map[string]any, options []string) {
	t.Helper()
	reqs := p.TransitionRequests()
	if len(reqs) <= n {
		t.Fatalf("%d transition requests, want more than %d", len(reqs), n)
	}
	raw, err := json.Marshal(reqs[n].State)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for _, o := range reqs[n].Questions[jev.QuestionPickA].Options {
		options = append(options, o.Name)
	}
	return state, options
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type atomicInt = atomic.Int32

// refused returns the judgment records that were marked refused.
func refused(t *testing.T, f *fx) []jev.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, streamID)
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "judgments/") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []jev.Record
	for _, d := range latest {
		var r jev.Record
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			t.Fatal(err)
		}
		if r.Refused != "" {
			out = append(out, r)
		}
	}
	return out
}
