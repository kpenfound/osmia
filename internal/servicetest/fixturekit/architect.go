package fixturekit

const (
	HandedDesign = "# Resumable uploads\n\nUploads that drop resume where they stopped.\n"
	ValidSpec    = "# Resumable uploads\n\nAn interrupted upload resumes from its last acknowledged chunk. It must not re-send acknowledged chunks.\n\n## Acceptance criteria\n\n1. An interrupted upload resumes from the last acknowledged chunk.\n2. Acknowledged chunks are never sent again.\n"
	ValidPlan    = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
	CyclicPlan = `{"version": 2, "units": [
  {"id": "resume", "task": "Resume from the last chunk.", "acceptance": ["TestResume passes"], "criteria": ["spec#1"], "depends_on": ["dedupe"], "footprint": ["internal.trace"]},
  {"id": "dedupe", "task": "Skip acknowledged chunks.", "acceptance": ["No chunk is sent twice"], "criteria": [], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
)

// ReviewSummary is how a fixture reviewer says it verified the acceptance.
const ReviewSummary = "The candidate does the task and its acceptance holds"
