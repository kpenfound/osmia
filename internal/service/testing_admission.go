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

// DrainConversations stops conversation paths and waits for their iteration in
// progress, so none writes a trace after it returns.
func (a TestAccess) DrainConversations() {
	_, projects := a.s.runtimes()
	for _, p := range projects {
		p.conversations.Quiesce()
	}
}

// ConversationHolds reads admission for one project's conversation path.
func (a TestAccess) ConversationHolds(project config.ProjectID, repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
	_, projects := a.s.runtimes()
	for _, p := range projects {
		if p.id == project {
			return p.conversations.holds(nil, repo, stream, op)
		}
	}
	panic("fixture project missing")
}

// SessionTrace resolves a session through the production lookup.
func (a TestAccess) SessionTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return a.s.sessionTrace(raw)
}

// ManagerEventsProfile returns the profile selector used by background turns.
func (a TestAccess) ManagerEventsProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return a.s.managerEventsProfile(cfg)
}

// ManagerOverride reads the owner's effective conversation profile.
func (a TestAccess) ManagerOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	return a.s.managerOverride(cfg)
}

// DesignerOverride reads the profile design turns run on.
func (a TestAccess) DesignerOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	return a.s.designerOverride(cfg)
}

var TestOperationClass = operationClass
var TestConversationOperation = conversationOperation
var TestEnsureSessionThread = ensureSessionThread

var TestStagePriority = stagePriority

// SetRuntimeStore binds a stopped fixture's runtime state.
func (a TestAccess) SetRuntimeStore(store *runtime.Store) { a.s.store = store }
