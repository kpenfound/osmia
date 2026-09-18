# M2 exit demonstration

The M2 exit condition is: an owner can hand in a design and ratify a buildable
plan using local context. `TestM2HandInToRatifiedPlan` in
`internal/service/m2_exit_test.go` shows it through the service's local API
client, which is what the `osmia` commands call. The service is wired the way
`osmia serve` wires it: one engine runs the librarian, the architect, the
committee and the chief of staff. That engine is core's fake enforcer driving
scripted fake agents over an in-memory MCP transport. The test makes no
provider or GitHub calls, starts no process or container and pushes nothing.
It runs in a temporary root with a local Git repository as the clone and a
local bare repository as its upstream.

## Running it

Tests run only inside Dagger, never with `go test` on the host. The test runs
as part of `DAGGER_X_RELEASE=v1.0.0-beta.13 dagger check`. To run it alone:

```sh
DAGGER_X_RELEASE=v1.0.0-beta.13 dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM2HandInToRatifiedPlan,-v,./internal/service \
  combined-output
```

## Setup

The root's configuration runs every role in a container with a fixture image,
gives each shed a committee of two (`[capacity] committee = 2`) and caps
debate at two rounds (`[shed] max_rounds = 2`). The clone tracks
`internal/trace/git.go`, and its upstream is a bare repository added as the
remote `upstream`, whose URL names the project's upstream `dagger/dagger`.

The owner hands in four designs from stdin. The fake agents tell the
workstreams apart by the first line of what was handed:

| Design | What happens to it |
| --- | --- |
| `# Resumable uploads` | Debated to consensus after a redraft, then ratified |
| `# Upload retention` | Debated to the round cap with a charter veto standing, overruled, then ratified across a restart |
| `# Upload error names the chunk` | Small: debate is skipped, then ratified |
| `# Upload quotas` | Abandoned |

The service runs three times. The first service runs no committee, so a
sketched workstream stays sketched until the owner acts on it. The second and
third run the committee as `osmia serve` does.

## The walkthrough

### 1. Onboarding

```sh
osmia project add dagger --upstream dagger/dagger --fork owner/dagger --clone <clone>
osmia handin p_… - < design.md    # refused: charter_empty
```

The fake librarian's extraction succeeds and writes `kb/trace.md` and
`kb/entities.json`. A hand-in is refused with `charter_empty` until the owner
writes two numbered rules into `charter.md`:

```text
1. Keep changes small.
2. Every change has a test.
```

`osmia status` then shows the charter ready with 2 rules. The entity
`internal.trace` resolves to its paths from the local map alone, and every
plan footprint in the demonstration names it.

### 2. Hand-in to `sketched`

Each hand-in returns the state `handed`. The fake architect's first draft of
each is valid, so all four workstreams move from `handed` to `sketched`, by
`service`/`architect-drafting`. `osmia status` lists four sketched
workstreams.

### 3. Abandoning the fourth workstream

```sh
osmia abandon w_… "Quotas wait for the billing work."
```

The workstream is `abandoned`. Its trace stays: `handed/stdin`, `spec.md` and
`plan.json` are still committed.

### 4. Skipping debate on the small workstream

```sh
osmia shed skip w_…
osmia ratify w_…
```

Before the skip, `POST /v1/ratify/w_…` with `{"spec": 1, "plan": 1}` is
refused because the workstream is sketched and its debate is not skipped.
After the skip, the packet (`GET /v1/packet/w_…`) is marked skipped, names spec revision 1 and plan
revision 1, holds no dissent and recommends `ratify: no objection stands`. The
chief of staff is told to present it. The fake chief of staff makes the
recommendation the attention item of its status, which `osmia status w_…`
shows: `Ratify the spec and plan: no objection stands.` The explicit
ratification is sealed, and the workstream moves to `ratified`.

### 5. A mason in the retention workstream

The service is stopped. The test fixture gives the retention workstream a
mason thread with one queued turn, the way the
[chief-of-staff demonstration](m2-chief-of-staff.md) creates its workers. The
service then starts again with its committee.

### 6. A question asked during the shed

The retention workstream enters the shed, and the mason's turn runs while
the workstream is `in-shed`. The mason calls `ask`:
`Do deleted uploads count against the retention period?` The chief of staff
escalates the question with a rephrasing, what is blocked, two options and a
recommendation. The owner answers it:

```sh
osmia inbox
osmia answer 1 "No. The period starts at upload and deletion ends it."
```

The inbox entry is number 1, batch `escalation_1`, with the mason's question
as asked. After the answer the inbox is empty. The chief of staff relays the
ruling with the scope `local`, and the mason's next turn, `answer_1`, receives
it on its own thread.

