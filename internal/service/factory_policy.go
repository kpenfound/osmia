package service

import (
	"context"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// policyRule is one rule of the operating policy with the reason it exists.
type policyRule struct{ text, reason string }

func policySection(title string, rules ...policyRule) string {
	var b strings.Builder
	b.WriteString("\n### " + title + "\n")
	for _, r := range rules {
		b.WriteString("- " + r.text + " Reason: " + r.reason + "\n")
	}
	return b.String()
}

// hasPolicy reports whether a role is given an operating policy.
func hasPolicy(role string) bool {
	return slices.Contains([]string{trace.Manager, masonRole, reviewerRole, architectRole, committeeRole}, role)
}

// FactoryInstructions is the operating policy of a feature workstream's turn.
// It is FactoryInstructionsFor with the feature kind.
func FactoryInstructions(scope coreadapter.Scope) string {
	feature, _ := kindreg.Lookup(kindreg.Feature)
	return FactoryInstructionsFor(scope, feature)
}

// FactoryInstructionsFor is the operating policy of a turn of the given
// workstream kind, written as plain sections of rules that each carry their
// reason. A rule about units, plans or unit sizing is stated only for a kind
// whose flags and documents say it has them, so a Hill Climb turn reads none.
// A role with no policy gets the empty string.
func FactoryInstructionsFor(scope coreadapter.Scope, kind kindreg.Kind) string {
	units := kind.Flags.Units
	planned := kind.Documents.Planned()
	final := scope.Role == committeeRole && delivery.IsFinalTurn(scope.Turn)

	authority := policyRule{"The owner controls intent, constraints and publication. The factory controls engineering choices within that intent.", "the owner answers for what the project becomes and what ships, and engineering detail inside what the owner ratified should not wait on the owner."}
	if planned {
		authority.text = "The owner controls intent, constraints and publication. The factory controls engineering choices and recorded plan revisions within that intent."
	}
	if !hasPolicy(scope.Role) {
		return ""
	}
	var rules []string
	switch scope.Role {
	case trace.Manager:
		rules = append(rules, chiefPolicy(units, planned)...)
	case masonRole:
		rules = append(rules, masonPolicy(units)...)
	case reviewerRole:
		rules = append(rules, reviewerPolicy(kind)...)
	case architectRole, committeeRole:
		rules = append(rules, planningPolicy(scope.Role, final, units, planned)...)
	}
	return "\n\n## Operating policy\nThis policy adds to your task. It never overrides a task-specific requirement or an owner decision." + policySection("Authority", authority) + strings.Join(rules, "") + "\n"
}

func chiefPolicy(units, planned bool) []string {
	commission := policyRule{"Commission climb-spec revisions with route_amendment.", "the architect drafts against the recorded climb spec and the committee examines the draft before it is decided."}
	decided := "a discovery's disposition (record_discovery) or a final review's blocking finding (triage_finding)"
	if planned {
		commission = policyRule{"Commission plan revisions with route_amendment.", "the architect drafts against the recorded spec and plan and the committee examines the draft before it is decided."}
		decided = "a plan-only amendment (decide_amendment), " + decided
	}
	investigate := policyRule{"Investigate engineering blockers with factory_context, inspect_code (including captured unit work) and read_remote_file, then answer engineering questions from that evidence and your judgment.", "others build on your answer, and it has to rest on records they can check."}
	decide := policyRule{"After investigating, call decide before you record a decision on " + decided + ". Jev's confident decision binds the outcome; otherwise you decide, with your reasoning recorded.", "the outcome tools refuse a subject you have not called decide for, and the owner delegated these engineering calls to Jev first."}
	contest := policyRule{"Contest a decision again when new evidence calls for it: repeated contests have no owner retry quota, though budgets and the loop guard still apply.", "new evidence should not wait for the owner, and the budgets keep the cost bounded."}
	escalate := policyRule{"Escalate intent decisions or a concrete operational limitation only after investigating them. Preserve explicit owner holds and rulings.", "the owner's attention is for what only the owner can decide, and a hold or ruling is the owner's decision."}
	gates := policyRule{"Ratification, abandonment and approving a candidate stay the owner's, including a candidate produced by revisions you commissioned. A proposal for one waits on a card for the owner to decide.", "these are the owner's gates, and approving work you commissioned would be reviewing yourself."}
	out := []string{policySection("Chief of staff: investigating and deciding", investigate, commission, decide, contest, escalate, gates)}
	if units {
		out = append(out, policySection("Chief of staff: footprints and constraints",
			policyRule{"Footprints schedule work and never bind what a mason changes, so do not commission a revision only to widen one.", "a mason already changes whatever its outcome needs, so a revision that only widens a footprint costs a round and changes nothing."},
			policyRule{"Write constraints only for the owner's hard limits, and put file-level instructions in guidance.", "constraints bind acceptance, while guidance can be revised as the work shows what is needed."}))
	}
	out = append(out, policySection("Chief of staff: final review and moving a workstream",
		policyRule{"Triage a final review's blocking findings one by one with decide and triage_finding: the ones that need code go into one revision, and the rest are left to the owner, who can accept them at delivery.", "one revision per review bounds the churn, and accepting a finding is the owner's call."},
		policyRule{"When the state machine leaves the workstream itself stuck or wrong, propose a move with move_workstream: to assembled for a fresh final review, to building to repair an assembled branch or a delivered one whose pull request is open, to sketched for a redraft or to in-shed for another round of debate once debate concluded, or to delivered to return a reopened workstream.", "the service owns state transitions, so you propose and it routes."},
		policyRule{"The service routes each proposal and its result says whether the workstream moved, stays where it is, is held, or has a pending owner decision on a card. A redraft, another round, a return to delivered and reopening after repeated blocking reviews stay unmade by default unless Jev finds the evidence in your note supports them.", "those moves discard or reopen finished work, so they need agreement on your evidence."},
		policyRule{"Tell the owner in plain words what you did, or point to the card where the move waits. The owner never has to type a command or repeat words to make a move happen.", "the owner decides from what you report, and should not need the factory's vocabulary."}))
	return out
}

func masonPolicy(units bool) []string {
	rules := []policyRule{
		{"Adapt implementation within your assigned outcome. Implementation guidance is revisable; observable acceptance and owner constraints remain binding.", "guidance records one way to reach the outcome, while acceptance and constraints record what the owner needs."},
		{"Your footprint schedules the unit; it does not limit what you change. Make the supporting changes your outcome needs wherever they are, including small changes in code another unit's footprint names: another unit's responsibility is its outcome, not its files.", "a footprint predicts where work lands, and a needed change outside it is still part of your outcome."},
		{"Record newly discovered necessary work with record_discovery and ask for coordination rather than taking another unit's outcome.", "work another unit owns needs the chief of staff to coordinate it, or two units end up changing the same outcome."},
	}
	if units {
		rules = slices.Insert(rules, 1, policyRule{"Consult neighboring assignments before changing ownership.", "the plan assigns each outcome to one unit, and a change of ownership is a plan change."})
	}
	return []string{policySection("Mason", rules...)}
}

func reviewerPolicy(kind kindreg.Kind) []string {
	rules := []policyRule{
		{"Verify observable acceptance using equivalent or stronger evidence with a reason, including applicable earlier evidence. Do not require conformity to implementation guidance or weaken required outcomes.", "guidance is one way to reach the outcome and acceptance is what the owner ratified."},
		{"Distinguish defects, missing evidence, faulty assignments and infrastructure failures.", "each has a different fix and a different owner."},
	}
	footprint := "A change outside the unit's footprint is not a finding when the task needs it."
	if kind.Flags.UpstreamDrift {
		footprint = "A change outside the unit's footprint is not a finding when the task, or a rebase of the candidate onto a moved feature branch, needs it."
	}
	rules = append(rules,
		policyRule{footprint, "a footprint schedules the unit and never limits what the outcome may change."},
		policyRule{"Record newly discovered necessary work with record_discovery and ask the chief to coordinate assignment changes.", "a reviewer finds work but does not reassign it."})
	return []string{policySection("Reviewer", rules...), policySection("Reviewer: the check gate", policyRule{strings.TrimSpace(execution.CheckGateReviewInstructions), "the service-owned gate, not a reviewer's own run, decides test passing."})}
}

func planningPolicy(role string, final, units, planned bool) []string {
	var out []string
	if units {
		sizing := []policyRule{
			{"When drafting or debating a plan, prefer coherent outcomes with explicit boundaries, constraints and adaptable guidance.", "a mason adapts within an outcome, boundaries say where another unit's outcome begins, and guidance can be revised."},
			{"One large unit is valid when practical, but size each unit so one mason turn can complete it.", "a unit is built by one mason turn, and a unit that cannot fit it stalls."},
			{"Give some unit the work of connecting the feature's parts in the running product, with acceptance that exercises the whole path rather than each part against fixtures.", "parts that pass against fixtures can still fail together."},
			{"Constraints hold only the owner's hard limits, never file-level instructions; put those in guidance.", "constraints bind acceptance, while guidance can be revised as the work shows what is needed."},
		}
		out = append(out, policySection("Plans and units", sizing...))
	}
	debate := policyRule{"Engineering objections are advice; charter and owner objections still block. Debate is bounded and does not require engineering unanimity.", "engineering disagreement should not stop work the owner's rules allow, and a round that never ends delivers nothing."}
	if units {
		debate.text = "Engineering objections about size, acceptance or fit are advice; charter and owner objections still block. Debate is bounded and does not require engineering unanimity."
	}
	review := policyRule{"When reviewing completed work, judge approved outcomes using applicable evidence.", "the approved outcomes are what the owner ratified, and the evidence is what shows them."}
	revise := policyRule{"A revision must preserve approved intent.", "the owner ratified that intent."}
	if planned {
		revise.text = "Assignment revisions must preserve approved intent; do not rewrite an existing plan merely to change its format."
		revise.reason = "the owner ratified that intent, and a rewrite for format alone risks losing assignments the owner approved."
	}
	out = append(out, policySection("Debate and revisions", debate, review, revise))
	if units && !final {
		out = append(out, policySection("Acceptance and the check gate", policyRule{"Acceptance requiring the whole check suite to pass or run, or asking a reviewer to show which tests ran, is an engineering defect to correct: the service-owned check gate already runs checks with smart-check selection.", "the gate decides test passing, so such an item cannot be met by the unit's own work and would block it on evidence no agent can produce."}))
		if role == committeeRole {
			out = append(out, policySection("Committee: reviewing evidence of passing tests", policyRule{strings.TrimSpace(execution.CheckGateReviewInstructions), "the service-owned gate, not a reviewer's own run, decides test passing."}))
		}
	}
	return out
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
		if st.Value == "" || intake.Finished(st.Value) {
			continue
		}
		if err := recordFactoryPolicy(ctx, r, stream, s.now(), st.Value != HandedState); err != nil {
			return err
		}
	}
	return nil
}
