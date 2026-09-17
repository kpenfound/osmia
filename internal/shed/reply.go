package shed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// ReplyTool is the tool the architect answers a round's objections with.
	ReplyTool = "reply"
	// replyName is the file name, without its extension, of the architect's
	// reply to a round. No member may carry it.
	replyName = "reply"
)

// Answer is the architect's answer to one objection that stands.
type Answer struct {
	Objection string `json:"objection"`
	Answer    string `json:"answer"`
}

// Reply is the architect's one reply to a round: its answers to the dissent
// that stood once the round was heard, and the revision its redraft recorded,
// if it redrafted. Revision is what the round debated. Problems is why a
// redraft was given up, and Failure why the architect's turn did not end
// normally; what it answered before failing is kept.
type Reply struct {
	Version  int      `json:"version"`
	Round    int      `json:"round"`
	Revision Pin      `json:"revision"`
	Turn     string   `json:"turn,omitempty"`
	Answers  []Answer `json:"answers"`
	Redraft  *Pin     `json:"redraft,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Failure  string   `json:"failure,omitempty"`
}

// ReplyPath is the workstream path of the architect's reply to a round.
func ReplyPath(round int) string { return roundPath(round, replyName) }

// ReplyDocumentID is the trace record ID of the architect's reply to a round.
func ReplyDocumentID(round int) string { return roundDocumentID(round, replyName) }

func (r Reply) check() error {
	switch {
	case r.Version != Version:
		return fmt.Errorf("unsupported shed reply version %d", r.Version)
	case r.Round < 1:
		return errors.New("shed reply requires a positive round")
	case r.Revision.Spec < 1 || r.Revision.Plan < 1:
		return errors.New("shed reply requires the revision the round debated")
	case r.Redraft != nil && !r.Redraft.After(r.Revision):
		return errors.New("a redraft is a later revision than the one the round debated")
	}
	seen := map[string]bool{}
	for _, a := range r.Answers {
		if strings.TrimSpace(a.Objection) == "" || strings.TrimSpace(a.Answer) == "" || seen[a.Objection] {
			return errors.New("a reply answers an objection once, with an answer")
		}
		seen[a.Objection] = true
	}
	return nil
}

// EncodeReply returns the file content of a valid reply.
func EncodeReply(r Reply) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if r.Answers == nil {
		r.Answers = []Answer{}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseReply reads a reply's file content, refusing unknown fields and
// invalid replies.
func ParseReply(data []byte) (Reply, error) {
	var r Reply
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return Reply{}, fmt.Errorf("shed reply: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Reply{}, errors.New("shed reply must be one object")
	}
	if len(r.Answers) == 0 {
		r.Answers = nil
	}
	return r, r.check()
}

// Replies returns the architect's recorded replies of the workstream, ordered
// by round. A reply whose content disagrees with its path is an error.
func Replies(repository *trace.Repository, stream config.WorkstreamID) ([]Reply, error) {
	return replies(repository, stream, replyName, ReplyPath)
}

// Redrafted returns the architect's recorded reports of the redrafts the owner
// asked for, ordered by the round each was asked for after. A report is a
// reply of the same shape, holding what the architect answered and which
// revision it redrafted to.
func Redrafted(repository *trace.Repository, stream config.WorkstreamID) ([]Reply, error) {
	return replies(repository, stream, redraftedName, RedraftedPath)
}

func replies(repository *trace.Repository, stream config.WorkstreamID, name string, path func(int) string) ([]Reply, error) {
	latest, err := shedFiles(repository, stream, name)
	if err != nil {
		return nil, err
	}
	var out []Reply
	for at, d := range latest {
		r, err := ParseReply([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		if path(r.Round) != at {
			return nil, fmt.Errorf("%s: records the %s of round %d", at, name, r.Round)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Reply) int { return a.Round - b.Round })
	return out, nil
}

// ReplyTurn is the architect's turn of a round: the dissent it may answer and
// where the tool keeps its answers.
type ReplyTurn struct {
	// Reply names the round, the revision it debated and the turn; the tool
	// adds the answers.
	Reply Reply
	// Standing is the dissent that stood once the round was heard.
	Standing []Dissent
	// Save keeps the turn's answers so far. It is called with every accepted
	// answer, before the tool reports success.
	Save func(Reply) error
}

// ReplyTools returns the reply tool of one architect turn. An answer that is
// invalid is an ordinary result, {"recorded":false,"reason":...}, so the
// architect reads why within the turn.
func ReplyTools(t ReplyTurn) ([]coreadapter.Tool, error) {
	if t.Save == nil {
		return nil, errors.New("the reply tool requires a place to keep answers")
	}
	t.Reply.Version = Version
	t.Reply.Answers, t.Reply.Redraft, t.Reply.Problems, t.Reply.Failure = nil, nil, nil, ""
	if err := t.Reply.check(); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	reply := coreadapter.Tool{Name: ReplyTool, Effect: coreadapter.ToolMemory,
		Description: "Answer one objection that stands after this round. objection: its ID. answer: what you say to it, and what you changed if you redraft. Answering an objection again replaces your earlier answer.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"objection":{"type":"string"},"answer":{"type":"string"}},"required":["objection","answer"],"additionalProperties":false}`)}
	reply.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input Answer
		if err := decodeInput(raw, &input); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.TrimSpace(input.Answer) == "":
			return refuse(invalid("a reply requires an answer"))
		case !slices.ContainsFunc(t.Standing, func(d Dissent) bool { return d.ID == input.Objection }):
			return refuse(invalid("no objection %s stands after round %d", input.Objection, t.Reply.Round))
		}
		next := t.Reply
		next.Answers = append(slices.DeleteFunc(slices.Clone(next.Answers), func(a Answer) bool { return a.Objection == input.Objection }), input)
		if err := t.Save(next); err != nil {
			return nil, err
		}
		t.Reply = next
		return encode(struct {
			Recorded  bool   `json:"recorded"`
			Objection string `json:"objection"`
		}{true, input.Objection})
	}
	return []coreadapter.Tool{reply}, nil
}
