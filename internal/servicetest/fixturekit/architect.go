package fixturekit

const (
	HandedDesign = "# Resumable uploads\n\nUploads that drop resume where they stopped.\n"
	ValidSpec    = "# Resumable uploads\n\nAn interrupted upload resumes from its last acknowledged chunk. It must not re-send acknowledged chunks.\n\n## Acceptance criteria\n\n1. An interrupted upload resumes from the last acknowledged chunk.\n2. Acknowledged chunks are never sent again.\n"
	ValidPlan    = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
	// CyclicPlan has a dependency cycle and leaves criterion 2 unserved.
	CyclicPlan = `{"version": 2, "units": [
  {"id": "resume", "task": "Resume from the last chunk.", "acceptance": ["TestResume passes"], "criteria": ["spec#1"], "depends_on": ["dedupe"], "footprint": ["internal.trace"]},
  {"id": "dedupe", "task": "Skip acknowledged chunks.", "acceptance": ["No chunk is sent twice"], "criteria": [], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
)
