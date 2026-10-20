package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/ghlog"
)

var cloneLocks sync.Map

type cloneLock struct{ sync.Mutex }
type lockKey struct{}
type lockHold struct {
	clone  string
	active atomic.Bool
	parent *lockHold
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

func holdsClone(ctx context.Context, clone string) bool {
	for hold, _ := ctx.Value(lockKey{}).(*lockHold); hold != nil; hold = hold.parent {
		if hold.clone == clone && hold.active.Load() {
			return true
		}
	}
	return false
}

// lockClone lends ownership to synchronous nested calls using the returned
// context. Callers must not share that context with concurrent goroutines.
func lockClone(ctx context.Context, path string) (context.Context, func()) {
	key := cloneKey(path)
	if holdsClone(ctx, key) {
		return ctx, func() {}
	}
	value, _ := cloneLocks.LoadOrStore(key, &cloneLock{})
	lock := value.(*cloneLock)
	start := time.Now()
	lock.Lock()
	if waited := time.Since(start); waited >= time.Second {
		scope := ghlog.ScopeOf(ctx)
		operation, _ := ctx.Value(operationKey{}).(string)
		ghlog.Logger(ctx).Warn("workspace clone lock wait", "project", scope.Project, "workstream", scope.Workstream, "operation", operation, "lock", "clone", "clone", key, "duration", waited)
	}
	parent, _ := ctx.Value(lockKey{}).(*lockHold)
	hold := &lockHold{clone: key, parent: parent}
	hold.active.Store(true)
	return context.WithValue(ctx, lockKey{}, hold), func() { hold.active.Store(false); lock.Unlock() }
}

// LocalStep checkpoints a synchronous local step of an operation. Jujutsu
// providers sharing a clone hold one lock from checkpoint through settlement,
// including the callback's durable progress write. Git callbacks run unlocked;
// their worktree mutations lock themselves. Fetch, push and external calls
// belong outside the callback. The callback must use its supplied context.
//
// Cancellation restores the interrupted step before releasing ownership. A
// process crash leaves its checkpoint for Recover at service startup. Steps
// must record progress or expose an inspectable effect before returning.
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
		ctx, unlock = lockClone(ctx, clone)
		defer unlock()
	}
	scope := &stepScope{operation: operation}
	scope.active.Store(true)
	defer scope.active.Store(false)
	ctx = context.WithValue(ctx, stepKey{}, scope)
	var opened []Provider
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		for _, p := range opened {
			if ctx.Err() != nil {
				_, restoreErr := p.Recover(cleanup)
				err = errors.Join(err, restoreErr)
			} else {
				err = errors.Join(err, p.Settle(cleanup, operation))
			}
		}
	}()
	for _, p := range checkpoints {
		if _, err = p.Recover(ctx); err != nil {
			return err
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
