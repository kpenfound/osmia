package trace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testJudgment = "judgment_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func routed(id string, expected uint64, from, to string, route Routing) Transaction {
	tx := transaction(id, expected, from, to, 0)
	tx.Transition = tx.Transition.withRouting(route)
	return tx
}

func routedUnit(id, unit string, expected uint64, from, to string, route Routing) Transaction {
	tx := routed(id, expected, from, to, route)
	tx.Transition.Subject, tx.Transition.Header.Unit = UnitSubject(unit), unit
	return tx
}

func TestValidRouting(t *testing.T) {
	for _, c := range []struct {
		name  string
		to    string
		route Routing
		want  bool
	}{
		{"nothing recorded", "checking", Routing{}, true},
		{"a followed default", "checking", Routing{DecidedBy: DecidedByCode, DefaultTo: "checking"}, true},
		{"a followed default that cites its judgment", "checking", Routing{DecidedBy: DecidedByCode, Judgment: testJudgment, DefaultTo: "checking"}, true},
		{"a departure", "implementing", Routing{DecidedBy: DecidedByJev, Judgment: testJudgment, DefaultTo: "checking"}, true},
		{"a departure without a judgment", "implementing", Routing{DecidedBy: DecidedByJev, DefaultTo: "checking"}, false},
		{"a departure without a default", "implementing", Routing{DecidedBy: DecidedByJev, Judgment: testJudgment}, false},
		{"a departure to the default", "checking", Routing{DecidedBy: DecidedByJev, Judgment: testJudgment, DefaultTo: "checking"}, false},
		{"a fallback", "checking", Routing{DecidedBy: DecidedByFallback, DefaultTo: "checking"}, true},
		{"a fallback citing a judgment", "checking", Routing{DecidedBy: DecidedByFallback, Judgment: testJudgment, DefaultTo: "checking"}, false},
		{"an owner decision", "approved", Routing{DecidedBy: DecidedByOwner, DefaultTo: "reviewing"}, true},
		{"an owner decision citing a judgment", "approved", Routing{DecidedBy: DecidedByOwner, Judgment: testJudgment}, false},
		{"a gate decision of Jev", "ratified", Routing{DecidedBy: DecidedByJevGate, Judgment: testJudgment, DefaultTo: "ratified"}, true},
		{"a gate decision of Jev without a judgment", "ratified", Routing{DecidedBy: DecidedByJevGate}, false},
		{"a reviewer verdict", "approved", Routing{DecidedBy: DecidedByReviewer, DefaultTo: "approved"}, true},
		{"a judgment and a default without a decider", "checking", Routing{Judgment: testJudgment, DefaultTo: "checking"}, false},
		{"a default without a decider", "checking", Routing{DefaultTo: "checking"}, false},
		{"an unknown decider", "checking", Routing{DecidedBy: "chief"}, false},
		{"a default that is not a state key", "checking", Routing{DecidedBy: DecidedByCode, DefaultTo: "not a state"}, false},
		{"a judgment that is not a key", "implementing", Routing{DecidedBy: DecidedByJev, Judgment: "not a key", DefaultTo: "checking"}, false},
	} {
		if got := ValidRouting(c.to, c.route); got != c.want {
			t.Errorf("%s: ValidRouting(%q, %+v) = %v, want %v", c.name, c.to, c.route, got, c.want)
		}
	}
}

func TestOnlyAJevDepartureIsDeparted(t *testing.T) {
	for _, c := range []struct {
		name       string
		t          Transition
		departed   bool
		deviated   bool
		defaultSet bool
	}{
		{"unrouted", Transition{To: "checking"}, false, false, false},
		{"followed", Transition{To: "checking", DecidedBy: DecidedByCode, DefaultTo: "checking"}, false, false, true},
		{"departure", Transition{To: "implementing", DecidedBy: DecidedByJev, Judgment: testJudgment, DefaultTo: "checking"}, true, true, true},
		{"fallback", Transition{To: "checking", DecidedBy: DecidedByFallback, DefaultTo: "checking"}, false, false, true},
		{"owner pinned off the default", Transition{To: "implementing", DecidedBy: DecidedByOwner, DefaultTo: "checking"}, false, true, true},
		{"gate decision off the default", Transition{To: "ratified", DecidedBy: DecidedByJevGate, Judgment: testJudgment, DefaultTo: "sketched"}, false, true, true},
		{"reviewer verdict", Transition{To: "approved", DecidedBy: DecidedByReviewer, DefaultTo: "approved"}, false, false, true},
	} {
		deviated, known := c.t.Deviated()
		if got := c.t.Departed(); got != c.departed || deviated != c.deviated || known != c.defaultSet {
			t.Errorf("%s: departed %v deviated %v known %v, want %v %v %v", c.name, got, deviated, known, c.departed, c.deviated, c.defaultSet)
		}
	}
}

