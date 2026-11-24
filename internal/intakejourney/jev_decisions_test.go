package intakejourney

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/doctor"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func newJevJourney(t *testing.T, p *jevfixture.Provider, on bool) *journey.ShedFixture {
	t.Helper()
	key := jevfixture.Key(t, on)
	return journey.NewDebateFixtureWith(t, 1, 1, jevfixture.Config(key), func(o *journey.Options) {
		if p != nil {
			o.JevProvider = p.Factory()
		}
	})
}

func TestAConfidentJevRatifiesAndTheWorkstreamBuilds(t *testing.T) {
	t.Parallel()
	p := &jevfixture.Provider{Confidence: 0.97}
	f := newJevJourney(t, p, true)
	defer f.Stop(t)
	ctx := context.Background()

	stream := f.ReachJevPacket(t)
	f.Scenario().AwaitFeature(t, stream, intake.BuildingState)

	if runtime, err := f.C.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeReady {
		t.Fatalf("Jev in the status: %+v %v", runtime.Jev, err)
	}
	requests := p.Decisions()
	if len(requests) != 1 || !slices.Equal(jevfixture.Options(requests[0]), []string{"ratify", jev.Defer}) {
		t.Fatalf("decision requests: %d, options %v", len(requests), requests)
	}

	docs := f.Documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 1 || docs[0].Actor != trace.JevActor {
		t.Fatalf("ratification records: %+v", docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if record.DecidedBy != trace.DecidedByJev || record.Judgment == "" || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("ratification %+v", record)
	}
	if sealed := f.Documents(t, stream, seal.DocumentID); len(sealed) != 1 {
		t.Fatalf("seal records: %+v", sealed)
	}

	var ratified trace.Transition
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == intake.OwnerSubject && tr.To == "ratified-1" {
			ratified = tr
		}
	}
	if ratified.Actor != trace.JevActor {
		t.Fatalf("the ratification transition: %+v", ratified)
	}

	criterion, err := f.C.TraceCriterion(ctx, stream, "spec#1")
	fixturekit.Must(t, err)
	if len(criterion.Rulings) == 0 || criterion.Rulings[0].Kind != "ratification" || criterion.Rulings[0].DecidedBy != trace.DecidedByJev || criterion.Rulings[0].Judgment != record.Judgment {
		t.Fatalf("trace rulings: %+v", criterion.Rulings)
	}
	if out := f.RunCLI(t, "trace", string(stream), "criterion", "spec#1"); !strings.Contains(out, "ratification: ratified (decided by Jev)\n") {
		t.Fatalf("the CLI's trace lacks the label:\n%s", out)
	}

	before := len(p.Requests())
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 0 {
		t.Fatalf("a ratification card remains: %+v", entries)
	}
	if len(p.Requests()) != before {
		t.Fatal("reading the inbox asked Jev")
	}
	for _, r := range p.Decisions() {
		for _, option := range jevfixture.Options(r) {
			if option != "ratify" && option != jev.Defer {
				t.Fatalf("Jev was offered %q, which is not the owner's ratification: %v", option, jevfixture.Options(r))
			}
		}
	}
}

func TestWithJevUnconfiguredTheRatificationReachesTheOwner(t *testing.T) {
	t.Parallel()
	p := &jevfixture.Provider{Confidence: 0.97}
	f := newJevJourney(t, p, false)
	defer f.Stop(t)
	ctx := context.Background()

	stream := f.ReachJevPacket(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	entries := f.InboxEntries(t, stream, api.InboxRatification)
	if len(entries) != 1 || entries[0].Recommendation != "ratify: no objection stands" || !slices.Equal(entries[0].Options, []string{"ratify"}) {
		t.Fatalf("the ratification card: %+v", entries)
	}
	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
		t.Fatalf("the workstream is %q, want it to wait for the owner: %v", state.Value, err)
	}
	if runtime, err := f.C.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeUnconfigured || runtime.Jev.Reason != jev.ReasonUnconfigured {
		t.Fatalf("Jev in the status: %+v %v", runtime.Jev, err)
	}
	if len(p.Requests()) != 0 {
		t.Fatalf("%d requests went out with no key", len(p.Requests()))
	}

	root, err := config.ResolveRoot(f.Opts.Config.Root, "")
	fixturekit.Must(t, err)
	checks := doctor.Run(ctx, doctor.Options{
		Root:     root,
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		Command:  func(_ context.Context, path string, _ ...string) (string, error) { return "tool 1.0.0", nil },
		CheckJJ:  func(context.Context) (string, error) { return "", os.ErrNotExist },
		Getenv:   func(string) string { return "" },
	})
	var found *doctor.Check
	for i := range checks {
		if checks[i].Name == "jev" {
			found = &checks[i]
		}
	}
	if found == nil || found.Status != doctor.Fail || !strings.Contains(found.Detail, "unconfigured") || !doctor.Failed(checks) {
		t.Fatalf("doctor's Jev check: %+v", found)
	}

	if _, err := f.C.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.Scenario().AwaitFeature(t, stream, intake.BuildingState)
	if record := f.Ratification(t, stream, 1); record.DecidedBy == trace.DecidedByJev || record.Judgment != "" {
		t.Fatalf("the owner's ratification %+v", record)
	}
}
