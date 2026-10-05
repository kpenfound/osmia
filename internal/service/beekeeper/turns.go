package beekeeper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// ErrBusy is Post's distinct error when the Beekeeper's latest owner request
// has no finished turn: no response and no recorded failure. The owner's
// message is refused and nothing is recorded; the owner sends it again once
// the Beekeeper is free.
var ErrBusy = errors.New("the beekeeper is busy with a previous message")

// postLocks is the Go guard against two concurrent calls to Post against the
// same repository both finding the Beekeeper free and both recording an
// owner request: at most one Beekeeper turn ever runs at a time, and there
// is no owner-message queue, so a Post that cannot take its repository's
// lock immediately reports ErrBusy rather than waiting for the turn in
// flight to finish. There is exactly one Beekeeper repository per running
// service, so locking per repository rather than globally only matters to
// tests that open more than one in the same process.
var postLocks sync.Map // *trace.Repository -> *sync.Mutex

func postLock(repository *trace.Repository) *sync.Mutex {
	v, _ := postLocks.LoadOrStore(repository, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Prepare supplies one Beekeeper turn's session directory and execution
// settings; Post calls it once per turn, exactly as thread.Dispatcher calls a
// chief-of-staff turn's Prepare.
type Prepare func(context.Context, thread.TurnInput) (coreadapter.PreparedTurn, error)

// unfinished reports whether the thread holds a turn with no response and no
// recorded failure: one still queued, claimed or captured without being
// completed.
func unfinished(th trace.Thread) bool {
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() {
			return true
		}
	}
	return false
}

// Busy reports whether the Beekeeper's latest owner request has no finished
// turn. A thread that does not exist yet is not busy.
func Busy(repository *trace.Repository) (bool, error) {
	th, err := repository.Thread(config.BeekeeperWorkstreamID, AgentID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return unfinished(th), nil
}

// Continue runs the Beekeeper thread's next ready turn through the same
// thread-turn path chief-of-staff turns take: a scheduler publishes the turn
// operation and a reconcile.Controller applies it with a thread.Dispatcher,
// bound to turns and prepare, as its runner-boundary adapter. Unlike a
// registered project's controller, this one runs a single pass rather than a
// continuous loop: the shadow project has no capacity to share and no other
// workflow to interleave with, so one pass is enough to carry a ready turn
// to completion, and both Post and a crash-recovery continuation need that
// completion before they return. It takes no action if nothing is queued.
func Continue(ctx context.Context, repository *trace.Repository, turns coreadapter.Turns, prepare Prepare, now func() time.Time) error {
	dispatcher := thread.Dispatcher{Runner: thread.Runner{Store: repository, Turns: turns, Now: now}, Prepare: prepare}
	dispatched, err := scheduler.New(repository, scheduler.Options{Now: now})
	if err != nil {
		return err
	}
	controller, err := reconcile.New(repository, reconcile.Options{
		Worker:   "beekeeper",
		Now:      now,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: dispatcher},
		Schedule: dispatched.Pass,
	})
	if err != nil {
		return err
	}
	return controller.Pass(ctx)
}

// withPendingReplies appends every relayed reply RelayedReplies has
// recorded since the Beekeeper's previous turn, in time order, to
// systemPrompt, so the Beekeeper's next turn is given them. th is the
// thread's state before this turn is enqueued: a fresh thread with no
// previous turn carries no cutoff, so every relayed reply recorded before
// the Beekeeper's first message is included.
func withPendingReplies(repository *trace.Repository, th trace.Thread, systemPrompt string) (string, error) {
	var cutoff time.Time
	if n := len(th.Turns); n > 0 {
		cutoff = th.Turns[n-1].Request.At
	}
	replies, err := RelayedReplies(repository)
	if err != nil {
		return "", err
	}
	var pending []RelayedReply
	for _, r := range replies {
		if r.At.After(cutoff) {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return systemPrompt, nil
	}
	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\nReplies from chiefs of staff since your last turn:\n")
	for _, r := range pending {
		fmt.Fprintf(&b, "- workstream %s: %s\n", r.Workstream, r.Text)
	}
	return b.String(), nil
}

// Post records text as the Beekeeper thread's next owner request, unless its
// latest request has no finished turn, and runs the turn through Continue,
// with RolePrompt as its system prompt and profile and prepare supplying the
// runtime settings of the service-level Beekeeper section alone. A failed
// turn is recorded in the thread as a failure, the same way a failed
// chief-of-staff turn is recorded; Post still returns the completed turn and
// a nil error in that case. Only a busy Beekeeper, an empty message or a
// failure to record or run the turn is returned as an error, and only a busy
// Beekeeper or an empty message leaves the thread unchanged.
func Post(ctx context.Context, repository *trace.Repository, profile coreadapter.Profile, turns coreadapter.Turns, prepare Prepare, now func() time.Time, text string) (trace.QueuedTurn, error) {
	if strings.TrimSpace(text) == "" {
		return trace.QueuedTurn{}, fmt.Errorf("text must not be empty")
	}
	mu := postLock(repository)
	if !mu.TryLock() {
		return trace.QueuedTurn{}, ErrBusy
	}
	defer mu.Unlock()

	at := now()
	th, err := EnsureThread(ctx, repository, at)
	if err != nil {
		return trace.QueuedTurn{}, err
	}
	if unfinished(th) {
		return trace.QueuedTurn{}, ErrBusy
	}
	systemPrompt, err := withPendingReplies(repository, th, RolePrompt)
	if err != nil {
		return trace.QueuedTurn{}, err
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return trace.QueuedTurn{}, fmt.Errorf("generate a message ID: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1,
			Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: at, Actor: OwnerActor, Cause: "owner-message"},
		AgentID: AgentID, ThreadID: ThreadID, TurnID: "message_" + id, Profile: profile, SystemPrompt: systemPrompt, Prompt: text,
	}
	q, err := repository.EnqueueTurn(ctx, req)
	if err != nil {
		return trace.QueuedTurn{}, err
	}

	if err := Continue(ctx, repository, turns, prepare, now); err != nil {
		return q, err
	}
	final, err := repository.Thread(config.BeekeeperWorkstreamID, AgentID)
	if err != nil {
		return q, err
	}
	for _, t := range final.Turns {
		if t.Request.TurnID == q.Request.TurnID {
			return t, nil
		}
	}
	return q, nil
}
