package service

import (
	"context"
	"errors"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) delivery() *delivery.Controller {
	return s.deliveryFor(s.current())
}

// deliveryFor binds the delivery controller to the configuration of its
// operation: a running operation keeps its Dagger Cloud rerun runner when
// reload installs new stages.
func (s *Service) deliveryFor(cfg *config.Config) *delivery.Controller {
	return delivery.New(delivery.Dependencies{
		Now: s.now, About: s.about, Current: s.current, Effective: s.effective,
		ConversationTrace: s.conversationTrace, Execution: s.execution, ResolveBase: s.resolveBase,
		EnsureCommittee: func(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, n int) error {
			return (&intake.Debate{S: s.intake().DebateDependencies(), Repository: repo}).EnsureCommittee(ctx, stream, n)
		},
		TrackTurn: s.turns.add, AgentProfile: s.agentProfile, RoleExecution: s.roleExecution,
		ContextFor: s.contextFor, Stoppable: s.stoppable, ThreadRunner: s.threadRunner,
		Held: s.held, ContinueStopped: s.continueStopped, MemoryTurns: memoryTurns, Step: s.step,
		Committee: s.options.Committee, PullRequests: s.options.PullRequests, Jev: s.jev, Router: routerPort{s},
		CloudStatuses: s.options.CloudStatuses, CloudRerun: s.configuredRerun(cfg), CloudTrace: s.configuredTrace(cfg), PostPushWait: s.options.FinalChecksWait,
		ChangeHeld: changeHeld, ChangeRepositoryHeld: changeRepositoryHeld,
		UpstreamDeliveryPending: upstreamDeliveryPending, UnresolvedWork: unresolvedWork, ReopenHeld: reopenHeld,
		BaseObservationAt: func(r *trace.Repository, w config.WorkstreamID) (bool, int, error) {
			observed, revision, err := baseObservationAt(r, w)
			return observed.Unavailable, revision, err
		},
		WorkgroupMergeOrderResolved: workgroupMergeOrderResolved, WorkgroupMergeOrderWaived: workgroupMergeOrderWaived, ReviewMoves: reviewMoves,
		DependentBaseChanged: dependentBaseChanged, BranchBase: branchBase, DriftRebaseNotes: driftRebaseNotes,
		WorkgroupEdgePending: workgroupEdgePending, PendingMergeOrderReferences: pendingMergeOrderReferences,
		EdgePublicationWait: edgePublicationWait, UnresolvedMergeOrderEdge: unresolvedMergeOrderEdge,
		CanonicalPublication: canonicalPublication, ChiefThread: s.chiefThread, RequestRebase: s.requestRebase,
	})
}

// requestRebase asks for the drift rebase of the workstream's branch onto its
// upstream's base branch now. A workstream that takes none (it is paused,
// held or has one in flight) is left to the schedule.
func (s *Service) requestRebase(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) error {
	_, api := s.lifecycle().RebaseWorkstream(ctx, string(stream))
	if api != nil && api.Code != delivery.Conflict {
		return errors.New(api.Message)
	}
	return nil
}