func TestRoutedTransitionRoundTrips(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	route := Routing{DecidedBy: DecidedByJev, Judgment: testJudgment, DefaultTo: "reviewing"}
	state := transact(t, r, routed("t1", 0, "", "checking", Routing{DecidedBy: DecidedByCode, DefaultTo: "checking"}))
	transact(t, r, routed("t2", state.Version, "checking", "implementing", route))

	check := func(r *Repository) {
		t.Helper()
		got, err := Read[Transition](r, streamID)
		if err != nil || len(got) != 2 {
			t.Fatalf("transitions %+v: %v", got, err)
		}
		if got[0].Routing() != (Routing{DecidedBy: DecidedByCode, DefaultTo: "checking"}) || got[0].Departed() {
			t.Fatalf("first routing %+v", got[0].Routing())
		}
		if got[1].Routing() != route || !got[1].Departed() {
			t.Fatalf("second routing %+v", got[1].Routing())
		}
	}
	check(r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	check(r)

	if _, err := r.Transact(ctx, routed("t2", state.Version, "checking", "implementing", route)); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	other := route
	other.DefaultTo = "approved"
	if _, err := r.Transact(ctx, routed("t2", state.Version, "checking", "implementing", other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry with another routing: %v", err)
	}
	bad := route
	bad.Judgment = ""
	if _, err := r.Transact(ctx, routed("t3", 2, "implementing", "checking", bad)); err == nil {
		t.Fatal("a departure without a judgment was recorded")
	}
	if got, err := Read[Transition](r, streamID); err != nil || len(got) != 2 {
		t.Fatalf("transitions after a refusal: %d, %v", len(got), err)
	}
}

func TestTransitionWithoutRoutingReadsAsUnrouted(t *testing.T) {
	plain := Transition{Header: header("transition", "plain"), Subject: "feature", From: "handed", To: "sketched", Reason: "drafted"}
	data, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"decided_by", "judgment", "default_to"} {
		if strings.Contains(string(data), field) {
			t.Fatalf("a transition without routing encodes %s: %s", field, data)
		}
	}
	record, err := decodeRecord(data)
	if err != nil {
		t.Fatalf("reading a record without routing: %v", err)
	}
	got, ok := record.(Transition)
	if !ok || got.Routing() != (Routing{}) || got.Departed() || got.To != "sketched" {
		t.Fatalf("read %+v", record)
	}
	if deviated, known := got.Deviated(); deviated || known {
		t.Fatal("a transition without a default reads as deviated")
	}
	if err := validate(got); err != nil {
		t.Fatalf("validating: %v", err)
	}
}

func TestRoutedTransitionCanFollowAnUnroutedOne(t *testing.T) {
	r, _, _ := create(t)
	tx := transaction("t1", 0, "", "handed", 0)
	transact(t, r, tx)
	state := transact(t, r, routed("t2", 1, "handed", "sketched", Routing{DecidedBy: DecidedByCode, DefaultTo: "sketched"}))
	if state.Version != 2 {
		t.Fatalf("state %+v", state)
	}
	all, err := Read[Transition](r, streamID)
	if err != nil || len(all) != 2 || all[0].Routing() != (Routing{}) || all[1].DecidedBy != DecidedByCode {
		t.Fatalf("read %+v: %v", all, err)
	}
}

func TestRoutingIsRecordedOnEveryCommitPath(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	code := func(to string) Routing { return Routing{DecidedBy: DecidedByCode, DefaultTo: to} }
	jev := Routing{DecidedBy: DecidedByJev, Judgment: testJudgment, DefaultTo: "ratified"}

	if _, err := r.SetFeatureStateRouted(ctx, header("transition", "f1"), "handed", "handed in", code("handed")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MoveFeatureStateRouted(ctx, header("transition", "f2"), "handed", "sketched", "drafted", code("sketched")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MoveFeatureStateRouted(ctx, header("transition", "f3"), "sketched", "in-shed", "debating", jev, routedUnit("u1", "u1", 0, "", "planned", code("planned"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetFeatureStateUnlessRouted(ctx, header("transition", "f4"), "abandoned", "ended", Routing{DecidedBy: DecidedByOwner, DefaultTo: "in-shed"}, "delivered"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetFeatureStateRouted(ctx, header("transition", "f1"), "handed", "handed in", code("sketched")); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry with another routing: %v", err)
	}

	doc := Document{Header: header("document", "spec"), Path: "spec.md", Content: "# Feature\n"}
	tx := routedUnit("u2", "u1", 1, "planned", "ready", code("ready"))
	if _, err := r.RecordDocumentsWith(ctx, []Document{doc}, tx); err != nil {
		t.Fatal(err)
	}

	got, err := Read[Transition](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Transition{}
	for _, tr := range got {
		by[tr.ID] = tr
	}
	for id, want := range map[string]Routing{"f1": code("handed"), "f2": code("sketched"), "f3": jev, "u1": code("planned"), "f4": {DecidedBy: DecidedByOwner, DefaultTo: "in-shed"}, "u2": code("ready")} {
		if by[id].Routing() != want {
			t.Errorf("%s recorded %+v, want %+v", id, by[id].Routing(), want)
		}
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "f5"), "assembled", "assembled"); err != nil {
		t.Fatal(err)
	}
	got, _ = Read[Transition](r, streamID)
	for _, tr := range got {
		if tr.ID == "f5" && tr.Routing() != (Routing{}) {
			t.Fatalf("an unrouted setter recorded %+v", tr.Routing())
		}
	}
}
