# Command line

Build with `go build -o osmia ./cmd/osmia`, or install with
`go install ./cmd/osmia`. Prepare the [configuration](configuration.md) first;
a top-level `config.toml` with profiles and no project is enough to start.

```sh
osmia serve --root ~/.osmia
# In another terminal:
osmia status --root ~/.osmia
osmia status w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef criterion spec#1
osmia trace w_0123456789abcdef0123456789abcdef unit parser
osmia trace w_0123456789abcdef0123456789abcdef commit 0123456789abcdef0123456789abcdef01234567
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project extract p_0123456789abcdef0123456789abcdef
osmia project remove p_0123456789abcdef0123456789abcdef
osmia handin p_0123456789abcdef0123456789abcdef design.md
osmia handin p_0123456789abcdef0123456789abcdef small-fix.md --skip-debate
osmia send w_0123456789abcdef0123456789abcdef "Start with the upload API."
osmia conversation w_0123456789abcdef0123456789abcdef
osmia inbox
osmia answer 1 "Resume uploads; never restart them."
osmia abandon w_0123456789abcdef0123456789abcdef "Superseded by the new upload design."
osmia shed object w_0123456789abcdef0123456789abcdef "The plan never names the retry budget."
osmia shed rule w_0123456789abcdef0123456789abcdef agent_committee_1-r1-1 dismiss "We accept the risk."
osmia shed overrule w_0123456789abcdef0123456789abcdef agent_committee_1-r1-2 "Ship it and note the risk."
osmia shed skip w_0123456789abcdef0123456789abcdef
osmia shed more w_0123456789abcdef0123456789abcdef 2
osmia shed redraft w_0123456789abcdef0123456789abcdef "Split the resume unit by what it addresses."
osmia ratify w_0123456789abcdef0123456789abcdef
osmia amendment w_0123456789abcdef0123456789abcdef 1
osmia amendment w_0123456789abcdef0123456789abcdef 1 approve "Checkpoints are what we meant."
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia status --json
```

The [onboarding walkthrough](m2-onboarding.md) runs `project add`, `handin`,
`status` and `project remove` in order for a new project. The
[chief-of-staff walkthrough](m2-chief-of-staff.md) runs `send`,
`conversation`, `status`, `inbox` and `answer` on an onboarded project, across
a service restart. The [M2 exit demonstration](m2-exit.md) runs
`handin` (with and without `--skip-debate`), `abandon`, `shed overrule`, `ratify`, `inbox` and
`answer` through their API calls, from hand-in to a sealed feature branch.

## Commands and flags

- `serve [--root PATH]` validates and starts the foreground service. SIGINT and
  SIGTERM drain requests and release ownership and the socket. A live owner is
  refused; a provably stale socket is recovered automatically. The service
  runs the librarian, architect, committee and chief-of-staff turns in each role's
  configured sandbox; see [running turns](service.md#running-turns).
- `status` shows health, loaded configuration digest/root, the active project
  and its trace path (or that none is configured), its charter state (ready or
  empty, rule count, recorded revision and numbering diagnostics), its latest
  knowledge-base extraction (number, `pending`, `running`, `succeeded` or
  `failed`, the time of its last activity and the reason when it failed or
  waits to retry), diagnostics, effective runtime controls and the project's
  context mode (`file`, a normal mode; see [context](context.md)), then each
  workstream with its state, open question count and context mode, and the
  chief of staff's goal and attention (`Attention: none` when nothing needs
  you), or `no status yet`. Open owner gates appear under the workstream even
  before a status is written. Reading the charter records any edit you made to
  it; see [charter](charter.md).
