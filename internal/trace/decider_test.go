package trace

import (
	"context"
	"reflect"
	"testing"
)

func TestValidDecider(t *testing.T) {
	for _, c := range []struct {
		by, judgment string
		want         bool
	}{
		{"", "", true}, {DecidedByOwner, "", true}, {DecidedByChief, "", true},
		{DecidedByJev, "judgment_ab12", true},
		{DecidedByJev, "", false}, {DecidedByOwner, "judgment_ab12", false}, {"", "judgment_ab12", false},
		{"someone", "", false}, {DecidedByJev, "bad key!", false},
	} {
		if got := ValidDecider(c.by, c.judgment); got != c.want {
			t.Errorf("ValidDecider(%q, %q) = %v, want %v", c.by, c.judgment, got, c.want)
		}
	}
	if Decider("", DecidedByOwner) != "owner" || Decider(DecidedByJev, DecidedByOwner) != "jev" {
		t.Fatal("Decider does not default to the record's own decider")
	}
}

func TestRulingDecidedByJevRoundTripsWithItsJudgment(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	question := Question{Header: header("question", "q1"), AskedBy: Actor{Kind: "agent", ID: "mason1"}, Question: "Which store?"}
	if err := r.Append(ctx, question); err != nil {
		t.Fatal(err)
	}
	ruling := func(id, by, judgment string) Ruling {
		h := header("ruling", id)
		if by == DecidedByJev {
			h.Actor = JevActor
		}
		return Ruling{Header: h, QuestionID: "q1", QuestionRevision: 1, Decision: DecisionRuling, OwnerResponse: "Files.", DecidedBy: by, Judgment: judgment}
	}
	for name, bad := range map[string]Ruling{
		"Jev without a judgment":        ruling("r1", DecidedByJev, ""),
		"a judgment without Jev":        ruling("r1", "", "judgment_ab12"),
		"an unknown decider":            ruling("r1", "someone", ""),
		"a judgment of another decider": ruling("r1", DecidedByOwner, "judgment_ab12"),
		"a judgment that is not a key":  ruling("r1", DecidedByJev, "not a key"),
	} {
		if err := r.Append(ctx, bad); err == nil {
			t.Fatalf("a ruling with %s was recorded", name)
		}
	}
	jev := ruling("r1", DecidedByJev, "judgment_ab12")
	if err := r.Append(ctx, jev); err != nil {
		t.Fatal(err)
	}
	owner := ruling("r2", "", "")
	if err := r.Append(ctx, owner); err != nil {
		t.Fatal(err)
	}
	check := func(r *Repository) {
		t.Helper()
		rulings, err := Read[Ruling](r, streamID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Ruling{}
		for _, ruling := range rulings {
			got[ruling.ID] = ruling
		}
		if !reflect.DeepEqual(got["r1"], jev) || got["r1"].Actor != JevActor || got["r1"].DecidedBy != DecidedByJev || got["r1"].Judgment != "judgment_ab12" {
			t.Fatalf("Jev's ruling = %+v", got["r1"])
		}
		if got["r2"].DecidedBy != "" || got["r2"].Judgment != "" || got["r2"].Actor == JevActor {
			t.Fatalf("the owner's ruling = %+v", got["r2"])
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
