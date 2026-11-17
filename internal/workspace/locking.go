package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/ghlog"
)

var cloneLocks sync.Map

// cleanupDeadline bounds the restoration and settlement LocalStep runs for
// its checkpoints, and any other cleanup that takes a clone lock after the
// context that asked for the work has ended. Commands the cleanup starts are
// killed when it passes.
var cleanupDeadline = 2 * time.Minute

// cloneLock is the ownership of one clone. A channel holds it so waiting for
// it can end with a context. The token is released when the owner lets go and
// no command started under the ownership is still running: a command that
// outlives a step keeps the clone locked until it exits.
type cloneLock struct {
	token chan struct{}

	mu       sync.Mutex
	owned    bool
	running  int
	recovery bool
}

type lockKey struct{}
type lockHold struct {
	clone  string
	lock   *cloneLock
	active atomic.Bool
	parent *lockHold
}

func cloneLockOf(key string) *cloneLock {
	value, _ := cloneLocks.LoadOrStore(key, &cloneLock{token: make(chan struct{}, 1)})
	return value.(*cloneLock)
}

func (l *cloneLock) acquire(ctx context.Context) error {
	select {
	case l.token <- struct{}{}:
	default:
		select {
		case l.token <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.mu.Lock()
	l.owned = true
	l.mu.Unlock()
	return nil
}

// release gives up the owner's hold. The clone stays locked while a command
// started under the hold is still running.
func (l *cloneLock) release() {
	l.mu.Lock()
	l.owned = false
	free := l.running == 0
	l.mu.Unlock()
	if free {
		<-l.token
	}
}

func (l *cloneLock) started() {
	l.mu.Lock()
	l.running++
	l.mu.Unlock()
}

// exited records that a command started under the lock has exited, and frees
// the clone when its owner has already let go.
func (l *cloneLock) exited() {
	l.mu.Lock()
	l.running--
	free := l.running == 0 && !l.owned
	l.mu.Unlock()
	if free {
		<-l.token
	}
}

func (l *cloneLock) markRecovery(needed bool) {
	l.mu.Lock()
	l.recovery = needed
	l.mu.Unlock()
}

// ErrCloneContended is the error of a step that could not take a clone's lock
// before its context ended.
var ErrCloneContended = errors.New("clone lock is held by another owner")

// ErrCloneRecovery is the error of a clone whose restoration failed, so its
// checkout may be dirty.
var ErrCloneRecovery = errors.New("clone needs recovery")

// ContentionError reports that the clone lock was still held by another owner
// when the context ended. Nothing ran under the lock.
type ContentionError struct {
	Clone  string
	Waited time.Duration
	Err    error
}

func (e *ContentionError) Error() string {
	return fmt.Sprintf("clone %s is locked by another owner: gave up after waiting %s: %v", e.Clone, e.Waited.Round(time.Millisecond), e.Err)
}
func (e *ContentionError) Is(target error) bool { return target == ErrCloneContended }
func (e *ContentionError) Unwrap() error        { return e.Err }

// IsContention reports whether err is a step that gave up waiting for a clone
// lock.
func IsContention(err error) bool { return errors.Is(err, ErrCloneContended) }

// RecoveryError reports that restoring a clone failed. The clone keeps its
// checkpoint and is marked as needing recovery; the next step on it recovers
// first and is refused when that fails too.
type RecoveryError struct {
	Clone string
	Err   error
}

func (e *RecoveryError) Error() string {
	return fmt.Sprintf("recover clone %s: %v (the clone keeps its checkpoint and needs recovery)", e.Clone, e.Err)
}
func (e *RecoveryError) Is(target error) bool { return target == ErrCloneRecovery }
func (e *RecoveryError) Unwrap() error        { return e.Err }

// IsRecoveryFailure reports whether err carries a failed clone restoration.
func IsRecoveryFailure(err error) bool { return errors.Is(err, ErrCloneRecovery) }

// Quiescence is what is known of a clone's readiness for new work.
type Quiescence struct {
	// Running counts the commands started under the clone's ownership that
	// have not exited.
	Running int
	// NeedsRecovery is set from a failed restoration until a Recover
	// succeeds, and while a checkpoint is on disk outside a step.
	NeedsRecovery bool
}

// Quiesced reports whether no started command is running and the clone needs
// no recovery.
func (q Quiescence) Quiesced() bool { return q.Running == 0 && !q.NeedsRecovery }

// CloneQuiescence reads the in-process state of the clone at path. A deadline
// that fires clears neither part: a command clears Running when it exits and
// a successful Recover clears NeedsRecovery.
func CloneQuiescence(path string) Quiescence {
	l := cloneLockOf(cloneKey(path))
	l.mu.Lock()
	defer l.mu.Unlock()
	return Quiescence{Running: l.running, NeedsRecovery: l.recovery}
}

// Quiescence reads the state of the provider's clone: the in-process state of
// CloneQuiescence, and a checkpoint left on disk by a step nobody is running.
func (j *Jujutsu) Quiescence() Quiescence {
	l := cloneLockOf(cloneKey(j.Clone))
	l.mu.Lock()
	q := Quiescence{Running: l.running, NeedsRecovery: l.recovery}
	owned := l.owned
	l.mu.Unlock()
	if !owned && !q.NeedsRecovery {
		if entries, err := os.ReadDir(j.checkpoints()); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
					q.NeedsRecovery = true
				}
			}
		}
	}
	return q
}

