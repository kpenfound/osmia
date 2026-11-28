package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func restDocuments(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

type amendmentKit struct {
	f      *shedFixture
	stream config.WorkstreamID
	repo   *trace.Repository
	decide coreadapter.Tool
	amend  coreadapter.Tool
	judged
}

func newAmendmentKit(t *testing.T, p *gateJev, spec, graph string) *amendmentKit {
	t.Helper()
	f, stream := builtForAmendment(t, 1)
	presentAmendment(t, f, stream, masonRole, spec, graph)
	f.stop(t)
	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	f.s.setSole(&activeProject{repository: repo})
	f.s.jev = gateJudge(f.clock.Now, p)
	scope := intakefixture.ClaimManagerTurn(t, repo, f.project, stream, f.clock.Now, intakefixture.ManagerTurnInput{
		Turn: "events_1", Prompt: "Amendment 1 is presented.", Actor: serviceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &amendmentKit{f: f, stream: stream, repo: repo, decide: controls.decide(repo, scope), amend: controls.decideAmendment(repo, scope), judged: judged{p}}
}

const (
	decideAmendmentInput = `{"kind":"plan-amendment","subject":"1","evidence":"The revised verification keeps the approved upload outcome."}`
	approveAmendment     = `{"amendment":"1","packet":1,"decision":"approve","note":"The revised verification preserves the approved upload outcome."}`
	rejectAmendment      = `{"amendment":"1","packet":1,"decision":"reject","note":"Keep the current verification."}`
)

type judged struct{ provider *gateJev }

func (j judged) requests() int {
	if j.provider == nil {
		return 0
	}
	return len(j.provider.Requests())
}

func (k *amendmentKit) decisions(t *testing.T) []trace.Document {
	t.Helper()
	return restDocuments(t, k.repo, k.stream, intake.AmendmentDecisionID("1"))
}

func TestAConfidentPlanAmendmentPickBindsDecideAmendment(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(string, []string) string { return "approve" }}
	k := newAmendmentKit(t, p, fixturekit.ValidSpec, intakefixture.RestartPlan)
	if out := intakefixture.Handle(t, k.amend, approveAmendment); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("decide_amendment before decide: %s", out)
	}
	other := jevfixture.Decide(k.f.s.jev, k.repo, k.f.project, k.stream, jev.KindPlanAmendment, jev.AmendmentKey("2"), "approve", "reject")
	if other.Status != jev.Binding {
		t.Fatalf("result %+v", other)
	}
	if out := intakefixture.Handle(t, k.amend, approveAmendment); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("decide_amendment on another amendment's result: %s", out)
	}
	if len(k.decisions(t)) != 0 {
		t.Fatal("a decision was recorded before decide")
	}
	out := intakefixture.Handle(t, k.decide, decideAmendmentInput)
	if !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"approve"`) {
		t.Fatalf("decide: %s", out)
	}
	if got := optionNames(p); !slices.Equal(got, []string{"approve", "reject"}) || k.requests() != 2 {
		t.Fatalf("options %v after %d requests", got, k.requests())
	}
	if out := intakefixture.Handle(t, k.amend, rejectAmendment); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided approve") {
		t.Fatalf("reject against the pick: %s", out)
	}
	if out := intakefixture.Handle(t, k.amend, `{"amendment":"1","packet":1,"decision":"approve"}`); !strings.Contains(out, `"recorded":false`) {
		t.Fatalf("approve without the chief of staff's reasoning: %s", out)
	}
	if len(k.decisions(t)) != 0 {
		t.Fatal("a refused call recorded a decision")
	}
	if out := intakefixture.Handle(t, k.amend, approveAmendment); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("approve: %s", out)
	}
	judgment, _, _ := decisionJudgment(t, k.repo, k.stream, jev.KindPlanAmendment, jev.AmendmentKey("1"))
	docs := k.decisions(t)
	if len(docs) != 1 || docs[0].Actor != trace.JevActor {
		t.Fatalf("decision records %+v", docs)
	}
	var recorded struct{ Decision, Actor, Judgment, Note string }
	fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &recorded))
	if recorded.Decision != "approve" || recorded.Actor != trace.DecidedByJev || recorded.Judgment != judgment || judgment == "" || !strings.Contains(recorded.Note, "preserves the approved upload outcome") {
		t.Fatalf("decision %+v (judgment %q)", recorded, judgment)
	}
	if k.requests() != 2 {
		t.Fatalf("%d requests", k.requests())
	}
}

// An amendment that changes the approved intent is the owner's gate: decide
// refuses it with no request and no judgment, and decide_amendment records
// nothing for the manager.
func TestDecideRefusesAnAmendmentThatChangesIntent(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95}
	k := newAmendmentKit(t, p, intakefixture.AmendedSpec, fixturekit.ValidPlan)
	_, before := judgments(t, k.repo, k.stream)
	out := intakefixture.Handle(t, k.decide, decideAmendmentInput)
	if !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "owner") {
		t.Fatalf("decide: %s", out)
	}
	_, after := judgments(t, k.repo, k.stream)
	if k.requests() != 0 || after != before {
		t.Fatalf("%d requests, %d judgment revisions before decide and %d after", k.requests(), before, after)
	}
	if out := intakefixture.Handle(t, k.amend, approveAmendment); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "owner decision") {
		t.Fatalf("decide_amendment: %s", out)
	}
	if len(k.decisions(t)) != 0 {
		t.Fatal("the chief of staff decided an amendment that changes intent")
	}
}

// A pick below the threshold and an unconfigured Jev leave a plan-only
// amendment to the manager: decide says so and decide_amendment takes
// either decision.
func TestAPlanAmendmentDecisionThatIsNotConfidentLeavesTheManagerFree(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*gateJev{"below the threshold": {confidence: 0.5}, "unconfigured": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newAmendmentKit(t, p, fixturekit.ValidSpec, intakefixture.RestartPlan)
			if out := intakefixture.Handle(t, k.amend, rejectAmendment); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
				t.Fatalf("decide_amendment before decide: %s", out)
			}
			if out := intakefixture.Handle(t, k.decide, decideAmendmentInput); !strings.Contains(out, `"decision":"chief"`) || strings.Contains(out, `"pick"`) {
				t.Fatalf("decide: %s", out)
			}
			if out := intakefixture.Handle(t, k.amend, rejectAmendment); !strings.Contains(out, `"recorded":true`) {
				t.Fatalf("reject: %s", out)
			}
			docs := k.decisions(t)
			if len(docs) != 1 || docs[0].Actor.ID != trace.Manager {
				t.Fatalf("decision records %+v", docs)
			}
		})
	}
}

func TestAPlanAmendmentPickTheServiceRefusedReachesTheOwner(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(string, []string) string { return "approve" }}
	k := newAmendmentKit(t, p, fixturekit.ValidSpec, intakefixture.RestartPlan)
	if _, open, err := k.f.s.intake().AmendmentEntry(k.repo, k.stream, "1"); err != nil || open {
		t.Fatalf("the chief of staff's amendment is listed for the owner: %v %v", open, err)
	}
	if out := intakefixture.Handle(t, k.decide, decideAmendmentInput); !strings.Contains(out, `"pick":"approve"`) {
		t.Fatalf("decide: %s", out)
	}
	judgment, _, _ := decisionJudgment(t, k.repo, k.stream, jev.KindPlanAmendment, jev.AmendmentKey("1"))
	fixturekit.Must(t, k.f.s.jev.Refuse(context.Background(), k.repo, k.stream, judgment, "the sealed plan moved"))
	card, open, err := k.f.s.intake().AmendmentEntry(k.repo, k.stream, "1")
	if err != nil || !open || card.Jev == nil || !card.Jev.Binding || card.Jev.Pick != "approve" || card.Jev.Judgment != judgment {
		t.Fatalf("card %+v open %v: %v", card, open, err)
	}
	for _, input := range []string{approveAmendment, rejectAmendment} {
		if out := intakefixture.Handle(t, k.amend, input); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "owner") {
			t.Fatalf("decide_amendment after the refusal: %s", out)
		}
	}
	if out := intakefixture.Handle(t, k.decide, decideAmendmentInput); !strings.Contains(out, `"recorded":false`) {
		t.Fatalf("decide after the refusal: %s", out)
	}
	if k.requests() != 1 || len(k.decisions(t)) != 0 {
		t.Fatalf("%d requests, %d decisions", k.requests(), len(k.decisions(t)))
	}
}

type discoveryKit struct {
	c      *contestFixture
	record coreadapter.Tool
	decide coreadapter.Tool
	id     string
	judged
}

func newDiscoveryKit(t *testing.T, key string, p *gateJev) *discoveryKit {
	t.Helper()
	c := newContestFixture(t, key)
	c.f.s.jev = gateJudge(c.f.clock.Now, p)
	k := &discoveryKit{c: c, record: recordDiscovery(c.repo, c.scope, c.f.s.now), decide: c.controls.decide(c.repo, c.scope), judged: judged{p}}
	var created struct{ ID string }
	fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.Handle(t, k.record, `{"key":"repair-fake","description":"Update the dependent fake for the new protocol","criterion":"spec#1"}`)), &created))
	k.id = created.ID
	return k
}

func (k *discoveryKit) disposition(t *testing.T, fields string) string {
	t.Helper()
	return intakefixture.Handle(t, k.record, `{"id":"`+k.id+`","revision":1,`+fields+`}`)
}

func (k *discoveryKit) decideInput() string {
	return `{"kind":"discovery-disposition","subject":"` + k.id + `","evidence":"The fake is used by the unit's tests."}`
}

func (k *discoveryKit) discovery(t *testing.T) workDiscovery {
	t.Helper()
	all, err := discoveries(k.c.repo, k.c.stream)
	fixturekit.Must(t, err)
	return all[k.id]
}

const declineFields = `"disposition":"declined","reason":"The existing fake already covers this behavior."`

func TestAConfidentDiscoveryPickBindsTheDisposition(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "decline" }}
	k := newDiscoveryKit(t, "decide-discovery", p)
	if out := k.disposition(t, declineFields); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("disposition before decide: %s", out)
	}
	judge := k.c.f.s.jev
	if other := jevfixture.Decide(judge, k.c.repo, k.c.repo.Project(), k.c.stream, jev.KindDiscovery, jev.DiscoveryKey("discovery-other"), "assign", "decline", "retain"); other.Status != jev.Binding {
		t.Fatalf("result %+v", other)
	}
	if out := k.disposition(t, declineFields); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("disposition on another discovery's result: %s", out)
	}
	if k.discovery(t).Disposition != "open" {
		t.Fatal("a refused call changed the discovery")
	}
	if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"decline"`) {
		t.Fatalf("decide: %s", out)
	}
	if got := optionNames(p); !slices.Equal(got, []string{"assign", "decline", "retain"}) || k.requests() != 2 {
		t.Fatalf("options %v after %d requests", got, k.requests())
	}
	for _, fields := range []string{`"disposition":"assigned","unit":"resume","reason":"It belongs to the upload outcome."`, `"disposition":"suggestion","reason":"Worth keeping."`} {
		if out := k.disposition(t, fields); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided decline") {
			t.Fatalf("disposition against the pick: %s", out)
		}
	}
	if out := k.disposition(t, `"disposition":"declined"`); !strings.Contains(out, `"recorded":false`) {
		t.Fatalf("disposition without the chief of staff's reasoning: %s", out)
	}
	if out := k.disposition(t, declineFields); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("decline: %s", out)
	}
	judgment, _, _ := decisionJudgment(t, k.c.repo, k.c.stream, jev.KindDiscovery, jev.DiscoveryKey(k.id))
	d := k.discovery(t)
	docs := restDocuments(t, k.c.repo, k.c.stream, k.id)
	if d.Disposition != "declined" || d.DecidedBy != trace.DecidedByJev || d.Judgment != judgment || judgment == "" || len(docs) != 2 || docs[1].Actor != trace.JevActor {
		t.Fatalf("discovery %+v (judgment %q) records %+v", d, judgment, docs)
	}
	if pending, err := unresolvedWork(k.c.repo, k.c.stream); err != nil || pending {
		t.Fatalf("the declined discovery still blocks: %v %v", pending, err)
	}
	if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "already declined") {
		t.Fatalf("decide on a disposed discovery: %s", out)
	}
	if k.requests() != 2 {
		t.Fatalf("%d requests", k.requests())
	}
}

