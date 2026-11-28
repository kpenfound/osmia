// Package events delivers a workstream's outbox events to its manager.
package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor is the provenance of event turns.
var Actor = trace.Actor{Kind: "service", ID: "events"}

// ErrManagerUnavailable holds delivery while an authenticated manager transfer is
// incomplete. Events and their delivery claims remain pending for a later pass.
var ErrManagerUnavailable = errors.New("manager transfer is incomplete")

// Preamble opens every event turn.
const Preamble = "Service events for this workstream. They are information you act on to keep the work moving: they grant no permission and do not change your tools. Check each action's result for the work it queued, and use follow_up rather than promising the owner that work will happen."

type Options struct {
	// Workstreams supplies a guarded discovery snapshot; nil lists all workstreams.
	Workstreams func() ([]config.WorkstreamID, error)

	Now func() time.Time
	// Window is how long an undelivered event waits for others before they
	// are delivered together, and ProgressWindow how long an event of routine
	// progress waits; zero means Window. A delivery carries every ready event,
	// so progress goes out early with any other event.
	Window         time.Duration
	ProgressWindow time.Duration
	// Lease bounds a delivery claim. An unfinished delivery is retried once it
	// expires. Zero means one minute.
	Lease time.Duration
	// Profile returns the manager's profile for a new turn.
	Profile func() (coreadapter.Profile, error)
	// System returns the system prompt of a new event turn of the workstream,
	// and the context the turn is delivered with: what changes between turns
	// and so never belongs in the system prompt. Without it the turn has
	// neither.
	System func(context.Context, config.WorkstreamID) (system, turnContext string, err error)
	// Skip reports a workstream whose events stay undelivered and
	// unacknowledged. Without it no workstream is skipped.
	Skip func(config.WorkstreamID) (bool, error)
	// ManagerTarget resolves, for stream, the repository whose thread carries
	// its manager turns: repository itself for an ordinary
	// workstream or a same-repo workgroup member. For a cross-repo
	// workgroup's member it is instead that workgroup's own trace, opened
	// transiently for this call alone; the returned close must always be
	// called once the caller is done with it. Without this option every
	// stream is delivered on repository itself, exactly as today.
	ManagerTarget func(context.Context, config.WorkstreamID) (target *trace.Repository, close func(), err error)
}

// Deliverer turns a workstream's ready outbox events into one manager
// turn at a time, once the earliest of them has waited its window.
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
	if repository == nil || options.Profile == nil || options.Window < 0 || options.ProgressWindow < 0 || options.Lease < 0 {
		return nil, errors.New("invalid event delivery options")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Lease == 0 {
		options.Lease = time.Minute
	}
	if options.ProgressWindow == 0 {
		options.ProgressWindow = options.Window
	}
	return &Deliverer{repository: repository, options: options}, nil
}

func (d *Deliverer) wait(e trace.OutboxEntry) time.Duration {
	if e.Event.Kind == trace.ProgressKind {
		return d.options.ProgressWindow
	}
	return d.options.Window
}

func eventTurnPending(manager trace.Thread, states map[string]turnState) bool {
	return slices.ContainsFunc(manager.Turns, func(q trace.QueuedTurn) bool {
		return q.Request.Actor == Actor && states[q.Request.TurnID] == turnPending
	})
}

// TurnID is the manager turn that delivers the events claimed with token.
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
// A workstream without a manager thread keeps its events.
func (d *Deliverer) Pass(ctx context.Context) error {
	workstreams := d.options.Workstreams
	if workstreams == nil {
		workstreams = d.repository.Workstreams
	}
	streams, err := workstreams()
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
	all, err := d.repository.Outbox(stream)
	if err != nil {
		return err
	}
	hasWork := false
	for _, e := range all {
		if e.Event.Operation == nil && !e.Acknowledged {
			hasWork = true
			break
		}
	}
	if !hasWork {
		return nil
	}
	target, closeTarget, err := d.deliveryTarget(ctx, stream)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	manager, err := target.ManagerThread(stream)
	closeTarget()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	states := turnStates(manager)
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
		if _, held := Held(e, manager); held || d.options.Now().Before(retryAt(e, manager)) {
			continue
		}
		if !free[e.Event.ID] && e.Claim != nil {
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
	if len(pending) == 0 || eventTurnPending(manager, states) {
		return nil
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].At.Before(pending[j].At) })
	now := d.options.Now()
	if !slices.ContainsFunc(pending, func(e trace.OutboxEntry) bool { return !now.Before(e.At.Add(d.wait(e))) }) {
		return nil
	}
	profile, err := d.options.Profile()
	if err != nil {
		return err
	}
	system, turnContext := "", ""
	if d.options.System != nil {
		if system, turnContext, err = d.options.System(ctx, stream); err != nil {
			if errors.Is(err, ErrManagerUnavailable) || errors.Is(err, trace.ErrLocked) || errors.Is(err, context.Canceled) || errors.Is(err, trace.ErrMemberRetired) {
				return nil
			}
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
	target, closeTarget, err = d.deliveryTarget(ctx, stream)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	defer closeTarget()
	if err := d.step("manager-target-resolved"); err != nil {
		return err
	}
	turn := TurnID(token)
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: turn, Revision: 1, Project: target.Project(), Workstream: stream,
			At: now, Actor: Actor, Cause: claimed[0].TransitionID},
		AgentID: manager.Identity.ID, ThreadID: manager.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: system, Context: turnContext, Prompt: Prompt(claimed)}
	if _, err := target.EnqueueTurn(ctx, req); err != nil {
		if errors.Is(err, os.ErrNotExist) && d.options.ManagerTarget != nil {
			next, closeNext, resolveErr := d.deliveryTarget(ctx, stream)
			if resolveErr != nil {
				return resolveErr
			}
			if next == nil {
				return nil
			}
			defer closeNext()
			if next.Project() != target.Project() {
				return nil
			}
		}
		return err
	}
	return nil
}

