# Command line

A project has one charter and one or more repos. A feature request runs as a
workgroup with one shared chief of staff; each member workstream targets one
repo, feature branch and pull request. Repo settings, KB and entities are
independent. The project pane is the web entry point, with member views as
drill-downs and exploratory sessions outside workgroups.

[Getting started](getting-started.md) walks through installing, configuring and
running `osmia` for the first time. To build from source instead, use
`go build -o osmia ./cmd/osmia` or `go install ./cmd/osmia`. A top-level
`config.toml` with profiles and no project is enough to start; see the
[configuration](configuration.md).

```sh
osmia doctor
osmia serve
# In another terminal:
osmia design open p_0123456789abcdef0123456789abcdef
osmia design list
osmia design send w_0123456789abcdef0123456789abcdef "Keep the public API compatible."
osmia design read w_0123456789abcdef0123456789abcdef
osmia design start w_0123456789abcdef0123456789abcdef
osmia status
osmia status w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef criterion spec#1
osmia trace w_0123456789abcdef0123456789abcdef unit parser
osmia trace w_0123456789abcdef0123456789abcdef commit 0123456789abcdef0123456789abcdef01234567
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project add-repo p_0123456789abcdef0123456789abcdef --upstream owner/other --clone ~/src/other
osmia project repos p_0123456789abcdef0123456789abcdef
osmia project memory p_0123456789abcdef0123456789abcdef
osmia project extract p_0123456789abcdef0123456789abcdef
osmia project rebase p_0123456789abcdef0123456789abcdef
osmia rebase w_0123456789abcdef0123456789abcdef
osmia project remove p_0123456789abcdef0123456789abcdef
osmia edit charter p_0123456789abcdef0123456789abcdef
osmia handin p_0123456789abcdef0123456789abcdef design.md
osmia edit spec w_0123456789abcdef0123456789abcdef
osmia edit plan w_0123456789abcdef0123456789abcdef --from plan.json
osmia handin p_0123456789abcdef0123456789abcdef small-fix.md --skip-debate
osmia handin p_0123456789abcdef0123456789abcdef dependent.md --base w_0123456789abcdef0123456789abcdef
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
osmia shed redraft w_0123456789abcdef0123456789abcdef "Split the resume unit in two."
osmia ratify w_0123456789abcdef0123456789abcdef
osmia amendment w_0123456789abcdef0123456789abcdef 1
osmia amendment w_0123456789abcdef0123456789abcdef 1 approve "Checkpoints are what we meant."
osmia session open p_0123456789abcdef0123456789abcdef --profile default --title "Explore the upload API"
osmia session commit w_0123456789abcdef0123456789abcdef "Try batching the retries."
osmia session check w_0123456789abcdef0123456789abcdef
osmia session push w_0123456789abcdef0123456789abcdef
osmia session end w_0123456789abcdef0123456789abcdef
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia config
osmia reload
osmia status --json
```

## Design Sessions

`osmia design open <project-id>` opens a project design conversation. The
agent receives the charter and every repo's knowledge base, and can read
committed files from any repo in the project. It cannot change source files,
use version control, access delivery credentials or take owner gates. It works
without Hearsay. Exploratory `osmia session` chats remain separate and keep
their writable worktrees.

Use `design send` and `design read` to discuss intent. The readiness pass asks
only about expensive-to-undo choices, genuine forks without a clear signal, or
tension with intent. Each question has a recommended answer; other inferred
assumptions appear in a collapsed list on the web page and can be overridden
by replying. The brief states decisions, intent and tradeoffs, and rejected
options.

`osmia design start <design-id>` hands the latest brief to the architect in
one owner action. A transcript accompanies the brief for reference; the brief
wins where they differ. Starting is refused until a brief exists or while a
turn is unfinished. The same durable thread continues as the workgroup chief
of staff's conversation, accessible with `osmia send` and `osmia conversation`.
In a project with several repos, the architect selects the affected repos and
the service creates their members automatically. The original design ID keeps
working for conversation, packet and ratification commands. One ratification
approves the shared project spec and every member; interrupted sealing remains
pending for the whole group until all members finish. Delivery approvals stay
per workstream.
The web page's **Open a design session** and **Start workgroup from brief** offer
the same flow, using `POST /v1/design` and `POST /v1/design/{workstream}/start`.
All design commands support `--json`.

## Dependencies, delivery and retention

The architect's workgroup graph must be acyclic. A **contract** edge (the
preferred default) resolves when its interface is in the ratified project
spec. A **hard block** starts no dependent unit before the upstream PR merges.
A **merge-order** edge allows parallel building, but keeps the dependent PR a
draft until the upstream merges. The service polls GitHub for merges; an
owner-recorded merge with `osmia merged` also counts. Merge is the only release
signal; tags, versions and agent completion do not release dependencies.

While waiting, dependent roles receive the upstream branch or commit. After
its own delivery approval, a published dependent PR is a draft linking the
upstream PR and stating the dependency. When the merge is recorded, the
dependent updates its reference to the merged commit, repairs check or review
failures within its slice, re-runs checks and final review, and presents a fresh
delivery approval before pushing and marking ready. No new owner-started
workstream is needed for that repair; only a design gap needs escalation.
Expected waits appear in status, such as `waiting on owner: repo-a#123 merge`,
and create no inbox entry or notification.

Every PR needs its own workstream's owner delivery approval of the description
and commit message, then service-owned signing as the owner. This covers draft
publication, updates and marking ready. There is no group-wide delivery
approval or pre-authorized update. Final-review acceptance remains a separate
gate. The factory never merges a PR, releases it or pushes a base branch.
The project pane shows planning, ready, building, paused, blocked (naming its
wait), PR open, delivered (merged) and cancelled. Stored workflow state
`delivered` also records PR publication, so it alone does not prove merge.
A ready but unmerged PR keeps its member unresolved, just as a draft does.
Resolution requires cancellation/abandonment or delivery with an owner-recorded
merge or a recorded merged outcome matching the current publication's PR.
A closed but unmerged PR, an outcome for an earlier PR and delivery without
merge evidence remain unresolved. This includes one-member workgroups;
their delivery cleanup keeps its existing trigger, separate from resolution.
The service observes every delivered member's current PR at most once per
minute, including members without a same-repo base or downstream edges. A
matching merged or closed outcome stops observation. Failed lookups are
retried without recording an outcome; closed without merge remains unresolved.

