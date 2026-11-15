package intakejourney

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/doctor"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// newJevJourney is a debate fixture of one member in one round whose service
// asks p for Jev's answers, with Jev's key set when on.
func newJevJourney(t *testing.T, p *jevfixture.Provider, on bool) *shedFixture {
	t.Helper()
	key := jevfixture.Key(t, on)
	return newDebateFixtureWith(t, 1, 1, jevfixture.Config(key), func(o *Options) {
		if p != nil {
			o.JevProvider = p.Factory()
		}
	})
}

// reachJevPacket hands in a design whose debate concludes by consensus and
// returns once the shed has concluded. Nobody ratifies it.
func (f *shedFixture) reachJevPacket(t *testing.T) config.WorkstreamID {
	t.Helper()
	f.upstream(t)
	faults := &intakefixture.Faults{}
	f.member(1, 1, 1, intakefixture.Concedes(faults))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	faults.Check(t)
	return stream
}

// runCLI runs an osmia command against the service of the fixture.
func (f *shedFixture) runCLI(t *testing.T, args ...string) string {
	t.Helper()
	root, err := config.ResolveRoot(f.opts.Config.Root, "")
	must(t, err)
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root.String()}, args...), strings.NewReader(""), &out, &diag)
	if code != 0 || diag.Len() != 0 {
		t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out.String(), diag.String())
	}
	return out.String()
}

// A confident Jev ratifies the packet with no owner command. No client reads
// the inbox before the build is under way. The workstream is sealed and built
// as after the owner's ratification, and every surface that shows the
// decision, the ratification record, the workstream's transitions, the trace
// through the API and the trace through the CLI, says Jev made it. Delivery
// approval is never asked of Jev.
func TestAConfidentJevRatifiesAndTheWorkstreamBuilds(t *testing.T) {
	t.Parallel()
	p := &jevfixture.Provider{Confidence: 0.97}
	f := newJevJourney(t, p, true)
	defer f.stop(t)
	ctx := context.Background()

	stream := f.reachJevPacket(t)
	f.scenario().AwaitFeature(t, stream, intake.BuildingState)

	if runtime, err := f.c.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeReady {
		t.Fatalf("Jev in the status: %+v %v", runtime.Jev, err)
	}
	requests := p.Decisions()
	if len(requests) != 1 || !slices.Equal(jevfixture.Options(requests[0]), []string{"ratify", jev.Defer}) {
		t.Fatalf("decision requests: %d, options %v", len(requests), requests)
	}

	docs := f.documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 1 || docs[0].Actor != trace.JevActor {
		t.Fatalf("ratification records: %+v", docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	must(t, err)
	if record.DecidedBy != trace.DecidedByJev || record.Judgment == "" || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("ratification %+v", record)
	}
	if sealed := f.documents(t, stream, seal.DocumentID); len(sealed) != 1 {
		t.Fatalf("seal records: %+v", sealed)
	}

	var ratified trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == intake.OwnerSubject && tr.To == "ratified-1" {
			ratified = tr
		}
	}
	if ratified.Actor != trace.JevActor {
		t.Fatalf("the ratification transition: %+v", ratified)
	}

	criterion, err := f.c.TraceCriterion(ctx, stream, "spec#1")
	must(t, err)
	if len(criterion.Rulings) == 0 || criterion.Rulings[0].Kind != "ratification" || criterion.Rulings[0].DecidedBy != trace.DecidedByJev || criterion.Rulings[0].Judgment != record.Judgment {
		t.Fatalf("trace rulings: %+v", criterion.Rulings)
	}
	if out := f.runCLI(t, "trace", string(stream), "criterion", "spec#1"); !strings.Contains(out, "ratification: ratified (decided by Jev)\n") {
		t.Fatalf("the CLI's trace lacks the label:\n%s", out)
	}

	// Reading the inbox now shows no ratification card and asks Jev nothing.
	before := len(p.Requests())
	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 0 {
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

// With Jev unconfigured the same packet reaches the owner as a card with the
// recommendation the packet gives, and nothing ratifies it until the owner
// does. The service started, status shows Jev unconfigured, no request went
// out, and osmia doctor fails its Jev check.
func TestWithJevUnconfiguredTheRatificationReachesTheOwner(t *testing.T) {
	t.Parallel()
	p := &jevfixture.Provider{Confidence: 0.97}
	f := newJevJourney(t, p, false)
	defer f.stop(t)
	ctx := context.Background()

	stream := f.reachJevPacket(t)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	entries := f.inboxEntries(t, stream, InboxRatification)
	if len(entries) != 1 || entries[0].Recommendation != "ratify: no objection stands" || !slices.Equal(entries[0].Options, []string{"ratify"}) {
		t.Fatalf("the ratification card: %+v", entries)
	}
	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
		t.Fatalf("the workstream is %q, want it to wait for the owner: %v", state.Value, err)
	}
	if runtime, err := f.c.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeUnconfigured || runtime.Jev.Reason != jev.ReasonUnconfigured {
		t.Fatalf("Jev in the status: %+v %v", runtime.Jev, err)
	}
	if len(p.Requests()) != 0 {
		t.Fatalf("%d requests went out with no key", len(p.Requests()))
	}

	root, err := config.ResolveRoot(f.opts.Config.Root, "")
	must(t, err)
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

	// The owner ratifies, and the workstream builds as it always has.
	if _, err := f.c.Ratify(ctx, stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.scenario().AwaitFeature(t, stream, intake.BuildingState)
	if record := f.ratification(t, stream, 1); record.DecidedBy == trace.DecidedByJev || record.Judgment != "" {
		t.Fatalf("the owner's ratification %+v", record)
	}
}
