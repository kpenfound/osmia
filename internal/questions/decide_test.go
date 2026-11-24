package questions_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

var routingOptions = []string{"answer", "route_amendment", "escalate"}

func askAs(t *testing.T, f fixture, agent, role, turn, text string) string {
	t.Helper()
	scope := f.turn(t, agent, role, turn)
	got := call(t, tools(t, f, agent, scope, start)[questions.AskTool], `{"question":"`+text+`"}`)
	i := strings.Index(got, `"question":"`)
	if i < 0 {
		t.Fatal(got)
	}
	rest := got[i+len(`"question":"`):]
	return rest[:strings.Index(rest, `"`)]
}

func rawChief(t *testing.T, f fixture, turn string) map[string]coreadapter.Tool {
	t.Helper()
	scope := f.turn(t, "chief-"+turn, trace.ChiefOfStaff, turn)
	list, err := questions.Tools(f.repo, "chief-"+turn, scope, func() time.Time { return start })
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]coreadapter.Tool{}
	for _, tool := range list {
		out[tool.Name] = tool
	}
	return out
}

func picking(option string, confidence float64) (*jev.Judge, *jevtest.Provider) {
	others := []string{}
	for _, o := range routingOptions {
		if o != option {
			others = append(others, o)
		}
	}
	p := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick(option, confidence, others...)}}
	return jevfixture.Answering(func() time.Time { return start }, p), p
}

const (
	answerInput   = `{"question":"%s","text":"In files.","citations":["charter#2"]}`
	escalateInput = `{"questions":["%s"],"rephrasing":"Is it part of the contract?","blocked":"The unit.","options":["Yes","No"],"recommendation":"Yes."}`
	routeInput    = `{"question":"%s","citations":["spec#1"],"change":"Clarify.","reason":"Needed"}`
)

func inputs(id string) map[string]string {
	r := strings.NewReplacer("%s", id)
	return map[string]string{"answer": r.Replace(answerInput), "escalate": r.Replace(escalateInput), "route_amendment": r.Replace(routeInput)}
}

func TestOutcomeToolsRefuseBeforeDecideAndOnAnotherQuestionsResult(t *testing.T) {
	f := setup(t)
	one := askAs(t, f, "mason1", "mason", "build1", "Where does state live?")
	two := askAs(t, f, "mason2", "mason", "build2", "Where do logs live?")
	chief := rawChief(t, f, "chief1")
	before := f.head(t)
	for name, input := range inputs(one) {
		got := call(t, chief[name], input)
		if !strings.Contains(got, `"recorded":false`) || !strings.Contains(got, "call decide") {
			t.Fatalf("%s before decide: %s", name, got)
		}
	}
	if f.head(t) != before {
		t.Fatal("a refused outcome changed the trace")
	}
	decideFallback(t, f.repo, stream, two)
	before = f.head(t)
	for name, input := range inputs(one) {
		if got := call(t, chief[name], input); !strings.Contains(got, "call decide") {
			t.Fatalf("%s with another question's result: %s", name, got)
		}
	}
	if f.head(t) != before {
		t.Fatal("a refused outcome changed the trace")
	}
	for _, q := range states(t, f) {
		if q.State != trace.QuestionOpen {
			t.Fatalf("question %s is %s", q.Asked.ID, q.State)
		}
	}
}

func TestABindingPickAllowsOnlyItsOutcomeWithTheChiefsTextAndSurvivesARestart(t *testing.T) {
	for _, pickName := range []string{"answer", "escalate"} {
		t.Run(pickName, func(t *testing.T) {
			f := setup(t)
			id := askAs(t, f, "mason1", "mason", "build1", "Where does state live?")
			j, p := picking(pickName, 0.95)
			res := jevfixture.Decide(j, f.repo, project, stream, jev.KindQuestionRouting, jev.QuestionKey(id), routingOptions...)
			if res.Status != jev.Binding || res.Pick != pickName {
				t.Fatalf("result %+v", res)
			}
			check := func(chief map[string]coreadapter.Tool) {
				t.Helper()
				for name, input := range inputs(id) {
					if name == pickName {
						continue
					}
					before := f.head(t)
					got := call(t, chief[name], input)
					if !strings.Contains(got, `"recorded":false`) || !strings.Contains(got, "Jev decided "+pickName) || !strings.Contains(got, res.Judgment) {
						t.Fatalf("%s against the pick: %s", name, got)
					}
					if f.head(t) != before {
						t.Fatalf("%s changed the trace", name)
					}
				}
			}
			check(rawChief(t, f, "chief1"))
			if err := f.repo.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			if f.repo, err = trace.Open(f.root, f.project); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.repo.Close() })
			chief := rawChief(t, f, "chief2")
			check(chief)
			if got := call(t, chief[pickName], inputs(id)[pickName]); !strings.Contains(got, `"recorded":true`) {
				t.Fatalf("the picked outcome: %s", got)
			}
			if len(p.Requests()) != 1 {
				t.Fatalf("requests = %d", len(p.Requests()))
			}
			q := states(t, f)[id]
			if pickName == "answer" {
				r := q.Ruling
				if r == nil || r.DecidedBy != trace.DecidedByJev || r.Judgment != res.Judgment || r.Actor != trace.JevActor || r.ReturnedAnswer != "In files." {
					t.Fatalf("ruling %+v", r)
				}
			} else if q.State != trace.QuestionEscalated {
				t.Fatalf("state %s", q.State)
			}
		})
	}
}

