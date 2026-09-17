package shed_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/shed"
)

// Every kind but fit blocks, and the record carries who objected to what.
func TestDissentRecordMarksWhatBlocks(t *testing.T) {
	r := shed.Record{Version: shed.Version, Round: 1, Member: alice, Revision: one}
	for k, kind := range shed.Kinds {
		o := objection(1, alice, k+1)
		o.Kind, o.Part = kind, "plan#resume"
		r.Objections = append(r.Objections, o)
	}
	conceded := record(2, alice, one, 0, shed.ObjectionID(1, alice, 3))
	got := shed.DissentRecord([]shed.Record{r, conceded}, nil)
	want := map[shed.Kind]bool{shed.Charter: true, shed.Fit: false, shed.Proof: true}
	if len(got) != len(want) {
		t.Fatalf("dissent record %+v", got)
	}
	for _, e := range got {
		blocking, open := want[e.Kind]
		if !open || e.Blocking != blocking || e.Blocking != e.Dissent.Blocking() || e.Member != alice || e.Part != "plan#resume" || e.Round != 1 || e.Revision != one {
			t.Fatalf("entry %+v", e)
		}
	}
	data, err := json.Marshal(got[0])
	if err != nil || !strings.Contains(string(data), `"blocking":true`) || !strings.Contains(string(data), `"kind":"charter"`) || !strings.Contains(string(data), `"member":"`+alice+`"`) || !strings.Contains(string(data), `"part":"plan#resume"`) {
		t.Fatalf("encoded entry %s %v", data, err)
	}
	if got := shed.DissentRecord(nil, nil); len(got) != 0 {
		t.Fatalf("dissent of no record: %+v", got)
	}
}