On member resolution (merged or cancelled), its own slice, spec, plan and
workspaces leave active state and bundles. The project spec remains active
for the shared chief and all unresolved members until every member resolves,
including a dependent waiting on or repairing after a merge. At both levels,
trace history is retained until owner archive. Archiving and eligible cleanup
of one terminal member preserve the survivor's original conversation and
project spec across restart; missing live history yields a truthful error or
hold and does not count as a merge. Archive and cleanup never establish a merge
or resolve an unmerged publication.
An archived member without pinned merge or cancel evidence keeps the group
unresolved and its project spec active; the pane shows **blocked** with a
missing-evidence diagnostic rather than **delivered (merged)**.

**Open question:** integration-gate edges await an owner ruling on how a combined
test of both branches runs. They are not supported and the architect must not
propose them.

## Amend and cancel

Send an amend or cancel request to the shared chief in any member's conversation,
including the original continued design chat:

```sh
osmia send <workstream-id> "Amend the workgroup: keep the API compatible and add resumable uploads."
osmia send <workstream-id> "Cancel this workstream; show the dependent impact."
osmia conversation <workstream-id>
```

The committee debates only the amendment delta. The chief presents one impact
summary; approve or reject it in the same chat. This is the amend's only owner
approval prompt; later publication approvals remain per workstream.

| Item | Impact | Action |
| --- | --- | --- |
| Unit | Section unchanged | Preserve its state and commits |
| Unit | Completed, section changed | Revision unit with new commits on the same branch |
| Unit | Section removed | Obsolete; revision unit removes its code |
| Unit | New section | New unit |
| Workstream | Not started | New slice and replan |
| Workstream | In progress | Pause at next unit boundary, then rework |
| Workstream | PR open | Same branch and PR; fresh review and individual delivery approval |
| Workstream | Merged | New follow-up workstream in the workgroup |

Contract sections cannot be changed inside a slice. Contract amendments cascade
along edges; reopened units queue behind their dependencies. Unaffected work
continues. Cancel can target one member or the entire group. Its impact summary
includes dependent cancellation or re-planning, and at cancel time asks whether
open PRs are closed or left as drafts. Merged work is never reverted; undoing it
is a new request. Stops occur at whole-unit boundaries, with unlanded work kept
on unit branches. Pending approved operations retain their exact pins through
archive and restart; unavailable retired-history effects stay explicitly held.
These conversation controls do not introduce a separate `cancel` command.

## Migration

