package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevtest"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

// newAssessingFixture is a mason fixture whose resume mason asks question 1.
// With p, the Jev boost is on and p answers its judgments; the caller sets
// the key's environment variable.
func newAssessingFixture(t *testing.T, p *jevtest.Provider, faults *faults) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", validPlan, "", func(opts *Options) {
		if p == nil {
			return
		}
		opts.JevProvider = p.Factory()
		configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		must(t, err)
		_, err = configFile.WriteString("\n[jev]\nenabled = true\nmodel = \"jev-test\"\napi_key_env = \"OSMIA_TEST_JEV_KEY\"\n")
		must(t, errors.Join(err, configFile.Close()))
	})
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = fake.asking(faults, "", "1")
	f.engine.mu.Unlock()
	return f, fake
}

// awaitEscalated waits until question id is escalated to the owner.
func (f *shedFixture) awaitEscalated(t *testing.T, stream config.WorkstreamID, id string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		asked, err := f.repository().Questions(stream)
		must(t, err)
		if i := slices.IndexFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == id }); i >= 0 && asked[i].State == trace.QuestionEscalated {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("question %s was never escalated: %+v", id, asked)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// questionTurns waits until a turn that delivered question id to the chief
// of staff has succeeded, and returns every completed turn that delivered it.
func (f *shedFixture) questionTurns(t *testing.T, stream config.WorkstreamID, id string) []trace.QueuedTurn {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().ChiefOfStaffThread(stream)
		must(t, err)
		var out []trace.QueuedTurn
		succeeded := false
		for _, q := range th.Turns {
			if strings.Contains(q.Request.Prompt, "Question "+id+" is open") && !q.CompletedAt.IsZero() {
				out = append(out, q)
				succeeded = succeeded || q.Status() != "failed" && q.Status() != "interrupted"
			}
		}
		if succeeded {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn delivered question %s: %+v", id, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// judgments returns the latest revision of every judgment record of the
// workstream, by ID.
func judgments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]jev.Record {
	t.Helper()
	documents, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	out := map[string]jev.Record{}
	for _, d := range documents {
		if !strings.HasPrefix(d.Path, "judgments/") {
			continue
		}
		var r jev.Record
		must(t, json.Unmarshal([]byte(d.Content), &r))
		out[d.ID] = r
	}
	return out
}

// With the boost on, the chief of staff's turn that delivers a question ends
// with Jev's signals on it, as advice the turn records. The judgment is
// recorded under its question and read back, not asked again, by the turn
// that delivers the question again after the first failed. A later question
// is compared against the owner's earlier ruling. The workflow takes its own
// course: the fake chief escalates both questions whatever the signals say.
//
// The boost is on through the configuration and its key's environment
// variable, so the test does not run in parallel.
func TestChiefQuestionSignals(t *testing.T) {
	t.Setenv("OSMIA_TEST_JEV_KEY", "key")
	p := &jevtest.Provider{Results: []jevtest.Result{
		{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
			amendmentQuestion: jevtest.Noul(0.82), scopeQuestion: jevtest.Noul(0.2)}}},
		{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
			amendmentQuestion: jevtest.Noul(0.1), scopeQuestion: jevtest.Noul(0.75), conflictQuestion: jevtest.Choice("ruling-1", 0.8, 0.7, noRuling)}}},
	}}
	faults := &faults{}
	f, fake := newAssessingFixture(t, p, faults)
	defer f.stop(t)
	f.answer("1", asks(faults, "2"))
	f.answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	var mu sync.Mutex
	failing := ""
	prompts := map[string][]string{}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, id := range []string{"1", "2"} {
			if !strings.Contains(req.Prompt, "Question "+id+" is open") {
				continue
			}
			mu.Lock()
			prompts[id] = append(prompts[id], req.Prompt)
			if id == "1" && failing == "" {
				failing = req.Name
			}
			fail := req.Name == failing
			mu.Unlock()
			if fail {
				return nil, errors.New("chief of staff unavailable")
			}
		}
		return fake.chief.turn(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "assessed", validPlan)
	f.awaitEscalated(t, stream, "1")

	turns := f.questionTurns(t, stream, "1")
	if len(turns) < 2 || turns[0].Status() != "failed" {
		t.Fatalf("question 1 was not delivered again after a failed turn: %+v", turns)
	}
	first := turns[0].Response.Advice
	if !strings.HasPrefix(first, questionAdvice) || !strings.Contains(first, "- Question 1 (judgment judgment_") || !strings.Contains(first, "route_amendment fits") || strings.Contains(first, "standing rule") {
		t.Fatalf("advice on question 1: %q", first)
	}
	for _, q := range turns[1:] {
		if q.Response.Advice != first {
			t.Fatalf("question 1 was assessed again: %q, want %q", q.Response.Advice, first)
		}
	}
	mu.Lock()
	for _, prompt := range prompts["1"] {
		if !strings.Contains(prompt, "\n\n"+first) {
			t.Errorf("the chief's prompt lacks the advice: %q", prompt)
		}
	}
	mu.Unlock()
	requests := p.Requests()
	if len(requests) != 1 {
		t.Fatalf("Jev requests for question 1: %d", len(requests))
	}
	state := requests[0].State.(map[string]any)
	asked := state["question"].(map[string]any)
	unit := state["unit"].(map[string]any)
	if asked["text"] != askedQuestion || asked["asked_by"] != masonRole || asked["unit"] != "resume" || unit["id"] != "resume" || unit["title"] != "Resume from the last chunk" || state["criteria"] == nil || state["spec"] != validSpec || state["plan"] == nil {
		t.Fatalf("judgment state %+v", state)
	}
	if _, ok := requests[0].Questions[conflictQuestion]; ok || len(requests[0].Questions) != 2 {
		t.Fatalf("questions without a ruling: %+v", requests[0].Questions)
	}
	recorded := judgments(t, f.repository(), stream)
	if len(recorded) != 1 {
		t.Fatalf("judgments %+v", recorded)
	}
	for id, r := range recorded {
		if !strings.Contains(first, id) || r.Task != questionAssessmentTask || r.Version != questionAssessmentVersion || r.State != jev.StateAccepted || r.ResolvedModel != "jev-1.13.0" ||
			!slices.ContainsFunc(r.Sources, func(s jev.Source) bool { return s.Kind == "question" && s.ID == "1" }) || !slices.ContainsFunc(r.Sources, func(s jev.Source) bool { return s.Kind == "seal" }) {
			t.Fatalf("judgment %s: %+v", id, r)
		}
	}

	// The owner rules on question 1; the mason's answer turn asks question 2.
	f.rule(t, "1")
	f.awaitEscalated(t, stream, "2")
	turns = f.questionTurns(t, stream, "2")
	if len(turns) != 1 {
		t.Fatalf("question 2 turns: %+v", turns)
	}
	advice := turns[0].Response.Advice
	if !strings.Contains(advice, "- Question 2 (judgment ") || !strings.Contains(advice, `the owner's ruling on question 1, "`+ownerRuling+`"`) || !strings.Contains(advice, "escalate fits") ||
		!strings.Contains(advice, "standing rule for the whole project") || strings.Contains(advice, "route_amendment") {
		t.Fatalf("advice on question 2: %q", advice)
	}
	requests = p.Requests()
	if len(requests) != 2 {
		t.Fatalf("Jev requests: %d", len(requests))
	}
	conflict, ok := requests[1].Questions[conflictQuestion]
	if !ok || len(conflict.Options) != 2 || conflict.Options[0].Name != "ruling-1" || conflict.Options[1].Name != noRuling {
		t.Fatalf("conflict question %+v", conflict)
	}
	if ruling := conflict.Options[0].Description.(map[string]string); ruling["ruling"] != ownerRuling || ruling["relayed"] != relayedRuling {
		t.Fatalf("ruling option %+v", ruling)
	}
	faults.check(t)
	fake.check(t)
}

