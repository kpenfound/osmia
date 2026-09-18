# M2 hand-in demonstration

`TestM2HandInToSketchedPlan` in `internal/service/handover_test.go` takes
handed material to a validated feature spec and plan. It drives the
production service through its local API client: project registration,
hand-in, status. The architect's engine, the chief of staff's engine, the MCP
transport and the GitHub issue client are fakes. The test uses no network,
live model or real container. It runs in a temporary root with a local Git
repository as the target clone.

## Running it

Tests run only inside Dagger, never with `go test` on the host. The test runs
as part of `dagger check`. To run it alone:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM2HandInToSketchedPlan,-v,./internal/service \
  combined-output
```

## What happens

1. The fixture onboards a project with `AddProject`. Its clone tracks
   `internal/trace/git.go`, so the entity map has an `internal.trace` entity
   for plan footprints to name.
2. The charter is still the empty template, so a hand-in is refused with
   `charter_empty`. The error names the charter file, and no workstream is
   created.
3. The owner writes two numbered rules into the charter. Three hand-ins
   follow, each creating its own workstream in state `handed`:
   - a file, copied to `handed/design.md`, source `file:<path>`;
   - an issue URL served by the fake issue client, copied to
     `handed/issue-3.md`, source the URL;
   - stdin, copied to `handed/stdin`, source `stdin`.

   Each copy is byte-for-byte identical to its input, carriage returns, tabs
   and non-ASCII text included.
4. The service asks the architect for draft 1 of each workstream. The fake
   architect delivers a spec with two acceptance criteria and a plan whose
   units depend on each other and leave `spec#2` unaddressed. The service
   records both files as revision 1 and moves the draft to `invalid-1` with
   the problems:

   ```
   draft 1 of the spec and plan is invalid:
   - spec#2: no unit addresses this criterion
   - unit "dedupe": dependency cycle dedupe -> resume -> dedupe
   ```

5. Draft 2's prompt carries those problems, and its view holds the previous
   draft under `draft/`. The fake architect delivers a valid plan. The service
   records revision 2 of both files and moves the workstream from `handed` to
   `sketched`. The actor is `service`/`architect-drafting`, and the reason is
   `the architect's draft 2 passed validation: spec.md revision 2 with 2
   acceptance criteria and plan.json revision 2 with 2 units`.
6. The fake chief of staff receives event turns for each workstream. They
   tell it about the hand-in and the move to `sketched`, but not about the
   invalid draft. On the `sketched` notice it writes a status with
   `set_status`. `Statuses` and `Status` then show all three workstreams as
   `sketched`, each with that status.

## Inspecting the record

Paths below are relative to `<root>/projects/<project-id>/workstreams/<id>`.

| File | What it shows |
| --- | --- |
| `handed/` | The unchanged copy of the handed input |
| `documents.jsonl` | The handed document with its source, then `spec` and `plan` revisions 1 and 2, authored by `agent`/`agent_architect` and caused by their draft operations |
| `spec.md`, `plan.json` | The latest revisions: the accepted draft |
| `events.jsonl` | The `handin` transition, the `draft` transitions (`drafting-1`, `invalid-1`, `drafting-2`) and `sketched`, from `handed` to `sketched` |
| `workflow.json` | The two `architect-draft` operations, one failed and one succeeded, and the notices in the outbox |
| `agents/agent_architect/log.jsonl` | The architect's two turns and their prompts |

Every trace change is a Git commit, so `git -C <root>/projects/<project-id> log`
lists the history. The [trace repository reference](trace.md) documents the
record formats, and [architect drafting](service.md#architect-drafting) the
drafting rules.

## Limits

The test drives the service package directly, so it injects the architect and
the chief of staff through `Options.Architect` and `Options.Threads`;
`osmia serve` builds both from core's enforcers (see
[running turns](service.md#running-turns)). The fake chief of staff writes its
status only through the production `set_status` tool.
