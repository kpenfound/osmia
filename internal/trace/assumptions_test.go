package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestSharedAssumptionsSelectMembersAndRetainLocalHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, r1, r2, p1, p2 := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	for _, member := range []struct {
		repo   *Repository
		stream config.WorkstreamID
	}{{r1, crossMember1}, {r2, crossMember2}} {
		if err := member.repo.RecordCrossRepoWorkgroup(ctx, member.stream, g.ID, at, owner, "join"); err != nil {
			t.Fatal(err)
		}
	}
	scope := claimCrossRepoTurn(t, wg, wg.Project(), host, ChiefOfStaff, ChiefOfStaff, "", "chief-assumptions")
	shared, err := wg.LogAssumption(ctx, ChiefOfStaff, scope, "Shared retry policy.", at)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := wg.LogAssumption(ctx, ChiefOfStaff, scope, "Client backoff.", at, crossMember2)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := wg.LogAssumption(ctx, ChiefOfStaff, scope, "Client backoff.", at, crossMember2)
	if err != nil || !equalAssumption(retry, selected) {
		t.Fatalf("replay=%+v error=%v, want %+v", retry, err, selected)
	}
	if selected.ID == shared.ID {
		t.Fatal("second judgment in a turn was lost")
	}
	// A local log predating shared-chief adoption still belongs only to its
	// member. Legacy entries have no affected-member field.
	legacy := Assumption{ID: "1", Text: "Member-local convention."}
	data, err := json.Marshal(assumptionsFile{Version: 1, Assumptions: []Assumption{legacy}})
	if err != nil {
		t.Fatal(err)
	}
	err = r1.RecordDocuments(ctx, []Document{{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: AssumptionsDocument, Revision: 1, Project: p1.ID, Workstream: crossMember1, At: at, Actor: owner, Cause: "local-judgment"}, Path: AssumptionsPath, Content: string(data)}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(a, b *Repository) {
		t.Helper()
		for _, test := range []struct {
			repo   *Repository
			stream config.WorkstreamID
			texts  []string
		}{
			{a, crossMember1, []string{legacy.Text, shared.Text}},
			{b, crossMember2, []string{shared.Text, selected.Text}},
		} {
			got, err := Assumptions(test.repo, test.stream)
			if err != nil {
				t.Fatal(err)
			}
			texts := []string{}
			for _, a := range got {
				texts = append(texts, a.Text)
			}
			if !slices.Equal(texts, test.texts) {
				t.Fatalf("member %s assumptions=%v, want %v", test.stream, texts, test.texts)
			}
		}
	}
	check(r1, r2)
	// Explicit outsiders and duplicate targets cannot change the log.
	for _, targets := range [][]config.WorkstreamID{
		{"w_00000000000000000000000000000099"}, {crossMember1, crossMember1},
	} {
		_, err := wg.LogAssumption(ctx, ChiefOfStaff, scope, "Invalid judgment.", at, targets...)
		var refusal *QuestionRefused
		if !errors.As(err, &refusal) {
			t.Fatalf("targets %v: want refusal, got %v", targets, err)
		}
	}
	check(r1, r2)
	worker := claimCrossRepoTurn(t, r1, p1.ID, crossMember1, "worker", "mason", "", "worker-assumption")
	if _, err := r1.LogAssumption(ctx, "worker", worker, "Wrong member.", at, crossMember2); err == nil {
		t.Fatal("worker could select another member")
	}
	wg, r1, r2 = reopenCrossRepoWorkgroup(t, root, p1, p2, wg, r1, r2, g.ID)
	check(r1, r2)
	// A cold authority open needs neither a live chief turn nor another
	// member's repository bound to the authority.
	if err := wg.Close(); err != nil {
		t.Fatal(err)
	}
	check(r1, r2)
	unrelated := config.WorkstreamID("w_00000000000000000000000000000099")
	if err := r1.CreateWorkstream(ctx, unrelated, at, owner); err != nil {
		t.Fatal(err)
	}
	got, err := Assumptions(r1, unrelated)
	if err != nil || len(got) != 0 {
		t.Fatalf("unrelated member inherited assumptions: %+v %v", got, err)
	}
	directory, err := root.WorkgroupTrace(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := Assumptions(r2, crossMember2); err == nil {
		t.Fatal("missing shared authority silently omitted assumptions")
	}
}

func equalAssumption(a, b Assumption) bool {
	return a.ID == b.ID && a.Text == b.Text && a.Thread == b.Thread && a.Turn == b.Turn && a.Actor == b.Actor && a.At.Equal(b.At) && slices.Equal(a.Workstreams, b.Workstreams)
}

func TestSameRepoChiefAssumptionsReachOnlyAffectedMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, r, _, p, _ := crossRepoFixture(t)
	member := config.WorkstreamID("w_00000000000000000000000000000023")
	if err := r.CreateWorkstream(ctx, member, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{crossMember1, member}, at, owner, "group"); err != nil {
		t.Fatal(err)
	}
	scope := claimCrossRepoTurn(t, r, p.ID, crossMember1, ChiefOfStaff, ChiefOfStaff, "", "shared-assumption")
	if _, err := r.LogAssumption(ctx, ChiefOfStaff, scope, "Shared.", at); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LogAssumption(ctx, ChiefOfStaff, scope, "Selected.", at, member); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		stream config.WorkstreamID
		count  int
	}{{crossMember1, 1}, {member, 2}} {
		got, err := Assumptions(r, test.stream)
		if err != nil || len(got) != test.count {
			t.Fatalf("member %s assumptions=%+v %v", test.stream, got, err)
		}
	}
}