func TestReplyRoundTripsAndRefusesInvalidContent(t *testing.T) {
	r := shed.Reply{Version: shed.Version, Round: 2, Revision: one, Turn: "reply-2-1", Answers: []shed.Answer{{Objection: shed.ObjectionID(1, alice, 1), Answer: "Split as charter#<1> asks."}}, Redraft: &two}
	data, err := shed.EncodeReply(r)
	if err != nil || !strings.Contains(string(data), "charter#<1> asks") || !strings.Contains(string(data), `"redraft": {`) || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("encoded:\n%s %v", data, err)
	}
	if got, err := shed.ParseReply(data); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("parsed %+v %v", got, err)
	}
	silent, err := shed.EncodeReply(shed.Reply{Version: shed.Version, Round: 1, Revision: one, Failure: "the agent crashed"})
	if err != nil || !strings.Contains(string(silent), `"answers": []`) || strings.Contains(string(silent), "redraft") {
		t.Fatalf("reply without answers:\n%s %v", silent, err)
	}
	if got, err := shed.ParseReply(silent); err != nil || got.Answers != nil || got.Failure != "the agent crashed" {
		t.Fatalf("parsed %+v %v", got, err)
	}
	if shed.ReplyPath(12) != "shed/round-12/reply.json" || shed.ReplyDocumentID(12) != "shed-round-12-reply" {
		t.Fatalf("path %s, ID %s", shed.ReplyPath(12), shed.ReplyDocumentID(12))
	}
	for name, mutate := range map[string]func(*shed.Reply){
		"version":        func(r *shed.Reply) { r.Version = 2 },
		"round":          func(r *shed.Reply) { r.Round = 0 },
		"revision":       func(r *shed.Reply) { r.Revision = shed.Pin{Spec: 1} },
		"redraft":        func(r *shed.Reply) { r.Redraft = &shed.Pin{Spec: 1, Plan: 1} },
		"empty answer":   func(r *shed.Reply) { r.Answers[0].Answer = " " },
		"no objection":   func(r *shed.Reply) { r.Answers[0].Objection = "" },
		"answered twice": func(r *shed.Reply) { r.Answers = append(r.Answers, r.Answers[0]) },
	} {
		bad := r
		bad.Answers = append([]shed.Answer(nil), r.Answers...)
		mutate(&bad)
		if _, err := shed.EncodeReply(bad); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
	for name, content := range map[string]string{
		"unknown field": `{"version":1,"round":1,"revision":{"spec":1,"plan":1},"answers":[],"extra":1}`,
		"two objects":   string(silent) + string(silent),
		"not JSON":      "reply",
	} {
		if _, err := shed.ParseReply([]byte(content)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// No member's record takes the reply's file.
	if _, err := shed.Encode(record(1, "reply", one, 0)); err == nil {
		t.Fatal("a member named reply was encoded")
	}
}

// The architect's replies sit beside the members' records without being read
// as one, and are read back by round.
func TestRepliesAreKeptApartFromTheMembersRecords(t *testing.T) {
	f := setup(t)
	member, err := shed.Encode(record(1, alice, one, 1))
	if err != nil {
		t.Fatal(err)
	}
	write := func(r shed.Reply, path string, revision int) {
		t.Helper()
		data, err := shed.EncodeReply(r)
		if err != nil {
			t.Fatal(err)
		}
		f.documents(t, stream, document(stream, shed.ReplyDocumentID(r.Round), path, string(data), revision))
	}
	first := shed.Reply{Version: shed.Version, Round: 1, Revision: one, Answers: []shed.Answer{{Objection: shed.ObjectionID(1, alice, 1), Answer: "Agreed."}}, Redraft: &two}
	second := shed.Reply{Version: shed.Version, Round: 2, Revision: two}
	f.documents(t, stream, document(stream, shed.DocumentID(1, alice), shed.Path(1, alice), string(member), 1))
	write(second, shed.ReplyPath(2), 1)
	write(first, shed.ReplyPath(1), 1)
	if got, err := shed.Records(f.repo, stream); err != nil || len(got) != 1 || got[0].Member != alice {
		t.Fatalf("records %+v %v", got, err)
	}
	if got, err := shed.Replies(f.repo, stream); err != nil || !reflect.DeepEqual(got, []shed.Reply{first, second}) {
		t.Fatalf("replies %+v %v", got, err)
	}
	// A reply that records another round than its path names is refused.
	write(shed.Reply{Version: shed.Version, Round: 4, Revision: one}, shed.ReplyPath(3), 1)
	if _, err := shed.Replies(f.repo, stream); err == nil || !strings.Contains(err.Error(), "shed/round-3/reply.json") {
		t.Fatalf("misplaced reply: %v", err)
	}
}

func replyTool(t *testing.T, turn shed.ReplyTurn) coreadapter.Tool {
	t.Helper()
	list, err := shed.ReplyTools(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != shed.ReplyTool || list[0].Effect != coreadapter.ToolMemory || list[0].Handle == nil || !json.Valid(list[0].InputSchema) {
		t.Fatalf("tools %+v", list)
	}
	return list[0]
}

func TestReplyAnswersOnlyTheDissentThatStands(t *testing.T) {
	var saved shed.Reply
	first, second := shed.ObjectionID(1, alice, 1), shed.ObjectionID(1, bob, 1)
	standing := shed.OpenDissent([]shed.Record{record(1, alice, one, 1), record(1, bob, one, 1)})
	turn := shed.ReplyTurn{Reply: shed.Reply{Round: 1, Revision: one, Turn: "reply-1-1", Answers: []shed.Answer{{Objection: "stale", Answer: "stale"}}, Failure: "stale"}, Standing: standing,
		Save: func(r shed.Reply) error { saved = r; return nil }}
	reply := replyTool(t, turn)
	if recorded, reason := call(t, reply, shed.Answer{Objection: shed.ObjectionID(2, alice, 1), Answer: "x"}); recorded || reason != "no objection "+shed.ObjectionID(2, alice, 1)+" stands after round 1" {
		t.Fatalf("answered an objection that does not stand: %q", reason)
	}
	if recorded, reason := call(t, reply, shed.Answer{Objection: first, Answer: " \n"}); recorded || reason != "a reply requires an answer" {
		t.Fatalf("empty answer: %q", reason)
	}
	if saved.Round != 0 {
		t.Fatalf("a refused answer was kept: %+v", saved)
	}
	for _, a := range []shed.Answer{{first, "No."}, {second, "Split."}, {first, "Yes: the unit is split."}} {
		if recorded, id := call(t, reply, a); !recorded || id != a.Objection {
			t.Fatalf("answer %+v: %q", a, id)
		}
	}
	want := shed.Reply{Version: shed.Version, Round: 1, Revision: one, Turn: "reply-1-1", Answers: []shed.Answer{{second, "Split."}, {first, "Yes: the unit is split."}}}
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("saved %+v, want %+v", saved, want)
	}
	if _, err := reply.Handle(context.Background(), json.RawMessage(`{"objection":"x","answer":"y","extra":1}`)); err == nil {
		t.Fatal("unknown input field accepted")
	}
}

func TestReplyKeepsNothingWhenSavingFails(t *testing.T) {
	failing := errors.New("disk full")
	fail := true
	var saved shed.Reply
	first := shed.ObjectionID(1, alice, 1)
	reply := replyTool(t, shed.ReplyTurn{Reply: shed.Reply{Round: 1, Revision: one}, Standing: shed.OpenDissent([]shed.Record{record(1, alice, one, 1)}),
		Save: func(r shed.Reply) error {
			if fail {
				return failing
			}
			saved = r
			return nil
		}})
	if _, err := reply.Handle(context.Background(), json.RawMessage(`{"objection":"`+first+`","answer":"lost"}`)); !errors.Is(err, failing) {
		t.Fatalf("error %v", err)
	}
	fail = false
	if recorded, _ := call(t, reply, shed.Answer{Objection: first, Answer: "kept"}); !recorded || len(saved.Answers) != 1 || saved.Answers[0].Answer != "kept" {
		t.Fatalf("saved %+v", saved)
	}
}

func TestReplyToolRequiresAPlaceToKeepAnswersAndAValidTurn(t *testing.T) {
	save := func(shed.Reply) error { return nil }
	for name, turn := range map[string]shed.ReplyTurn{
		"save":     {Reply: shed.Reply{Round: 1, Revision: one}},
		"round":    {Reply: shed.Reply{Revision: one}, Save: save},
		"revision": {Reply: shed.Reply{Round: 1}, Save: save},
	} {
		if _, err := shed.ReplyTools(turn); err == nil {
			t.Errorf("%s: tool built", name)
		}
	}
}
