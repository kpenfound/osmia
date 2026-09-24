package amendment

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func unit(id string, footprint string, criteria ...string) plan.Unit {
	u := plan.Unit{ID: id, Footprint: []string{footprint}, DependsOn: []string{}}
	for _, c := range criteria {
		u.Addresses = append(u.Addresses, plan.Address{Criterion: c, Proof: plan.Proof{Kind: "new-test", Name: "Test" + id}})
	}
	return u
}

// A unit that addresses a changed criterion in either plan is reworked, even
// when the affected set omits it; any other affected unit in both plans is
// notified; units only in the new plan are added and units only in the old
// plan removed. Unaffected units are in no class.
func TestClassifySortsAffectedUnits(t *testing.T) {
	t.Parallel()
	before := plan.Plan{Units: []plan.Unit{unit("resume", "a", "spec#1"), unit("dedupe", "a", "spec#2"), unit("moved", "a", "spec#1"), unit("audit", "b", "spec#3"), unit("gone", "c", "spec#3"), unit("quiet", "d", "spec#3")}}
	after := plan.Plan{Units: []plan.Unit{unit("resume", "a", "spec#1"), unit("dedupe", "b", "spec#2"), unit("moved", "a", "spec#2"), unit("audit", "b", "spec#3"), unit("added", "c", "spec#1"), unit("quiet", "d", "spec#3")}}
	rework, notify, added, removed := Classify([]string{"spec#1"}, []string{"dedupe", "moved", "added", "gone"}, before, after)
	for name, got := range map[string][]string{"rework": rework, "notify": notify, "added": added, "removed": removed} {
		want := map[string][]string{"rework": {"moved", "resume"}, "notify": {"dedupe"}, "added": {"added"}, "removed": {"gone"}}[name]
		if !slices.Equal(got, want) {
			t.Errorf("%s %v, want %v", name, got, want)
		}
	}
}

func pin(seal, revision, spec, graph int) Pin {
	return Pin{Seal: seal, SealRevision: revision, Spec: spec, Plan: graph}
}

// A report holds across a chain of applied amendments that took every seal
// between its own and the current one without reworking the unit.
func TestCarriesReport(t *testing.T) {
	t.Parallel()
	apps := []Application{
		{Amendment: "1", From: pin(1, 1, 1, 1), To: pin(1, 2, 1, 2), Notify: []string{"dedupe"}},
		{Amendment: "2", From: pin(1, 2, 1, 2), To: pin(2, 3, 2, 2), Rework: []string{"resume"}, Notify: []string{"audit"}},
	}
	for _, tc := range []struct {
		name              string
		unit              string
		reported, current int
		want              bool
	}{
		{"the same seal", "resume", 2, 2, true},
		{"a notified unit", "audit", 1, 2, true},
		{"an unaffected unit", "dedupe", 1, 2, true},
		{"a reworked unit", "resume", 1, 2, false},
		{"a seal no amendment took", "dedupe", 1, 3, false},
		{"a later seal", "dedupe", 3, 2, false},
	} {
		if got := CarriesReport(apps, tc.unit, tc.reported, tc.current); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A review holds when the applied amendments lead from its seal, spec and
// plan to the current ones and none reworks, notifies or removes the unit.
func TestCarriesReview(t *testing.T) {
	t.Parallel()
	apps := []Application{
		{Amendment: "2", From: pin(1, 2, 1, 2), To: pin(2, 3, 2, 2), Rework: []string{"resume"}, Removed: []string{"gone"}},
		{Amendment: "1", From: pin(1, 1, 1, 1), To: pin(1, 2, 1, 2), Notify: []string{"dedupe"}},
	}
	for _, tc := range []struct {
		name              string
		unit              string
		reviewed, current Pin
		want              bool
	}{
		{"the current revisions", "resume", pin(2, 0, 2, 2), pin(2, 0, 2, 2), true},
		{"an unaffected unit across both", "audit", pin(1, 0, 1, 1), pin(2, 0, 2, 2), true},
		{"a notified unit before its amendment", "dedupe", pin(1, 0, 1, 1), pin(2, 0, 2, 2), false},
		{"a notified unit reviewed after its amendment", "dedupe", pin(1, 0, 1, 2), pin(2, 0, 2, 2), true},
		{"a reworked unit", "resume", pin(1, 0, 1, 2), pin(2, 0, 2, 2), false},
		{"a removed unit", "gone", pin(1, 0, 1, 2), pin(2, 0, 2, 2), false},
		{"revisions no amendment left", "audit", pin(1, 0, 3, 1), pin(2, 0, 2, 2), false},
		{"an owner edit after the reseal", "audit", pin(1, 0, 1, 1), pin(2, 0, 3, 2), false},
	} {
		if got := CarriesReview(apps, tc.unit, tc.reviewed, tc.current); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.name, got, tc.want)
		}
	}
}

// Read keeps the latest revision of each application and orders them by the
// seal.json revision they recorded; other amendment documents are ignored.
func TestFromDocumentsReadsLatestApplications(t *testing.T) {
	t.Parallel()
	doc := func(id, path string, revision int, a Application) trace.Document {
		data, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		return trace.Document{Header: trace.Header{ID: id, Revision: revision}, Path: path, Content: string(data)}
	}
	docs := []trace.Document{
		doc(DocumentID("2"), Path("2"), 1, Application{Amendment: "2", To: pin(2, 3, 2, 2)}),
		doc(DocumentID("1"), Path("1"), 1, Application{Amendment: "1", To: pin(1, 2, 1, 2)}),
		doc(DocumentID("1"), Path("1"), 2, Application{Amendment: "1", To: pin(1, 2, 1, 2), Applied: &Applied{Held: []string{"resume"}}}),
		doc("amendment_3_affected", "amendments/3/affected.json", 1, Application{Amendment: "3"}),
		doc(DocumentID("4"), "amendments/4/round-1/application.json", 1, Application{Amendment: "4"}),
	}
	got, err := FromDocuments(docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Amendment != "1" || got[0].Applied == nil || got[1].Amendment != "2" {
		t.Fatalf("applications %+v", got)
	}
}
