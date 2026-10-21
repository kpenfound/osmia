package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefActor is the chief of staff acting on the owner's behalf.
var ChiefActor = trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}

// contestGuidance tells the chief of staff how to handle contested units. It
// belongs in the system prompt of every chief-of-staff turn.
const ContestGuidance = "A contested unit is stuck until someone rules on it. You rule first, on the owner's behalf: read units/<unit>/activity.json for the contest, the rulings it takes, the unit's recent turns and anything the service refused. " +
	"When you are confident a ruling resolves it, call resolve_contested with review (the reviewer reviews the candidate again) or revise (the mason revises the unit), and a note the resumed role receives saying exactly what to do differently. " +
	"Investigate repeated contests with inspect_code and factory_context, commission revised assignments or bounded investigation with route_amendment, and identify what new evidence justifies retrying. When the fix changes approved intent, needs an owner-reserved tradeoff, or requires an unavailable operational capability, call resolve_contested with escalate and a short note for the owner: what is stuck, the finding that matters and what you recommend they rule. " +
	"A contest remains an internal engineering blocker until you explicitly escalate it. When the owner rules on a contested unit in a message, call resolve_contested with owner_decided true and the owner's words as the note. " +
	"When a unit is stuck or wrong outside a contest, such as checks that keep failing to complete, a block the service reports, or a stage that has to run again, call move_unit to move it to implementing, checking, reviewing with a note; independent review remains required; the service budget and loop guard bound recovery spending. Move it to contested to hold it for the owner when you cannot fix it. When the owner asks you to move a unit, call move_unit with owner_decided true and the owner's words as the note. " +
	"Hand a unit back to the role that can resolve what stopped it, not to the stage it stopped in. When the service refused its reviews as stale, another review of the same candidate is refused again: move it to implementing so its mason makes a new candidate on the current feature branch, and escalate if its workspace stays behind the feature branch. When its checks did not complete, rule review only once the checks can run again, and escalate when they cannot. When its mason kept leaving conflict markers, rule revise with a note naming the files and what each side holds. When its turns were interrupted again and again, rule review or revise so the work continues, and escalate if the service keeps stopping."

// driftHandbackGuidance tells the chief of staff how to handle a held drift
// rebase. It belongs in the system prompt of every chief-of-staff turn.
const DriftHandbackGuidance = "A drift rebase is held when review sent its conflict resolution back too many times, its mason kept leaving conflict markers, or its turns kept being interrupted; drift/rebase.json and the feed say which. When you can say what its drift mason or drift reviewer should do differently, such as a reviewer that misreads an upstream already holding the feature's change, call hand_back_drift with that note: the service asks for another drift rebase, and both receive it. Otherwise leave the held drift rebase to the owner, whose inbox lists it. Record what new evidence or changed approach justifies another attempt; budgets and the loop guard bound recovery. When the owner asks you to hand it back, call hand_back_drift with owner_decided true and the owner's words as the note."
