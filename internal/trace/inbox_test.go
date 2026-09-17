package trace

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// The owner rules on an escalation once, the ruling and its notice land in one
// commit, and the chief of staff relays it once to the whole batch.
func TestOwnerRulesOnceAndTheChiefOfStaffRelaysOnce(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	chief := chiefThread(t, r, "chief1")
	for i, agent := range []string{"mason1", "mason2", "mason3", "mason4"} {
		scope := askerThread(t, r, agent, "mason", "build_"+agent)
		if _, err := r.Ask(ctx, agent, scope, "Question from "+agent, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	escalated := at.Add(time.Hour)
	batch := EscalationRequest{Questions: []string{"2", "1"}, Rephrasing: "Should uploads resume after a restart?", Blocked: "The upload unit.", Options: []string{"Resume", "Restart"}, Recommendation: "Resume."}
	single := EscalationRequest{Questions: []string{"3"}, Rephrasing: "Is the log format fixed?", Blocked: "The review.", Recommendation: "Yes."}
	for _, req := range []EscalationRequest{batch, single} {
		if _, err := r.EscalateQuestions(ctx, "chief", chief, req, escalated); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := r.Inbox()
	if err != nil || len(entries) != 2 {
		t.Fatalf("inbox: %+v %v", entries, err)
	}
	first := entries[0]
	asked := func(e InboxEntry) (ids []string) {
		for _, q := range e.Questions {
			ids = append(ids, q.Asked.ID+" "+q.Asked.Question)
		}
		return ids
	}
	if first.Number != 1 || first.Workstream != streamID || first.Batch != "escalation_2" || first.Rephrasing != batch.Rephrasing || first.Blocked != batch.Blocked ||
		!slices.Equal(first.Options, batch.Options) || first.Recommendation != batch.Recommendation || !first.EscalatedAt.Equal(escalated) || first.State != QuestionEscalated ||
		!slices.Equal(asked(first), []string{"2 Question from mason2", "1 Question from mason1"}) {
		t.Fatalf("batch entry: %+v", first)
	}
	if e := entries[1]; e.Number != 2 || e.Batch != "escalation_3" || len(e.Options) != 0 || !slices.Equal(asked(e), []string{"3 Question from mason3"}) {
		t.Fatalf("single entry: %+v", e)
	}

	// Nothing is relayed before the owner rules.
	when := escalated.Add(time.Hour)
	for id, reason := range map[string]string{"1": "question 1 has no ruling from the owner to relay", "4": "question 4 has no ruling from the owner to relay", "9": "there is no question 9 in this workstream"} {
		if got := refusal(t, second(r.RelayRuling(ctx, "chief", chief, id, "Resume.", ScopeLocal, when))); got != reason {
			t.Fatalf("relay of question %s: %s", id, got)
		}
	}

	owner := Actor{Kind: "owner", ID: "local"}
	before, err := r.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(step string) {
		t.Helper()
		after, err := r.Outbox(streamID)
		rulings, readErr := Read[Ruling](r, streamID)
		if err != nil || readErr != nil || len(after) != len(before) || len(rulings) != 0 || questionStates(t, r)["1"].State != QuestionEscalated {
			t.Fatalf("%s wrote: %d events, %d rulings", step, len(after)-len(before), len(rulings))
		}
	}
	if _, err := r.Rule(ctx, 9, "Resume.", owner, when); !errors.Is(err, ErrInboxEntry) {
		t.Fatalf("unknown entry: %v", err)
	}
	for name, call := range map[string]func() error{
		"blank ruling": func() error { return second(r.Rule(ctx, 1, " \n", owner, when)) },
		"no timestamp": func() error { return second(r.Rule(ctx, 1, "Resume uploads.", owner, time.Time{})) },
	} {
		if err := call(); err == nil || err.Error() != "a ruling needs text and a timestamp" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	r.failPublication = func(step string) error {
		if step == "before-ref" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := r.Rule(ctx, 1, "Resume uploads.", owner, when); err == nil {
		t.Fatal("failed publication reported success")
	}
	r.failPublication = nil
	unchanged("a refused or failed ruling")

	ruled, err := r.Rule(ctx, 1, "Resume uploads.", owner, when)
	if err != nil || ruled.Number != 1 || ruled.Batch != "escalation_2" || len(ruled.Questions) != 2 {
		t.Fatalf("ruled entry: %+v %v", ruled, err)
	}
	wantRuling := func(id string) Ruling {
		return Ruling{Header: Header{Schema: "osmia.trace.ruling", Version: 1, ID: id, Revision: 1, Project: projectID, Workstream: streamID, Unit: "unit1", At: when,
			Actor: owner, Cause: "question_" + id + "_escalated", Depth: 4}, QuestionID: id, QuestionRevision: 2, Decision: DecisionRuling, OwnerResponse: "Resume uploads."}
	}
	for _, id := range []string{"1", "2"} {
		s := questionStates(t, r)[id]
		if s.State != QuestionRuled || s.Ruling == nil || !reflect.DeepEqual(*s.Ruling, wantRuling(id)) {
			t.Fatalf("ruled question %s: %+v", id, s)
		}
		if state, err := r.Workflow(streamID, QuestionSubject(id)); err != nil || state != (WorkflowState{Version: 3, Value: QuestionRuled}) {
			t.Fatalf("workflow state of question %s: %+v %v", id, state, err)
		}
		committed(t, r, id+"/rulings.jsonl")
	}
	after, err := r.Outbox(streamID)
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("a batch ruling raised %d events: %v", len(after)-len(before), err)
	}
	var event *OutboxEntry
	for i, e := range after {
		if e.TransitionID == "question_2_ruled" {
			event = &after[i]
		}
	}
	if event == nil || event.Event.Kind != NoticeKind || event.Event.Body != "The owner ruled on inbox entry 1, escalation_2 (questions 2, 1): Resume uploads." || !event.At.Equal(when) {
		t.Fatalf("event: %+v", event)
	}
	if got := statusOf(t, r).OpenQuestions; got != 2 {
		t.Fatalf("open questions after the ruling: %d", got)
	}

	// A second answer conflicts and changes nothing.
	if _, err := r.Rule(ctx, 1, "Restart uploads.", owner, when); !errors.Is(err, ErrRuled) {
		t.Fatalf("second ruling: %v", err)
	}
	if s := questionStates(t, r)["1"]; !reflect.DeepEqual(*s.Ruling, wantRuling("1")) {
		t.Fatalf("second ruling changed the first: %+v", s.Ruling)
	}
	if got := refusal(t, second(r.AnswerQuestion(ctx, "chief", chief, "1", "Resume.", []string{"charter#1"}, when))); got != "question 1 has the owner's ruling; relay it with relay_ruling" {
		t.Fatalf("answer to a ruled question: %s", got)
	}

	relayedAt := when.Add(time.Hour)
	mason := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: "mason1_thread", Turn: "build_mason1", Role: "mason"}
	failure(t, second(r.RelayRuling(ctx, "mason1", mason, "1", "Resume.", ScopeLocal, relayedAt)), "only a chief-of-staff turn may choose what happens to a question")
	for reason, call := range map[string]func() error{
		"text is required: the owner's ruling as the askers should read it": func() error {
			return second(r.RelayRuling(ctx, "chief", chief, "1", " ", ScopeLocal, relayedAt))
		},
		"scope must be local, for the askers only, or notify, for a notice to the whole project": func() error {
			return second(r.RelayRuling(ctx, "chief", chief, "1", "Resume.", "everyone", relayedAt))
		},
	} {
		if got := refusal(t, call()); got != reason {
			t.Fatalf("refusal %q, want %q", got, reason)
		}
	}
	if s := questionStates(t, r)["1"]; s.State != QuestionRuled || s.Ruling.Revision != 1 {
		t.Fatalf("a refused relay changed question 1: %+v", s)
	}
	questions, err := r.RelayRuling(ctx, "chief", chief, "1", "Uploads resume after a restart.", ScopeNotify, relayedAt)
	if err != nil || !slices.Equal(questions, []string{"2", "1"}) {
		t.Fatalf("relayed questions %v: %v", questions, err)
	}
	if got := refusal(t, second(r.RelayRuling(ctx, "chief", chief, "2", "Again.", ScopeLocal, relayedAt))); got != "question 2 is already answered" {
		t.Fatalf("second relay: %s", got)
	}
	if _, err := r.Rule(ctx, 1, "Restart uploads.", owner, relayedAt); !errors.Is(err, ErrRuled) {
		t.Fatalf("ruling on a relayed entry: %v", err)
	}
	if _, err := r.Rule(ctx, 2, "Fixed.", owner, relayedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RelayRuling(ctx, "chief", chief, "3", "The log format is fixed.", ScopeLocal, relayedAt); err != nil {
		t.Fatal(err)
	}

	check := func(r *Repository) {
		t.Helper()
		s := questionStates(t, r)
		for _, id := range []string{"1", "2"} {
			want := wantRuling(id)
			want.Revision, want.At, want.Actor, want.Cause, want.Depth = 2, relayedAt, Actor{Kind: "agent", ID: "chief"}, "request_chief1", 3
			want.ReturnedAnswer, want.Scope = "Uploads resume after a restart.", ScopeNotify
			if q := s[id]; q.State != QuestionAnswered || !reflect.DeepEqual(*q.Ruling, want) {
				t.Fatalf("relayed question %s: %+v", id, q.Ruling)
			}
		}
		if q := s["3"]; q.State != QuestionAnswered || q.Ruling.Scope != ScopeLocal || q.Ruling.OwnerResponse != "Fixed." || q.Ruling.ReturnedAnswer != "The log format is fixed." {
			t.Fatalf("local ruling: %+v", q.Ruling)
		}
		if q := s["4"]; q.State != QuestionOpen || q.Ruling != nil {
			t.Fatalf("untouched question: %+v", q)
		}
		entries, err := r.Inbox()
		if err != nil || len(entries) != 2 || entries[0].State != QuestionAnswered || entries[1].State != QuestionAnswered {
			t.Fatalf("inbox after the rulings: %+v %v", entries, err)
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

// An escalation carries its inbox number, and a ruling holds what the owner
// said, what went back or both, with a scope only on what went back.
func TestEscalationAndRulingValidation(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	question := Question{Header: header("question", "q1"), AskedBy: Actor{Kind: "agent", ID: "mason1"}, Question: "Which store?", SentToOwner: "Which store should uploads use?",
		Escalation: &Escalation{Batch: "escalation_q1", Questions: []string{"q1"}, Blocked: "The unit.", Recommendation: "Files."}}
	if err := r.Append(ctx, question); err == nil {
		t.Fatal("an escalation without an inbox number was recorded")
	}
	question.Escalation.Inbox = 1
	if err := r.Append(ctx, question); err != nil {
		t.Fatal(err)
	}
	ruling := func(id, owner, answer, scope string) Ruling {
		return Ruling{Header: header("ruling", id), QuestionID: "q1", QuestionRevision: 1, Decision: DecisionRuling, OwnerResponse: owner, ReturnedAnswer: answer, Scope: scope}
	}
	for name, bad := range map[string]Ruling{
		"neither response nor answer": ruling("r1", "", " ", ""),
		"unknown scope":               ruling("r1", "Files.", "Use files.", "everyone"),
		"scope without an answer":     ruling("r1", "Files.", "", ScopeNotify),
	} {
		if err := r.Append(ctx, bad); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	for _, good := range []Ruling{ruling("r1", "Files.", "", ""), ruling("r2", "", "Use files.", ""), ruling("r3", "Files.", "Use files.", ScopeLocal), ruling("r4", "Files.", "Use files.", ScopeNotify)} {
		if err := r.Append(ctx, good); err != nil {
			t.Fatalf("ruling %s: %v", good.ID, err)
		}
	}
}
