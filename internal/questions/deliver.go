package questions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor is the provenance of answer turns.
var Actor = trace.Actor{Kind: "service", ID: "questions"}

// TurnID is the turn that delivers the answer to question id on the asker's
// thread. A question is answered once, so its thread holds the turn at most
// once.
func TurnID(id string) string { return "answer_" + id }

// Prompt is the text of the turn that delivers a validated ruling.
func Prompt(q trace.Question, ruling trace.Ruling) string {
	var b strings.Builder
	opening := "Answer to your question %s."
	if ruling.Decision == trace.DecisionRuling {
		opening = "The owner ruled on your question %s. The chief of staff relays the ruling."
	}
	fmt.Fprintf(&b, opening+"\n\n", q.ID)
	sections := []envelope.Section{{Name: "question", Text: q.Question}}
	if ruling.Decision == trace.DecisionRuling {
		sections = append(sections, envelope.Section{Name: "owner_response", Text: ruling.OwnerResponse}, envelope.Section{Name: "returned_answer", Text: ruling.ReturnedAnswer})
	} else {
		sections = append(sections, envelope.Section{Name: "answer", Text: ruling.ReturnedAnswer})
	}
	if len(ruling.Citations) > 0 {
		sections = append(sections, envelope.Section{Name: "citations", Text: strings.Join(ruling.Citations, "\n")})
	}
	part, _ := envelope.Render(sections...)
	b.WriteString(part)
	return b.String()
}

// Deliver queues the ruling's returned answer as the next turn of the thread
// that asked, which unparks it, and reports whether it queued the turn. The
// turn carries the asking turn's system prompt and unit, and the profile the
// asker's role is bound to now. A question without a ruling, one whose answer
// is already on the thread, and one whose asking agent the trace does not hold
// queue nothing.
func Deliver(ctx context.Context, repository *trace.Repository, q trace.QuestionState, profile func(role string) (coreadapter.Profile, error), at time.Time) (bool, error) {
	if q.Ruling == nil {
		return false, nil
	}
	asked, stream := q.Asked, q.Asked.Workstream
	thread, err := repository.Thread(stream, asked.AskedBy.ID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	system := ""
	for _, turn := range thread.Turns {
		if turn.Request.TurnID == TurnID(asked.ID) {
			return false, nil
		}
		if turn.Request.TurnID == asked.Turn {
			system = turn.Request.SystemPrompt
		}
	}
	p, err := profile(thread.Identity.Role)
	if err != nil {
		return false, fmt.Errorf("profile of role %s: %w", thread.Identity.Role, err)
	}
	turn := TurnID(asked.ID)
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: asked.Project, Workstream: stream, Unit: asked.Unit,
			At: at, Actor: Actor, Cause: trace.QuestionSubject(asked.ID) + "_" + trace.QuestionAnswered, Depth: q.Ruling.Depth + 1},
		AgentID: thread.Identity.ID, ThreadID: thread.Identity.ThreadID, TurnID: turn, Profile: p, SystemPrompt: system,
		Prompt: Prompt(asked, *q.Ruling)})
	return err == nil, err
}

// Deliverer queues every answered question's answer on its asker's thread.
// The answer tool only records; this pass is the one path that delivers, so
// an answer recorded before a crash is delivered by the first pass after it.
type Deliverer struct {
	Repository *trace.Repository
	Now        func() time.Time
	// Profile returns the profile a role's new turn runs with.
	Profile func(role string) (coreadapter.Profile, error)
	// Skip reports a workstream whose answers stay undelivered.
	Skip func(config.WorkstreamID) (bool, error)
}

// Pass delivers the undelivered answers of every workstream.
func (d *Deliverer) Pass(ctx context.Context) error {
	if d.Repository == nil || d.Now == nil || d.Profile == nil {
		return errors.New("answer delivery requires a trace, a clock and profiles")
	}
	streams, err := d.Repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Skip != nil {
			skip, err := d.Skip(stream)
			if err != nil {
				return err
			}
			if skip {
				continue
			}
		}
		states, err := d.Repository.Questions(stream)
		if err != nil {
			return fmt.Errorf("workstream %s questions: %w", stream, err)
		}
		for _, q := range states {
			if q.State != trace.QuestionAnswered {
				continue
			}
			if _, err := Deliver(ctx, d.Repository, q, d.Profile, d.Now()); err != nil {
				return fmt.Errorf("workstream %s question %s: %w", stream, q.Asked.ID, err)
			}
		}
	}
	return nil
}
