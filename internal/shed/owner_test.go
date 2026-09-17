package shed_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
)

// ownerRecord is the owner's record of one round with that many objections.
func ownerRecord(round int, pin shed.Pin, objections int) shed.Record {
	r := shed.Record{Version: shed.Version, Round: round, Member: shed.OwnerMember, Revision: pin}
	for k := 1; k <= objections; k++ {
		r.Objections = append(r.Objections, shed.Objection{ID: shed.ObjectionID(round, shed.OwnerMember, k), Kind: shed.Owner, Argument: "This is not what I handed in."})
	}
	return r
}

func rulings(round int, pin shed.Pin, rs ...shed.Ruling) shed.Rulings {
	return shed.Rulings{Version: shed.Version, Round: round, Revision: pin, Rulings: rs}
}

// The owner's objections are a record of the same shape as a member's, with
// neither a part nor a citation, and they need the owner's kind.
func TestOwnerRecordRoundTripsAndRefusesInvalidContent(t *testing.T) {
	t.Parallel()
	r := ownerRecord(2, one, 2)
	data, err := shed.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shed.Parse(data)
	if err != nil || !reflect.DeepEqual(got, r) || !got.Owned() || got.Silent() {
		t.Fatalf("round trip %+v %v", got, err)
	}
	if id := got.Objections[1].ID; id != "owner-r2-2" {
		t.Fatalf("objection ID %q", id)
	}
	for name, broken := range map[string]func(*shed.Record){
		"a member's kind":   func(r *shed.Record) { r.Objections[0].Kind = shed.Fit },
		"no argument":       func(r *shed.Record) { r.Objections[0].Argument = " " },
		"a concession":      func(r *shed.Record) { r.Concessions = []shed.Concession{{Objection: "owner-r2-1", Reason: "no"}} },
		"a failure":         func(r *shed.Record) { r.Failure = "the owner's turn failed" },
		"a turn":            func(r *shed.Record) { r.Turn = "shed-2-owner-1" },
		"an out-of-band ID": func(r *shed.Record) { r.Objections[0].ID = "owner-r2-7" },
	} {
		broken := broken
		next := ownerRecord(2, one, 2)
		broken(&next)
		if _, err := shed.Encode(next); err == nil {
			t.Fatalf("%s was encoded", name)
		}
	}
	// No committee member may carry a reserved name.
	for _, name := range []string{shed.OwnerMember, "reply", "rulings", "more"} {
		taken := record(1, name, one, 1)
		taken.Objections[0].Kind = shed.Owner
		taken.Objections[0].Part, taken.Objections[0].Citations = "", nil
		if name != shed.OwnerMember {
			if _, err := shed.Encode(taken); err == nil {
				t.Fatalf("a member named %s was encoded", name)
			}
			continue
		}
		if _, err := shed.Encode(taken); err != nil {
			t.Fatalf("the owner's record: %v", err)
		}
	}
	// A member's objection cannot carry the owner's kind.
	member := record(1, alice, one, 1)
	member.Objections[0].Kind = shed.Owner
	if _, err := shed.Encode(member); err == nil {
		t.Fatal("a member raised the owner's kind")
	}
}

// The owner's objection blocks and no member's turn settles it.
func TestOwnerObjectionBlocksAndOnlyTheOwnerSettlesIt(t *testing.T) {
	t.Parallel()
	entries := shed.DissentRecord([]shed.Record{ownerRecord(1, one, 1), record(2, alice, two, 0)}, nil)
	if len(entries) != 1 || !entries[0].Blocking || entries[0].Member != shed.OwnerMember || entries[0].Kind != shed.Owner {
		t.Fatalf("dissent record %+v", entries)
	}
	if len(shed.Standing(entries)) != 1 {
		t.Fatalf("the owner's objection was settled: %+v", entries)
	}
	dismissed := shed.DissentRecord([]shed.Record{ownerRecord(1, one, 1)}, []shed.Rulings{rulings(2, two, shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Dismissed, Note: "I was wrong."})})
	if len(dismissed) != 1 || dismissed[0].Blocking || dismissed[0].Disposition != shed.Dismissed || dismissed[0].Note != "I was wrong." {
		t.Fatalf("dismissed %+v", dismissed)
	}
	if got := shed.Standing(dismissed); len(got) != 0 {
		t.Fatalf("a dismissed objection still stands: %+v", got)
	}
}

