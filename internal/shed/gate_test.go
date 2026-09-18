package shed_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
)

// entries is the dissent record of one member's objections, each with the
// given kind and disposition.
func entry(id string, kind shed.Kind, disposition shed.Disposition) shed.Entry {
	e := shed.Entry{Dissent: shed.Dissent{Objection: shed.Objection{ID: id, Kind: kind, Part: "spec#1", Argument: "It does not hold."}, Member: alice, Round: 1, Revision: one}}
	e.Blocking = e.Dissent.Blocking()
	if disposition != "" {
		e.Disposition, e.Blocking = disposition, disposition == shed.Sustained
	}
	return e
}

// An overruled objection is the owner's recorded disposition: it stays in the
// dissent record, blocks no longer and leaves the debate.
func TestOverruledObjectionIsSettledAndBlocksNoLonger(t *testing.T) {
	t.Parallel()
	records := []shed.Record{record(1, alice, one, 2)}
	veto := shed.ObjectionID(1, alice, 1)
	advice := shed.ObjectionID(1, alice, 2)
	records[0].Objections[0].Kind = shed.Charter
	overruled := rulings(1, one, shed.Ruling{Objection: veto, Disposition: shed.Overruled, Note: "I accept the risk."})
	entries := shed.DissentRecord(records, []shed.Rulings{overruled})
	if len(entries) != 2 {
		t.Fatalf("dissent record %+v", entries)
	}
	if e := entries[0]; e.ID != veto || e.Blocking || e.Disposition != shed.Overruled || e.Note != "I accept the risk." {
		t.Fatalf("the overruled veto %+v", e)
	}
	if e := entries[1]; e.ID != advice || e.Blocking || e.Disposition != "" {
		t.Fatalf("the advisory objection %+v", e)
	}
	// Neither the architect nor a further round answers a settled objection,
	// and nothing blocks ratification.
	if standing := shed.Standing(entries); len(standing) != 1 || standing[0].ID != advice {
		t.Fatalf("standing %+v", standing)
	}
	if blocked := shed.Blocked(entries); len(blocked) != 0 {
		t.Fatalf("blocked %+v", blocked)
	}
	// A sustained objection blocks, and is neither settled nor ratifiable.
	sustained := shed.DissentRecord(records, []shed.Rulings{rulings(1, one, shed.Ruling{Objection: veto, Disposition: shed.Sustained})})
	if blocked := shed.Blocked(sustained); len(blocked) != 1 || blocked[0].ID != veto {
		t.Fatalf("a sustained veto %+v", blocked)
	}
	if standing := shed.Standing(sustained); len(standing) != 2 {
		t.Fatalf("a sustained veto left the debate: %+v", standing)
	}
	if !shed.Overruled.Settled() || !shed.Dismissed.Settled() || shed.Sustained.Settled() {
		t.Fatal("what a disposition settles")
	}
	// An overrule is a recorded disposition like any other.
	data, err := shed.EncodeRulings(overruled)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := shed.ParseRulings(data); err != nil || !reflect.DeepEqual(got, overruled) {
		t.Fatalf("round trip %+v %v", got, err)
	}
}