- `status <workstream-id>` shows one workstream of the active project: the same
  facts and owner gates, then `Units:` with one line per unit of the sealed plan or follow-up and its state
  and the latest available card (headline, happened and any owner action) beneath it
  once the workstream is building, and for a landed unit its commit on the
  feature branch, the reviewed candidate and base with the approval that
  landed it, and the criteria it meets, then the full status (goal, attention, note, one line per active
  agent, and when it was written), or `Status: none yet` before the chief of
  staff writes one. A workstream the project does not hold fails with
  `not_found` (exit 4); with no project configured it fails with `no_project`
  (exit 4). See [workstream status](service.md#workstream-status).
- `trace <workstream-id> [unit <id>|criterion <spec#n>|commit <full-sha>]`
  walks the active project's durable trace through the service API. With no
  selector it lists sealed revisions, criteria, units and delivery. Selectors
  show revision identifiers, provenance, evidence and explicit gaps. `--json`
  returns the complete typed response. Delivered and abandoned workstreams
  remain readable. A missing selector returns `not_found` (exit 4); malformed
  input returns `validation` (exit 4).
- `send <workstream-id> <message>` sends a message to the workstream's chief
  of staff. The message is one argument; quote it. The service records it
  before answering, and the output names the message's turn ID and its state
  (`queued`). The chief of staff answers it as its next turn, after any turn
  already running. A message to an abandoned workstream is recorded but never
  answered; the next service start completes it as cancelled and
  `conversation` lists it as `failed`. An empty message, or a workstream the active project does
  not hold, fails with `validation` (exit 4); with no project configured it
  fails with `no_project` (exit 4). See [conversation](service.md#conversation).
- `conversation <workstream-id>` lists the owner's messages to that
  workstream's chief of staff and its final responses, oldest first. Each
  entry shows when it was sent or answered, who wrote it, its turn ID and the
  turn's state (`queued`, `running`, `done` or `failed`), then its text
  indented. It fails like `send`.
- `inbox` lists every question the chief of staff escalated to you that has
  no ruling, across the workstreams of the active project, oldest escalation
  first. Questions escalated together are one entry. Each entry shows its
  inbox number, when it was escalated, its workstream and batch, then the
  chief of staff's rephrasing, what is blocked, the options, its
  recommendation and each question as its asker put it. An entry of an
  abandoned workstream is left out. Without a project or a trace the inbox is
  empty. See [inbox and rulings](service.md#inbox-and-rulings).
- `answer <inbox-number> <ruling>` records your ruling on an inbox entry. The
  ruling is one argument; quote it. The service records it before answering;
  the chief of staff then rephrases it for every asker of the entry, whose
  threads resume with it as their next turn. The number is the one `inbox`
  shows; it stays the entry's number for the life of the project. A number
  that is not a positive integer is a usage error (exit 2). An empty ruling or
  a number no entry carries fails with `validation` (exit 4); with no project
  configured it fails with `no_project` (exit 4). An entry that already has a
  ruling, or belongs to an abandoned workstream, fails with `conflict`
  (exit 5) and records nothing.
- `charter` lists the charter rules the chief of staff proposed from your
  rulings that wait for your decision, oldest first: the workstream and
  question, the rule and the number it would take, the ruling it comes from
  and your words in it. `charter <workstream-id> <question>` shows one
  proposal in any state; once it is `chartered` it shows the number the rule
  took and the charter revision that records it.
  `charter <workstream-id> <question> <ratify|decline> [note]` decides it.
  `ratify` has the service append the rule to your `charter.md` as its next
  number, under a `## Standing rulings` heading and with the source ruling in a
  comment, and every later bundle on the project carries it as a notice; an
  edit you save to the charter meanwhile is recorded, not overwritten.
  `decline` changes nothing but the proposal. The note is optional and is one
  argument; quote it. Deciding again the same way prints the recorded
  decision; deciding the other way fails with `conflict` (exit 5), as does a
  proposal of an abandoned workstream. An unknown proposal fails with
  `not_found` (exit 4). You can also tell the workstream's chief of staff your
  decision with `send`. See [charter proposals](service.md#charter-proposals).
- `contested <workstream-id> <unit> <review|revise> <note>` records your
  direction for a contested unit shown by `status`. Quote the required note.
  `review` requests another reviewer verdict on the same candidate; `revise`
  returns the findings and your note to the mason. For a mason contest,
  `status` shows whether the mason gave up or exhausted the clean-turn bound;
  only `revise` is accepted, resuming its thread in the existing workspace
  with a fresh clean-turn allowance. Neither decision approves a unit.
- `abandon <workstream-id> <reason>` abandons a workstream of the active
  project that is neither delivered nor abandoned. The reason is one argument;
  quote it. The service records the move to `abandoned` with you as actor and
  your reason, cancels the workstream's running turns (their recorded work is
  kept), completes its queued turns as cancelled, and runs no turn of the
  workstream again, including after a restart. Nothing is deleted: the trace,
  `handed/` and any branch stay. `status` then shows the state `abandoned`. An
  empty reason, or a workstream the active project does not hold, fails with
  `validation` (exit 4); a delivered or already abandoned workstream fails
  with `conflict` (exit 5). See [abandoning](service.md#abandoning).
- `shed object <workstream-id> <argument>` adds your own objection to the
  current round of a workstream's debate. The argument is one argument; quote
  it. It is recorded as yours, stands in the dissent record and blocks, and the
  architect answers it in its reply to the round. Dismiss it with `shed rule`
  when it is settled.
- `shed rule <workstream-id> <objection-id> <sustain|dismiss> [note]` rules on
  one objection that stands. `sustain` makes it block whatever its kind, until
  its member concedes it after a redraft; `dismiss` records your disposition
  and it blocks no longer. The note is optional and is one argument; quote it.
  An objection that does not stand fails with `not_found` (exit 4).
- `shed overrule <workstream-id> <objection-id> [reason]` overrules one
  objection that stands, charter vetoes included: your disposition is recorded
  against the revision the documents are at, the objection stays in the dissent
  record and blocks no longer. Use it at the decision point, where `shed rule`
  is for settling an objection while debate runs. The reason is optional and is
  one argument; quote it. An objection that does not stand fails with
  `not_found` (exit 4).
- `shed skip <workstream-id>` skips debate for a `sketched` or `in-shed`
  workstream. No further committee or architect turn starts; the workstream
  still needs your ratification of the spec and the plan. A running round,
  reply or redraft, and a debate already skipped, fail with `conflict`
  (exit 5): a turn already dispatched runs to its record, so the skip waits for
  it.
- `shed more <workstream-id> <rounds>` asks for that many further rounds once
  debate has concluded, between 1 and `shed.max_rounds`; what you ask for is
  what runs, and it replaces the cap. Debate resumes from the conclusion. A
  debate still running or never concluded fails with `conflict` (exit 5), a
  count out of range with `validation` (exit 4), and a count that is not a
  number is a usage error (exit 2).
- `shed redraft <workstream-id> <note>` sends the spec and the plan back to
  the architect once debate has concluded. The note is what to change and is
  one argument; quote it. The architect writes the redraft, and debate resumes
  with one round that reads it. A debate still running or never concluded, a
  skipped one, and a conclusion you have already asked a redraft for fail with
  `conflict` (exit 5); an empty note fails with `validation` (exit 4).
- `ratify <workstream-id>` ratifies the spec and the plan of a workstream in
  the shed. It reads your ratification packet, pins the revisions the packet
  names and ratifies exactly those, so revisions that moved since are refused.
  Ratification is refused, with every reason listed, when an objection still
  blocks and you have not overruled it, when the plan does not validate, when
  the revisions are not the current ones, and when the workstream is not in the
  shed: all `conflict` (exit 5). What passes records your approval of those
  revisions, which asks the service to seal them: it fetches upstream, records
  the seal and the plan's footprints, creates the feature branch in its own
  workspace on your clone and moves the workstream to `ratified`, then to
  `building` with the state of each unit of the plan recorded. The command
  prints the recorded detail, which says the sealing is asked for. Ratifying
  revisions you have ratified already records nothing while their sealing is
  asked for, pending or running, and says which; once it has failed, the same
  command asks for the sealing again, which is how you retry one after putting
  right what failed it. A workstream with no packet yet fails with `not_found`
  (exit 4). See [the ratification gate](service.md#the-ratification-gate) and
  [sealing](service.md#sealing).
- `amendment <workstream-id> <n>` shows amendment `n` of a building or
  assembled workstream: its state, the debate round it reached, the packet the
  chief of staff presented with its revision, and your latest decision.
  `amendment <workstream-id> <n> <approve|reject|round|overrule> [note]`
  decides it. The command reads the packet first and decides exactly that
  revision, so a packet presented again since is refused. `approve` versions
  the proposed spec and/or plan and reseals; it is refused while a charter
  veto, size split or proof objection stands. `overrule` approves over the
  objections that stand and records them. `reject` leaves the sealed spec and
  plan in force. `round` asks the committee for one more bounded debate round,
  up to `shed.max_rounds` rounds in all, after which the chief of staff
  presents the amendment again. The requester receives your ruling as its next
  turn and the unit it parked resumes its stage. The note is optional and is
  one argument; quote it. Deciding the same packet revision again the same way
  prints the recorded decision; another decision on it, an amendment that is
  not presented, and a refused approval fail with `conflict` (exit 5). An
  unknown amendment fails with `not_found` (exit 4). You can also tell the
  workstream's chief of staff your decision with `send`. See
  [amendment decisions](service.md#amendment-decisions).
- `delivery <workstream-id>` shows an assembled workstream's final report,
  unshown criteria first, and the drafted pull request description, then any
  approved description and the state of its publication. For a delivered
  workstream it shows the report, the approved description and the pull
  request it was delivered as.
- `approve <workstream-id> [description-file]` approves the drafted
  description, or the file's text instead, for the presented final report.
  The service then pushes the branch to your fork and opens the pull request
  itself; `delivery` shows its progress. A report with gaps, a presentation
  that changed and a delivered workstream fail with `conflict` (exit 5). After
  a publication was refused, approving again asks for another one. See
  [owner delivery approval](service.md#owner-delivery-approval) and
  [publication](service.md#publication).
- Editing the documents needs no command. You edit `spec.md` and `plan.json` in
  the workstream's directory under the trace yourself. The service records what
  you changed as a new revision of yours before any turn reads it, and the next
  round debates it. The two are one draft: if what you leave does not validate,
  neither file is recorded and neither is debated, your chief of staff reports
  the problems, and the recorded revisions stay. Until you correct them, a
  redraft by the architect of a file you edited is given up rather than written
  over your edit. See [the owner in the shed](service.md#the-owner-in-the-shed).
- `project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH
  [--base-branch NAME]` registers a project with the running service: it
  validates the request, generates the project ID, writes
  `projects/<id>/config.toml`, creates the trace repository with a charter
  template and an entity map seeded from the clone's tracked files, lists the ID in
  `active_projects`, requests the librarian's first knowledge-base extraction
  and activates the project without a restart. The clone path is made
  absolute by the client and must be an existing local Git repository outside
  the Osmia root; nothing is written to it. The output names the project ID,
  the trace path and the next step: writing the charter in
  `<trace>/charter.md`. The extraction runs in the service after the command
  returns; `status` shows its state, and the project is usable whether it
  succeeds or fails; after a failure `osmia project extract` starts a new
  attempt. Operation
  stays single-project: adding another project while one is active is refused
  and the error names the active project. Repeating the active project's exact registration
  returns it again.
- `project extract <project-id>` starts a new
  [knowledge-base extraction](knowledge-base.md#extraction) of the active
  project: a librarian turn that rewrites `kb/<subsystem>.md` and
  `kb/entities.json` from the clone. The command returns once the extraction
  is recorded as pending; follow it with `status`. A project ID that is not the
  active project fails with `not_found` (exit 4). While an extraction is still
  pending or running, another is refused with `conflict` (exit 5) naming the
  extraction to wait for.
- `project remove <project-id>` takes the active project out of
  `active_projects` and closes its runtime state. The trace directory and the
  clone are kept. Adding the same upstream again afterwards creates a new
  project ID and a new trace; see [configuration](configuration.md#project-registration).
- `handin <project-id> <path|issue-url|-> [--skip-debate]` hands one input to the active
  project and creates a workstream in state `handed`. The input is a file
  (made absolute by the client and read by the service), a GitHub issue URL
  (`https://github.com/OWNER/REPO/issues/NUMBER`, fetched by the service), or
  `-` for stdin (read by the client, at most 512 KiB of UTF-8 text; the client
  refuses larger or non-UTF-8 stdin itself with exit 4, before sending a
  request, and that message has no `validation:` prefix). The output names the
  workstream ID, its state, the path of the copy under the trace's
  `workstreams/<id>/handed/` and the recorded source. The client sends a new
  idempotency key with each command, so a request the API retries creates one
  workstream, and running the command again creates another. A project whose
  charter has no rules fails with `charter_empty` (exit 4), naming the project
  and the path to its `charter.md`. A project ID that is not the active project
  fails with `not_found` (exit 4). An unreadable, empty, non-UTF-8 or oversized
  input, or a URL of another shape, fails with `validation` (exit 4); an issue
  the service cannot fetch fails with `internal` (exit 5). A refused hand-in
  creates nothing. See [service](service.md#hand-in) for what is
  recorded. The service then asks the architect for the spec and plan; see
  [architect drafting](service.md#architect-drafting). With `--skip-debate`
  the hand-in also skips debate, for small work: once the architect's draft is
  valid, the workstream enters the shed without a committee, no round runs, and
  the spec and the plan wait for your `ratify`. The output then ends with
  `Debate: skipped; ratify the spec and the plan once they are drafted`.
- `pause <all|project-id|workstream-id> [--hard] [--reason TEXT]` stores an
  owner-attributed pause; the default mode is soft. The status and command output
  show each pause's source, reason and UTC set time.
- `resume <all|project-id|workstream-id>` clears that scope's pause. Parent pauses
  still apply; all effective pauses are displayed.
- `priority set <workstream-id>...` stores the active project's ordered list.
  The chief of staff sets the same list when you ask it to in a message; see
  [service](service.md#priority-at-the-owners-request).
  `priority clear` removes that preference. Both, and `pause`/`resume` of a
  workstream, need an active project and say so otherwise.
- `profiles` displays every role's effective binding and its source
  (`configuration` or `owner_override`), plus runtime diagnostics. `status`
  shows the same profile information.
  `profiles set <role> <profile>` overrides a binding.
  `profiles clear <role>` restores its configured default.

Every command accepts `--root PATH`, defaulting to `~/.osmia`. Flags may occur
before or after positional arguments; value flags also accept `--flag=value`.
Duplicate and unknown flags are errors. `--help` prints the supported syntax.

Clients connect to `osmia.sock` under the root. If configuration specifies another
socket name, pass `--socket PATH` to each client command; relative paths resolve
against the root. For example, with `listen.socket = "local.sock"`:

```sh
osmia status --root ~/.osmia --socket local.sock --json
```

Clients never load configuration, runtime, trace or project files. They obtain
the active project and effective values over HTTP using the service client.
Changing socket configuration requires restarting the service and updating the
client's `--socket` argument.

Project and workstream arguments are persistent IDs (`p_` or `w_` followed by
32 lowercase hexadecimal digits), not display names. Workstream IDs must be known
to the service's record repository. The standalone entry point does not
discover workstreams. The runtime commands store controls. A pause holds new
worker turns in its scope while chief-of-staff turns still run; priority orders
which workstream starts units and takes freed turn slots first.

## Output and exit codes

All client commands accept `--json`. Status returns an object with `health`,
`configuration`, `runtime` and `status` API responses, where `status` lists
every workstream except the librarian's with its full status (`null` before
the first) and each role's effective profile and source;
`status <workstream-id>` returns that workstream's status response; profiles returns the runtime
response. `abandon` returns the API's abandon response: `project`,
`workstream`, `state` and `reason`. `send` returns the accepted message entry and `conversation` the
API's conversation response (see [conversation](service.md#conversation)).
`inbox` returns the API's inbox response and `answer` its answer response (see
[inbox and rulings](service.md#inbox-and-rulings)).
Mutations return `mutation` (the API acknowledgement) and `runtime`
(the subsequent effective-state response). Project commands return the API's
project response: the project view (ID, name, upstream, fork, clone, base
branch, trace and charter paths) and `next_step`; `project extract` returns
the project view and the pending extraction. `handin` returns the API's
hand-in response: `project`, `workstream`, `state`, `handed` (the absolute
path of the copy), `source`, and `skip_debate` when the hand-in skipped
debate. Output is one JSON value plus
newline, without progress text. Profile map keys are sorted in human output and
JSON. The responses are separate API requests, not an atomic snapshot.

Human mutation output identifies the affected scope and prints effective controls,
including defaults after clearing overrides. If the mutation succeeds but reading
the effective state fails, stderr says it was acknowledged; inspect status before
retrying. Failures leave stdout empty and write actionable diagnostics to stderr.
Raw configuration/parser and server error text is omitted from failure messages.
Project, hand-in, abandon, single-workstream status, send, conversation, inbox and answer failures print the service's
message, which names the field, project or workstream ID or path at fault and
never raw file contents.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure |
| 2 | Invalid command, flags, arguments or root |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection, no project is configured, unknown project or workstream, empty charter on hand-in, or stdin over the hand-in limit or not UTF-8 |
| 5 | API conflict (including an extraction already running or abandoning a delivered or abandoned workstream), project already active, unsupported operation, restart required or internal failure |
| 6 | Foreground startup/service failure, including ownership conflict |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6, check configuration and runtime validity and permissions, stop any
existing owner, and ensure the socket path is unused or stale. Do not delete a
live-owned socket. Unsupported responses identify the M1 limit; restart-required
responses instruct the operator to stop and start the service.

Detached management, install/upgrade commands, completion, web/tailnet and reload
are unavailable. The command examples in
the design describe the eventual product; this reference lists the implemented
surface.
