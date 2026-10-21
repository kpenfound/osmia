package intake

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
)

func TestRoundOperationInputIsValidated(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]coreadapter.Operation{
		"another action":      {Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"another boundary":    {Boundary: "vcs", Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"unknown field":       {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1,"extra":1}`)},
		"no round":            {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":0,"spec":1,"plan":1}`)},
		"no spec revision":    {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":0,"plan":1}`)},
		"no plan revision":    {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1}`)},
		"negative resumption": {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1,"resume":-1}`)},
	} {
		if _, err := DecodeRound(op); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if in, err := DecodeRound(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":2,"spec":3,"plan":4}`)}); err != nil || in != (RoundInput{Round: 2, Spec: 3, Plan: 4}) {
		t.Fatalf("decoded %+v %v", in, err)
	}
	if in, err := DecodeRound(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":2,"spec":3,"plan":4,"resume":2}`)}); err != nil || in != (RoundInput{Round: 2, Spec: 3, Plan: 4, Resume: 2}) {
		t.Fatalf("decoded resumption %+v %v", in, err)
	}
}

func TestDraftOperationInputIsValidated(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]coreadapter.Operation{
		"another action":        {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"draft":1}`)},
		"unknown field":         {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":1,"extra":1}`)},
		"no draft":              {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":0}`)},
		"a negative resumption": {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":1,"resume":-1}`)},
	} {
		if _, err := DecodeDraft(op); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if in, err := DecodeDraft(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":2,"resume":1}`)}); err != nil || in != (DraftInput{Draft: 2, Resume: 1}) {
		t.Fatalf("a resumption: %+v %v", in, err)
	}
}

func TestReplyOperationInputIsValidated(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]coreadapter.Operation{
		"another action":        {Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"a round":               {Boundary: coreadapter.RunnerBoundary, Action: RoundAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"another boundary":      {Boundary: "vcs", Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)},
		"unknown field":         {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1,"extra":1}`)},
		"no round":              {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":0,"spec":1,"plan":1}`)},
		"no revision":           {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1}`)},
		"a negative resumption": {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1,"resume":-1}`)},
	} {
		if _, err := DecodeShed(op, ReplyAction); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := DecodeRound(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"round":1,"spec":1,"plan":1}`)}); err == nil {
		t.Error("a reply decoded as a round")
	}
}

// The conclusion at the round limit names what stopped the debate: the
// configured cap, the further rounds the owner asked for, or the round that
// debated the redraft they asked for instead.
func TestBoundNamesWhatStoppedTheDebate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		requests []shed.More
		redrafts []shed.Redraft
		limit    int
		want     string
	}{
		"the configured cap":          {nil, nil, 3, "at the shed.max_rounds cap of 3"},
		"the rounds asked for":        {[]shed.More{{Round: 1, Rounds: 1}}, nil, 2, "at round 2, the last of the further rounds the owner asked for"},
		"the redraft asked for":       {nil, []shed.Redraft{{Round: 2}}, 3, "at round 3, the round that debated the redraft the owner asked for"},
		"the later of the two":        {[]shed.More{{Round: 1, Rounds: 1}}, []shed.Redraft{{Round: 3}}, 4, "at round 4, the round that debated the redraft the owner asked for"},
		"the rounds asked for, later": {[]shed.More{{Round: 3, Rounds: 2}}, []shed.Redraft{{Round: 1}}, 5, "at round 5, the last of the further rounds the owner asked for"},
	} {
		if got := Bound(3, tc.requests, tc.redrafts, tc.limit); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
