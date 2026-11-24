package executionjourney

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type triageKit struct {
	f          *journey.ShedFixture
	stream     string
	decide     func(subject string) string
	triage     func(finding, decision, note string) string
	amend      func(input string) string
	repository *trace.Repository
}

func newTriageKit(t *testing.T, key string, provider *jevtest.Provider) *triageKit {
	t.Helper()
	f, stream, repository, a := journey.NewFinalFixture(t, key)
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."},
			map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "Resume ignores the last acknowledged chunk."},
			map[string]any{"severity": "advisory", "finding": "The resume log is noisy."}}})
		return err
	})
	if result := journey.SettleOperation(t, f.S, repository, stream, op, a); result.Outcome != "succeeded" {
		t.Fatalf("the review %+v", result)
	}
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	inspect := service.InspectForTest(f.S.Core())
	now := func() time.Time { return fixturekit.DemoStart }
	if provider == nil {
		inspect.SetJudge(jevfixture.Unconfigured(now))
	} else {
		inspect.SetJudge(jevfixture.Answering(now, provider))
	}
	scope := intakefixture.ClaimChiefTurn(t, repository, f.Project, stream, now, intakefixture.ChiefTurnInput{
		Turn: "events_1", Prompt: "The final review is recorded.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	decide, triage := inspect.DecideTool(repository, scope), inspect.TriageTool(repository, scope)
	list, err := questions.Tools(repository, trace.ChiefOfStaff, scope, f.S.Now)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(list, func(tool coreadapter.Tool) bool { return tool.Name == questions.RouteAmendmentTool })
	if i < 0 {
		t.Fatal("no route_amendment tool")
	}
	route := list[i]
	return &triageKit{f: f, stream: string(stream), repository: repository,
		decide: func(subject string) string {
			return intakefixture.Handle(t, decide, `{"kind":"final-review-triage","subject":"`+subject+`","evidence":"The report and the branch agree."}`)
		},
		triage: func(finding, decision, note string) string {
			return intakefixture.Handle(t, triage, `{"review":1,"finding":"`+finding+`","decision":"`+decision+`","note":"`+note+`"}`)
		},
		amend: func(input string) string { return intakefixture.Handle(t, route, input) }}
}

func TestFinalReviewTriageAsksJevOncePerBlockingFinding(t *testing.T) {
	t.Parallel()
	provider := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick("code", 0.96, "owner"), jevfixture.Pick("owner", 0.95, "code")}}
	k := newTriageKit(t, "triage-jev", provider)
	stream := config.WorkstreamID(k.stream)

	if out := k.triage("finding-1", "code", "Wire Dedupe into the upload path."); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("triage before decide: %s", out)
	}
	for _, subject := range []string{"1:finding-9", "2:finding-1", "1:finding-3", "finding-1"} {
		if out := k.decide(subject); !strings.Contains(out, `"recorded":false`) {
			t.Fatalf("decide %s: %s", subject, out)
		}
	}
	if res, err := jev.Recorded(k.repository, stream, jev.KindTriage, jev.FindingKey(1, "finding-9")); err != nil || res.Status != jev.None || len(provider.Requests()) != 0 {
		t.Fatalf("a request or judgment for a finding that does not stand: %+v %v, %d requests", res, err, len(provider.Requests()))
	}
	if out := k.decide("1:finding-1"); !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"code"`) {
		t.Fatalf("decide finding-1: %s", out)
	}
	if out := k.triage("finding-2", "owner", "Not worth building."); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("triage on another finding's result: %s", out)
	}
	if out := k.decide("1:finding-2"); !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"owner"`) {
		t.Fatalf("decide finding-2: %s", out)
	}
	if got := len(provider.Requests()); got != 2 {
		t.Fatalf("%d requests for two findings", got)
	}
	if out := k.decide("1:finding-1"); !strings.Contains(out, `"pick":"code"`) || len(provider.Requests()) != 2 {
		t.Fatalf("decide again: %s after %d requests", out, len(provider.Requests()))
	}
	for finding, other := range map[string]string{"finding-1": "owner", "finding-2": "code"} {
		if out := k.triage(finding, other, "Against the pick."); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided") {
			t.Fatalf("triage %s as %s: %s", finding, other, out)
		}
	}
	if all, _, err := followup.ReadTriage(k.repository, stream); err != nil || len(all) != 0 {
		t.Fatalf("a refused call recorded %+v: %v", all, err)
	}
	if out := k.triage("finding-1", "code", "Wire Dedupe into the upload path."); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("triage finding-1: %s", out)
	}
	if out := k.triage("finding-2", "owner", "The resume unit already restarts from the last chunk."); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("triage finding-2: %s", out)
	}
	if out := k.triage("finding-1", "code", "Again."); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "already triaged") {
		t.Fatalf("triage finding-1 twice: %s", out)
	}

	all, _, err := followup.ReadTriage(k.repository, stream)
	fixturekit.Must(t, err)
	if len(all) != 2 || all[0].Finding != "finding-1" || all[0].Outcome != followup.TriageCode || all[1].Finding != "finding-2" || all[1].Outcome != followup.TriageOwner {
		t.Fatalf("triage %+v", all)
	}
	for _, d := range all {
		res, err := jev.Recorded(k.repository, stream, jev.KindTriage, jev.FindingKey(1, d.Finding))
		if err != nil || res.Status != jev.Binding || !d.ByJev() || d.Judgment != res.Judgment || res.Judgment == "" {
			t.Fatalf("triage %+v against %+v: %v", d, res, err)
		}
	}
	for _, d := range intakefixture.StreamDocuments(t, k.repository, stream, followup.TriageDocumentID) {
		if d.Actor != trace.JevActor {
			t.Fatalf("a Jev decision written by %+v", d.Actor)
		}
	}

	notes, err := delivery.TriageNotes(k.repository, stream, 1)
	fixturekit.Must(t, err)
	for _, want := range []string{"Left to you:", "Resume ignores the last acknowledged chunk.", "The resume unit already restarts from the last chunk.", "decided by Jev, judgment " + all[1].Judgment, "needing code"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("the owner's notes lack %q: %s", want, notes)
		}
	}

	out := k.amend(`{"citations":["spec#2"],"change":"Call Dedupe before sending a chunk.","reason":"The final review found it unwired."}`)
	if !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("route_amendment: %s", out)
	}
	filed, err := trace.Read[trace.Amendment](k.repository, stream)
	fixturekit.Must(t, err)
	if len(filed) == 0 {
		t.Fatal("no amendment was filed")
	}
	change := filed[len(filed)-1].Change
	if !strings.Contains(change, "The upload path never calls Dedupe.") || !strings.Contains(change, "final review 1, finding-1") || strings.Contains(change, "Resume ignores") {
		t.Fatalf("the revision states %q", change)
	}
	after, _, err := followup.ReadTriage(k.repository, stream)
	fixturekit.Must(t, err)
	if len(after) != 2 || after[0].Amendment != filed[len(filed)-1].ID || after[1].Amendment != "" {
		t.Fatalf("triage after the revision %+v", after)
	}
	if pending, err := followup.PendingCode(k.repository, stream); err != nil || len(pending) != 0 {
		t.Fatalf("pending %+v: %v", pending, err)
	}
	if len(provider.Requests()) != 2 {
		t.Fatalf("%d requests", len(provider.Requests()))
	}
}

