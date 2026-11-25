package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// HoldTurns tells the repository which reserved turns a previous service
// session left running on a client that may still reattach them. A held turn
// is not reported interrupted and is not abandoned until it is no longer held.
// f may be called with the repository's lock held and must not call back into
// it.
func (r *Repository) HoldTurns(f func(turn string) bool) {
	r.held.Store(&f)
}

func (r *Repository) turnHeld(turn string) bool {
	for p := r; p != nil; p = p.parent {
		if f := p.held.Load(); f != nil && *f != nil && (*f)(turn) {
			return true
		}
	}
	return false
}

// ReclaimTurn makes the current service session the owner of a turn that a
// previous session reserved and never captured a result for, because the
// client running it reattached after the restart. The claim, its token and
// its session directory are unchanged, so the turn's result is captured as
// for an uninterrupted turn. A turn that is not reserved by a previous session,
// or already has a result, is refused with ErrClaim.
func (r *Repository) ReclaimTurn(ctx context.Context, stream config.WorkstreamID, agent, turn string) (QueuedTurn, error) {
	r, err := r.scoped(stream)
	if err != nil {
		return QueuedTurn{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return QueuedTurn{}, err
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return QueuedTurn{}, err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return QueuedTurn{}, os.ErrNotExist
	}
	for i, q := range t.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Claim == nil || q.Response != nil || t.Active != turn {
			return QueuedTurn{}, ErrClaim
		}
		if q.Claim.ServiceSession == r.session {
			return q, nil
		}
		claim := *q.Claim
		claim.ServiceSession = r.session
		q.Claim = &claim
		t.Turns[i] = q
		log.Threads[agent] = t
		if err := r.saveThread(ctx, stream, log); err != nil {
			return QueuedTurn{}, err
		}
		return q, nil
	}
	return QueuedTurn{}, ErrClaim
}

// SessionClient returns the client that holds the thread's saved session: the
// one that ran the latest turn that ended with a session to resume. A turn
// recorded without a client ran on the local client. It reports false when the
// thread has no saved session.
func (t Thread) SessionClient() (string, bool) {
	for i := len(t.Turns) - 1; i >= 0; i-- {
		q := t.Turns[i]
		if q.Response == nil || q.CompletedAt.IsZero() || !coreadapter.ValidSession(q.Response.Result.Session) {
			continue
		}
		client := q.Response.Result.Client
		if client == "" {
			client = coreadapter.LocalClient
		}
		return client, true
	}
	return "", false
}

// The kinds of client event the service records for a turn that runs on a
// remote client.
const (
	// ClientPlaced records that an attempt of the turn was offered to a client
	// and claimed.
	ClientPlaced = "placed"
	// ClientReturned records that the turn's result or view from its client was
	// taken: later copies are answered as already recorded.
	ClientReturned = "returned"
	// ClientLate records that a view or result arrived for a turn whose
	// recovery had already begun, and was not used.
	ClientLate = "late"
	// ClientSkipped records that a fallback profile of the turn's chain was
	// skipped because the client the turn runs on cannot run it. What names
	// the profile.
	ClientSkipped = "skipped"
)

// ClientEvent is the content of a document recording where a turn ran and
// what its client sent back. Client is the tailnet node's stable ID.
type ClientEvent struct {
	Kind    string    `json:"kind"`
	Turn    string    `json:"turn"`
	Thread  string    `json:"thread,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Client  string    `json:"client"`
	Name    string    `json:"name,omitempty"`
	What    string    `json:"what,omitempty"`
	Answer  string    `json:"answer,omitempty"`
	At      time.Time `json:"at"`
}

// ClientEventPrefix starts the ID and path of every client event document.
const ClientEventPrefix = "client-event-"

// ClientEventID is the ID of a client event document. n distinguishes events
// of one kind for a turn, such as its attempts or repeated late returns.
func ClientEventID(kind, turn string, n int) string {
	sum := sha256.Sum256([]byte(turn))
	return fmt.Sprintf("%s%s-%x-%d", ClientEventPrefix, kind, sum[:8], n)
}

// RecordClientEvent records one client event for a turn of stream as a
// document, once: recording an event whose ID is already recorded does
// nothing and reports false.
func (r *Repository) RecordClientEvent(ctx context.Context, stream config.WorkstreamID, unit string, n int, e ClientEvent) (bool, error) {
	id := ClientEventID(e.Kind, e.Turn, n)
	existing, err := Read[Document](r, stream)
	if err != nil {
		return false, err
	}
	for _, d := range existing {
		if d.ID == id {
			return false, nil
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	h := Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: 1, Project: r.Project(), Workstream: stream, Unit: unit, At: e.At,
		Actor: Actor{Kind: "service", ID: "client-dispatch"}, Cause: e.Turn}
	err = r.RecordDocuments(ctx, []Document{{Header: h, Path: "clients/" + id + ".json", Content: string(data) + "\n"}})
	return err == nil, err
}

// ClientEvents returns the client events recorded for turn in stream, oldest
// first.
func (r *Repository) ClientEvents(stream config.WorkstreamID, turn string) ([]ClientEvent, error) {
	docs, err := Read[Document](r, stream)
	if err != nil {
		return nil, err
	}
	var events []ClientEvent
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, ClientEventPrefix) {
			continue
		}
		var e ClientEvent
		if err := json.Unmarshal([]byte(d.Content), &e); err != nil {
			return nil, fmt.Errorf("client event %s: %w", d.ID, err)
		}
		if e.Turn == turn {
			events = append(events, e)
		}
	}
	return events, nil
}