// The packet puts what blocks ratification first and recommends what the
// dissent that stands calls for.
func TestPacketPresentsWhatBlocksFirstAndRecommends(t *testing.T) {
	t.Parallel()
	advice := entry("a-r1-1", shed.Fit, "")
	veto := entry("a-r1-2", shed.Charter, "")
	size := entry("a-r1-3", shed.Size, shed.Sustained)
	settled := entry("a-r1-4", shed.Proof, shed.Overruled)
	packet := shed.Present(2, one, false, "debate stopped after round 2", []shed.Entry{advice, veto, size, settled})
	if ids := packetIDs(packet); !reflect.DeepEqual(ids, []string{"a-r1-2", "a-r1-3", "a-r1-1", "a-r1-4"}) {
		t.Fatalf("the packet lists %v", ids)
	}
	if packet.Round != 2 || packet.Revision != one || packet.Skipped || packet.Conclusion != "debate stopped after round 2" {
		t.Fatalf("packet %+v", packet)
	}
	want := "do not ratify yet: ratification is blocked by 2 objections (a-r1-2, a-r1-3); overrule or sustain each one, or ask for a redraft"
	if packet.Recommendation != want {
		t.Fatalf("recommendation %q, want %q", packet.Recommendation, want)
	}
	if got := shed.Recommend([]shed.Entry{advice, settled}); got != "ratify: nothing blocks, and 2 objections stand as advice on the record" {
		t.Fatalf("recommendation with nothing blocking: %q", got)
	}
	if got := shed.Recommend([]shed.Entry{advice}); got != "ratify: nothing blocks, and 1 objection stands as advice on the record" {
		t.Fatalf("recommendation %q for one objection", got)
	}
	if got := shed.Recommend(nil); got != "ratify: no objection stands" {
		t.Fatalf("recommendation with no dissent: %q", got)
	}
	if got := shed.Recommend([]shed.Entry{veto}); !strings.Contains(got, "blocked by 1 objection (a-r1-2)") {
		t.Fatalf("recommendation with one blocking: %q", got)
	}
	// The order the packet presents is the order it is recorded in, and the
	// entries it holds are the dissent record's own.
	data, err := shed.EncodePacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shed.ParsePacket(data)
	if err != nil || !reflect.DeepEqual(got, packet) {
		t.Fatalf("round trip %+v %v", got, err)
	}
	if round, ok := shed.PacketRound(shed.PacketPath(2)); !ok || round != 2 {
		t.Fatalf("the path of the packet of round 2 reads back as %d %v", round, ok)
	}
	for _, path := range []string{shed.RulingsPath(2), shed.Path(2, alice), shed.RatificationPath(2), "shed/round-0/packet.json", "shed/packet.json", "packet.json"} {
		if _, ok := shed.PacketRound(path); ok {
			t.Fatalf("%s reads back as a packet", path)
		}
	}
	if round, ok := shed.RatificationRound(shed.RatificationPath(3)); !ok || round != 3 {
		t.Fatalf("the path of the ratification of round 3 reads back as %d %v", round, ok)
	}
	for _, path := range []string{shed.PacketPath(3), shed.Path(3, alice), "shed/round-0/ratification.json", "ratification.json"} {
		if _, ok := shed.RatificationRound(path); ok {
			t.Fatalf("%s reads back as a ratification", path)
		}
	}
}

func packetIDs(p shed.Packet) []string {
	out := []string{}
	for _, e := range p.Dissent {
		out = append(out, e.ID)
	}
	return out
}

// A packet, a ratification and a request for a redraft each require what they
// are about, and are read back by the round they belong to.
func TestGateFilesRequireTheirContentAndRoundTrip(t *testing.T) {
	t.Parallel()
	packet := shed.Present(1, one, true, "the owner skipped debate", nil)
	for name, broken := range map[string]func(*shed.Packet){
		"no version":        func(p *shed.Packet) { p.Version = 0 },
		"no round":          func(p *shed.Packet) { p.Round = 0 },
		"no revision":       func(p *shed.Packet) { p.Revision = shed.Pin{Spec: 1} },
		"no conclusion":     func(p *shed.Packet) { p.Conclusion = " " },
		"no recommendation": func(p *shed.Packet) { p.Recommendation = "" },
	} {
		next := packet
		broken(&next)
		if _, err := shed.EncodePacket(next); err == nil {
			t.Fatalf("a packet with %s was encoded", name)
		}
	}
	if _, err := shed.ParsePacket([]byte(`{"version":1,"round":1,"revision":{"spec":1,"plan":1},"conclusion":"c","recommendation":"r","extra":1}`)); err == nil {
		t.Fatal("an unknown field was parsed")
	}

	entries := []shed.Entry{entry("a-r1-1", shed.Fit, ""), entry("a-r1-2", shed.Charter, shed.Overruled)}
	ratification := shed.Ratify(3, one, entries)
	if len(ratification.Dispositions) != 1 || ratification.Dispositions[0] != (shed.Ruling{Objection: "a-r1-2", Disposition: shed.Overruled}) {
		t.Fatalf("ratification %+v", ratification)
	}
	data, err := shed.EncodeRatification(ratification)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := shed.ParseRatification(data); err != nil || !reflect.DeepEqual(got, ratification) {
		t.Fatalf("round trip %+v %v", got, err)
	}
	// A blocking objection is not ratified, whatever the caller passes.
	if _, err := shed.EncodeRatification(shed.Ratify(1, one, []shed.Entry{entry("a-r1-3", shed.Charter, "")})); err == nil {
		t.Fatal("a blocking objection was ratified")
	}
	if _, err := shed.EncodeRatification(shed.Ratification{Version: shed.Version, Round: 1}); err == nil {
		t.Fatal("a ratification without revisions was encoded")
	}

	redraft := shed.Redraft{Version: shed.Version, Round: 2, Revision: one, Note: "Split the resume unit."}
	data, err = shed.EncodeRedraft(redraft)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := shed.ParseRedraft(data); err != nil || got != redraft {
		t.Fatalf("round trip %+v %v", got, err)
	}
	for name, broken := range map[string]func(*shed.Redraft){
		"no note":     func(r *shed.Redraft) { r.Note = " " },
		"no round":    func(r *shed.Redraft) { r.Round = 0 },
		"no revision": func(r *shed.Redraft) { r.Revision = shed.Pin{} },
		"no version":  func(r *shed.Redraft) { r.Version = 2 },
	} {
		next := redraft
		broken(&next)
		if _, err := shed.EncodeRedraft(next); err == nil {
			t.Fatalf("a request with %s was encoded", name)
		}
	}
}