func TestFinalReviewTriageThatIsNotConfidentLeavesTheChiefFree(t *testing.T) {
	t.Parallel()
	for name, provider := range map[string]*jevtest.Provider{
		"below the threshold": {Results: []jevtest.Result{jevfixture.Pick("code", 0.5, "owner"), jevfixture.Pick("code", 0.5, "owner")}},
		"unconfigured":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newTriageKit(t, "triage-free-"+strings.ReplaceAll(name, " ", "-"), provider)
			stream := config.WorkstreamID(k.stream)
			if out := k.triage("finding-1", "owner", "Not worth building."); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
				t.Fatalf("triage before decide: %s", out)
			}
			for _, finding := range []string{"finding-1", "finding-2"} {
				if out := k.decide("1:" + finding); !strings.Contains(out, `"decision":"chief"`) || strings.Contains(out, `"pick"`) {
					t.Fatalf("decide %s: %s", finding, out)
				}
			}
			if out := k.triage("finding-1", "owner", "Not worth building."); !strings.Contains(out, `"recorded":true`) {
				t.Fatalf("triage finding-1: %s", out)
			}
			if out := k.triage("finding-2", "code", "Resume must honour the acknowledged chunk."); !strings.Contains(out, `"recorded":true`) {
				t.Fatalf("triage finding-2: %s", out)
			}
			all, _, err := followup.ReadTriage(k.repository, stream)
			fixturekit.Must(t, err)
			if len(all) != 2 || all[0].Outcome != followup.TriageOwner || all[1].Outcome != followup.TriageCode || all[0].ByJev() || all[1].ByJev() {
				t.Fatalf("triage %+v", all)
			}
		})
	}
}