func cloneKey(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func holdFor(ctx context.Context, clone string) *lockHold {
	for hold, _ := ctx.Value(lockKey{}).(*lockHold); hold != nil; hold = hold.parent {
		if hold.clone == clone && hold.active.Load() {
			return hold
		}
	}
	return nil
}

func holdsClone(ctx context.Context, clone string) bool { return holdFor(ctx, clone) != nil }

// lockClone lends ownership to synchronous nested calls using the returned
// context. Callers must not share that context with concurrent goroutines.
// Waiting ends with the context: the error is then a ContentionError naming
// the clone and the wait, and nothing has run.
func lockClone(ctx context.Context, path string) (context.Context, func(), error) {
	key := cloneKey(path)
	if holdsClone(ctx, key) {
		return ctx, func() {}, nil
	}
	lock := cloneLockOf(key)
	start := time.Now()
	if err := lock.acquire(ctx); err != nil {
		waited := time.Since(start)
		scope := ghlog.ScopeOf(ctx)
		operation, _ := ctx.Value(operationKey{}).(string)
		ghlog.Logger(ctx).Warn("workspace clone lock contended", "project", scope.Project, "workstream", scope.Workstream, "operation", operation, "lock", "clone", "clone", key, "duration", waited)
		return ctx, func() {}, &ContentionError{Clone: key, Waited: waited, Err: err}
	}
	if waited := time.Since(start); waited >= time.Second {
		scope := ghlog.ScopeOf(ctx)
		operation, _ := ctx.Value(operationKey{}).(string)
		ghlog.Logger(ctx).Warn("workspace clone lock wait", "project", scope.Project, "workstream", scope.Workstream, "operation", operation, "lock", "clone", "clone", key, "duration", waited)
	}
	parent, _ := ctx.Value(lockKey{}).(*lockHold)
	hold := &lockHold{clone: key, lock: lock, parent: parent}
	hold.active.Store(true)
	return context.WithValue(ctx, lockKey{}, hold), func() { hold.active.Store(false); lock.release() }, nil
}

// LocalStep checkpoints a synchronous local step of an operation. Jujutsu
// providers sharing a clone hold one lock from checkpoint through settlement,
// including the callback's durable progress write. Git callbacks run unlocked;
// their worktree mutations lock themselves. Fetch, push and external calls
// belong outside the callback. The callback must use its supplied context.
//
// Waiting for a clone lock ends with the context: the step then returns a
// ContentionError before any Recover or Checkpoint runs. Commands the step
// starts stop when the context ends. Cancellation restores the interrupted
// step, and a step that ends normally settles, each under the cleanup
// deadline, before ownership is released; ownership is released only after
// every command started under it has exited. A restoration or settlement that
// fails keeps the checkpoint, marks the clone as needing recovery and returns
// a RecoveryError. The next step on the clone recovers first and is refused
// when that fails. A process crash leaves its checkpoint for Recover at
// service startup. Steps must record progress or expose an inspectable effect
// before returning.
func LocalStep(ctx context.Context, operation string, at time.Time, providers []Provider, step func(context.Context) error) (err error) {
	if scope, _ := ctx.Value(stepKey{}).(*stepScope); scope != nil && scope.active.Load() {
		return errors.New("checkpointed local steps cannot be nested")
	}
	var clones []string
	var checkpoints []Provider
	for _, p := range providers {
		if j, ok := p.(*Jujutsu); ok {
			key := cloneKey(j.Clone)
			if !slices.Contains(clones, key) {
				clones = append(clones, key)
			}
			if !slices.Contains(checkpoints, p) {
				checkpoints = append(checkpoints, p)
			}
		}
	}
	ctx = WithOperation(ctx, operation)
	slices.Sort(clones)
	for _, clone := range clones {
		var unlock func()
		if ctx, unlock, err = lockClone(ctx, clone); err != nil {
			return err
		}
		defer unlock()
	}
	scope := &stepScope{operation: operation}
	scope.active.Store(true)
	defer scope.active.Store(false)
	ctx = context.WithValue(ctx, stepKey{}, scope)
	var opened []Provider
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupDeadline)
		defer cancel()
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		for _, p := range opened {
			var cleanupErr error
			if j, ok := p.(*Jujutsu); ok && CloneQuiescence(j.Clone).Running > 0 {
				// A command the step started is still running. The checkpoint
				// stays for the Recover that runs once the command has exited.
				cleanupErr = errors.New("a command started by the step is still running")
			} else if ctx.Err() != nil {
				_, cleanupErr = p.Recover(cleanup)
			} else {
				cleanupErr = p.Settle(cleanup, operation)
			}
			if cleanupErr != nil {
				err = errors.Join(err, quarantine(p, cleanupErr))
			}
		}
	}()
	for _, p := range checkpoints {
		recovery, cancel := context.WithTimeout(ctx, cleanupDeadline)
		_, recoverErr := p.Recover(recovery)
		cancel()
		if recoverErr != nil {
			return quarantine(p, recoverErr)
		}
		// A checkpoint write may fail after its rename. Include it in
		// cleanup even when Checkpoint reports that durability error.
		opened = append(opened, p)
		if err = p.Checkpoint(ctx, operation, at); err != nil {
			return err
		}
	}
	return step(ctx)
}

// quarantine marks the clone of p as needing recovery and wraps the failure
// that left it so. Its checkpoint stays on disk.
func quarantine(p Provider, err error) error {
	var clone string
	if j, ok := p.(*Jujutsu); ok {
		clone = cloneKey(j.Clone)
		cloneLockOf(clone).markRecovery(true)
	}
	var already *RecoveryError
	if errors.As(err, &already) {
		return err
	}
	return &RecoveryError{Clone: clone, Err: err}
}

type operationKey struct{}
type stepKey struct{}
type stepScope struct {
	operation string
	active    atomic.Bool
}

// WithOperation identifies an operation's scratch worktrees. Include the
// workstream when using operation names that are local to a workstream.
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, operationKey{}, operation)
}

// ReplayName names a scratch worktree without exposing operation text as a path.
func ReplayName(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return fmt.Sprintf(".replay-%x", sum[:16])
}

func networkUnlocked(ctx context.Context, clone string) error {
	if holdsClone(ctx, cloneKey(clone)) {
		return errors.New("network operation inside a clone-locked local step")
	}
	return nil
}
