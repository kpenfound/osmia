package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// Pool exposes the shared role pool for admission assertions.
func (a TestAccess) Pool() *scheduler.Shared { return a.s.pool }

// ResizeCheckSlots changes the fixture's in-memory check limit.
func (a TestAccess) ResizeCheckSlots(limit int) { a.s.checkSlots.resize(limit) }

// AcquireCheckSlot reserves capacity through the production admission path.
func (a TestAccess) AcquireCheckSlot(ctx context.Context, run CheckSlotRun) (func(), error) {
	return a.s.checkSlots.acquire(ctx, run)
}

// AcquireReviewCheckSlot includes the review's committee reservation.
func (a TestAccess) AcquireReviewCheckSlot(ctx context.Context, run CheckSlotRun, operation string) (func(), error) {
	return a.s.acquireReviewCheckSlot(ctx, run, operation)
}

// HasReviewReservation reports whether an operation still owns a reservation.
func (a TestAccess) HasReviewReservation(operation string) bool {
	_, found := a.s.reviewSlots.Load(operation)
	return found
}

// GitOperationLimit returns the composed repository admission policy.
func (a TestAccess) GitOperationLimit(repo *trace.Repository, other func(coreadapter.Operation) (func(), bool)) func(coreadapter.Operation) (func(), bool) {
	return a.s.gitOperationLimit(repo, other)
}

// TryGitOperation reserves the same capacity background observers use.
func (a TestAccess) TryGitOperation(repo *trace.Repository) (func(), bool) {
	return a.s.tryGitOperation(repo)
}

// WakeConversations wakes background conversation and network observers.
func (a TestAccess) WakeConversations() { a.s.conversationSignal.wake() }

// DrainConversations joins conversation paths before seeding recovery evidence.
func (a TestAccess) DrainConversations() {
	_, projects := a.s.runtimes()
	for _, p := range projects {
		p.conversations.Drain()
	}
}

// ConversationHolds reads admission for one project's conversation path.
func (a TestAccess) ConversationHolds(project config.ProjectID, repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
	_, projects := a.s.runtimes()
	for _, p := range projects {
		if p.id == project {
			return p.conversations.holds(repo, stream, op)
		}
	}
	panic("fixture project missing")
}

// SessionTrace resolves a session through the production lookup.
func (a TestAccess) SessionTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return a.s.sessionTrace(raw)
}

// ChiefEventsProfile returns the profile selector used by background turns.
func (a TestAccess) ChiefEventsProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return a.s.chiefEventsProfile(cfg)
}

// ChiefOverride reads the owner's effective conversation profile.
func (a TestAccess) ChiefOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	return a.s.chiefOverride(cfg)
}

var TestOperationClass = operationClass
var TestConversationOperation = conversationOperation
var TestEnsureSessionThread = ensureSessionThread

var TestStagePriority = stagePriority

// SetRuntimeStore binds a stopped fixture's runtime state.
func (a TestAccess) SetRuntimeStore(store *runtime.Store) { a.s.store = store }