// A ruling overrides what the kind means: a sustained fit objection blocks, a
// dismissed charter veto does not, and the latest ruling wins.
func TestRulingOverridesWhatTheKindMeans(t *testing.T) {
	t.Parallel()
	fit := record(1, alice, one, 1)
	veto := record(1, bob, one, 1)
	veto.Objections[0].Kind = shed.Charter
	veto.Objections[0].Citations = []string{"charter#1"}
	records := []shed.Record{fit, veto}
	sustained := shed.ObjectionID(1, alice, 1)
	dismissed := shed.ObjectionID(1, bob, 1)
	entries := shed.DissentRecord(records, []shed.Rulings{
		rulings(1, one, shed.Ruling{Objection: sustained, Disposition: shed.Dismissed}),
		rulings(2, two, shed.Ruling{Objection: sustained, Disposition: shed.Sustained}, shed.Ruling{Objection: dismissed, Disposition: shed.Dismissed}),
	})
	want := map[string]struct {
		blocking    bool
		disposition shed.Disposition
	}{sustained: {true, shed.Sustained}, dismissed: {false, shed.Dismissed}}
	if len(entries) != 2 {
		t.Fatalf("dissent record %+v", entries)
	}
	for _, e := range entries {
		if e.Blocking != want[e.ID].blocking || e.Disposition != want[e.ID].disposition {
			t.Fatalf("entry %+v, want %+v", e, want[e.ID])
		}
	}
	if got := shed.Standing(entries); len(got) != 1 || got[0].ID != sustained {
		t.Fatalf("standing %+v", got)
	}
	// An unruled objection keeps what its kind means, and a ruling on an
	// objection that no longer stands changes nothing.
	plain := shed.DissentRecord(records, []shed.Rulings{rulings(1, one, shed.Ruling{Objection: "agent_committee_9-r1-1", Disposition: shed.Sustained})})
	if len(plain) != 2 || plain[0].Blocking || plain[0].Disposition != "" || !plain[1].Blocking {
		t.Fatalf("unruled dissent %+v", plain)
	}
	data, err := json.Marshal(entries[0])
	if err != nil || !strings.Contains(string(data), `"disposition":"sustained"`) {
		t.Fatalf("encoded entry %s %v", data, err)
	}
}

func TestRulingsRoundTripAndRefuseInvalidContent(t *testing.T) {
	t.Parallel()
	r := rulings(3, two, shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Sustained, Note: "Still open."}, shed.Ruling{Objection: shed.ObjectionID(1, alice, 1), Disposition: shed.Dismissed})
	data, err := shed.EncodeRulings(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shed.ParseRulings(data)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip %+v %v", got, err)
	}
	for name, broken := range map[string]shed.Rulings{
		"no version":   {Round: 1, Revision: one, Rulings: r.Rulings},
		"no round":     {Version: shed.Version, Revision: one, Rulings: r.Rulings},
		"no revision":  {Version: shed.Version, Round: 1, Rulings: r.Rulings},
		"no objection": rulings(1, one, shed.Ruling{Disposition: shed.Sustained}),
		"a verb":       rulings(1, one, shed.Ruling{Objection: "owner-r1-1", Disposition: "sustain"}),
		"one objection twice": rulings(1, one, shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Sustained},
			shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Dismissed}),
	} {
		if _, err := shed.EncodeRulings(broken); err == nil {
			t.Fatalf("%s was encoded", name)
		}
	}
	for name, content := range map[string]string{
		"an unknown field": `{"version":1,"round":1,"revision":{"spec":1,"plan":1},"rulings":[],"extra":1}`,
		"two objects":      `{"version":1,"round":1,"revision":{"spec":1,"plan":1},"rulings":[]} {}`,
	} {
		if _, err := shed.ParseRulings([]byte(content)); err == nil {
			t.Fatalf("%s was parsed", name)
		}
	}
	if empty, err := shed.ParseRulings([]byte(`{"version":1,"round":1,"revision":{"spec":1,"plan":1},"rulings":[]}`)); err != nil || empty.Rulings != nil {
		t.Fatalf("empty rulings %+v %v", empty, err)
	}
}

