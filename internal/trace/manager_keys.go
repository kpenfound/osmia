package trace

// ManagerKey is a stored name of the manager role: the short form it carries
// in the keys of its durable decision documents and outbox events. The shed
// decisions of a round live at shed/<round>/chief.json with a record ID derived
// from it, a contest decision at units/<unit>/chief-<event ID>.json with an
// event ID derived from it, and the manager's notices take their event IDs
// from it. It is also the by of the manager's follow-up records and climb
// steering notes. Saved traces hold these keys and their readers find
// decisions by them, so the value must not change.
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

// ManagerRulerName is a stored name of the manager role: the By of the unit
// and workstream move documents and of the contest rulings the manager made.
// Saved traces hold it and the readers of those records match it:
// conversation history (internal/service/conversation.go) lists the manager's
// unit and workstream moves by it, the trace walk (contestDecider in
// internal/service/trace_walk.go) reports a contest ruling that holds it as
// the manager's, and DisplayRuler, which execution.ContestRuler applies to a
// saved ruling, shows it as the manager. The value must not change.
const ManagerRulerName = "chief of staff"

// DisplayRuler maps the By of a saved move or ruling to the name shown
// outside the service: the manager's stored ruler name becomes ManagerDisplay
// and every other value is returned as it is.
func DisplayRuler(by string) string {
	if by == ManagerRulerName {
		return ManagerDisplay
	}
	return by
}
