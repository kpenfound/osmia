package service

import (
	"context"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const factoryPolicySubject = "factory-policy"
const factoryPolicyVersion = "adaptive"

// acceptanceDraftInstructions applies to every architect submission path.
const acceptanceDraftInstructions = "\n\nAcceptance states observable behaviours and may name tests the unit adds as evidence. Never require a whole-suite check pass or run. There is no need to list existing tests to keep passing: the factory's service-owned check gate catches regressions. The factory runs and selects checks itself, using smart-check selection. A passing service-owned check gate is the evidence that required tests ran and passed."

// checkGateReviewInstructions gives both reviewer roles the same test evidence rule.
const checkGateReviewInstructions = " Test passing is decided by the service-owned check gate. When the gate passed for the candidate, every acceptance item or criterion requiring tests to pass (whole-suite, scoped or named) is satisfied as to passing: the passing gate is the evidence that required tests ran and passed. Never raise a finding by itself because only a smart-check selection ran, because you cannot see that a named test was selected, or because you cannot run tests yourself. Still judge from the work whether tests the unit was asked to add exist and exercise the stated behaviour."

// factoryInstructions applies current authority to queued and resumed turns without
// rewriting their original requests or changing task-specific owner requirements.
func factoryInstructions(scope coreadapter.Scope) string {
	var role string
	switch scope.Role {
	case trace.ChiefOfStaff:
		role = "Investigate engineering blockers with factory_context, inspect_code (including captured unit work) and read_remote_file. Answer engineering questions from evidence and judgment. Commission plan revisions with route_amendment and decide plan-only amendments yourself with recorded reasoning. Coordinate record_discovery dispositions. Repeated contests have no owner retry quota; budgets and the loop guard still apply. Never approve your own candidate. Escalate intent decisions or a concrete operational limitation only after investigation. Preserve explicit owner holds and rulings. Footprints schedule work and never bind what a mason changes, so never commission a revision only to widen one. Write constraints only for the owner's hard limits, never file-level instructions; those belong in guidance. Triage a final review's findings: group the blocking ones that need code into one revision, and leave the rest to the owner, who can accept them at delivery. When the state machine leaves the workstream itself stuck or wrong, move it with move_workstream: to assembled for a fresh final review, or to building to repair an assembled branch or a delivered one whose pull request is open; the owner's gates stay the owner's."
	case masonRole:
		role = "Adapt implementation within your assigned outcome. Implementation guidance is revisable; observable acceptance and owner constraints remain binding. Consult neighboring assignments before changing ownership. Your footprint schedules the unit; it does not limit what you change. Make the supporting changes your outcome needs wherever they are, including small changes in code another unit's footprint names: another unit's responsibility is its outcome, not its files. Record newly discovered necessary work with record_discovery and ask for coordination rather than taking another unit's outcome."
	case reviewerRole:
		role = "Verify observable acceptance using equivalent or stronger evidence with a reason, including applicable earlier evidence. Do not require conformity to implementation guidance or weaken required outcomes. Distinguish defects, missing evidence, faulty assignments and infrastructure failures. A change outside the unit's footprint is not a finding when the task, or a rebase of the candidate onto a moved feature branch, needs it. Record newly discovered necessary work with record_discovery and ask the chief to coordinate assignment changes."
	case architectRole, committeeRole:
		role = "When drafting or debating a plan, prefer coherent outcomes with explicit boundaries, constraints and adaptable guidance. One large unit is valid when practical, but size each unit so one mason turn can complete it. Give some unit the work of connecting the feature's parts in the running product, with acceptance that exercises the whole path rather than each part against fixtures. Constraints hold only the owner's hard limits, never file-level instructions; put those in guidance. Engineering objections about size, acceptance or fit are advice; charter and owner objections still block. Debate is bounded and does not require engineering unanimity. When reviewing completed work, judge approved outcomes using applicable evidence. Assignment revisions must preserve approved intent; do not rewrite an existing plan merely to change its format."
	default:
		return ""
	}
	if scope.Role == architectRole || scope.Role == committeeRole {
		role += " Acceptance requiring the whole check suite to pass or run, or asking a reviewer to show which tests ran, is an engineering defect to correct: the service-owned check gate already runs checks with smart-check selection."
	}
	if scope.Role == reviewerRole || scope.Role == committeeRole {
		role += checkGateReviewInstructions
	}
	return "\n\nCurrent factory operating policy (takes precedence over older role instructions, including those in conversation history; preserves task-specific requirements and owner decisions): the owner controls intent, constraints and publication. The factory controls engineering choices and recorded plan revisions within that intent. " + role + "\n"
}

// recordFactoryPolicy commits adoption and its recovery notice together. Repeating
// reconciliation never loses the notice or creates another one after acknowledgment.
func recordFactoryPolicy(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, at time.Time, revisit bool) error {
	st, err := r.Workflow(stream, factoryPolicySubject)
	if err != nil || st.Value == factoryPolicyVersion {
		return err
	}
	if st.Value != "" {
		return fmt.Errorf("unsupported factory policy %q", st.Value)
	}
	id := factoryPolicySubject + "-" + factoryPolicyVersion
	tx := trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: serviceActor, Cause: factoryPolicySubject},
		Subject: factoryPolicySubject, To: factoryPolicyVersion, Reason: "Current factory authority applies to subsequent turns; sealed intent, assignments and owner decisions are preserved.",
	}}
	if revisit {
		tx.Events = []trace.Event{trace.Notice(id, "chief", "Reassess this in-flight workstream under current factory authority. Read factory_context and current activity for unresolved questions, contested units and pending amendments, including blockers whose earlier notices were already acknowledged. Investigate and resolve engineering decisions within approved intent; commission a plan revision where assignments or acceptance methods need adaptation. Do not automatically rewrite plans, reopen completed work, clear explicit owner holds or substitute for an owner decision. Existing escalations remain recorded. Ratification and publication still require the owner.")}
	}
	_, err = r.Transact(ctx, tx)
	return err
}

func (s *Service) reconcileFactoryPolicy(ctx context.Context, r *trace.Repository) error {
	streams, err := r.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if stream == librarianWorkstream(r.Project()) {
			continue
		}
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