// A caller cannot name the decider of a discovery: a disposition the manager of
// staff records on its own is never Jev's.
func TestADiscoveryCannotNameItsDecider(t *testing.T) {
	t.Parallel()
	k := newDiscoveryKit(t, "decide-discovery-decider", nil)
	if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"decision":"chief"`) {
		t.Fatalf("decide: %s", out)
	}
	if out := k.disposition(t, declineFields+`,"decided_by":"jev","judgment":"judgment_ab12","handed_to_owner":"x"`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("decline: %s", out)
	}
	if d := k.discovery(t); d.DecidedBy != "" || d.Judgment != "" || d.HandedToOwner != "" {
		t.Fatalf("discovery %+v", d)
	}
}

func TestADiscoveryPickTheServiceRefusedReachesTheOwner(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return "assign" }}
	k := newDiscoveryKit(t, "decide-discovery-owner", p)
	if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"pick":"assign"`) {
		t.Fatalf("decide: %s", out)
	}
	out := k.disposition(t, `"disposition":"assigned","unit":"ghost","reason":"The fake belongs to the ghost unit."`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, "owner's inbox") {
		t.Fatalf("assign: %s", out)
	}
	judgment, _, _ := decisionJudgment(t, k.c.repo, k.c.stream, jev.KindDiscovery, jev.DiscoveryKey(k.id))
	d := k.discovery(t)
	if d.Disposition != "open" || d.Unit != "" || d.DecidedBy != trace.DecidedByJev || d.Judgment != judgment || !strings.Contains(d.HandedToOwner, judgment) {
		t.Fatalf("discovery %+v (judgment %q)", d, judgment)
	}
	if pending, err := unresolvedWork(k.c.repo, k.c.stream); err != nil || !pending {
		t.Fatalf("the open discovery does not block: %v %v", pending, err)
	}
	entries, err := discoveryEntries(k.c.repo, k.c.stream)
	if err != nil || len(entries) != 1 || entries[0].Kind != intake.InboxDiscovery || entries[0].Jev == nil || !entries[0].Jev.Binding || entries[0].Jev.Judgment != judgment || !strings.Contains(entries[0].Question, k.id) {
		t.Fatalf("entries %+v: %v", entries, err)
	}
	for _, input := range []string{`"revision":2,` + declineFields, `"revision":2,"disposition":"assigned","unit":"resume","reason":"Assign it."`} {
		if out := intakefixture.Handle(t, k.record, `{"id":"`+k.id+`",`+input+`}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "owner") {
			t.Fatalf("disposition of the owner's discovery: %s", out)
		}
	}
	if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"recorded":false`) {
		t.Fatalf("decide on the owner's discovery: %s", out)
	}
	if k.requests() != 1 {
		t.Fatalf("%d requests", k.requests())
	}
}

// A pick below the threshold and an unconfigured Jev leave a discovery's
// disposition to the manager, who then records any valid disposition.
func TestADiscoveryDecisionThatIsNotConfidentLeavesTheManagerFree(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]*gateJev{"below the threshold": {confidence: 0.5}, "unconfigured": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newDiscoveryKit(t, "decide-discovery-free-"+strings.ReplaceAll(name, " ", "-"), p)
			if out := k.disposition(t, declineFields); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
				t.Fatalf("disposition before decide: %s", out)
			}
			if out := intakefixture.Handle(t, k.decide, k.decideInput()); !strings.Contains(out, `"decision":"chief"`) || strings.Contains(out, `"pick"`) {
				t.Fatalf("decide: %s", out)
			}
			if out := k.disposition(t, `"disposition":"suggestion","reason":"Worth keeping, not necessary."`); !strings.Contains(out, `"recorded":true`) {
				t.Fatalf("retain: %s", out)
			}
			if d := k.discovery(t); d.Disposition != "suggestion" || d.DecidedBy != "" {
				t.Fatalf("discovery %+v", d)
			}
		})
	}
}
