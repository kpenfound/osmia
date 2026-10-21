package intake

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestQuestionAssessmentSignals(t *testing.T) {
	t.Parallel()
	stream := config.WorkstreamID("w_1")
	rulings := []AssessedRuling{
		{Ruling: trace.Ruling{QuestionID: "1", OwnerResponse: "Keep the store in memory."}, Workstream: stream},
		{Ruling: trace.Ruling{QuestionID: "4", OwnerResponse: "Every file carries a licence header."}, Workstream: "w_2"},
	}
	a := Assessment{Stream: stream, Rulings: rulings}
	for _, tc := range []struct {
		name    string
		answers map[string]systemone.Answer
		want    []string
	}{
		{"below every threshold", map[string]systemone.Answer{AmendmentQuestion: jevtest.Noul(0.59), ScopeQuestion: jevtest.Noul(0.69), ConflictQuestion: jevtest.Choice("ruling-1", 0.59, 0.5, "ruling-2", NoRuling)}, nil},
		{"amendment", map[string]systemone.Answer{AmendmentQuestion: jevtest.Noul(0.6)}, []string{"the honest answer may change the sealed spec or plan (probability 0.60)"}},
		{"no conflict", map[string]systemone.Answer{ConflictQuestion: jevtest.Choice(NoRuling, 0.95, 0.9, "ruling-1", "ruling-2")}, nil},
		{"conflict", map[string]systemone.Answer{ConflictQuestion: jevtest.Choice("ruling-2", 0.7, 0.6, "ruling-1", NoRuling)}, []string{`may contradict the owner's ruling on question 4 in workstream w_2, "Every file carries a licence header." (probability 0.70)`}},
		{"unknown ruling", map[string]systemone.Answer{ConflictQuestion: jevtest.Choice("ruling-3", 0.9, 0.9, NoRuling)}, nil},
		{"project", map[string]systemone.Answer{ScopeQuestion: jevtest.Noul(0.7)}, []string{"standing rule for the whole project rather than this feature (probability 0.70)"}},
		{"wrong kind", map[string]systemone.Answer{AmendmentQuestion: jevtest.Choice("yes", 0.9, 0.9, "no")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := a.Signals(systemone.Response{Answers: tc.answers})
			if len(got) != len(tc.want) {
				t.Fatalf("signals %q, want %q", got, tc.want)
			}
			for i := range got {
				if !strings.Contains(got[i], tc.want[i]) {
					t.Fatalf("signal %q, want one containing %q", got[i], tc.want[i])
				}
			}
		})
	}
}

// A recovery continuation delivers the events of the turn it continues, however
// many times the service stopped.
func TestDeliveryTurnFollowsRecovery(t *testing.T) {
	t.Parallel()
	response := func(id string) *trace.TurnResponse {
		return &trace.TurnResponse{Header: trace.Header{ID: id}}
	}
	events := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Actor: trace.Actor{Kind: "service", ID: "events"}}, TurnID: "events_1"}, Response: response("response-1")}
	first := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Actor: RecoveryActor, Cause: "response-1"}, TurnID: "chief-recover-1"}, Response: response("response-2")}
	second := trace.TurnRequest{Header: trace.Header{Actor: RecoveryActor, Cause: "response-2"}, TurnID: "chief-recover-2"}
	th := trace.Thread{Turns: []trace.QueuedTurn{events, first, {Request: second}}}
	for _, tc := range []struct {
		req  trace.TurnRequest
		want string
	}{
		{events.Request, "events_1"},
		{first.Request, "events_1"},
		{second, "events_1"},
		{trace.TurnRequest{Header: trace.Header{Actor: RecoveryActor, Cause: "missing"}, TurnID: "orphan"}, "orphan"},
	} {
		if got := DeliveryTurn(th, tc.req); got != tc.want {
			t.Errorf("delivery turn of %s: %s, want %s", tc.req.TurnID, got, tc.want)
		}
	}
}

// A question too large for a judgment is not assessed; the rulings a judgment
// cannot fit are the oldest.
func TestQuestionAssessmentFitsJevContext(t *testing.T) {
	t.Parallel()
	question := trace.QuestionState{Asked: trace.Question{Header: trace.Header{ID: "1", Revision: 1}, Question: "Which store?"}}
	question.Latest = question.Asked
	var rulings []AssessedRuling
	for i := range 40 {
		// Two-byte runes keep each ruling at its rune bound yet too large for
		// every one of them to fit.
		text := fmt.Sprintf("%02d", i) + strings.Repeat("é", MaxRulingRunes-2)
		rulings = append(rulings, AssessedRuling{Ruling: trace.Ruling{Header: trace.Header{ID: "r", Revision: 1, Workstream: "w_1"}, QuestionID: "q", OwnerResponse: strings.Repeat("é", MaxRulingRunes)}, Workstream: "w_1", Question: text})
	}
	a := Assessor{Stream: "w_1"}
	got, ok, err := a.Judgment(question, MasonRole, nil, rulings[len(rulings)-MaxAssessedRulings:])
	fixturekit.Must(t, err)
	if !ok {
		t.Fatal("a small question was not assessed")
	}
	options := got.Request.Questions[ConflictQuestion].Options
	if len(got.Rulings) == 0 || len(got.Rulings) >= MaxAssessedRulings || len(options) != len(got.Rulings)+1 {
		t.Fatalf("offered %d rulings in %d options", len(got.Rulings), len(options))
	}
	if newest := got.Rulings[len(got.Rulings)-1]; newest.Question != rulings[len(rulings)-1].Question {
		t.Fatal("the newest ruling was not offered")
	}
	if got.Cause != "1" || got.Task != QuestionAssessmentTask || got.Request.Validate() != nil {
		t.Fatalf("judgment %+v", got.Judgment)
	}
	question.Asked.Question = strings.Repeat("é", MaxAssessmentState)
	if _, ok, err := a.Judgment(question, MasonRole, nil, nil); err != nil || !ok {
		t.Fatalf("a long question is assessed from its start: %v %v", ok, err)
	}
}
