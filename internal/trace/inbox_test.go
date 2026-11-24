package trace

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

var inboxOwner = Actor{Kind: "owner", ID: "local"}

type escalatedInbox struct {
	r         *Repository
	scoped    *Repository
	root      config.Root
	project   config.Project
	chief     coreadapter.Scope
	batch     EscalationRequest
	escalated time.Time
	before    []OutboxEntry
}

func (i escalatedInbox) when() time.Time      { return i.escalated.Add(time.Hour) }
func (i escalatedInbox) relayedAt() time.Time { return i.when().Add(time.Hour) }

func (i escalatedInbox) wantRuling(id string) Ruling {
	return Ruling{Header: Header{Schema: "osmia.trace.ruling", Version: 1, ID: id, Revision: 1, Project: projectID, Workstream: streamID, Unit: "unit1", At: i.when(),
		Actor: inboxOwner, Cause: "question_" + id + "_escalated", Depth: 4}, QuestionID: id, QuestionRevision: 2, Decision: DecisionRuling, OwnerResponse: "Resume uploads."}
}

func newEscalatedInbox(t *testing.T) escalatedInbox {
	t.Helper()
	ctx := context.Background()
	r, root, p := create(t)
	scoped := mustScoped(t, r, streamID)
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
	return escalatedInbox{r: r, scoped: scoped, root: root, project: p, chief: chief, batch: batch, escalated: escalated}
}

func (i escalatedInbox) abandoned(t *testing.T) escalatedInbox {
	t.Helper()
	if _, err := i.r.SetFeatureState(context.Background(), header("transition", "abandoned"), "abandoned", "Owner abandoned the workstream"); err != nil {
		t.Fatal(err)
	}
	before, err := i.r.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	i.before = before
	return i
}

func (i escalatedInbox) ruled(t *testing.T) escalatedInbox {
	t.Helper()
	i = i.abandoned(t)
	if _, err := i.r.Rule(context.Background(), 1, "Resume uploads.", inboxOwner, i.when(), "delivered"); err != nil {
		t.Fatal(err)
	}
	return i
}

func TestEscalatedQuestionsAreNumberedInboxEntries(t *testing.T) {
	i := newEscalatedInbox(t)
	entries, err := i.r.Inbox()
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
	if first.Number != 1 || first.Workstream != streamID || first.Batch != "escalation_2" || first.Rephrasing != i.batch.Rephrasing || first.Blocked != i.batch.Blocked ||
		!slices.Equal(first.Options, i.batch.Options) || first.Recommendation != i.batch.Recommendation || !first.EscalatedAt.Equal(i.escalated) || first.State != QuestionEscalated ||
		!slices.Equal(asked(first), []string{"2 Question from mason2", "1 Question from mason1"}) {
		t.Fatalf("batch entry: %+v", first)
	}
	if e := entries[1]; e.Number != 2 || e.Batch != "escalation_3" || len(e.Options) != 0 || !slices.Equal(asked(e), []string{"3 Question from mason3"}) {
		t.Fatalf("single entry: %+v", e)
	}
}

func TestRelayingBeforeTheOwnerRulesIsRefused(t *testing.T) {
	i := newEscalatedInbox(t)
	for id, reason := range map[string]string{"1": "question 1 has no ruling from the owner to relay", "4": "question 4 has no ruling from the owner to relay", "9": "there is no question 9 in this workstream"} {
		if got := refusal(t, second(i.r.RelayRuling(context.Background(), "chief", i.chief, id, "Resume.", ScopeLocal, i.when()))); got != reason {
			t.Fatalf("relay of question %s: %s", id, got)
		}
	}
}

