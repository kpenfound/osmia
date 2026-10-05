# Command line

[Getting started](getting-started.md) walks through installing, configuring and
running `osmia` for the first time. To build from source instead, use
`go build -o osmia ./cmd/osmia` or `go install ./cmd/osmia`. A top-level
`config.toml` with profiles and no project is enough to start; see the
[configuration](configuration.md).

```sh
osmia doctor
osmia serve
# In another terminal:
osmia status
osmia status w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef
osmia trace w_0123456789abcdef0123456789abcdef criterion spec#1
osmia trace w_0123456789abcdef0123456789abcdef unit parser
osmia trace w_0123456789abcdef0123456789abcdef commit 0123456789abcdef0123456789abcdef01234567
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project memory p_0123456789abcdef0123456789abcdef
osmia project extract p_0123456789abcdef0123456789abcdef
osmia project rebase p_0123456789abcdef0123456789abcdef
osmia project remove p_0123456789abcdef0123456789abcdef
osmia handin p_0123456789abcdef0123456789abcdef design.md
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
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia config
osmia reload
osmia status --json
```

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
  active project
  and its trace path (or that none is configured), its charter state (ready or
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
  workstream even before a status is written. Reading the charter records any edit you made to
  it; see [charter](charter.md).
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
- `send <workstream-id> <message>` sends a message to the workstream's chief
  of staff. The message is one argument; quote it. The service records it
  before answering, and the output names the message's turn ID and its state
  (`queued`). The chief of staff answers it as its next turn, after any turn
  already running. A message to an abandoned workstream is recorded but never
  answered; the next service start completes it as cancelled and
  `conversation` lists it as `failed`. An empty message, or a workstream the active project does
  not hold, fails with `validation` (exit 4); with no project configured it
  fails with `no_project` (exit 4).
- `conversation <workstream-id>` lists the owner's messages to that
  workstream's chief of staff and its final responses, oldest first. Each
  entry shows when it was sent or answered, who wrote it, its turn ID and the
  turn's state (`queued`, `running`, `done` or `failed`), then its text
  indented. It fails like `send`.
- `inbox` lists every owner decision waiting for you across the workstreams
  of the active project, oldest first: escalated questions, ratification
  packets, contested units, presented amendments, held drift rebases,
  delivery approvals, failing publications, notices held from the chief of
  staff and loop guard pauses.
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
  asked of GitHub until you restart `serve` with the token. Any other failing
  publication is listed once three attempts in a row have failed: its question carries the last failure, such as GitHub's
  refusal with the token permissions it asks for, and it says when the next
  attempt runs. It takes no answer; fix the cause and it leaves the inbox once
  an attempt publishes. A held drift rebase, whose conflict resolution review
  sent back `shed.max_bounces` times, is answered with `project rebase`, which
  asks for another, or by asking the chief of staff with `send` to hand it
  back with a note its drift mason and reviewer receive; it leaves the inbox
  once either is recorded.
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
- `archive <workstream-id>` archives a delivered or abandoned workstream. It
  leaves the list of work: `status` names it on its `Archived:` line, the web
  page moves it to its Archived group, and `status <workstream-id>` marks it
  `archived=true`. Nothing is deleted: the trace, `handed/` and any branch
  stay, and the archive is recorded in the root's `runtime.json`. Archiving an
  archived workstream changes nothing. A workstream in any other state fails
  with `conflict` (exit 5); abandon it first.
- `unarchive <workstream-id>` returns an archived workstream to the list of
  work. A workstream that is not archived is left as it is.
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
  unshown criteria first, and the drafted pull request description, then any
  approved description and the state of its publication. For a delivered
  workstream it shows the report, the approved description and the pull
  request it was delivered as.
- `approve <workstream-id> [description-file] [--message-file FILE | --messages-file FILE]` approves the drafted
  description, or the file's text instead, for the presented final report.
  `--message-file` supplies the commit message when delivery has one commit.
  For multiple commits, `--messages-file` reads a JSON array of
  `{"commit":"<presented revision>","message":"<edited text>"}` objects, in the
  order returned by `delivery --json` in `messages`. Include every commit.
  The service attributes the outgoing commits to your Git identity, removes
  agent co-author trailers, adds your sign-off and signs before pushing the
  branch to your repository or fork and opening the pull request; `delivery` shows its progress. A report with gaps, a presentation
  that changed and a delivered workstream fail with `conflict` (exit 5). After
  a publication was refused, approving again asks for another one.
- Editing the documents needs no command. You edit `spec.md` and `plan.json` in
  the workstream's directory under the trace yourself. The service records what
  you changed as a new revision of yours before any turn reads it, and the next
  round debates it. The two are one draft: if what you leave does not validate,
  neither file is recorded and neither is debated, your chief of staff reports
  the problems, and the recorded revisions stay. Until you correct them, a
  redraft by the architect of a file you edited is given up rather than written
  over your edit.
- `project add <name> --upstream OWNER/REPO [--fork OWNER/REPO] --clone PATH
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
  attempt. Adding a
  project while one or more are active is refused and the error names an
  active project. Repeating an active project's exact registration returns it
  again.
- `project extract <project-id>` starts a new
  knowledge-base extraction of the active
  project: a librarian turn that rewrites `kb/<subsystem>.md` and
  `kb/entities.json` from the clone. The command returns once the extraction
  is recorded as pending; follow it with `status`. A project ID that is not the
  active project fails with `not_found` (exit 4). While an extraction is still
  pending or running, another is refused with `conflict` (exit 5) naming the
  extraction to wait for.
- `project rebase <project-id>` asks for a drift rebase
  of every `building` or `assembled` workstream of the active project that is
  not paused, whatever its `upstream_rebase` cadence. The command returns once
  the request is recorded and lists each covered workstream with the number of
  the drift rebase that answers it (`Covered: <workstream> drift rebase <n>`),
  and each skipped workstream with why (`Skipped: <workstream>: <reason>`). The
  foreman runs the drift rebases on the project's lander once no landing or
  other drift rebase holds it; follow them with `status`. A request also
  resumes a workstream whose drift rebase is held. A project ID that is
  not the active project fails with `not_found` (exit 4).
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
  creates nothing. The service then asks the architect for the spec and plan. With `--skip-debate`
  the hand-in also skips debate, for small work: once the architect's draft is
  valid, the workstream enters the shed without a committee, no round runs, and
  the spec and the plan wait for your `ratify`. The output then ends with
  `Debate: skipped; ratify the spec and the plan once they are drafted`.
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
Project, hand-in, abandon, single-workstream status, send, conversation, inbox, answer and reload failures print the service's
message, which names the field, project or workstream ID or path at fault and
never raw file contents.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure, or a failed `doctor` check |
| 2 | Invalid command, flags, arguments or root |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection, no project is configured, unknown project or workstream, empty charter on hand-in, or stdin over the hand-in limit or not UTF-8 |
| 5 | API conflict (including an extraction already running or abandoning a delivered or abandoned workstream), project already active, unsupported operation, restart required or internal failure |
| 6 | Foreground startup failure, including ownership conflict, or a failure that stopped a running service |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6 at startup, run `osmia doctor`, which names the cause and its fix. Do not delete a
live-owned socket. When a running service stops after a failure, stderr (the
`service.log` of a detached service) names it on one line after the time it
stopped, starting with the project whose work failed. Credentials in URLs and
the `GITHUB_TOKEN` value are redacted. Unsupported responses identify unavailable operations; restart-required
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
project. Dependent requests open in the push repository. After the dependency integrates,
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
runs and opens each trace the way `serve` does, which completes a trace
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
