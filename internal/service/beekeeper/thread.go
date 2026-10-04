package beekeeper

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// AgentID and ThreadID identify the Beekeeper's one durable thread in the
// shadow project, the same way trace.ChiefOfStaff identifies a workstream's
// chief-of-staff thread. There is exactly one Beekeeper thread, shared by
// every registered project, recorded with the same thread and trace
// functions a chief-of-staff thread uses.
const (
	AgentID  = "beekeeper"
	ThreadID = "beekeeper"
)

// OwnerActor is the provenance of the owner's messages to the Beekeeper, the
// same value conversations with a chief of staff use.
var OwnerActor = trace.Actor{Kind: "owner", ID: "local"}

// ReplyActor is the provenance of the Beekeeper's own replies, recorded as
// turn responses on its thread.
var ReplyActor = trace.Actor{Kind: "beekeeper", ID: AgentID}

// RolePrompt opens the system prompt of every Beekeeper turn: the owner's
// one assistant for the whole factory, with its two tools and the limits
// the owner's gates put on it.
const RolePrompt = "You are the Beekeeper, the owner's one assistant for the whole factory, not any single project. " +
	"You can list every registered project and its workstreams with their current status, and you can message the chief of staff of any workstream in any registered project. " +
	"A chief of staff's reply to a message you sent is recorded in this chat; it does not interrupt the turn you are in, and you are given every reply recorded since your last turn at the start of your next turn. " +
	"You hold none of the owner's gates: you cannot approve or refuse delivery, rule on a contested unit, approve a spec or plan, edit or ratify the charter, answer a question or open a workstream."

// EnsureThread creates the Beekeeper's thread identity in the shadow
// project's repository unless it already exists, and returns the thread.
// The first creation fixes the identity's timestamp; later calls leave it
// unchanged. It uses the same trace.Repository.CreateThread every
// chief-of-staff thread is created with; no Beekeeper-specific store exists.
func EnsureThread(ctx context.Context, repository *trace.Repository, at time.Time) (trace.Thread, error) {
	agent := trace.Agent{
		Header: trace.Header{
			Schema: "osmia.trace.agent", Version: trace.Version, ID: AgentID, Revision: 1,
			Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID,
			At: at, Actor: Actor, Cause: "beekeeper-thread-create",
		},
		Role:     AgentID,
		ThreadID: ThreadID,
	}
	if err := repository.CreateThread(ctx, agent); err != nil {
		return trace.Thread{}, err
	}
	return repository.Thread(config.BeekeeperWorkstreamID, AgentID)
}

// Author kinds for a Message: the owner, the Beekeeper itself, a named
// workstream's chief of staff relaying a reply, or a failure note recorded
// when a turn did not produce a reply.
const (
	AuthorOwner        = "owner"
	AuthorBeekeeper    = "beekeeper"
	AuthorChiefOfStaff = "chief_of_staff"
	AuthorFailure      = "failure"
)

// Author identifies who sent one Message. Workstream names the chief of
// staff for AuthorChiefOfStaff and is empty otherwise.
type Author struct {
	Kind       string              `json:"kind"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
}

// Message is one entry of the Beekeeper chat, built only from its thread's
// recorded turns; there is no separate message store.
type Message struct {
	Turn   string    `json:"turn"`
	Author Author    `json:"author"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// Messages returns the Beekeeper chat's most recent messages, oldest first,
// each with its author, and whether older messages exist. A limit of zero or
// less uses 50. It reads the Beekeeper's thread with trace.Repository.Thread
// and applies no store or windowing beyond this bound: every message the
// thread holds is considered, and only the requested tail is returned. A
// thread that does not exist yet reports no messages and no older ones.
func Messages(repository *trace.Repository, limit int) ([]Message, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	t, err := repository.Thread(config.BeekeeperWorkstreamID, AgentID)
	if errors.Is(err, os.ErrNotExist) {
		return []Message{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	all := []Message{}
	for _, q := range t.Turns {
		req := q.Request
		all = append(all, Message{Turn: req.TurnID, Author: Author{Kind: AuthorOwner}, Text: req.Prompt, At: req.At})
		if q.Response == nil || q.CompletedAt.IsZero() {
			continue
		}
		switch q.Status() {
		case "failed", "interrupted":
			text := q.Response.Failure
			if text == "" {
				text = "the turn did not complete"
			}
			all = append(all, Message{Turn: req.TurnID, Author: Author{Kind: AuthorFailure}, Text: text, At: q.Response.At})
		default:
			if q.Response.Result.FinalResponse != "" {
				all = append(all, Message{Turn: req.TurnID, Author: Author{Kind: AuthorBeekeeper}, Text: q.Response.Result.FinalResponse, At: q.Response.At})
			}
		}
	}
	hasOlder := false
	if len(all) > limit {
		hasOlder = true
		all = all[len(all)-limit:]
	}
	return all, hasOlder, nil
}