func TestRequestForFurtherRoundsRoundTripsAndBoundsTheLimit(t *testing.T) {
	t.Parallel()
	m := shed.More{Version: shed.Version, Round: 3, Rounds: 2}
	data, err := shed.EncodeMore(m)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := shed.ParseMore(data); err != nil || got != m {
		t.Fatalf("round trip %+v %v", got, err)
	}
	for name, broken := range map[string]shed.More{
		"no version": {Round: 1, Rounds: 1},
		"no round":   {Version: shed.Version, Rounds: 1},
		"no rounds":  {Version: shed.Version, Round: 1},
		"negative":   {Version: shed.Version, Round: 1, Rounds: -1},
	} {
		if _, err := shed.EncodeMore(broken); err == nil {
			t.Fatalf("%s was encoded", name)
		}
	}
	if _, err := shed.ParseMore([]byte(`{"version":1,"round":1,"rounds":1,"extra":1}`)); err == nil {
		t.Fatal("an unknown field was parsed")
	}
	// A request replaces the cap: what the owner asked for is what runs,
	// whether that is beyond the configured cap or short of it.
	for name, tc := range map[string]struct {
		configured int
		requests   []shed.More
		want       int
	}{
		"no request":         {3, nil, 3},
		"beyond the cap":     {3, []shed.More{{Round: 3, Rounds: 2}}, 5},
		"short of the cap":   {5, []shed.More{{Round: 1, Rounds: 1}}, 2},
		"the last one wins":  {3, []shed.More{{Round: 3, Rounds: 2}, {Round: 5, Rounds: 1}}, 6},
		"the highest counts": {3, []shed.More{{Round: 5, Rounds: 1}, {Round: 3, Rounds: 2}}, 6},
	} {
		if got := shed.Limit(tc.configured, tc.requests); got != tc.want {
			t.Fatalf("%s: limit %d, want %d", name, got, tc.want)
		}
	}
}

// The owner's files of a round are read back by round, and the members'
// records never hold one of them.
func TestOwnerFilesAreReadBackByRoundAndAreNotMemberRecords(t *testing.T) {
	t.Parallel()
	f := setup(t)
	record := ownerRecord(1, one, 1)
	content, err := shed.Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	ruled, err := shed.EncodeRulings(rulings(1, one, shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Dismissed}))
	if err != nil {
		t.Fatal(err)
	}
	asked, err := shed.EncodeMore(shed.More{Version: shed.Version, Round: 1, Rounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	member, err := shed.Encode(shed.Record{Version: shed.Version, Round: 1, Member: alice, Revision: one})
	if err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream,
		document(stream, shed.DocumentID(1, shed.OwnerMember), shed.Path(1, shed.OwnerMember), string(content), 1),
		document(stream, shed.RulingsDocumentID(1), shed.RulingsPath(1), string(ruled), 1),
		document(stream, shed.MoreDocumentID(1), shed.MorePath(1), string(asked), 1),
		document(stream, shed.DocumentID(1, alice), shed.Path(1, alice), string(member), 1))
	records, err := shed.Records(f.repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Member != alice || !records[1].Owned() {
		t.Fatalf("records %+v", records)
	}
	all, err := shed.AllRulings(f.repo, stream)
	if err != nil || len(all) != 1 || all[0].Round != 1 || len(all[0].Rulings) != 1 {
		t.Fatalf("rulings %+v %v", all, err)
	}
	requests, err := shed.Requests(f.repo, stream)
	if err != nil || len(requests) != 1 || requests[0] != (shed.More{Version: shed.Version, Round: 1, Rounds: 2}) {
		t.Fatalf("requests %+v %v", requests, err)
	}
	// A file whose content disagrees with its path is an error, never a
	// silently misplaced record.
	elsewhere, err := shed.EncodeRulings(rulings(2, one, shed.Ruling{Objection: "owner-r1-1", Disposition: shed.Sustained}))
	if err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream, document(stream, shed.RulingsDocumentID(1), shed.RulingsPath(1), string(elsewhere), 2))
	if _, err := shed.AllRulings(f.repo, stream); err == nil || !strings.Contains(err.Error(), shed.RulingsPath(1)) {
		t.Fatalf("misplaced rulings: %v", err)
	}
}
