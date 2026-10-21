package checks

// The statuses of a check run.
const (
	ChecksPassed = "passed"
	ChecksFailed = "failed"
	// ChecksIncomplete is a run that reported no result for its checks: the
	// export, the engine or Dagger failed, or the run timed out.
	ChecksIncomplete = "incomplete"
	// ChecksSuperseded is a run whose candidate was replaced, or whose unit
	// left checking, before it ran.
	ChecksSuperseded = "superseded"
)