// Without the boost, and whenever a judgment falls back, the turn that
// delivers a question carries no signals and the chief of staff chooses for
// it as it always does.
//
// The boost is on through the configuration and its key's environment
// variable, so the test does not run in parallel.
func TestChiefQuestionWithoutSignals(t *testing.T) {
	t.Setenv("OSMIA_TEST_JEV_KEY", "key")
	for _, tc := range []struct {
		name   string
		result *jevtest.Result
		reason jev.Reason
	}{
		{"disabled", nil, ""},
		{"rate limited", &jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "slow down"}}, jev.ReasonRateLimited},
		{"uncertain", &jevtest.Result{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
			amendmentQuestion: jevtest.Noul(0.45), scopeQuestion: jevtest.Noul(0.6)}}}, jev.ReasonDeclined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p *jevtest.Provider
			if tc.result != nil {
				p = &jevtest.Provider{Results: []jevtest.Result{*tc.result}}
			}
			faults := &faults{}
			f, fake := newAssessingFixture(t, p, faults)
			defer f.stop(t)
			stream := f.seedBuilding(t, "unassessed", validPlan)
			f.awaitEscalated(t, stream, "1")
			turns := f.questionTurns(t, stream, "1")
			if len(turns) != 1 || turns[0].Response.Advice != "" || strings.Contains(turns[0].Request.Prompt, questionAdvice) {
				t.Fatalf("question turns %+v", turns)
			}
			recorded := judgments(t, f.repository(), stream)
			if tc.result == nil {
				if len(recorded) != 0 {
					t.Fatalf("judgments with the boost off: %+v", recorded)
				}
			} else {
				if len(recorded) != 1 {
					t.Fatalf("judgments %+v", recorded)
				}
				for _, r := range recorded {
					if r.State != jev.StateFallback || r.Reason != tc.reason {
						t.Fatalf("judgment %+v, want a %s fallback", r, tc.reason)
					}
					if tc.reason == jev.ReasonDeclined && r.Answers[amendmentQuestion].Noul != 0.45 {
						t.Fatalf("declined answers were not kept: %+v", r.Answers)
					}
				}
			}
			faults.check(t)
			fake.check(t)
		})
	}
}

