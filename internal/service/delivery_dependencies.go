package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) delivery() *delivery.Controller {
	return s.deliveryWithChecks(s.configuredChecks(s.current()))
}

func (s *Service) deliveryWithChecks(checks ReviewChecks) *delivery.Controller {
	return delivery.New(delivery.Dependencies{
		Now: s.now, About: s.about, Current: s.current, Effective: s.effective,
		ConversationTrace: s.conversationTrace, Execution: s.execution, ResolveBase: s.resolveBase,
		EnsureCommittee: func(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, n int) error {
			return (&intake.Debate{S: s.intake().DebateDependencies(), Repository: repo}).EnsureCommittee(ctx, stream, n)
		},
		TrackTurn: s.turns.add, AgentProfile: s.agentProfile, RoleExecution: s.roleExecution,
		ContextFor: s.contextFor, Stoppable: s.stoppable, ThreadRunner: s.threadRunner,
		Held: s.held, ContinueStopped: s.continueStopped, MemoryTurns: memoryTurns, Step: s.step,
		Committee: s.options.Committee, Checks: checks, PullRequests: s.options.PullRequests, Jev: s.jev,
		AcquireReviewCheckSlot: func(ctx context.Context, p config.ProjectID, w config.WorkstreamID, review int, operation string) (func(), error) {
			return s.acquireReviewCheckSlot(ctx, CheckSlotRun{Project: p, Workstream: w, Review: review}, operation)
		},
		ChangeHeld: changeHeld, ChangeRepositoryHeld: changeRepositoryHeld,
		UpstreamDeliveryPending: upstreamDeliveryPending, UnresolvedWork: unresolvedWork, ReopenHeld: reopenHeld,
		BaseObservationAt: func(r *trace.Repository, w config.WorkstreamID) (bool, int, error) {
			observed, revision, err := baseObservationAt(r, w)
			return observed.Unavailable, revision, err
		},
		WorkgroupMergeOrderResolved: workgroupMergeOrderResolved, ReviewMoves: reviewMoves,
		DependentBaseChanged: dependentBaseChanged, BranchBase: branchBase, DriftRebaseNotes: driftRebaseNotes,
		WorkgroupEdgePending: workgroupEdgePending, PendingMergeOrderReferences: pendingMergeOrderReferences,
		EdgePublicationWait: edgePublicationWait, UnresolvedMergeOrderEdge: unresolvedMergeOrderEdge,
		CanonicalPublication: canonicalPublication,
	})
}
