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

On that answer turn, the fake mason calls `done` with an outcome and a report
for `spec#1`: what it did, evidence and the planned proof's location. The
service snapshots the unit workspace as a candidate commit and records
`units/resume/report.json` revision 1 in the trace. The unit moves to
`reviewing`. The test checks the candidate's branch and parent commit, the
report and transition causes, and delivery and acknowledgement of the chief
of staff's start, question and finish notices. `dedupe` remains `planned`,
has no workspace or report, and receives no mason turn. Its dependency can
become ready only after landing, which is outside this demonstration.

The trace files to inspect are `events.jsonl` for unit transitions,
`questions/1/` for the question and answer, the mason's thread record for
both turns, and `units/resume/report.json` for the report and candidate.