// The files of the gate are read back by round, and none of them is a
// member's record: a committee member cannot carry their names.
func TestGateFilesAreReadBackByRoundAndAreNotMemberRecords(t *testing.T) {
	t.Parallel()
	f := setup(t)
	packet, err := shed.EncodePacket(shed.Present(1, one, false, "debate concluded by consensus after round 1", nil))
	if err != nil {
		t.Fatal(err)
	}
	ratification, err := shed.EncodeRatification(shed.Ratify(1, one, nil))
	if err != nil {
		t.Fatal(err)
	}
	asked, err := shed.EncodeRedraft(shed.Redraft{Version: shed.Version, Round: 1, Revision: one, Note: "Split the resume unit."})
	if err != nil {
		t.Fatal(err)
	}
	redrafted, err := shed.EncodeReply(shed.Reply{Version: shed.Version, Round: 1, Revision: one, Redraft: &two})
	if err != nil {
		t.Fatal(err)
	}
	member, err := shed.Encode(record(1, alice, one, 1))
	if err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream,
		document(stream, shed.PacketDocumentID(1), shed.PacketPath(1), string(packet), 1),
		document(stream, shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratification), 1),
		document(stream, shed.RedraftDocumentID(1), shed.RedraftPath(1), string(asked), 1),
		document(stream, shed.RedraftedDocumentID(1), shed.RedraftedPath(1), string(redrafted), 1),
		document(stream, shed.DocumentID(1, alice), shed.Path(1, alice), string(member), 1))
	records, err := shed.Records(f.repo, stream)
	if err != nil || len(records) != 1 || records[0].Member != alice {
		t.Fatalf("records %+v %v", records, err)
	}
	presented, doc, found, err := shed.LatestPacket(f.repo, stream)
	if err != nil || !found || presented.Round != 1 || doc.Path != shed.PacketPath(1) || doc.Revision != 1 {
		t.Fatalf("packet %+v %+v %v %v", presented, doc, found, err)
	}
	ratified, err := shed.Ratifications(f.repo, stream)
	if err != nil || len(ratified) != 1 || ratified[0].Revision != one {
		t.Fatalf("ratifications %+v %v", ratified, err)
	}
	requests, err := shed.Redrafts(f.repo, stream)
	if err != nil || len(requests) != 1 || requests[0].Note != "Split the resume unit." {
		t.Fatalf("requests %+v %v", requests, err)
	}
	// The architect's report of a redraft is not one of its replies.
	reports, err := shed.Redrafted(f.repo, stream)
	if err != nil || len(reports) != 1 || reports[0].Redraft == nil || *reports[0].Redraft != two {
		t.Fatalf("redrafts %+v %v", reports, err)
	}
	if replies, err := shed.Replies(f.repo, stream); err != nil || len(replies) != 0 {
		t.Fatalf("replies %+v %v", replies, err)
	}
	for _, name := range []string{"packet", "ratification", "redraft", "redrafted"} {
		if _, err := shed.Encode(record(1, name, one, 1)); err == nil {
			t.Fatalf("a member named %s was encoded", name)
		}
	}
	// A file whose content disagrees with its path is an error.
	elsewhere, err := shed.EncodePacket(shed.Present(2, one, false, "debate concluded", nil))
	if err != nil {
		t.Fatal(err)
	}
	f.documents(t, stream, document(stream, shed.PacketDocumentID(1), shed.PacketPath(1), string(elsewhere), 2))
	if _, _, _, err := shed.LatestPacket(f.repo, stream); err == nil || !strings.Contains(err.Error(), shed.PacketPath(1)) {
		t.Fatalf("a misplaced packet: %v", err)
	}
}
