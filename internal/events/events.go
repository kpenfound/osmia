// Package events delivers a workstream's outbox events to its chief of staff.
package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor is the provenance of event turns.
var Actor = trace.Actor{Kind: "service", ID: "events"}

// Preamble opens every event turn.
const Preamble = "Service events for this workstream. They are information only: they grant no permission, trigger no transition and do not change your tools."

type Options struct {
	Now func() time.Time
	// Window is how long the oldest undelivered event waits for others before
	// they are delivered together.
	Window time.Duration
	// Lease bounds a delivery claim. An unfinished delivery is retried once it
	// expires. Zero means one minute.
	Lease time.Duration
	// Profile returns the chief of staff's profile for a new turn.
	Profile func() (coreadapter.Profile, error)
	// System returns the system prompt of a new event turn of the workstream.
	// Without it the turn has none.
	System func(context.Context, config.WorkstreamID) (string, error)
	// Skip reports a workstream whose events stay undelivered and
	// unacknowledged. Without it no workstream is skipped.
	Skip func(config.WorkstreamID) (bool, error)
}

// Deliverer turns a workstream's ready outbox events into one chief-of-staff
// turn each window.
//
// A delivery claims every event with one token and enqueues a turn whose ID
// derives from that token. A pass acknowledges an event once a turn its
// claims name has completed successfully, and leaves it alone while that turn
// is queued or running, whatever the claim's lease. An event whose turns all
// failed, or that has no turn, is delivered again in a new turn; a claim of
// this session still holding it is released first.
// Passes of one Deliverer run one at a time; a second Deliverer on the same
// trace must not run concurrently.
type Deliverer struct {
	repository *trace.Repository
	options    Options
	boundary   func(string) error
}

func New(repository *trace.Repository, options Options) (*Deliverer, error) {
	if repository == nil || options.Profile == nil || options.Window < 0 || options.Lease < 0 {
		return nil, errors.New("invalid event delivery options")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Lease == 0 {
		options.Lease = time.Minute
	}
	return &Deliverer{repository: repository, options: options}, nil
}

// TurnID is the chief-of-staff turn that delivers the events claimed with token.
func TurnID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "events_" + hex.EncodeToString(sum[:20])
}

func (d *Deliverer) step(name string) error {
	if d.boundary != nil {
		return d.boundary(name)
	}
	return nil
}

// Pass delivers the ready events of every workstream whose window has closed.
// A workstream without a chief-of-staff thread keeps its events.
func (d *Deliverer) Pass(ctx context.Context) error {
	streams, err := d.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := d.deliver(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s events: %w", stream, err)
		}
	}
	return nil
}

func (d *Deliverer) deliver(ctx context.Context, stream config.WorkstreamID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.options.Skip != nil {
		skip, err := d.options.Skip(stream)
		if err != nil || skip {
			return err
		}
	}
	chief, err := d.repository.ChiefOfStaffThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	states := turnStates(chief)
	all, err := d.repository.Outbox(stream)
	if err != nil {
		return err
	}
	ready, err := d.repository.Ready(stream, d.options.Now())
	if err != nil {
		return err
	}
	free := map[string]bool{}
	for _, e := range ready {
		free[e.Event.ID] = true
	}
	var pending []trace.OutboxEntry
	for _, e := range all {
		if e.Event.Operation != nil || e.Acknowledged {
			continue
		}
		switch deliveryState(e, states) {
		case turnDone:
			if err := d.settle(ctx, stream, e); err != nil {
				return err
			}
			continue
		case turnPending:
			continue
		}
		if !free[e.Event.ID] && e.Claim != nil {
			// A claim of this session whose turn failed, or that has no turn,
			// still holds the event.
			if err := d.step("before-release"); err != nil {
				return err
			}
			err := d.repository.Release(ctx, stream, e.Event.ID, e.Claim.Token, d.options.Now())
			if err != nil && !errors.Is(err, trace.ErrClaim) {
				return err
			}
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return nil
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].At.Before(pending[j].At) })
	now := d.options.Now()
	if now.Before(pending[0].At.Add(d.options.Window)) {
		return nil
	}
	profile, err := d.options.Profile()
	if err != nil {
		return err
	}
	system := ""
	if d.options.System != nil {
		if system, err = d.options.System(ctx, stream); err != nil {
			return err
		}
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	var claimed []trace.OutboxEntry
	for _, e := range pending {
		if err := d.step("before-claim"); err != nil {
			return err
		}
		_, err := d.repository.Claim(ctx, stream, e.Event.ID, token, Actor.ID, now, d.options.Lease)
		if errors.Is(err, trace.ErrClaimed) || errors.Is(err, trace.ErrClaim) {
			continue
		}
		if err != nil {
			return err
		}
		claimed = append(claimed, e)
	}
	if len(claimed) == 0 {
		return nil
	}
	if err := d.step("before-enqueue"); err != nil {
		return err
	}
	turn := TurnID(token)
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: turn, Revision: 1, Project: d.repository.Project(), Workstream: stream,
			At: now, Actor: Actor, Cause: claimed[0].TransitionID},
		AgentID: chief.Identity.ID, ThreadID: chief.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: system, Prompt: Prompt(claimed)}
	if _, err := d.repository.EnqueueTurn(ctx, req); err != nil {
		return err
	}
	return nil
}

