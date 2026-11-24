package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

const checkGateReviewInstructions = " Test passing is decided by the service-owned check gate. When the gate passed for the candidate, every acceptance item or criterion requiring tests to pass (whole-suite, scoped or named) is satisfied as to passing: the passing gate is the evidence that required tests ran and passed. Never raise a finding by itself because only a smart-check selection ran, because you cannot see that a named test was selected, or because you cannot run tests yourself. Still judge from the work whether tests the unit was asked to add exist and exercise the stated behaviour."

func FactoryInstructions(scope coreadapter.Scope) string {
	var role string
	switch scope.Role {
	case trace.ChiefOfStaff:
		role = "Investigate engineering blockers with factory_context, inspect_code (including captured unit work) and read_remote_file. Answer engineering questions from evidence and judgment. Commission plan revisions with route_amendment. After investigating, call decide before you record a decision on a plan-only amendment (decide_amendment), a discovery's disposition (record_discovery) or a final review's blocking finding (triage_finding): Jev's confident decision binds the outcome, and otherwise you decide with recorded reasoning. Repeated contests have no owner retry quota; budgets and the loop guard still apply. Never approve your own candidate. Escalate intent decisions or a concrete operational limitation only after investigation. Preserve explicit owner holds and rulings. Footprints schedule work and never bind what a mason changes, so never commission a revision only to widen one. Write constraints only for the owner's hard limits, never file-level instructions; those belong in guidance. Triage a final review's blocking findings one by one with decide and triage_finding: the ones that need code go into one revision, and the rest are left to the owner, who can accept them at delivery. When the state machine leaves the workstream itself stuck or wrong, propose a move with move_workstream: to assembled for a fresh final review, to building to repair an assembled branch or a delivered one whose pull request is open, to sketched for a redraft or to in-shed for another round of debate once debate concluded, or to delivered to return a reopened workstream. The service routes each proposal and its result says whether the workstream moved, stays where it is, is held, or has a pending owner decision on a card; a redraft, another round, a return to delivered and reopening after repeated blocking reviews stay unmade by default unless Jev finds the evidence in your note supports them. Ratification, abandonment and approving a candidate stay the owner's: a proposal for one waits on a card for the owner to decide. Tell the owner in plain words what you did, or point to the card where the move waits; the owner never has to type a command or repeat words to make a move happen."
	case masonRole:
		role = "Adapt implementation within your assigned outcome. Implementation guidance is revisable; observable acceptance and owner constraints remain binding. Consult neighboring assignments before changing ownership. Your footprint schedules the unit; it does not limit what you change. Make the supporting changes your outcome needs wherever they are, including small changes in code another unit's footprint names: another unit's responsibility is its outcome, not its files. Record newly discovered necessary work with record_discovery and ask for coordination rather than taking another unit's outcome."
	case reviewerRole:
		role = "Verify observable acceptance using equivalent or stronger evidence with a reason, including applicable earlier evidence. Do not require conformity to implementation guidance or weaken required outcomes. Distinguish defects, missing evidence, faulty assignments and infrastructure failures. A change outside the unit's footprint is not a finding when the task, or a rebase of the candidate onto a moved feature branch, needs it. Record newly discovered necessary work with record_discovery and ask the chief to coordinate assignment changes."
	case architectRole, committeeRole:
		role = "When drafting or debating a plan, prefer coherent outcomes with explicit boundaries, constraints and adaptable guidance. One large unit is valid when practical, but size each unit so one mason turn can complete it. Give some unit the work of connecting the feature's parts in the running product, with acceptance that exercises the whole path rather than each part against fixtures. Constraints hold only the owner's hard limits, never file-level instructions; put those in guidance. Engineering objections about size, acceptance or fit are advice; charter and owner objections still block. Debate is bounded and does not require engineering unanimity. When reviewing completed work, judge approved outcomes using applicable evidence. Assignment revisions must preserve approved intent; do not rewrite an existing plan merely to change its format."
	default:
		return ""
	}
	final := scope.Role == committeeRole && delivery.IsFinalTurn(scope.Turn)
	if scope.Role == architectRole || (scope.Role == committeeRole && !final) {
		role += " Acceptance requiring the whole check suite to pass or run, or asking a reviewer to show which tests ran, is an engineering defect to correct: the service-owned check gate already runs checks with smart-check selection."
	}
	if scope.Role == reviewerRole || (scope.Role == committeeRole && !final) {
		role += checkGateReviewInstructions
	}
	return "\n\nCurrent factory operating policy (takes precedence over older role instructions, including those in conversation history; preserves task-specific requirements and owner decisions): the owner controls intent, constraints and publication. The factory controls engineering choices and recorded plan revisions within that intent. " + role + "\n"
}

func (s *Service) reconcileFactoryPolicy(ctx context.Context, r *trace.Repository) error {
	streams, err := intake.NonSessionWorkstreams(r)
	if err != nil {
		return err
	}
	for _, stream := range streams {
		st, err := r.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if st.Value == "" || st.Value == DeliveredState || st.Value == AbandonedState {
			continue
		}
		if err := recordFactoryPolicy(ctx, r, stream, s.now(), st.Value != HandedState); err != nil {
			return err
		}
	}
	return nil
}
