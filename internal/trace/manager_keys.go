package trace

// ManagerKey is a stored name of the manager role: the short form it carries
// in the keys of its durable decision documents and outbox events. The shed
// decisions of a round live at shed/<round>/chief.json with a record ID derived
// from it, a contest decision at units/<unit>/chief-<event ID>.json with an
// event ID derived from it, and the manager's notices take their event IDs
// from it. Saved traces hold these keys and their readers find decisions by
// them, so the value must not change.
const ManagerKey = "chief"

// ContestDecisionID is the event ID, and so the record ID, of the manager's
// decision on a contest.
func ContestDecisionID(contest string) string { return EventID(contest, ManagerKey) }

// ContestDecisionPrefix is the path prefix of the manager's contest decision
// documents of the unit with the given subject.
func ContestDecisionPrefix(subject string) string {
	return "units/" + subject + "/" + ManagerKey + "-"
}

// ContestDecisionPath is the workstream path of the manager's decision on a
// contest of the unit with the given subject.
func ContestDecisionPath(subject, contest string) string {
	return ContestDecisionPrefix(subject) + ContestDecisionID(contest) + ".json"
}
