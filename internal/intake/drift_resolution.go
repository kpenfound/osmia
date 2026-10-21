package intake

const (
	// driftMasonAgent is the agent and thread of a workstream's mason that
	// resolves the conflicts of its drift rebases, and driftReviewerAgent
	// the reviewer that reads each resolution. Neither name is one a unit's
	// agent can take.
	DriftMasonAgent    = "drift-mason"
	DriftReviewerAgent = "drift-reviewer"
	// driftsDirectory is the directory under the root holding the
	// resolution workspaces of drift rebases, by project and workstream.
	DriftsDirectory = "drifts"
	// The outcomes drift/rebase.json records while a conflicted drift
	// rebase is resolved: resolved once the replay holds a candidate for
	// review, rejected once a review sent it back to the mason.
	DriftResolved = "resolved"
	DriftRejected = "rejected"
)