func TestABelowThresholdOrFallbackResultAllowsAnyOutcome(t *testing.T) {
	for name, decide := range map[string]func(t *testing.T, f fixture, id string){
		"below the threshold": func(t *testing.T, f fixture, id string) {
			j, _ := picking("escalate", 0.5)
			if res := jevfixture.Decide(j, f.repo, project, stream, jev.KindQuestionRouting, jev.QuestionKey(id), routingOptions...); res.Status != jev.Nonbinding {
				t.Fatalf("result %+v", res)
			}
		},
		"unconfigured": func(t *testing.T, f fixture, id string) {
			if res := jevfixture.Decide(jevfixture.Unconfigured(func() time.Time { return start }), f.repo, project, stream, jev.KindQuestionRouting, jev.QuestionKey(id), routingOptions...); res.Status != jev.FallbackOnly {
				t.Fatalf("result %+v", res)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			id := askAs(t, f, "mason1", "mason", "build1", "Where does state live?")
			decide(t, f, id)
			if got := call(t, rawChief(t, f, "chief1")["answer"], inputs(id)["answer"]); !strings.Contains(got, `"recorded":true`) {
				t.Fatalf("answer: %s", got)
			}
			if r := states(t, f)[id].Ruling; r == nil || r.DecidedBy != "" || r.Actor.Kind != "agent" {
				t.Fatalf("ruling %+v", r)
			}
		})
	}
}

func TestAQuestionJevDoesNotRouteKeepsItsRoute(t *testing.T) {
	f := setup(t)
	id := askAs(t, f, "architect1", "architect", "draft1", "Does this project need a change?")
	if got := call(t, rawChief(t, f, "chief1")["answer"], inputs(id)["answer"]); !strings.Contains(got, `"recorded":true`) {
		t.Fatalf("answer: %s", got)
	}
	if recs := f.judgments(t); len(recs) != 0 {
		t.Fatalf("judgments recorded: %d", len(recs))
	}
}

func TestARefusedAmendmentRoutingGoesToTheOwnerWithJevsJudgment(t *testing.T) {
	f := setup(t)
	id := askAs(t, f, "mason1", "mason", "build1", "Where does state live?")
	j, p := picking("route_amendment", 0.96)
	res := jevfixture.Decide(j, f.repo, project, stream, jev.KindQuestionRouting, jev.QuestionKey(id), routingOptions...)
	if res.Status != jev.Binding {
		t.Fatalf("result %+v", res)
	}
	got := call(t, rawChief(t, f, "chief1")["route_amendment"], inputs(id)["route_amendment"])
	if !strings.Contains(got, `"recorded":true`) || !strings.Contains(got, "owner's inbox") {
		t.Fatalf("route: %s", got)
	}
	q := states(t, f)[id]
	if q.State != trace.QuestionEscalated || !strings.Contains(q.Latest.Escalation.Recommendation, res.Judgment) {
		t.Fatalf("question %+v", q)
	}
	if again, err := jev.Recorded(f.repo, stream, jev.KindQuestionRouting, jev.QuestionKey(id)); err != nil || again.Status != jev.Binding || again.Pick != "route_amendment" {
		t.Fatalf("recorded %+v: %v", again, err)
	}
	if len(p.Requests()) != 1 {
		t.Fatalf("requests = %d", len(p.Requests()))
	}
}

func TestRelayingAnOwnersRulingNeedsNoDecideResult(t *testing.T) {
	f := setup(t)
	id := askAs(t, f, "mason1", "mason", "build1", "Where does state live?")
	scope := f.turn(t, "chief", trace.ChiefOfStaff, "chief1")
	if _, err := f.repo.EscalateQuestions(context.Background(), "chief", scope, trace.EscalationRequest{Questions: []string{id}, Rephrasing: "Where does state live?", Blocked: "The unit.", Recommendation: "Files."}, start); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Rule(context.Background(), 1, "Files.", owner, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	chief := tools(t, f, "chief", scope, start.Add(2*time.Hour))
	if got := call(t, chief["relay_ruling"], `{"question":"`+id+`","text":"Keep state in files.","scope":"local"}`); !strings.Contains(got, `"recorded":true`) {
		t.Fatalf("relay: %s", got)
	}
	if recs := f.judgments(t); len(recs) != 0 {
		t.Fatalf("%d judgments recorded", len(recs))
	}
}
