# M1 demonstration

The M1 exit condition is: a role receives successive turns and survives a
restart with an inspectable record. `TestM1ThreadContinuityAcrossRestart` in
`internal/service/continuity_test.go` demonstrates it using the production
service, trace repository, reconciliation controller, thread runner, turn
dispatcher and turn isolation path. The agent backend, container engine, MCP
transport and workspace provider are fakes. The test uses no network, live
model or real container, and it runs in a temporary root with a local Git
repository as the target clone.

## Running it

```sh
go test ./internal/service -run TestM1ThreadContinuityAcrossRestart -v
```

The test runs as part of `go test ./...` and
`DAGGER_X_RELEASE=v1.0.0-beta.13 dagger check`. It removes its temporary roots
when it finishes. To keep them for inspection, name a directory that does not
contain previous results:

```sh
rm -rf /tmp/osmia-m1
OSMIA_M1_DEMO_DIR=/tmp/osmia-m1 go test ./internal/service -run TestM1ThreadContinuityAcrossRestart -count=1 -v
```

Each case gets its own directory, `<dir>/resume` and `<dir>/replay`, and the
verbose output prints its root and trace paths. Keep `<dir>` short: the service
socket lives under the root, and Unix socket paths are limited to about 100
bytes.

## What happens

1. Before the first start, the owner's first message is accepted into the
   `agent_mason` thread (role `mason`). A `thread-turn` operation intent is
   published with the `deliver_first` transition.
2. The service starts. Its controller scans durable operations, and the
   [turn dispatcher](trace.md#turn-dispatch) runs the first turn inside the
   isolated boundary. The fake backend writes private notes and edits its
   private file view.
3. While the first turn is active, a second message and its intent are
   accepted. The second turn cannot be claimed while the first is active.
4. The service stops as the backend returns. The first turn's response, cost
   and completion are recorded, but its operation is left claimed with no
   recorded result. The second operation has not been touched.
5. The service starts again. Without a wakeup from the first lifetime, the
   controller finds both intents. Inspection shows the first turn completed, so
   its operation is finished without running it again. The second turn runs:
   - `resume` case: the fake engine accepts the saved session `session-first`,
     and the backend receives only the new prompt.
   - `replay` case: the fake engine reports the session unavailable. The
     backend starts fresh with bounded JSON context from the owned log: the
     first request and its final response.
   In both cases the second turn reads the notes written by the first turn.

Inside both turns, the fake backend checks the boundary:

- The MCP server lists only `file_read`, `file_write`, `notes_read` and
  `notes_write`. The role's grant also names `shell` and `git_push`, but the
  mason test grant has no execute permission and VCS tools are never exposed.
- Writes to `.git/config`, to paths outside the view and to absolute paths
  fail, and so do reads of `.git/HEAD` and of the clone.
- The single mount is a fresh copy without VCS metadata. The request carries
  no VCS access, and its environment holds only the service MCP token, even
  though the service process has `GITHUB_TOKEN` and `GH_TOKEN` set.

The test then checks that the clone and its `.git` directory are unchanged,
and that the injected credential appears in no file under the root.

## Inspecting the record

Paths below are relative to `<root>/projects/<project-id>`; the workstream
directory is `workstreams/<workstream-id>`.

| File | What it shows |
| --- | --- |
| `workstreams/<id>/agents/agent_mason/identity.jsonl` | The stable role and thread identity |
| `workstreams/<id>/agents/agent_mason/log.jsonl` | Two requests and two responses. Each response names its `request_id`, keeps the request's `cause` and `depth`, and holds the final response, backend session and session directory |
| `workstreams/<id>/workflow.json` | `threads.agent_mason.turns`: sequence, claim (the service session differs between turns because of the restart), attempts with `path` `resume` or `replay`, source session and replay bounds. `operations`: claim, observe, effect, result and acknowledge actions with their repository session. `transactions`: the two `deliver_*` transitions and their outbox events |
| `workstreams/<id>/events.jsonl` | The `deliver_first` and `deliver_second` transitions; each `cause` is its request ID |
| `workstreams/<id>/ledger.jsonl` | One cost record per attempt, with full scope and `AttemptID`. The first turn's cost is known; the second's is unknown (`CostKnown: false`) |
| `notes/mason.md` | The mason's private notes, last written by the second turn |

The first turn's operation history has two `claim` actions from different
repository sessions. Its second attempt observes `completed` and records the
result without an `effect`. Each operation has exactly one `effect`, `result`
and `acknowledge` action.

Every trace change is a Git commit, so `git -C <root>/projects/<project-id> log`
lists the history. The trace publishes through a private index, so compare
files with `git show HEAD:<path>` rather than `git status`. Backend session
directories are under `<root>/sessions/agent_mason/<turn>`. `<root>/views`
is empty after each turn because per-turn file views are removed. The target
clone is `<dir>/<case>/clone`.

The typed read API returns the same history: `Repository.Thread`,
`Repository.Operations`, `Repository.Outbox` and
`trace.Read[T]` for `TurnRequest`, `TurnResponse`, `Transition` and `Cost`. The
test uses these calls for its assertions. See the
[trace repository reference](trace.md) for the record formats.

## Limits

The fake container engine reports the requested policy as the policy it
established. It shows that the service builds and checks the boundary, but it
is not evidence of OS enforcement; see [turn isolation](isolation.md). The
local HTTP API accepts no workstream messages; the only turn it starts is the
librarian's [knowledge-base extraction](knowledge-base.md#extraction). The
test publishes the first intent through its own trace repository before the
service starts, and the second through the repository handle that the service
passes to `Options.Threads`.