### 7. Consensus after a redraft

In round 1 of the resumable-uploads shed, the first member raises a charter
veto on `spec#2`, citing `charter#2`, and a size objection on `plan#resume`,
citing `kb/entities.json#internal.trace`. The second member raises nothing.
The architect answers both and redrafts the plan: `resume` is split into
`resume-read` and `resume-write`, and `dedupe` gets a test. In round 2 the
first member concedes both objections. The shed goes
`round-1, heard-1, reply-1, replied-1, round-2, heard-2, concluded-2`, and the
conclusion reads
`debate concluded by consensus after round 2: no objection stands`.

```sh
osmia ratify w_…
```

The packet names spec revision 1 and plan revision 2 and recommends
`ratify: no objection stands`. The chief of staff raises it as the attention
item. The ratification is sealed.

### 8. The cap with a veto still open

In the retention shed, the first member raises the same charter veto. The
architect answers it without a redraft, and round 2 changes nothing. Debate
stops at `concluded-2`: `debate stopped after round 2, at the
shed.max_rounds cap of 2, with 1 objection standing, 1 of them blocking; the
cap approves nothing`. The packet recommends
`do not ratify yet: ratification is blocked by 1 objection (…); overrule or
sustain each one, or ask for a redraft`, and the chief of staff's attention
item reads `A charter veto blocks the plan: overrule it, sustain it or ask
for a redraft.`

```sh
osmia ratify w_…    # refused: the veto blocks and has no disposition
osmia shed overrule w_… agent_committee_1-r1-1 "The reviewer's judgement is enough for retention."
osmia ratify w_…
```

Ratification is refused, naming the objection: `… blocks and has no
disposition`. The overrule is recorded in `shed/round-2/rulings.json`, against
spec revision 1 and plan revision 1, and the packet then recommends
`ratify: nothing blocks, and 1 objection stands as advice on the record`. The ratification records the
disposition in `shed/round-2/ratification.json`.

### 9. A restart in the middle of ratification

The test's fault point stops the service after the sealing has created the
feature branch and before it records the seal. At that moment the clone holds
`osmia/w_…` at the upstream commit, and the trace holds no seal.

The next service finishes the sealing it finds unfinished. Its inspection
observes `the clone has feature branch osmia/w_…; the sealing resumes from
it`. There is one sealing operation, one `seal.json` revision and one feature
branch, and the workstream is `ratified`.

### 10. What ratification left behind

For each ratified workstream, `seal.json` holds:

- the upstream commit (`upstream`, `main` and the commit the bare upstream's
  `main` is at);
- the `sha256:` hash of the ratified `spec.md`;
- the ratified revisions;
- the branch `osmia/<workstream-id>` and its workspace under
  `<root>/branches/<project-id>/<workstream-id>`;
- each unit's footprint, resolved to entities and paths.

For the resumable-uploads workstream, those units are `resume-read`,
`resume-write` and `dedupe`. The clone has four worktrees: its own and one
per ratified workstream, each on its feature branch at the sealed commit.
Nothing is pushed.

### 11. The abandoned workstream

Across both restarts, the abandoned workstream runs no architect or committee
turn after its first draft. It never enters the shed, gets no committee and
has no feature branch or workspace.

## Inspecting the record

Paths below are relative to
`<root>/projects/<project-id>/workstreams/<workstream-id>`.

| File | What it shows |
| --- | --- |
| `shed/round-<n>/agent_committee_<i>.json` | Each member's objections and concessions in round `n`, against the revision it read |
| `shed/round-<n>/reply.json` | The architect's answers and the redraft it delivered |
| `shed/round-<n>/packet.json` | The ratification packet, one revision per change in what it says |
| `shed/round-<n>/rulings.json` | The owner's overrule, against the revision |
| `shed/round-<n>/ratification.json` | The ratified revisions, the dispositions and the dissent record |
| `seal.json` | The seal |
| `events.jsonl` | The feature, shed, owner and seal transitions with their actors and reasons |
| `questions/1/` | The mason's question, the escalation, the owner's ruling and what was relayed |

The [trace reference](trace.md) documents these records, and the
[service reference](service.md) documents the shed, ratification and sealing.

## Limits

- The first service runs no committee. With a committee, a sketched
  workstream enters the shed and round 1 starts in the same pass, so the
  owner has no moment to skip debate before a round has run.
- The architect has no `ask` tool in M2, and the demonstration's committee
  asks nothing, so the question during the shed comes from a mason thread the
  test fixture creates, as in the
  [chief-of-staff demonstration](m2-chief-of-staff.md).
- The restart is a stop at a test fault point, not a crash of the process.