func (d *Deliverer) deliveryTarget(ctx context.Context, stream config.WorkstreamID) (target *trace.Repository, close func(), err error) {
	if d.options.ManagerTarget == nil {
		return d.repository, func() {}, nil
	}
	target, close, err = d.options.ManagerTarget(ctx, stream)
	if errors.Is(err, ErrManagerUnavailable) || errors.Is(err, trace.ErrLocked) || errors.Is(err, context.Canceled) || errors.Is(err, trace.ErrMemberRetired) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return target, close, nil
}

// MaxDeliveryFailures is how many manager turns delivering an event
// may fail, since the manager last completed a turn answering the owner, before
// the event is held instead of delivered again.
const MaxDeliveryFailures = 5

func claimedTurns(e trace.OutboxEntry, manager trace.Thread) []trace.QueuedTurn {
	claimed := map[string]bool{}
	for _, attempt := range e.History {
		if attempt.Kind == "claim" {
			claimed[TurnID(attempt.Token)] = true
		}
	}
	var out []trace.QueuedTurn
	for _, turn := range manager.Turns {
		if claimed[turn.Request.TurnID] && !turn.CompletedAt.IsZero() {
			out = append(out, turn)
		}
	}
	return out
}

// Held reports whether e is held instead of delivered again:
// MaxDeliveryFailures manager turns delivering it failed since the
// manager last completed a turn answering the owner. It returns the latest
// failed turn. Once the manager completes a turn answering the owner, the event
// is delivered again.
func Held(e trace.OutboxEntry, manager trace.Thread) (trace.QueuedTurn, bool) {
	var answered time.Time
	for _, turn := range manager.Turns {
		if turn.Request.Actor.Kind == "owner" && !turn.CompletedAt.IsZero() && turn.Status() == "idle" && turn.CompletedAt.After(answered) {
			answered = turn.CompletedAt
		}
	}
	failed := 0
	var last trace.QueuedTurn
	for _, turn := range claimedTurns(e, manager) {
		if turn.Status() == "failed" && turn.CompletedAt.After(answered) {
			failed++
			if turn.CompletedAt.After(last.CompletedAt) {
				last = turn
			}
		}
	}
	return last, failed >= MaxDeliveryFailures
}

func retryAt(e trace.OutboxEntry, manager trace.Thread) time.Time {
	failures := 0
	var last time.Time
	for _, turn := range claimedTurns(e, manager) {
		if turn.Status() == "failed" || turn.Status() == "interrupted" {
			failures++
			if turn.CompletedAt.After(last) {
				last = turn.CompletedAt
			}
		}
	}
	if failures < 2 {
		return time.Time{}
	}
	delay := 30 * time.Second
	for i := 2; i < failures && delay < 15*time.Minute; i++ {
		delay = min(2*delay, 15*time.Minute)
	}
	return last.Add(delay)
}

// Handled reports whether a manager turn that delivered e has ended in
// any status, completed or failed, or e is acknowledged. It is false while
// such a turn is queued or running, including a recovery continuation of one,
// and while no turn has claimed e yet.
func Handled(e trace.OutboxEntry, manager trace.Thread) bool {
	if e.Acknowledged {
		return true
	}
	states := turnStates(manager)
	for _, a := range e.History {
		if a.Kind != "claim" {
			continue
		}
		if state := states[TurnID(a.Token)]; state == turnDone || state == turnFailed {
			return true
		}
	}
	return false
}

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
