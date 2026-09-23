# M3 sequential implementation demonstration

`TestM3SequentialImplementation` in `internal/service/m3_exit_test.go` follows a
ratified two-unit plan through the service's local API client, the same API
used by the `osmia` commands. A fake mason and chief of staff run through
core's fake enforcer and an in-memory MCP transport. The test uses a temporary
Osmia root, a local Git clone and bare upstream. It starts no model,
container, remote push or pull request.

Run it with `dagger check`, or run just the demonstration inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM3SequentialImplementation,-v,./internal/service \
  combined-output
```

The architect drafts a spec with two acceptance criteria and a plan with
`resume` followed by `dedupe`. `dedupe` depends on `resume`. After the owner
ratifies, the build operation moves the workstream to `building`, makes
`resume` ready and leaves `dedupe` planned. The test pauses dispatch while it
checks these states through both status API views.

The service creates `resume`'s unit branch and workspace from the feature
branch. The mason receives a sealed bundle with its criterion and planned
proof in an isolated file view. Its first turn asks whether acknowledged
chunks survive a restart. The unit moves to `waiting`, while its workspace
keeps the mason's file. The chief of staff receives the question and
escalates it. The owner answers through the inbox; the chief relays the
ruling; the answer arrives on the mason's existing thread. The controller
records a move back to `implementing`.

On that answer turn, the fake mason calls `done` with an outcome, an
owner-facing card and a report for `spec#1`: what it did, evidence and the
planned proof's location. The
service snapshots the unit workspace as a candidate commit and records
`units/resume/report.json` revision 1 in the trace. The unit moves to
`reviewing`. The test checks the candidate's branch and parent commit, the
report and card, transition causes, and delivery and acknowledgement of the chief
of staff's start, question and finish notices. `dedupe` remains `planned`,
has no workspace or report, and receives no mason turn. Its dependency can
become ready only after landing, which the
[landing demonstration](#landing-and-recovery-demonstration) shows.

The trace files to inspect are `events.jsonl` for unit transitions,
`questions/1/` for the question and answer, the mason's thread record for
both turns, and `units/resume/report.json` for the report and candidate.

## Landing and recovery demonstration

`TestM3LandingDemonstration` in `internal/service/landing_demo_test.go` lands
the units of one workstream on its feature branch. Fake masons, reviewer and
librarian run through the same local API, temporary Osmia root and local Git
clone as above. The test starts no model, container, remote push or pull
request. Run it with `dagger check`, or alone inside Dagger:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM3LandingDemonstration,-v,./internal/service \
  combined-output
```

The plan has three units: `resume` and `audit` are independent, and `dedupe`
depends on `resume`. `resume`'s mason reports a learning with its criterion.
When `resume` moves to review, `audit`'s workspace is created from the same
feature branch commit, so it is in flight when `resume` lands.

The first landing is interrupted twice. After its commit is made, the
landing fails and waits to retry. Meanwhile the reviewer approves `audit` on
the base `resume` has not yet moved. The service then stops after moving the
feature branch but before recording the landing. On restart, the landing
finds its commit on the branch by its `Osmia-Operation` trailer and records it
without a second commit. The landing operation keeps its retry history, and
`units/resume/landing.json` names the reviewed candidate, base, approval,
criteria and commit. The same landing moves `resume` to `merged` and then
`dedupe` to `ready`; its mason starts only after that.

The librarian then folds `resume`'s learning into `kb/internal.md`.
`kb/sources.json` links the new prose revision to `resume`'s landing
revision, commit and the refresh operation. `dedupe`'s mason bundle includes
the refreshed prose; `resume`'s did not.

`audit`'s approved workspace does not descend from the landed commit, so the
service rebases it. `units/audit/rebase.json` records the approved snapshot,
its old base and the rebased commit. The approval no longer holds, and
`audit` returns to review. The reviewer reviews the rebased candidate on the
new base, and only that approval lands. `dedupe` lands after `resume`; the
feature branch ends with one commit per unit, each on the one before.

`osmia status <workstream>` and `GET /v1/status/<workstream-id>` show each
merged unit's landing: its commit, the reviewed candidate and base, the
approval and the criteria. In the trace, follow `events.jsonl` for the unit,
landing and rebase transitions and their causes, `units/<unit>/landing.json`,
`units/<unit>/review.json` and `units/audit/rebase.json`, the landing
operations' history, and the librarian workstream's `kb-refresh` operations
with `kb/sources.json`.
