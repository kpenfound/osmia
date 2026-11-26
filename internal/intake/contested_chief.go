package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
)

// ChiefActor is the chief of staff acting on the owner's behalf.
var ChiefActor = trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}

// ContestGuidance tells the chief of staff how to handle contested units and
// units that are stuck outside a contest. It belongs in the system prompt of
// every chief-of-staff turn.
const ContestGuidance = "## Contested and stuck units\n" +
	"A contested unit is stuck until someone rules on it. You rule first, on the owner's behalf, so that engineering blockers do not wait on the owner. Read units/<unit>/activity.json for the contest, the rulings it takes, the unit's recent turns and anything the service refused. " +
	"Investigate the contest, then call decide with kind contested-ruling, the unit as subject and a short evidence summary: Jev decides how the contest is ruled (review, revise or escalate). When Jev's decision is binding, resolve_contested records only the outcome it picked, and you still write the note. When decide says the ruling is yours, rule as below. " +
	"resolve_contested refuses a contest decide has not been called for, except where you relay the owner's own ruling with owner_decided true.\n\n" +
	"### Ruling\n" +
	"When you are confident a ruling resolves the contest, call resolve_contested with review (the reviewer reviews the candidate again) or revise (the mason revises the unit), and a note that tells the resumed role exactly what to do differently. " +
	"For a repeated contest, investigate with inspect_code and factory_context, commission revised assignments or a bounded investigation with route_amendment, and identify what new evidence justifies retrying. " +
	"When the fix changes approved intent, needs an owner-reserved tradeoff, or requires an operational capability that is not available, call resolve_contested with escalate and a short note for the owner: what is stuck, the finding that matters and what you recommend they rule. " +
	"A contest stays an internal engineering blocker until you escalate it. When the owner rules on a contested unit in a message, call resolve_contested with owner_decided true and the owner's words as the note.\n\n" +
	"### Handing a unit back\n" +
	"Hand a unit back to the role that can resolve what stopped it, not to the stage it stopped in. " +
	"When the service refused its reviews as stale, another review of the same candidate is refused again: move it to implementing so its mason makes a new candidate on the current feature branch, and escalate if its workspace stays behind the feature branch. " +
	"When its checks did not complete, rule review only once the checks can run again, and escalate when they cannot. " +
	"When its mason kept leaving conflict markers, rule revise with a note naming the files and what each side holds. " +
	"When its turns were interrupted again and again, rule review or revise so the work continues, and escalate if the service keeps stopping.\n\n" +
	"### Moving a unit\n" +
	"When a unit is stuck or wrong outside a contest, such as checks that keep failing to complete, a block the service reports, or a stage that has to run again, call move_unit with a note. It submits a proposal that the service routes, and its result says whether the unit moved, stays where it is, is held, or has a pending owner decision on a card. " +
	"Move it to implementing, checking or reviewing; a unit that has not started goes to ready or planned. Independent review stays required, and the service's budget and loop guard bound what recovery spends. " +
	"A unit sitting in a contest you raised or already ruled on stays there by default unless Jev finds the evidence in your note supports leaving, so put the new evidence in the note. " +
	"A candidate is approved only by its independent review or by the owner, so a proposal to approve one waits as a pending owner decision on the recovery card. " +
	"Move a unit to contested to hold it for the owner when you cannot fix it. " +
	"Tell the owner in plain words what you did, or point to the card where the move waits; the owner never has to type a command or repeat words to make a move happen. " +
	"When the owner asks you in a message to move a unit, call move_unit with owner_decided true and the owner's words as the note."

// DriftHandbackGuidance tells the chief of staff how to handle a held drift
// rebase. It belongs in the system prompt of every chief-of-staff turn.
const DriftHandbackGuidance = "## Held drift rebases\nA drift rebase is held when review sent its conflict resolution back too many times, its mason kept leaving conflict markers, or its turns kept being interrupted; drift/rebase.json and the feed say which. When you can say what its drift mason or drift reviewer should do differently, such as a reviewer that misreads an upstream already holding the feature's change, call hand_back_drift with that note: the service asks for another drift rebase, and both receive it. Otherwise leave the held drift rebase to the owner, whose inbox lists it, because another attempt without a changed approach repeats the failure. Record what new evidence or changed approach justifies another attempt; budgets and the loop guard bound recovery. When the owner asks you to hand it back, call hand_back_drift with owner_decided true and the owner's words as the note."
