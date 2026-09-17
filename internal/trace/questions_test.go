package trace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// askerThread creates a thread of the role with one claimed turn and returns
// the scope of that turn.
func askerThread(t *testing.T, r *Repository, agent, role, turn string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	if err := r.CreateThread(ctx, Agent{Header: header("agent", agent), Role: role, ThreadID: agent + "_thread"}); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_"+turn), AgentID: agent, ThreadID: agent + "_thread", TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Build the unit"}
	req.Unit = "unit1"
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, streamID, agent, "token_"+turn, "/owned/"+turn, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: agent + "_thread", Turn: turn, Role: role}
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var refused *QuestionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refused.Reason
}

func questionStates(t *testing.T, r *Repository) map[string]QuestionState {
	t.Helper()
	list, err := r.Questions(streamID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]QuestionState{}
	for i, q := range list {
		if i > 0 && list[i-1].Asked.At.After(q.Asked.At) {
			t.Fatalf("questions out of order: %+v", list)
		}
		out[q.Asked.ID] = q
	}
	return out
}

// committed checks that the trace's head commit holds the question file as
// it is on disk.
func committed(t *testing.T, r *Repository, name string) {
	t.Helper()
	name = "workstreams/" + string(streamID) + "/questions/" + name
	disk, err := os.ReadFile(r.directory + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.gitBytes(context.Background(), nil, "", "cat-file", "blob", "HEAD:"+name)
	if err != nil || string(head) != string(disk) || len(disk) == 0 {
		t.Fatalf("%s is not committed as written: %v", name, err)
	}
}

func TestAskRecordsTheQuestionAndOneEventTogether(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	chief := chiefThread(t, r, "chief1")
	mason := askerThread(t, r, "mason1", "mason", "build1")
	before, err := r.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Ask(ctx, "chief", chief, "May I ask?", at); err == nil || !strings.Contains(err.Error(), "cannot ask") {
		t.Fatalf("chief of staff asked: %v", err)
	}
	if got := refusal(t, second(r.Ask(ctx, "mason1", mason, " \n", at))); !strings.Contains(got, "question is required") {
		t.Fatalf("empty question: %s", got)
	}
	// A publication that fails before the ref moves leaves neither the
	// question nor its event.
	r.failPublication = func(step string) error {
		if step == "before-ref" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := r.Ask(ctx, "mason1", mason, "Which store?", at); err == nil {
		t.Fatal("failed publication reported success")
	}
	r.failPublication = nil
	if got, _ := r.Outbox(streamID); len(questionStates(t, r)) != 0 || len(got) != len(before) {
		t.Fatalf("failed ask left records: %d events", len(got))
	}

	asked := at.Add(time.Minute)
	q, err := r.Ask(ctx, "mason1", mason, "Which store?", asked)
	if err != nil {
		t.Fatal(err)
	}
	want := Question{Header: Header{Schema: "osmia.trace.question", Version: 1, ID: "1", Revision: 1, Project: projectID, Workstream: streamID, Unit: "unit1", At: asked,
		Actor: Actor{Kind: "agent", ID: "mason1"}, Cause: "request_build1", Depth: 3}, AskedBy: Actor{Kind: "agent", ID: "mason1"}, Thread: "mason1_thread", Turn: "build1", Question: "Which store?"}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("question:\n%+v\nwant\n%+v", q, want)
	}
	stored, err := Get[Question](r, streamID, "1", 1)
	if err != nil || !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored question: %+v %v", stored, err)
	}
	committed(t, r, "1/question.jsonl")
	if s := questionStates(t, r)["1"]; s.State != QuestionOpen || s.Ruling != nil || !reflect.DeepEqual(s.Latest, want) {
		t.Fatalf("state: %+v", s)
	}
	if state, err := r.Workflow(streamID, QuestionSubject("1")); err != nil || state != (WorkflowState{Version: 1, Value: QuestionOpen}) {
		t.Fatalf("workflow state: %+v %v", state, err)
	}
	after, err := r.Outbox(streamID)
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("outbox grew by %d: %v", len(after)-len(before), err)
	}
	var event *OutboxEntry
	for i, e := range after {
		if e.TransitionID == "question_1_open" {
			event = &after[i]
		}
	}
	if event == nil || event.Event.Kind != NoticeKind || event.Event.Body != "Question 1 is open, asked by the mason: Which store?" || !event.At.Equal(asked) {
		t.Fatalf("event: %+v", event)
	}
	if got := statusOf(t, r).OpenQuestions; got != 1 {
		t.Fatalf("open questions: %d", got)
	}

	if got := refusal(t, second(r.Ask(ctx, "mason1", mason, "And which key?", asked))); got != "this turn already asked question 1; end the turn, the answer arrives as your next turn" {
		t.Fatalf("second ask of one turn: %s", got)
	}
	reviewer := askerThread(t, r, "reviewer1", "reviewer", "review1")
	next, err := r.Ask(ctx, "reviewer1", reviewer, "Is the log format fixed?", asked.Add(time.Minute))
	if err != nil || next.ID != "2" {
		t.Fatalf("second question: %+v %v", next, err)
	}
	// A turn that is not this session's active claim cannot ask.
	stale := mason
	stale.Turn = "other"
	if _, err := r.Ask(ctx, "mason1", stale, "Which store?", asked); err == nil {
		t.Fatal("unclaimed turn asked")
	}
	if got, _ := r.Outbox(streamID); len(questionStates(t, r)) != 2 || len(got) != len(before)+2 {
		t.Fatalf("refused asks left records: %d events", len(got))
	}
}