func TestQuestionAssessmentSignals(t *testing.T) {
	t.Parallel()
	stream := config.WorkstreamID("w_1")
	rulings := []assessedRuling{
		{ruling: trace.Ruling{QuestionID: "1", OwnerResponse: "Keep the store in memory."}, workstream: stream},
		{ruling: trace.Ruling{QuestionID: "4", OwnerResponse: "Every file carries a licence header."}, workstream: "w_2"},
	}
	a := assessment{stream: stream, rulings: rulings}
	for _, tc := range []struct {
		name    string
		answers map[string]systemone.Answer
		want    []string
	}{
		{"below every threshold", map[string]systemone.Answer{amendmentQuestion: jevtest.Noul(0.59), scopeQuestion: jevtest.Noul(0.69), conflictQuestion: jevtest.Choice("ruling-1", 0.59, 0.5, "ruling-2", noRuling)}, nil},
		{"amendment", map[string]systemone.Answer{amendmentQuestion: jevtest.Noul(0.6)}, []string{"the honest answer may change the sealed spec or plan (probability 0.60)"}},
		{"no conflict", map[string]systemone.Answer{conflictQuestion: jevtest.Choice(noRuling, 0.95, 0.9, "ruling-1", "ruling-2")}, nil},
		{"conflict", map[string]systemone.Answer{conflictQuestion: jevtest.Choice("ruling-2", 0.7, 0.6, "ruling-1", noRuling)}, []string{`may contradict the owner's ruling on question 4 in workstream w_2, "Every file carries a licence header." (probability 0.70)`}},
		{"unknown ruling", map[string]systemone.Answer{conflictQuestion: jevtest.Choice("ruling-3", 0.9, 0.9, noRuling)}, nil},
		{"project", map[string]systemone.Answer{scopeQuestion: jevtest.Noul(0.7)}, []string{"standing rule for the whole project rather than this feature (probability 0.70)"}},
		{"wrong kind", map[string]systemone.Answer{amendmentQuestion: jevtest.Choice("yes", 0.9, 0.9, "no")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := a.signals(systemone.Response{Answers: tc.answers})
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
	first := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Actor: recoveryActor, Cause: "response-1"}, TurnID: "chief-recover-1"}, Response: response("response-2")}
	second := trace.TurnRequest{Header: trace.Header{Actor: recoveryActor, Cause: "response-2"}, TurnID: "chief-recover-2"}
	th := trace.Thread{Turns: []trace.QueuedTurn{events, first, {Request: second}}}
	for _, tc := range []struct {
		req  trace.TurnRequest
		want string
	}{
		{events.Request, "events_1"},
		{first.Request, "events_1"},
		{second, "events_1"},
		{trace.TurnRequest{Header: trace.Header{Actor: recoveryActor, Cause: "missing"}, TurnID: "orphan"}, "orphan"},
	} {
		if got := deliveryTurn(th, tc.req); got != tc.want {
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
	var rulings []assessedRuling
	for i := range 40 {
		// Two-byte runes keep each ruling at its rune bound yet too large for
		// every one of them to fit.
		text := fmt.Sprintf("%02d", i) + strings.Repeat("é", maxRulingRunes-2)
		rulings = append(rulings, assessedRuling{ruling: trace.Ruling{Header: trace.Header{ID: "r", Revision: 1, Workstream: "w_1"}, QuestionID: "q", OwnerResponse: strings.Repeat("é", maxRulingRunes)}, workstream: "w_1", question: text})
	}
	a := assessor{stream: "w_1"}
	got, ok, err := a.judgment(question, masonRole, nil, rulings[len(rulings)-maxAssessedRulings:])
	must(t, err)
	if !ok {
		t.Fatal("a small question was not assessed")
	}
	options := got.Request.Questions[conflictQuestion].Options
	if len(got.rulings) == 0 || len(got.rulings) >= maxAssessedRulings || len(options) != len(got.rulings)+1 {
		t.Fatalf("offered %d rulings in %d options", len(got.rulings), len(options))
	}
	if newest := got.rulings[len(got.rulings)-1]; newest.question != rulings[len(rulings)-1].question {
		t.Fatal("the newest ruling was not offered")
	}
	if got.Cause != "1" || got.Task != questionAssessmentTask || got.Request.Validate() != nil {
		t.Fatalf("judgment %+v", got.Judgment)
	}
	question.Asked.Question = strings.Repeat("é", maxAssessmentState)
	if _, ok, err := a.judgment(question, masonRole, nil, nil); err != nil || !ok {
		t.Fatalf("a long question is assessed from its start: %v %v", ok, err)
	}
}