Startup wraps every existing one-repo project in a default project, retaining
its ID, charter and repo settings. Building, delivered, abandoned and archived
non-session workstreams each appear in a one-workstream workgroup using their
existing chief thread. Exploratory sessions stay outside workgroups with the
same kind, thread and exclusion from factory passes. Traces, rulings, decisions
and archive records are preserved without owner action. Migration resumes after
a crash and repeated starts neither lose nor duplicate records. See
[configuration](configuration.md#project-and-workgroup-migration).

## Commands and flags

- `serve [--detach] [--root PATH]` validates and starts the service. Detached
  mode waits for readiness and appends output to the private root `service.log`.
  `stop` requests local shutdown over the Unix socket. SIGINT and
  SIGTERM drain requests and release ownership and the socket. A live owner is
  refused; a provably stale socket is recovered automatically. The service
  runs the librarian, architect, committee and chief-of-staff turns in each role's
  configured sandbox. A startup failure prints no detail; run `doctor`.
- `doctor [--json] [--root PATH]` checks what `serve` needs; see
  [doctor](#doctor).
- `status` shows health, loaded configuration digest/root, each configuration
  file on disk against the loaded configuration as `config` prints it, the
  active projects and repos
  and their trace paths (or that none is configured), its charter state (ready or
  empty, rule count, recorded revision and numbering diagnostics), its latest
  knowledge-base extraction (number, `pending`, `running`, `succeeded` or
  `failed`, the time of its last activity and the reason when it failed or
  waits to retry), diagnostics, the last failed reload (`Last reload failed at
  <time>: <file>: <field>: <reason>`) until a reload succeeds, effective
  runtime controls and the project's
  context mode (`file`, a normal mode), with
  `budget.per_day` today's known spend against it (`Daily budget: USD <spend>
  of USD <limit> spent on <day>`, marked `at least` with the count of attempts
  whose cost is unknown), today's
  provider usage (`Provider usage on <day>:`,
  then per provider `<provider>: USD <spend> known spend`, marked `at least`
  like the daily budget, with `Limit: <status> kind=<kind> until <time>` or
  `until cleared` while a usage limit is in force, `Fallback: <role> runs
  <profile> in place of <configured>` and `Paused: <role> has no fallback
  available` for the roles the limit affects, and an `unattributed:` line for
  costs no attempt names a provider for), the
  capacity (`Capacity (per workstream <n>):`, then
  `<role>: <used> of <limit> slot(s) in use` for mason, reviewer and
  committee, each followed by `Waiting: <workstream> turn <turn> of <agent>
  (<reason>)` or `Waiting: <workstream> unit <unit> (<reason>)` for the work
  waiting for a slot), each
  role and profile whose latest turn attempts failed with infrastructure
  failures (`Infrastructure failures: <role> on profile <profile> failed <n>
  time(s) in a row; last at <time>: <failure>`), the
  workspace backend new workstreams get
  (`Workspaces: setting=<setting> new_workstreams=<backend> jj=<version>`,
  `none` when `workspaces = "jujutsu"` finds no supported `jj`, which a
  diagnostic explains), then each
  workstream with its state, open question count, context mode and workspace
  backend (`workspaces=git` or `workspaces=jujutsu`), its latest
  drift rebase (`Drift: rebase <n> <outcome> at <time>`) once it has one, and the
  chief of staff's goal and attention (`Attention: none` when nothing needs
  you), or `no status yet`. Archived workstreams are named on one line,
  `Archived: <workstream-id>...`, instead. Open owner gates appear under the
  workstream even before a status is written. Write the charter with
  `osmia edit charter`; see [charter](charter.md).
- `status <workstream-id>` shows one workstream of the active project: the same
  facts and owner gates, its latest drift rebase with its reason and one
  `Moved:` line for each of its upstream moved events, then `Units:` with one line per unit of the sealed plan or follow-up and its state
  and the latest available card (headline, happened and any owner action) beneath it
  once the workstream is building, and for a landed unit its commit on the
  feature branch, the reviewed candidate and base with the approval that
  landed it, and the criteria it meets, then the full status (goal, attention, note, one line per active
  agent, and when it was written), or `Status: none yet` before the chief of
  staff writes one. `Live agents:` below the narrative lines shows each active
  or parked turn's role, optional unit, state, RFC 3339 start time, elapsed
  seconds, effective profile, optional attempt number and `resume` or `replay`
  path, and a parked question's trace number. A workstream the project does not hold fails with
  `not_found` (exit 4); with no project configured it fails with `no_project`
  (exit 4).
- `trace <workstream-id> [unit <id>|criterion <spec#n>|commit <full-sha>]`
  walks the active project's durable trace through the service API. With no
  selector it lists sealed revisions, criteria, units and delivery. Selectors
  show revision identifiers, provenance, evidence and explicit gaps. `--json`
  returns the complete typed response. Delivered and abandoned workstreams
  remain readable. A missing selector returns `not_found` (exit 4); malformed
  input returns `validation` (exit 4).
- `send <workstream-id> <message>` sends a message to the workstream's shared workgroup chief
  of staff. The message is one argument; quote it. The service records it
  before answering, and the output names the message's turn ID and its state
  (`queued`). The chief of staff answers it as its next turn, after any turn
  already running. A message to an abandoned workstream is recorded but never
  answered; the next service start completes it as cancelled and
  `conversation` lists it as `failed`. An empty message, or a workstream the active project does
  not hold, fails with `validation` (exit 4); with no project configured it
  fails with `no_project` (exit 4).
- `conversation <workstream-id>` lists the owner's messages to its
  workgroup's shared chief of staff and its final responses, oldest first. Each
  entry shows when it was sent or answered, who wrote it, its turn ID and the
  turn's state (`queued`, `running`, `done` or `failed`), then its text
  indented. It fails like `send`.
- `inbox` lists every owner decision waiting for you across the workstreams
  across active projects, oldest first: escalated questions, ratification
  packets, contested units, presented amendments, held drift rebases,
  delivery approvals, failing publications, parked bases, notices held from
  the chief of staff and loop guard pauses.
  Questions escalated together are one entry, headed by its inbox number, when
  it was escalated, its workstream and batch; any other entry is headed by its
  kind, when it opened, its workstream and its unit or amendment. Each shows
  the question (the chief of staff's rephrasing of an escalation, the
  service's statement of any other decision), what is blocked, the options
  (`none until what blocks it is resolved` for a ratification packet an
  objection blocks), the recommendation when there is one and each escalated question as its asker
  put it. Then it shows the revision the decision is taken on (the packet
  revision and the spec and plan revisions it names, the amendment packet
  revision, or the final review, report revision and commit) and the command
  that answers it: `answer`, with `answer <number> --accept` when there is a
  quick reply, `ratify`, `contested`, `amendment`, or `delivery` then
  `approve`. A publication the service holds because it was started without
  `GITHUB_TOKEN` is listed as soon as it is asked for: nothing is pushed or
  asked of GitHub until you restart `serve` with the token, or you merge the
  branch into upstream yourself and record it with `merged`. Any other failing
  publication is listed once three attempts in a row have failed: its question carries the last failure, such as GitHub's
  refusal with the token permissions it asks for, and it says when the next
  attempt runs. It takes no answer; fix the cause and it leaves the inbox once
  an attempt publishes. A final review whose project checks did not complete
  three times in a row is listed with the last failure and when the checks run
  next; it takes no answer, and it leaves the inbox once a run completes. A
  held drift rebase, whose conflict resolution review
  sent back `shed.max_bounces` times, or whose drift mason or reviewer turn
  failed or ended without a verdict, is answered with `rebase`, which
  asks for another, or by asking the chief of staff with `send` to hand it
  back with a note its drift mason and reviewer receive; it leaves the inbox
  once either is recorded.
  A parked base is a workstream whose base workstream, or an ancestor of it,
  was abandoned or lost its branch; nothing of it runs but the chief of staff
  and the check of its base. When its own base was abandoned it is answered
  with `upstream`; otherwise it leaves the inbox once the base is available
  again, or you abandon the workstream.
  Notices are held once the chief of staff's turns delivering them failed five
  times in a row; `send` a message once the cause is fixed, and they follow
  its answer. A loop guard pause, set when a workstream ran
  `loop.max_sessions` sessions without progress, is answered with `resume`. Decided entries, superseded packet revisions and entries of an
  abandoned workstream are left out. Without a project or a trace the inbox is
  empty.
- `answer <inbox-number> <ruling>` records your ruling on an inbox entry. The
  ruling is one argument; quote it. The service records it before answering;
  the chief of staff then rephrases it for every asker of the entry, whose
  threads resume with it as their next turn. The number is the one `inbox`
  shows; it stays the entry's number for the life of the project. A number
  that is not a positive integer is a usage error (exit 2). An empty ruling or
  a number no entry carries fails with `validation` (exit 4); with no project
  configured it fails with `no_project` (exit 4). An entry that already has a
  ruling, or belongs to an abandoned workstream, fails with `conflict`
  (exit 5) and records nothing. While several projects are active, inbox
  numbers are counted per project: `answer` finds the project whose open
  escalation carries the number, and when more than one does it fails with
  `validation` (exit 4), listing them; `--project <project-id>` names the
  project to answer in, with or without `--accept`.
- `answer <inbox-number> --accept` records the entry's eligible `quick_reply`
  as your ruling through the same path. When the entry has no eligible quick
  reply, or the inbox lists no escalation with that number because it is
  already ruled, belongs to an abandoned workstream or does not exist, it
  fails with `validation`
  (exit 4), explains that you must give a ruling explicitly, and records
  nothing. A number that is not a positive integer, or a ruling given as well,
  is a usage error (exit 2). The inbox JSON response includes
  `quick_reply` for clients to inspect before offering acceptance.
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
  decision with `send`.
- `contested <workstream-id> <unit> <review|revise> <note>` records your
  direction for a contested unit shown by `status`. Quote the required note.
  The workstream's chief of staff rules on a contest first when it is
  confident, and raises the rest to your inbox with its findings; you can
  rule on any contested unit yourself.
  `review` requests another reviewer verdict on the same candidate; `revise`
  returns the findings and your note to the mason. For a mason contest,
  `status` shows whether the mason gave up or exhausted the clean-turn bound;
  only `revise` is accepted, resuming its thread in the existing workspace
  with a fresh clean-turn allowance. Neither decision approves a unit.
- `move <workstream-id> <sketched|in-shed|ratified|building|assembled|delivered|abandoned> <note>`
  moves a workstream the state machine left stuck or wrong. Quote the
  required note. Before ratification, `sketched` asks the architect for a
  redraft with your note once debate has concluded, `in-shed` asks for one
  more round of debate, and `ratified` ratifies the latest ratification
  packet; `abandoned` abandons the workstream with your note as the reason.
  Those are the same actions as `shed redraft`, `shed more`, `ratify` and
  `abandon`, with the same refusals. `assembled` asks for a fresh final review
  of the branch as it is, which the reader receives with your note: use it
  after a final review failed or the branch changed under it. It moves a
  `building` workstream whose units have all merged, reruns the review of an
  `assembled` one, and returns a `delivered` workstream whose pull request is
  open to final review. `building` reopens an `assembled` workstream, or a
  `delivered` one whose pull request is open, on its existing feature branch.
  It stays building until a revision filed after the move is decided and its
  units land, or until you move it to `assembled`, and its final review then
  runs again. The open pull request is updated only after that final review
  and your delivery approval. `delivered` returns a reopened workstream to
  delivered while its feature branch is still what its open pull request
  carries. A workstream whose pull request is merged or closed, or whose merge
  you recorded with `merged`, does not reopen. A move the workstream's state
  refuses, or one made while its publication is in flight, fails with
  `conflict` (exit 5). The chief of staff moves a workstream to `building` or
  `assembled` on its own judgment. It moves it to any other state only when
  you ask it to with `send`. After three final reviews with blocking findings,
  reopening is yours to decide.
- `move <workstream-id> <unit> <implementing|checking|reviewing|approved|contested> <note>`
  moves a unit the state machine left stuck or wrong. The unit must have
  started and not merged: it is implementing, checking, reviewing, approved,
  waiting or contested. Quote the required note. `implementing` gives the mason
  a turn with your note and a fresh clean-turn allowance in its existing
  workspace; `checking` runs the checks again on the recorded candidate;
  `reviewing` has the reviewer review the recorded candidate again with your
  note; `approved` records your approval of the recorded candidate, which the
  foreman lands; `contested` holds the unit in your inbox. Moving a unit to the
  state it is in restarts that stage. A unit the foreman is landing or rebasing
  fails with `conflict` (exit 5), as does a unit that has not started or has
  merged; an unknown unit fails with `not_found` (exit 4). The chief of staff
  moves units on your behalf too, within the limit it shares with its
  contested rulings, and moves a unit when you ask it to with `send`.
- `abandon <workstream-id> <reason>` abandons a workstream of the active
  project that is neither delivered nor abandoned. The reason is one argument;
  quote it. The service records the move to `abandoned` with you as actor and
  your reason, cancels the workstream's running turns (their recorded work is
  kept), completes its queued turns as cancelled, and runs no turn of the
  workstream again, including after a restart. The trace, `handed/` and every
  branch stay. Once its turns are done, the service removes the workstream's
  workspaces, committing each unmerged unit's files to that unit's branch
  first, and its session directories, agent transcripts included. `status`
  then shows the state `abandoned`. An empty reason, or a workstream the
  active project does not hold, fails with `validation` (exit 4); a delivered
  or already abandoned workstream fails with `conflict` (exit 5).
- `rebase <workstream-id>` asks for a drift rebase of one `building` or
  `assembled` workstream, or a delivered workstream with an open pull request,
  that is not paused, whatever its project's
  `upstream_rebase` cadence: the foreman fetches upstream, or the base
  workstream's branch, and rebases the feature branch and every unit in flight
  onto it. The command returns once the request is recorded and names the
  drift rebase that answers it (`Drift rebase <n> requested for workstream
  <workstream>`). Asking again before that drift rebase starts names the same
  one. The foreman runs it on the project's lander once no landing or other
  drift rebase holds it; follow it with `status <workstream-id>`. A request
  also resumes a workstream whose drift rebase is held. A workstream that is
  not eligible for drift, or is paused, fails with `conflict` (exit 5) and
  why. The web page offers the same as Check for drift in a building or
  assembled or delivered workstream's menu. Drift checks for delivered PRs run only
  on request; workgroup merge detection still polls GitHub for dependencies. A changed branch passes conflict
  resolution and final review, then asks for fresh owner approval before
  updating the existing remote branch and PR. An unchanged branch needs no
  approval or push; closed or merged PRs cannot be updated. The workstream
  stays delivered. `project rebase` asks the same of every eligible
  workstream of a project.
- `upstream <workstream-id> <base-workstream-id>` continues a workstream
  parked on its abandoned base from upstream instead. The base is the
  abandoned workstream the inbox entry names. The service fetches the
  project's base branch and checks that it already holds the changes of the
  base the workstream builds on: merging the commit of the base the feature
  branch was last rebased onto, or the base's branch before the workstream is
  sealed, into upstream must be clean and change nothing, so squash and
  rebase merges count. It then records upstream as the workstream's base and
  releases the park, with you as actor, and the workstream's next drift
  rebase replays its own commits onto upstream and moves its seal there. The
  output names the upstream commit and the integrated base commit. A
  workstream that is not parked, whose base is another workstream or is not
  abandoned, or whose base's changes upstream lacks or conflicts with, fails
  with `conflict` (exit 5) and stays parked.
- `archive <workstream-id>` permanently archives delivered or abandoned work.
  The archived list retains its title, ID, outcome, archive time and, where one
  exists, its delivery link (the pull request URL, or the recorded upstream
  commit for an owner-recorded merge); `status <workstream-id>` marks it
  `archived=true`. The service then deletes the workstream's own independent
  storage, its dedicated trace repository and its temporary workspaces,
  including handed inputs, decisions, turns and complete check output, from
  disk. It does not rewrite or prune any Git history. The archive record
  remains in `runtime.json`, and target-repository branches remain. Archive
  pins `resolution_evidence` as `merged`, `cancelled` or `unresolved`, and
  `resolved` is true only with merged or cancelled evidence. An older flag
  without evidence is re-derived while history is available; otherwise the
  member remains unresolved with a visible diagnostic. Archive
  cannot be undone; `unarchive` returns a conflict. A repeated archive request
  changes nothing. Other states fail with `conflict` (exit 5); abandon the
  workstream first. Previously archived workstreams receive the same one-time
  cleanup automatically, with their title, outcome and delivery link filled in
  while the trace is still present. Cleanup is deferred, with a logged reason,
  while the workstream has unfinished turns, pending or unacknowledged
  operations or a temporary workspace still being cleaned, while a retained
  workstream depends on it as a base, or until the knowledge base has
  incorporated its learnings; failures are retried, and an interrupted deletion
  is resumed, not mistaken for a live trace. The web archived list distinguishes
  pending cleanup from a deleted trace.
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
- `ratify <workstream-id>` ratifies the spec and plan of a one-member workgroup
  in the shed. For a multi-repo workgroup it covers the shared project spec,
  graph and every slice and plan in the packet, with the same command and
  `POST /v1/ratify/{workstream}` request shape. Invalid graphs or incomplete
  member coverage are refused before writes. Interrupted sealing reconciles
  from the exact approved pins; all members remain effectively pending and
  unable to build or release contract edges until every pinned seal completes.
  No additional repo-selection or graph approval is required. It reads your ratification packet, pins the revisions the packet
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
  (exit 4).
- `amendment <workstream-id> <n>` shows amendment `n` of a building or
  assembled workstream: its state, the debate round it reached, the packet the
  chief of staff presented with its revision, and your latest decision.
  `amendment <workstream-id> <n> <approve|reject|round|overrule> [note]`
  decides it. The command reads the packet first and decides exactly that
  revision, so a packet presented again since is refused. `approve` versions
  the proposed spec and/or plan and reseals; it is refused while a charter
  veto, size or acceptance objection stands. `overrule` approves over the
  objections that stand and records them. `reject` leaves the sealed spec and
  plan in force. `round` asks the committee for one more bounded debate round,
  up to `shed.max_rounds` rounds in all, after which the chief of staff
  presents the amendment again. The requester receives your ruling as its next
  turn and the unit it parked resumes its stage. The note is optional and is
  one argument; quote it. Deciding the same packet revision again the same way
  prints the recorded decision; another decision on it, an amendment that is
  not presented, and a refused approval fail with `conflict` (exit 5). An
  unknown amendment fails with `not_found` (exit 4). You can also tell the
  workstream's chief of staff your decision with `send`.
- `delivery <workstream-id>` shows an assembled workstream's final report,
  its summary, blocking then advisory findings, and every criterion with the
  merged units that serve it, unserved first, and the drafted pull request
  description, then any
  approved description and the state of its publication. For a delivered
  workstream it shows the report, the approved description and the pull
  request it was delivered as, or the merge you recorded with `merged`.
- `approve <workstream-id> [description-file] [--message-file FILE | --messages-file FILE] [--accept]` approves the drafted
  description, or the file's text instead, for the presented final report.
  A report with blocking findings or an unserved criterion is approved only
  with `--accept`, which accepts them as they are; the approval records what
  it accepted.
  `--message-file` supplies the commit message when delivery has one commit.
  For multiple commits, `--messages-file` reads a JSON array of
  `{"commit":"<presented revision>","message":"<edited text>"}` objects, in the
  order returned by `delivery --json` in `messages`. Include every commit.
  The service attributes the outgoing commits to your Git identity, removes
  agent co-author trailers, adds your sign-off and signs before pushing the
  branch to your repository or fork and opening the pull request; `delivery` shows its progress. A report with blocking findings approved
  without `--accept`, a presentation that changed and a delivered workstream
  fail with `conflict` (exit 5). After
  a publication was refused, approving again asks for another one.
- `merged <workstream-id>` records an assembled workstream delivered because
  you merged its feature branch into upstream yourself, outside the factory.
  The service fetches the project's base branch and checks that it already
  holds the changes of the feature branch tip: merging the tip into upstream
  must be clean and change nothing, so fast-forward, merge, squash and rebase
  merges count. It then records the merge in `final/merge.json` and moves the
  workstream to delivered, with you as actor. It pushes nothing and opens no
  pull request, and needs neither a final review nor a delivery approval. The
  output names the upstream commit and the branch tip it holds. A publication
  waiting for `GITHUB_TOKEN` is refused in the same step and leaves the inbox.
  A workstream that is not assembled, whose feature branch upstream lacks or
  conflicts with, or whose publication the service could still carry out
  fails with `conflict` (exit 5) and stays assembled. The web page offers the
  same as Record my merge upstream in an assembled workstream's menu.
- `edit charter <project-id> [--from FILE]`, `edit spec <workstream-id>
  [--from FILE]` and `edit plan <workstream-id> [--from FILE]` edit the
  project's charter, or a workstream's spec or plan before ratification. The
  command reads the latest revision and opens it as `charter.md`, `spec.md` or
  `plan.json` in a new temporary directory with `$VISUAL`, or else `$EDITOR`,
  attached to the terminal. With `--from` it reads the edited content from
  that file instead and opens no editor. Without `--from` and with neither
  variable set it fails with exit 4 and asks you to set `EDITOR` or use
  `--from`; an unreadable `--from` file is exit 2, and an editor that exits
  with an error is exit 1 and keeps the file. Unchanged content prints `No
  changes.` and sends nothing. Otherwise the command saves the content as your
  next revision of the one it read and prints the new revision number
  (`Recorded spec.md revision 3.`); the next round debates an edited spec or
  plan. The spec and the plan are one draft: editing one sends the other
  unchanged at its current revision, and a draft that does not validate is
  refused with `validation` (exit 4), naming the problems, and records
  nothing. A revision recorded since the command read it fails with
  `conflict` (exit 5), as does a spec or plan that is already ratified. On
  either failure the edited file is kept and its path printed: run the
  command again with `--from <that path>` to retry against the latest
  revision. A successful save removes the temporary directory. The web page
  edits the same documents through the same API.
- `project add <name> --upstream OWNER/REPO [--fork OWNER/REPO] --clone PATH
  [--base-branch NAME]` registers a project with the running service: it
  validates the request, generates the project ID, writes
  `projects/<id>/config.toml`, creates the trace repository with a charter
  template and an entity map seeded from the clone's tracked files, lists the ID in
  `active_projects`, requests the librarian's first knowledge-base extraction
  and activates the project without a restart. The clone path is made
  absolute by the client and must be an existing local Git repository outside
  the Osmia root; nothing is written to it. The output names the project ID,
  the trace path and the next step: writing the charter with
  `osmia edit charter <project-id>`. The extraction runs in the service after the command
  returns; `status` shows its state, and the project is usable whether it
  succeeds or fails; after a failure `osmia project extract` starts a new
  attempt. Adding a
  project while others are active is supported with a separate clone. Repeating
  an active project's exact registration returns it again.
- `project add-repo <project-id> --upstream OWNER/REPO [--fork OWNER/REPO]
  --clone PATH [--base-branch NAME]` adds a repo to the default project's ID.
  It uses the same validation and recoverable registration as `project add`,
  creates the repo's own configuration, trace, KB and entity map and shares
  the existing project charter. The API is `POST /v1/projects/repos`.
- `project repos <project-id>` lists that project's repos, marking its default
  repo and showing each repo's own upstream, fork, clone and KB location.
  `--json` returns the repo views from `GET /v1/config`.
- `project extract <repo-id>` starts a new
  knowledge-base extraction of the active
  repo registration: a librarian turn that rewrites `kb/<subsystem>.md` and
  `kb/entities.json` from the clone. The command returns once the extraction
  is recorded as pending; follow it with `status`. A project ID that is not the
  active project fails with `not_found` (exit 4). While an extraction is still
  pending or running, another is refused with `conflict` (exit 5) naming the
  extraction to wait for.
- `project rebase <repo-id>` asks for a drift rebase
  of every `building` or `assembled` workstream of that active repo that is
  not paused, whatever its `upstream_rebase` cadence. The command returns once
  the request is recorded and lists each covered workstream with the number of
  the drift rebase that answers it (`Covered: <workstream> drift rebase <n>`),
  and each skipped workstream with why (`Skipped: <workstream>: <reason>`). The
  foreman runs the drift rebases on the project's lander once no landing or
  other drift rebase holds it; follow them with `status`. A request also
  resumes a workstream whose drift rebase is held. A project ID that is
  not the active project fails with `not_found` (exit 4).
- `project remove <project-id>` takes the active project out of
  `active_projects` and closes that repo registration's runtime state. Remove
  additional repos before removing the default repo. Its trace repository, every
  workstream's own trace repository and the clone are kept. Adding the same
  upstream again afterwards creates a new project ID and new trace
  repositories; see [configuration](configuration.md#project-registration).
- `handin <project-id> <path|issue-url|-> [--base WORKSTREAM] [--skip-debate]` hands one input to the active
  project and initially creates a one-workstream workgroup in the named repo,
  in state `handed`, with the document as its brief. The input is a file
  (made absolute by the client and read by the service), a GitHub issue URL
  (`https://github.com/OWNER/REPO/issues/NUMBER`, fetched by the service), or
  `-` for stdin (read by the client, at most 512 KiB of UTF-8 text; the client
  refuses larger or non-UTF-8 stdin itself with exit 4, before sending a
  request, and that message has no `validation:` prefix). The service records
  the input in the workstream's trace as its handed document. The output names
  the workstream ID, its state and the recorded source. The client sends a new
  idempotency key with each command, so a request the API retries creates one
  workstream, and running the command again creates another. A project whose
  charter has no rules fails with `charter_empty` (exit 4), naming the project
  and the `osmia edit charter` command that writes it. A project ID that is not the active project
  fails with `not_found` (exit 4). An unreadable, empty, non-UTF-8 or oversized
  input, or a URL of another shape, fails with `validation` (exit 4); an issue
  the service cannot fetch fails with `internal` (exit 5). A refused hand-in
  creates nothing. With one active repo, the architect drafts the spec and plan
  directly. With two or more active repos, the architect first selects the affected
  repos from the document and project context, through the same path as a design
  start. Go validates and durably records the selection before allocating exactly
  one member per selected repo. Empty, duplicate or outside-project selections
  are refused before allocation. A selection turn that finishes without a valid
  selection leaves the start invalid and allocates no members.
  Selecting only the named repo keeps the original workstream. Selecting one
  other repo creates one member there. Either single selection gets the ordinary
  ratification packet; several selected repos get the project spec, contracts,
  graph, slices and plans, with one all-member ratification. Every member receives
  the immutable input, and the hand-in's original chief-of-staff conversation
  continues as the group's one shared thread. Selection, allocation, conversation
  adoption and sealing recover across restarts without duplication. There is no
  repo-selection approval or additional owner step.
  With `--skip-debate` the hand-in skips debate, for small work: once the
  architect's draft is
  valid, the workstream enters the shed without a committee, no round runs, and
  the spec and the plan wait for your `ratify`. The output then ends with
  `Debate: skipped; ratify the spec and the plan once they are drafted`.
  The skip applies to every selected member. Each member still needs its own
  final-review acceptance and delivery approval before publication.
- `pause <all|project-id|workstream-id> [--hard] [--reason TEXT]` stores an
  owner-attributed pause; the default mode is soft. A soft pause holds new work
  and lets running turns finish; `--hard` also stops the turns running in its
  scope, which continue after `resume`. The
  status and command output show each pause's source, reason and UTC set time.
- `resume <all|project-id|workstream-id>` clears that scope's pause. Parent pauses
  still apply; all effective pauses are displayed.
- `priority set <workstream-id>...` stores the ordered list of the project the
  workstreams belong to; while several projects are active the project is
  found from the first workstream.
  The chief of staff sets the same list when you ask it to in a message.
  `priority clear` removes that preference; while several projects are active
  it fails with `validation` (exit 4) and lists the active project IDs. Both,
  and `pause`/`resume` of a workstream, need an active project and say so
  otherwise; a workstream's pause names the project that holds it, and a
  workstream in no active project fails with `not_found` (exit 4).
- `profiles` displays every role's effective binding and its source
  (`configuration` or `owner_override`), plus runtime diagnostics. `status`
  shows the same profile information.
  `profiles set <role> <profile>` overrides a binding.
  `profiles clear <role>` restores its configured default.
- `config` shows the loaded configuration's digest and root, then one line
  per configuration file comparing it on disk with the loaded configuration
  (`Config file: <path>[ (project <id>)]: unchanged`, `changed`, or
  `invalid: <field>: <reason>`; see [disk drift](running.md#disk-drift)),
  the configuration diagnostics and the last failed reload. Reading it applies
  nothing; `reload` does. It takes no argument.
- `reload` asks the service to [reload its configuration](running.md#reload)
  and prints `Configuration reloaded: <digest>`, followed by `Restart required
  to apply: <settings>` when the files change settings that only a restart
  applies. A file that fails validation fails with `validation` (exit 4),
  naming the file and field; the loaded configuration stays in force.
- `session open <project-id> --profile <name> [--title TEXT]` opens a
  [session](design.md#68-sessions): a direct chat with one agent on its own
  worktree, outside the rest of the factory. `--profile` is required and
  must be one the service already runs mason turns on for the project; an
  unknown profile fails with `validation` (exit 4), recording nothing, and
  a project ID that is not a registered project fails with `not_found`
  (exit 4). The output names the
  session's workstream ID, its fixed profile, its branch and the base
  commit its worktree starts from (the project's default branch head at
  opening time). Sending the agent a message is not a command; it happens
  through the web page or the API.
- `session commit <session-id> <message>` commits every current change in
  the session's worktree onto its branch under your message, which is one
  argument; quote it. The output names the commit the branch now points at.
  It is refused, recording nothing, while the agent is mid-turn or the
  session has ended (`conflict`, exit 5), and on an empty message or a
  worktree with nothing to commit (`validation`, exit 4). The client sends a
  new request key with each command, so a request the API retries produces
  one commit rather than a second one.
- `session check <session-id>` checks the session's latest commit. With the
  Jev boost on, it runs the same smart-check selection a unit's own check run
  uses, scoped to what changed between the session's starting commit and its
  head; with the boost off, it runs the full `dagger check` the service runs
  before every review. The output names the run number, the checked commit,
  whether the run was smart or full (with its selected checks when smart),
  its status and exit code. Only one check per session runs at a time; a
  second request while one is still running, or one sent after the session
  has ended, fails with `conflict` (exit 5).
- `session push <session-id>` pushes the session's branch to the project's
  remote under its own branch name, using credentials the service holds. It
  never pushes to the default branch, never forces and never opens a pull
  request. It is refused, recording nothing, when the remote branch carries
  commits the session branch does not reach, or once the session has ended:
  both `conflict` (exit 5).
- `session end <session-id>` ends a session. It is refused while the agent
  is mid-turn (`conflict`, exit 5). Once ended, the worktree and the agent's
  stored context are released and the output shows the session's state as
  `ended`; its branch, commits, chat and trace stay readable, and every
  later message, commit, check and push against it fails with `conflict`
  (exit 5).
- A session ID is the workstream ID `session open` returned; a session ID
  not held by any registered project, or the workstream it names is not a
  session, fails with `not_found` (exit 4).

Every command accepts `--root PATH`, defaulting to `~/.local/share/osmia` with
its top-level file at `~/.config/osmia/config.toml` (see
[root and identity](configuration.md#root-and-identity)). An explicit root holds
its own `config.toml`. Flags may occur
before or after positional arguments; value flags also accept `--flag=value`.
Duplicate and unknown flags are errors. `--help` prints the supported syntax.
`--version` prints the stamped [release](release.md) version and commit and
exits 0 without contacting the service; like `--help`, it takes precedence
over a command.

Clients connect to `osmia.sock` under the root. If configuration specifies another
socket name, pass `--socket PATH` to each client command; relative paths resolve
against the root. For example, with `listen.socket = "local.sock"`:

```sh
osmia status --socket local.sock --json
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
response; `config` returns the API's configuration response; `reload` returns the API's reload response: `digest` and
`restart_required`. `abandon` returns the API's abandon response: `project`,
`workstream`, `state` and `reason`. `send` returns the accepted message entry and `conversation` the
API's conversation response.
`inbox` returns the API's inbox response and `answer` its answer response.
Mutations return `mutation` (the API acknowledgement) and `runtime`
(the subsequent effective-state response). Project commands return the API's
project response: the project view (ID, name, upstream, fork, clone, base
branch and trace path) and `next_step`; `project extract` returns
the project view and the pending extraction. `handin` returns the API's
hand-in response: `project`, `workstream`, `state`, `source`, and
`skip_debate` when the hand-in skipped debate. `edit` has no JSON output. Output is one JSON value plus
newline, without progress text. Profile map keys are sorted in human output and
JSON. The responses are separate API requests, not an atomic snapshot.

Human mutation output identifies the affected scope and prints effective controls,
including defaults after clearing overrides. If the mutation succeeds but reading
the effective state fails, stderr says it was acknowledged; inspect status before
retrying. Failures leave stdout empty and write actionable diagnostics to stderr.
Raw configuration/parser and server error text is omitted from failure messages.
Project, hand-in, edit, abandon, single-workstream status, send, conversation, inbox, answer and reload failures print the service's
message, which names the field, project or workstream ID or path at fault and
never raw file contents.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure, an `edit` editor that exited with an error, or a failed `doctor` check |
| 2 | Invalid command, flags, arguments or root, or an unreadable `edit --from` file |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection (including an `edit` draft that does not validate), no project is configured, unknown project or workstream, empty charter on hand-in, stdin over the hand-in limit or not UTF-8, or `edit` without `--from` while neither `VISUAL` nor `EDITOR` is set |
| 5 | API conflict (including an extraction already running, abandoning a delivered or abandoned workstream, or an `edit` of a revision that has since changed or a ratified draft), project already active, unsupported operation, restart required or internal failure |
| 6 | Foreground startup failure, including ownership conflict, or a failure that stopped a running service |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6 at startup, run `osmia doctor`, which names the cause and its fix. Do not delete a
live-owned socket. For exit 6, the cause differs by how `serve` was invoked.
The `serve --detach` launcher reports a child that never became ready as
human-readable text on the terminal that started it, one line naming the
cause after the time it happened, with credentials in URLs and the
`GITHUB_TOKEN` value redacted. A foreground `osmia serve`, or the detached
child itself stopping after it started, reports its own failure as an
error-level record in its [structured log](running.md#service-log) — its own
standard error, or `service.log` for the detached child — with the same
redaction. Unsupported responses identify unavailable operations; restart-required
responses instruct the operator to stop and start the service.

Detached management, install/upgrade commands and completion are
unavailable; the service serves the [page](running.md#web-page) with the
inbox, answered inline, the active work, each workstream's conversation and
the controls. The command examples in
the design describe the eventual product; this reference lists the implemented
surface.

`project memory <project-id>` prints a read-only Hearsay setup export as JSON:
configuration fragments, scope/entity mappings and anchor artifact handles.
It uses the local trace and contacts no memory service. Merge and validate the
fragments when configuring Hearsay; see [memory setup](hearsay.md).

A hand-in's `--base <workstream-id>` selects an available feature on the same
repo. In a project with several active repos, the affected-repo selection must
include that repo, and only its member inherits the base. Dependent requests
open in the push repository. After the dependency integrates,
delivery maintenance requires a fresh review and owner approval before opening
the upstream request, or retargeting the existing request when no fork is
configured. Publication relationships remain in the trace.

When `fork` is omitted, Osmia pushes its feature branch to `upstream` and
opens the pull request there against `base_branch`. A fork equal to upstream
(case-insensitive) is normalized to omission. The clone can have just an
`origin` remote matching upstream. Existing configurations with a separate
fork continue to push there. Osmia never pushes to the configured base branch
or merges the pull request.

For dependent workstreams without a fork, the pull request initially targets
the parent's feature branch. After the parent integrates, fresh review and
owner delivery approval authorize retargeting that same pull request to the
project's base branch with the approved description. Interrupted updates are
reconciled against the recorded request before retrying.

## Doctor

`osmia doctor` checks what `osmia serve` needs and what turns need once it
runs, without a service, and prints what it found grouped by area. It reads
the root and the configuration directly, the way `serve` does.

| Group | Checks |
| --- | --- |
| `root` | The root is a writable directory, or does not exist yet; whether a service owns it; `service.log`, when present, is a private regular file; the socket is free or stale. |
| `config` | The top-level `config.toml` and every active project's `config.toml` load and validate; `listen.web`, when set, can be bound. |
| `toolchain` | `git`; `jj`, required for `workspaces = "jujutsu"` and a warning for `auto`; every agent a role's profile or fallback chain names; `docker` with its daemon answering, when a role uses `sandbox = "container"`; `sbx`, when a role uses `sandbox = "sbx"`; `dagger`, which reviewer checks run, as a warning. |
| `projects` | Per project: the clone is a Git work tree; it has a remote naming the upstream, which answers and has the base branch; with a fork, a remote naming the fork, which answers. |
| `github` | `GITHUB_TOKEN` is set, as a warning, and GitHub accepts it; it can read each project's upstream and fork. |
| `state` | Per project: the trace opens; `runtime.json` opens. |

A failure (`✗`) is something that stops the service from starting or its work
from running. A warning (`!`) will probably bite but stops nothing. Every
warning and failure prints its fix on the next line. Configuration errors name
the file and field, never a value read from the file.

Checks that need something an earlier check found missing are left out: with
no root only the toolchain and GitHub checks run, and with a configuration
that does not load neither do the project checks. While a service owns the
root, the `state` checks and the `listen.web` check are left to it; `osmia
status` reports them. Otherwise doctor holds the root's ownership lock while it
runs and opens each trace the way `serve` does: it resumes any incomplete
migration from the shared trace layout to a dedicated repository per
workstream, and completes a trace
publication a stopped service left unfinished.

```
$ osmia doctor
root
  ✓ root directory                              /home/kyle/.local/share/osmia
  ✓ ownership lock                              no service owns this root
  ✓ socket                                      /home/kyle/.local/share/osmia/osmia.sock is stale; serve removes it

config
  ✓ config.toml                                 /home/kyle/.config/osmia/config.toml (2 profiles, 1 active projects)
  ✓ project p_0123456789abcdef0123456789abcdef  widgets (acme/widgets)

toolchain
  ✓ git                                         /usr/bin/git (git version 2.50.1)
  ! jj                                          jj is missing: exec: "jj": executable file not found in $PATH; new workstreams use Git worktrees
      → install jj 0.45.0 or later for Jujutsu workspaces, or set workspaces = "git"
  ✓ claude                                      /usr/local/bin/claude (2.1.251 (Claude Code))
  ✓ dagger                                      /usr/local/bin/dagger (dagger v0.20.5)

projects
  ✓ widgets clone                               /home/kyle/src/widgets
  ✗ widgets upstream                            remote "origin" has no branch main
      → set base_branch in the project's config.toml to a branch of acme/widgets

github
  ✓ GITHUB_TOKEN                                authenticates as kyle
  ✓ widgets acme/widgets                        readable by kyle

state
  ✓ widgets trace                               /home/kyle/.local/share/osmia/projects/p_0123456789abcdef0123456789abcdef (3 workstreams)
  ✓ runtime.json                                /home/kyle/.local/share/osmia/runtime.json

15 checks: 13 passed, 1 warnings, 1 failed
```

`--json` prints the checks as a JSON array of `group`, `name`, `status`
(`pass`, `warn` or `fail`), `detail` and `remediation`. Doctor exits 1 when a
check failed and 0 when only warnings are present.
