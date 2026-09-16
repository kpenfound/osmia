# M1 trace repository

`internal/trace` persists the service's typed records in the dedicated local Git
repository at `<root>/projects/<project-id>`. `Create` accepts a resolved
`config.Root`, the configured project, a timestamp and an actor. It reserves the
repository with an exclusive directory creation and preserves an existing
`config.toml`. A target clone and the Osmia root must be separate, non-nested
paths. `Repository` is a trace handle; it does not implement a target workspace
or provide delivery operations.

`CreateWorkstream` reserves a supplied `config.WorkstreamID`. Project and
workstream manifests retain their identity and creation provenance. `Workstreams`
returns identities validated against those manifests, including terminal streams.
The package supplies storage APIs; onboarding and lifecycle commands are separate.

## Files and records

Creation initializes `charter.md`, `kb/entities.json`, `kb/`, `notes/` and
`workstreams/`. Workstream creation initializes `handed/`, `shed/`, `amendments/`,
`questions/`, `units/` and `agents/`, plus the empty document, transition and cost
logs. Spec and plan files appear when their first document revision is appended.
Empty directories exist on disk; Git records files.

Every record carries a schema named `osmia.trace.<kind>`, version `1`, record ID,
one-based revision, project and optional workstream/unit identity, timestamp,
actor, cause and causal depth. Actors name an owner, service component or agent.
The caller supplies stable causal message/operation/record references; storage
does not infer authority, readiness or workflow transitions from them.

| Go record | File relative to the project or workstream | Payload |
| --- | --- | --- |
| `Document` | `documents.jsonl` and its document path | Path and complete content of each revision |
| `Transition` | `events.jsonl` | Subject, prior/resulting state and reason |
| `Question` | `questions/<id>/question.jsonl` | Asking actor, original question and owner-facing text |
| `Ruling` | `questions/<question-id>/rulings.jsonl` | Question revision, decision, owner response, returned answer and affected references |
| `Agent` | `agents/<id>/identity.jsonl` | Stable role/thread identity and current backend session |
| `TurnRequest` | `agents/<agent-id>/log.jsonl` | Thread/turn identity, profile, resume identity, system prompt, request and replay context |
| `TurnResponse` | `agents/<agent-id>/log.jsonl` | Exact request revision, thread/turn identity, adapter result and any execution failure |
| `Cost` | `ledger.jsonl` | Adapter ledger entry with attempt, full scope, time and explicit cost knowledge |

Only documents can be project-scoped. Project document paths are `charter.md`,
`kb/entities.json`, `kb/<name>.md` and `notes/<role>.md`; workstream document paths
are `spec.md`, `plan.json` and `handed/<name>`. Handed inputs are immutable.
Document paths retain one record identity. Agent role and thread IDs stay stable
across backend-session revisions. An agent may have no backend session before
its first turn. Failed execution may have no session identity when it includes
an explicit failure. Unknown cost is stored as zero with `CostKnown: false`;
it is distinguishable from a known zero-cost result.

`Append(ctx, record)` accepts concrete record values. Revisions must begin at one
and increase consecutively for each kind/ID within its scope. It preserves prior
JSONL entries, writes the latest document content to its ordinary file and commits
the affected files. For example, changing a spec retains both full document
revisions in `documents.jsonl` and both versions of `spec.md` in Git history.
There is no terminal-history deletion operation.

`Read[trace.Document](repository, workstreamID)` enumerates typed revisions;
`Get[trace.Document](repository, workstreamID, id, revision)` retrieves one.
Use an empty workstream ID for project documents. Enumeration follows file and
line order, not a global event ordering. Reads use ordinary files, including the
owned request/final-response log, without backend transcript files, the target
clone or Hearsay. The execution payloads reuse the core adapter contracts;
trace accounting does not change the operational ledger adapter's file format.

## Failure and ownership boundaries

One handle holds an exclusive process lock until `Close`; concurrent calls on
that handle serialize. External writers must not modify or move the repository
while it is open. Files use root-confined operations and temporary-file replacement
with file and directory sync. Existing symlinks, hardlink aliases, special files,
absolute/traversal paths, unsupported schema versions, unknown or duplicate JSON
fields, invalid provenance and inconsistent revision identities are rejected.
Every existing path component is checked before access.

Git uses an isolated local configuration, no inherited Git routing or credentials,
no global/system configuration, and disabled hooks. It hashes supplied bytes using
plumbing commands, bypassing attributes and filters. Only trace-owned files enter
its commits; project configuration is left in place. Linked worktrees, redirected
object stores, Git symlinks and changes to the isolated `.git/config` are rejected.
A local Git executable is required for create, append and reopen. The service owns
this executable access; runtime agents receive no trace-repository handle.

`Open` checks manifests, every record and the committed file contents. For damaged
records or uncommitted files it returns a non-nil handle **and an error**; callers
must close that handle. Typed reads return valid records together with any
path/line corruption diagnostics. They never skip a damaged line silently or
truncate a log. A missing or modified committed file, or an uncommitted record file,
requires reconciliation and prevents further appends. Typed reads can inspect
pending records even when a prior Git commit failed.

An append is not an atomic workflow-state/outbox transaction. A write or Git
failure can leave inspectable, uncommitted files; the error does not claim rollback
or successful delivery. This package reports that condition without repairing,
retrying or reconciling side effects. Controllers remain responsible for owner
gates and workflow policy.