func TestRefusedOrFailedRulingWritesNothing(t *testing.T) {
	ctx := context.Background()
	i := newEscalatedInbox(t).abandoned(t)
	unchanged := func(step string) {
		t.Helper()
		after, err := i.r.Outbox(streamID)
		rulings, readErr := Read[Ruling](i.r, streamID)
		if err != nil || readErr != nil || len(after) != len(i.before) || len(rulings) != 0 || questionStates(t, i.r)["1"].State != QuestionEscalated {
			t.Fatalf("%s wrote: %d events, %d rulings", step, len(after)-len(i.before), len(rulings))
		}
	}
	if _, err := i.r.Rule(ctx, 9, "Resume.", inboxOwner, i.when()); !errors.Is(err, ErrInboxEntry) {
		t.Fatalf("unknown entry: %v", err)
	}
	if e, err := i.r.Rule(ctx, 1, "Resume uploads.", inboxOwner, i.when(), "delivered", "abandoned"); !errors.Is(err, ErrFeatureState) || e.Number != 1 || e.Workstream != streamID {
		t.Fatalf("ruling on a refused workstream's entry: %+v %v", e, err)
	}
	for name, call := range map[string]func() error{
		"blank ruling": func() error { return second(i.r.Rule(ctx, 1, " \n", inboxOwner, i.when())) },
		"no timestamp": func() error { return second(i.r.Rule(ctx, 1, "Resume uploads.", inboxOwner, time.Time{})) },
	} {
		if err := call(); err == nil || err.Error() != "a ruling needs text and a timestamp" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	i.scoped.failPublication = func(step string) error {
		if step == boundaryBeforeCommit {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := i.r.Rule(ctx, 1, "Resume uploads.", inboxOwner, i.when()); err == nil {
		t.Fatal("failed publication reported success")
	}
	i.scoped.failPublication = nil
	unchanged("a refused or failed ruling")
}

func TestOwnerRulingOnABatchRulesEveryQuestionAndRaisesOneEvent(t *testing.T) {
	i := newEscalatedInbox(t).abandoned(t)
	ruled, err := i.r.Rule(context.Background(), 1, "Resume uploads.", inboxOwner, i.when(), "delivered")
	if err != nil || ruled.Number != 1 || ruled.Batch != "escalation_2" || len(ruled.Questions) != 2 {
		t.Fatalf("ruled entry: %+v %v", ruled, err)
	}
	for _, id := range []string{"1", "2"} {
		s := questionStates(t, i.r)[id]
		if s.State != QuestionRuled || s.Ruling == nil || !reflect.DeepEqual(*s.Ruling, i.wantRuling(id)) {
			t.Fatalf("ruled question %s: %+v", id, s)
		}
		if state, err := i.r.Workflow(streamID, QuestionSubject(id)); err != nil || state != (WorkflowState{Version: 3, Value: QuestionRuled}) {
			t.Fatalf("workflow state of question %s: %+v %v", id, state, err)
		}
		committed(t, i.r, id+"/rulings.jsonl")
	}
	after, err := i.r.Outbox(streamID)
	if err != nil || len(after) != len(i.before)+1 {
		t.Fatalf("a batch ruling raised %d events: %v", len(after)-len(i.before), err)
	}
	var event *OutboxEntry
	for n, e := range after {
		if e.TransitionID == "question_2_ruled" {
			event = &after[n]
		}
	}
	if event == nil || event.Event.Kind != NoticeKind || event.Event.Body != "The owner ruled on inbox entry 1, escalation_2 (questions 2, 1): Resume uploads." || !event.At.Equal(i.when()) {
		t.Fatalf("event: %+v", event)
	}
	if got := statusOf(t, i.r).OpenQuestions; got != 2 {
		t.Fatalf("open questions after the ruling: %d", got)
	}
}

func TestASecondRulingIsRefusedAndTheFirstStands(t *testing.T) {
	ctx := context.Background()
	i := newEscalatedInbox(t).ruled(t)
	if _, err := i.r.Rule(ctx, 1, "Restart uploads.", inboxOwner, i.when()); !errors.Is(err, ErrRuled) {
		t.Fatalf("second ruling: %v", err)
	}
	if _, err := i.r.Rule(ctx, 1, "Restart uploads.", inboxOwner, i.when(), "abandoned"); !errors.Is(err, ErrFeatureState) {
		t.Fatalf("second ruling on a refused workstream's entry: %v", err)
	}
	if s := questionStates(t, i.r)["1"]; !reflect.DeepEqual(*s.Ruling, i.wantRuling("1")) {
		t.Fatalf("second ruling changed the first: %+v", s.Ruling)
	}
}

func TestAnAnswerToARuledQuestionIsRefusedInFavourOfRelaying(t *testing.T) {
	i := newEscalatedInbox(t).ruled(t)
	if got := refusal(t, second(i.r.AnswerQuestion(context.Background(), "chief", i.chief, "1", "Resume.", []string{"charter#1"}, i.when()))); got != "question 1 has the owner's ruling; relay it with relay_ruling" {
		t.Fatalf("answer to a ruled question: %s", got)
	}
}

func TestRefusedRelayChangesNothing(t *testing.T) {
	ctx := context.Background()
	i := newEscalatedInbox(t).ruled(t)
	mason := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: "mason1_thread", Turn: "build_mason1", Role: "mason"}
	failure(t, second(i.r.RelayRuling(ctx, "mason1", mason, "1", "Resume.", ScopeLocal, i.relayedAt())), "only a chief-of-staff turn may choose what happens to a question")
	for reason, call := range map[string]func() error{
		"text is required: the owner's ruling as the askers should read it": func() error {
			return second(i.r.RelayRuling(ctx, "chief", i.chief, "1", " ", ScopeLocal, i.relayedAt()))
		},
		"scope must be local, for the askers only, or notify, for a notice to the whole project": func() error {
			return second(i.r.RelayRuling(ctx, "chief", i.chief, "1", "Resume.", "everyone", i.relayedAt()))
		},
	} {
		if got := refusal(t, call()); got != reason {
			t.Fatalf("refusal %q, want %q", got, reason)
		}
	}
	if s := questionStates(t, i.r)["1"]; s.State != QuestionRuled || s.Ruling.Revision != 1 {
		t.Fatalf("a refused relay changed question 1: %+v", s)
	}
}

func TestChiefRelaysARulingOnceAndItSurvivesReopening(t *testing.T) {
	ctx := context.Background()
	i := newEscalatedInbox(t).ruled(t)
	questions, err := i.r.RelayRuling(ctx, "chief", i.chief, "1", "Uploads resume after a restart.", ScopeNotify, i.relayedAt())
	if err != nil || !slices.Equal(questions, []string{"2", "1"}) {
		t.Fatalf("relayed questions %v: %v", questions, err)
	}
	if got := refusal(t, second(i.r.RelayRuling(ctx, "chief", i.chief, "2", "Again.", ScopeLocal, i.relayedAt()))); got != "question 2 is already answered" {
		t.Fatalf("second relay: %s", got)
	}
	if _, err := i.r.Rule(ctx, 1, "Restart uploads.", inboxOwner, i.relayedAt()); !errors.Is(err, ErrRuled) {
		t.Fatalf("ruling on a relayed entry: %v", err)
	}
	if _, err := i.r.Rule(ctx, 2, "Fixed.", inboxOwner, i.relayedAt()); err != nil {
		t.Fatal(err)
	}
	if _, err := i.r.RelayRuling(ctx, "chief", i.chief, "3", "The log format is fixed.", ScopeLocal, i.relayedAt()); err != nil {
		t.Fatal(err)
	}

	check := func(r *Repository) {
		t.Helper()
		s := questionStates(t, r)
		for _, id := range []string{"1", "2"} {
			want := i.wantRuling(id)
			want.Revision, want.At, want.Actor, want.Cause, want.Depth = 2, i.relayedAt(), Actor{Kind: "agent", ID: "chief"}, "request_chief1", 3
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
	check(i.r)
	if err := i.r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(i.root, i.project)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

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