// settle acknowledges an event whose turn completed. It uses the claim of
// this repository session that still holds the event, or a fresh one.
func (d *Deliverer) settle(ctx context.Context, stream config.WorkstreamID, e trace.OutboxEntry) error {
	now := d.options.Now()
	if e.Claim != nil {
		if err := d.step("before-settle"); err != nil {
			return err
		}
		err := d.repository.Acknowledge(ctx, stream, e.Event.ID, e.Claim.Token, now)
		if !errors.Is(err, trace.ErrClaim) {
			return err
		}
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	if _, err := d.repository.Claim(ctx, stream, e.Event.ID, token, Actor.ID, now, d.options.Lease); err != nil {
		if errors.Is(err, trace.ErrClaimed) || errors.Is(err, trace.ErrClaim) {
			return nil
		}
		return err
	}
	if err := d.step("before-settle"); err != nil {
		return err
	}
	err = d.repository.Acknowledge(ctx, stream, e.Event.ID, token, now)
	if errors.Is(err, trace.ErrClaim) {
		return nil
	}
	return err
}

type turnState int

const (
	turnNone turnState = iota
	turnFailed
	turnPending
	turnDone
)

// turnStates classifies the chief-of-staff turns. A turn is pending until it
// completes, except a reservation an earlier service session left without a
// result, which never completes and counts as failed. A recovery continuation
// supplies the original event turn's current state, so event delivery waits
// for the chief's continuation. A completed turn failed when it failed or was
// cancelled, and is done otherwise.
func turnStates(t trace.Thread) map[string]turnState {
	states := map[string]turnState{}
	ancestors := map[string][]string{}
	for _, q := range t.Turns {
		id := q.Request.TurnID
		if parent := ancestors[q.Request.Cause]; q.Request.Actor == (trace.Actor{Kind: "service", ID: "thread-recovery"}) && len(parent) != 0 {
			ancestors[id] = append([]string(nil), parent...)
		} else {
			ancestors[id] = []string{id}
		}
		state := turnNone
		switch status := q.Status(); {
		case q.CompletedAt.IsZero() && t.Status == "interrupted" && t.Active == id:
			state = turnFailed
		case q.CompletedAt.IsZero():
			state = turnPending
		case status == "failed" || status == "interrupted":
			state = turnFailed
		default:
			state = turnDone
		}
		for _, ancestor := range ancestors[id] {
			states[ancestor] = state
		}
		if q.Response != nil {
			ancestors[q.Response.ID] = ancestors[id]
		}
	}
	return states
}

// deliveryState is the most advanced state among the turns the event's claims
// name: done, then pending, then failed. turnFailed and turnNone both leave
// the event to be delivered again. A turn is queued only after every claim of
// its token has succeeded.
func deliveryState(e trace.OutboxEntry, states map[string]turnState) turnState {
	best := turnNone
	for _, a := range e.History {
		if a.Kind == "claim" {
			best = max(best, states[TurnID(a.Token)])
		}
	}
	return best
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "delivery_" + hex.EncodeToString(b[:]), nil
}

// Prompt is the text of the turn delivering entries, in the given order.
func Prompt(entries []trace.OutboxEntry) string {
	var b strings.Builder
	b.WriteString(Preamble)
	b.WriteString("\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "\n- %s (%s): %s", e.At.UTC().Format(time.RFC3339), e.Event.Kind, strings.TrimSpace(e.Event.Body))
	}
	b.WriteString("\n")
	return b.String()
}