func second[T any](_ T, err error) error { return err }

func TestChiefOfStaffChoosesOnceForEachQuestion(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	chief := chiefThread(t, r, "chief1")
	for i, agent := range []string{"mason1", "mason2", "mason3", "mason4"} {
		scope := askerThread(t, r, agent, "mason", "build_"+agent)
		if _, err := r.Ask(ctx, agent, scope, "Question from "+agent, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	mason := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: "mason1_thread", Turn: "build_mason1", Role: "mason"}
	when := at.Add(time.Hour)

	// Only the chief of staff chooses, and an answer needs text and a citation.
	if _, err := r.AnswerQuestion(ctx, "mason1", mason, "1", "Use the file store.", []string{"charter#1"}, when); err == nil || !strings.Contains(err.Error(), "only a chief-of-staff turn") {
		t.Fatalf("mason answered: %v", err)
	}
	if _, err := r.EscalateQuestions(ctx, "mason1", mason, EscalationRequest{Questions: []string{"1"}, Rephrasing: "r", Blocked: "b", Recommendation: "c"}, when); err == nil || !strings.Contains(err.Error(), "only a chief-of-staff turn") {
		t.Fatalf("mason escalated: %v", err)
	}
	for reason, call := range map[string]func() error{
		"an answer needs at least one citation; escalate a question the record does not settle": func() error {
			return second(r.AnswerQuestion(ctx, "chief", chief, "1", "Use the file store.", nil, when))
		},
		"text is required: the answer the asker receives": func() error {
			return second(r.AnswerQuestion(ctx, "chief", chief, "1", " ", []string{"charter#1"}, when))
		},
		"there is no question 9 in this workstream": func() error {
			return second(r.AnswerQuestion(ctx, "chief", chief, "9", "Use the file store.", []string{"charter#1"}, when))
		},
	} {
		if got := refusal(t, call()); got != reason {
			t.Fatalf("refusal %q, want %q", got, reason)
		}
	}
	if s := questionStates(t, r)["1"]; s.State != QuestionOpen || s.Ruling != nil {
		t.Fatalf("refused answers changed question 1: %+v", s)
	}

	ruling, err := r.AnswerQuestion(ctx, "chief", chief, "1", "Use the file store.", []string{"charter#1", "kb/store.md"}, when)
	if err != nil {
		t.Fatal(err)
	}
	wantRuling := Ruling{Header: Header{Schema: "osmia.trace.ruling", Version: 1, ID: "1", Revision: 1, Project: projectID, Workstream: streamID, Unit: "unit1", At: when,
		Actor: Actor{Kind: "agent", ID: "chief"}, Cause: "request_chief1", Depth: 3}, QuestionID: "1", QuestionRevision: 1, Decision: DecisionAnswer, ReturnedAnswer: "Use the file store.", Citations: []string{"charter#1", "kb/store.md"}}
	if !reflect.DeepEqual(ruling, wantRuling) {
		t.Fatalf("ruling:\n%+v\nwant\n%+v", ruling, wantRuling)
	}
	if got := refusal(t, second(r.AnswerQuestion(ctx, "chief", chief, "1", "Use the database.", []string{"charter#2"}, when))); got != "question 1 is already answered" {
		t.Fatalf("second answer: %s", got)
	}
	if got := refusal(t, second(r.EscalateQuestions(ctx, "chief", chief, EscalationRequest{Questions: []string{"1"}, Rephrasing: "r", Blocked: "b", Recommendation: "c"}, when))); got != "question 1 is already answered" {
		t.Fatalf("escalation of an answered question: %s", got)
	}

	// A batch escalates together or not at all.
	request := EscalationRequest{Questions: []string{"2", "3"}, Rephrasing: "Should uploads resume after a restart?", Blocked: "The upload unit and its review.", Options: []string{"Resume", "Restart"}, Recommendation: "Resume."}
	for reason, change := range map[string]func(*EscalationRequest){
		"questions is required: list at least one open question":           func(e *EscalationRequest) { e.Questions = nil },
		"rephrasing is required: the question as the owner should read it": func(e *EscalationRequest) { e.Rephrasing = "" },
		"blocked is required: what waits on the owner's answer":            func(e *EscalationRequest) { e.Blocked = " " },
		"recommendation is required: what you would decide":                func(e *EscalationRequest) { e.Recommendation = "" },
		"options must not contain an empty entry":                          func(e *EscalationRequest) { e.Options = []string{"Resume", ""} },
		"question 2 is listed twice":                                       func(e *EscalationRequest) { e.Questions = []string{"2", "2"} },
		"question 1 is already answered":                                   func(e *EscalationRequest) { e.Questions = []string{"2", "1"} },
		"there is no question 7 in this workstream":                        func(e *EscalationRequest) { e.Questions = []string{"2", "7"} },
	} {
		bad := request
		change(&bad)
		if got := refusal(t, second(r.EscalateQuestions(ctx, "chief", chief, bad, when))); got != reason {
			t.Fatalf("refusal %q, want %q", got, reason)
		}
	}
	if s := questionStates(t, r); s["2"].State != QuestionOpen || s["2"].Latest.Revision != 1 {
		t.Fatalf("a refused batch escalated question 2: %+v", s["2"])
	}
	batch, err := r.EscalateQuestions(ctx, "chief", chief, request, when)
	if err != nil || batch != "escalation_2" {
		t.Fatalf("batch %q: %v", batch, err)
	}
	for _, name := range []string{"1/rulings.jsonl", "2/question.jsonl", "3/question.jsonl"} {
		committed(t, r, name)
	}
	if got := refusal(t, second(r.AnswerQuestion(ctx, "chief", chief, "2", "Resume.", []string{"charter#1"}, when))); got != "question 2 is escalated to the owner; only the owner's ruling answers it" {
		t.Fatalf("answer to an escalated question: %s", got)
	}
	again := request
	again.Questions = []string{"3", "4"}
	if got := refusal(t, second(r.EscalateQuestions(ctx, "chief", chief, again, when))); got != "question 3 is escalated to the owner; only the owner's ruling answers it" {
		t.Fatalf("second escalation: %s", got)
	}
	single := request
	single.Questions, single.Options = []string{"4"}, nil
	if batch, err := r.EscalateQuestions(ctx, "chief", chief, single, when); err != nil || batch != "escalation_4" {
		t.Fatalf("single escalation %q: %v", batch, err)
	}

	check := func(r *Repository) {
		t.Helper()
		s := questionStates(t, r)
		if len(s) != 4 || s["1"].State != QuestionAnswered || !reflect.DeepEqual(*s["1"].Ruling, wantRuling) || s["1"].Latest.Revision != 1 {
			t.Fatalf("answered question: %+v", s["1"])
		}
		for _, id := range []string{"2", "3"} {
			q := s[id]
			e := Escalation{Batch: "escalation_2", Questions: []string{"2", "3"}, Blocked: request.Blocked, Options: request.Options, Recommendation: request.Recommendation}
			if q.State != QuestionEscalated || q.Ruling != nil || q.Latest.Revision != 2 || q.Latest.SentToOwner != request.Rephrasing || !reflect.DeepEqual(*q.Latest.Escalation, e) ||
				q.Latest.Question != q.Asked.Question || q.Latest.Thread != q.Asked.Thread || q.Latest.Actor != (Actor{Kind: "agent", ID: "chief"}) || q.Asked.Escalation != nil {
				t.Fatalf("escalated question %s: %+v", id, q)
			}
		}
		if q := s["4"]; q.State != QuestionEscalated || q.Latest.Escalation.Batch != "escalation_4" || len(q.Latest.Escalation.Options) != 0 {
			t.Fatalf("single escalation: %+v", q)
		}
		// An escalated question stays open until it is ruled on.
		if got := statusOf(t, r).OpenQuestions; got != 3 {
			t.Fatalf("open questions: %d", got)
		}
	}
	check(r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}
