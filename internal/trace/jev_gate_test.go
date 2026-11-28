package trace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestJevRulingsAreWrittenByJevAndNeverByAnotherActor(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	question := Question{Header: header("question", "q1"), AskedBy: Actor{Kind: "agent", ID: "mason1"}, Question: "Which store?"}
	if err := r.Append(ctx, question); err != nil {
		t.Fatal(err)
	}
	ruling := func(id string, revision int, by, judgment string, actor Actor, relayed string) Ruling {
		h := header("ruling", id)
		h.Revision, h.Actor = revision, actor
		return Ruling{Header: h, QuestionID: "q1", QuestionRevision: 1, Decision: DecisionRuling, OwnerResponse: "Files.", ReturnedAnswer: relayed, DecidedBy: by, Judgment: judgment}
	}
	manager := Actor{Kind: "agent", ID: Manager}
	owner := Actor{Kind: "owner", ID: "local"}
	for name, bad := range map[string]Ruling{
		"Jev's decision with the owner as actor":          ruling("b1", 1, DecidedByJev, "judgment_ab12", owner, ""),
		"Jev's decision with the chief of staff as actor": ruling("b2", 1, DecidedByJev, "judgment_ab12", manager, ""),
		"Jev as the actor of an owner decision":           ruling("b3", 1, "", "", JevActor, ""),
		"Jev as the actor of a chief of staff decision":   ruling("b4", 1, DecidedByManager, "", JevActor, ""),
		"Jev's decision by a service other than Jev":      ruling("b5", 1, DecidedByJev, "judgment_ab12", Actor{Kind: "service", ID: "questions"}, ""),
	} {
		if err := r.Append(ctx, bad); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	jev, own := ruling("r1", 1, DecidedByJev, "judgment_ab12", JevActor, ""), ruling("r2", 1, "", "", owner, "")
	for _, good := range []Ruling{jev, own, ruling("r3", 1, DecidedByManager, "", manager, "")} {
		if err := r.Append(ctx, good); err != nil {
			t.Fatalf("ruling %s: %v", good.ID, err)
		}
	}
	for name, bad := range map[string]Ruling{
		"a relay of Jev's decision by the owner":      ruling("r1", 2, DecidedByJev, "judgment_ab12", owner, "Files."),
		"a relay of Jev's decision that returns none": ruling("r1", 2, DecidedByJev, "judgment_ab12", manager, ""),
		"the owner's decision relayed by Jev":         ruling("r2", 2, "", "", JevActor, "Files."),
	} {
		if err := r.Append(ctx, bad); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	if err := r.Append(ctx, ruling("r1", 2, DecidedByJev, "judgment_ab12", manager, "Files.")); err != nil {
		t.Fatalf("the chief of staff's relay of Jev's decision: %v", err)
	}
}

func TestJevDecisionDocumentsAreWrittenByJevAndNeverByAnotherActor(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	owner := Actor{Kind: "owner", ID: "local"}
	document := func(id, path, content string, actor Actor) Document {
		h := header("document", id)
		h.Actor = actor
		return Document{Header: h, Path: path, Content: content}
	}
	const ratificationPath, decisionPath = "shed/round-1/ratification.json", "amendments/1/decision.json"
	jevRatification := `{"version":1,"round":1,"decided_by":"jev","judgment":"judgment_ab12"}`
	ownerRatification := `{"version":1,"round":1}`
	jevDecision := `{"decision":"approve","actor":"jev","judgment":"judgment_ab12"}`
	ownerDecision := `{"decision":"approve","actor":"local"}`
	for name, bad := range map[string]Document{
		"Jev's ratification written by the owner":       document("shed-round-1-ratification", ratificationPath, jevRatification, owner),
		"the owner's ratification written by Jev":       document("shed-round-1-ratification", ratificationPath, ownerRatification, JevActor),
		"Jev's amendment decision written by the owner": document("amendment_1_decision", decisionPath, jevDecision, owner),
		"the owner's amendment decision written by Jev": document("amendment_1_decision", decisionPath, ownerDecision, JevActor),
	} {
		if err := r.RecordDocuments(ctx, []Document{bad}); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	for _, good := range []Document{
		document("shed-round-1-ratification", ratificationPath, jevRatification, JevActor),
		document("amendment_1_decision", decisionPath, jevDecision, JevActor),
		document("shed-round-2-ratification", "shed/round-2/ratification.json", ownerRatification, owner),
		document("amendment_2_decision", "amendments/2/decision.json", ownerDecision, owner),
	} {
		if err := r.RecordDocuments(ctx, []Document{good}); err != nil {
			t.Fatalf("%s: %v", good.Path, err)
		}
	}
	if err := r.RecordDocuments(ctx, []Document{document("spec", "spec.md", jevRatification, JevActor)}); err != nil {
		t.Fatal(err)
	}
}

func TestJevRulesOnAnEscalationAndTheManagerRelaysIt(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	manager := managerThread(t, r, "chief1")
	for i, agent := range []string{"mason1", "mason2"} {
		scope := askerThread(t, r, agent, "mason", "build_"+agent)
		if _, err := r.Ask(ctx, agent, scope, "Question from "+agent, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1", "2"} {
		req := EscalationRequest{Questions: []string{id}, Rephrasing: "Should uploads resume?", Blocked: "The upload unit.", Options: []string{"Resume", "Restart"}, Recommendation: "Resume."}
		if _, err := r.EscalateQuestions(ctx, "chief", manager, req, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	when := at.Add(2 * time.Hour)
	owner := Actor{Kind: "owner", ID: "local"}
	for name, call := range map[string]func() error{
		"a judgment of the owner":   func() error { return second(r.RuleDecided(ctx, 1, "Resume.", owner, "judgment_ab12", when)) },
		"a judgment of nobody":      func() error { return second(r.RuleDecided(ctx, 1, "Resume.", JevActor, "", when)) },
		"a judgment that is no key": func() error { return second(r.RuleDecided(ctx, 1, "Resume.", JevActor, "not a key", when)) },
	} {
		if err := call(); err == nil {
			t.Fatalf("%s was recorded", name)
		}
	}
	if rulings, err := Read[Ruling](r, streamID); err != nil || len(rulings) != 0 || questionStates(t, r)["1"].State != QuestionEscalated {
		t.Fatalf("a refused ruling wrote: %+v %v", rulings, err)
	}

	if _, err := r.RuleDecided(ctx, 1, "Resume.", JevActor, "judgment_ab12", when); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rule(ctx, 2, "Restart.", owner, when); err != nil {
		t.Fatal(err)
	}
	states := questionStates(t, r)
	jev, own := states["1"], states["2"]
	if jev.State != QuestionRuled || jev.Ruling == nil || jev.Ruling.Actor != JevActor || jev.Ruling.DecidedBy != DecidedByJev || jev.Ruling.Judgment != "judgment_ab12" || jev.Ruling.OwnerResponse != "Resume." {
		t.Fatalf("Jev's ruling: %+v", jev)
	}
	if own.State != QuestionRuled || own.Ruling == nil || own.Ruling.Actor != owner || own.Ruling.DecidedBy != "" || own.Ruling.Judgment != "" {
		t.Fatalf("the owner's ruling: %+v", own)
	}
	outbox, err := r.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, "ruled on inbox entry") {
			notices = append(notices, e.Event.Body)
		}
	}
	if len(notices) != 2 || !strings.HasPrefix(notices[0], "Jev, the decision maker for the owner, ruled on inbox entry 1,") || !strings.HasPrefix(notices[1], "The owner ruled on inbox entry 2,") {
		t.Fatalf("notices: %q", notices)
	}

	if err := second(r.RuleDecided(ctx, 1, "Resume.", JevActor, "judgment_ab12", when)); err == nil {
		t.Fatal("an escalation ruled twice")
	}
	if _, err := r.RelayRuling(ctx, "chief", manager, "1", "Resume.", ScopeLocal, when.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	relayed := questionStates(t, r)["1"]
	if relayed.State != QuestionAnswered || relayed.Ruling == nil || relayed.Ruling.Revision != 2 || relayed.Ruling.Actor.Kind != "agent" || relayed.Ruling.DecidedBy != DecidedByJev || relayed.Ruling.Judgment != "judgment_ab12" || relayed.Ruling.ReturnedAnswer != "Resume." {
		t.Fatalf("the relay of Jev's ruling: %+v", relayed)
	}
}
