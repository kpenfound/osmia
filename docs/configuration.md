# Configuration

`internal/config.Load` reads and validates the entire declarative configuration.
It returns a configuration only when the top-level file and every active
repo configuration pass. It creates no directories, repositories, sockets or state and does
not launch agents. The local service owns startup and the Unix
socket. Registration through `osmia project add` writes
the project file and edits `active_projects`; see [project registration](#project-registration).
A running service applies edited files through [reload](running.md#reload).
Startup performs the recoverable migrations described below.

## Projects, repos and workgroups

A project is exactly one repo with its own charter, upstream, optional fork, clone, base branch, landing style, caps and entity map. Its `p_…` ID identifies it in `active_projects` and in API requests. Its authoritative charter is `charter.md` in `projects/<project-id>/trace.db`, edited with `osmia edit charter <project-id>` or the web UI. Charters and entity maps are independent for every project.

A workgroup represents one feature across any number of selected active projects. It has one member workstream per project, one shared chief, one workgroup spec, contracts and graph, and one ratification. The workgroup appears under each member's project card and sidebar group row, with members labelled by project name. Briefs, workgroup specs, slices and all factory state stay outside target repositories. File-based context supports the whole flow without Hearsay.

Each member's own charter governs its slice, plan and units. The workgroup spec and contracts must satisfy every member's charter; the architect, committee and chief receive them all. `charter#n` means the citing member's rule, and `<project>/charter#n` names another project's rule. Unknown projects or rules are refused. Conflicting charters go to the owner and cannot be dismissed by the chief. A proposed charter amendment is recorded and decided in the project whose rule it changes.

The owner chooses an ordered project list at hand-in: API `projects` (`project` alone is a one-element shorthand), repeatable CLI `--with <project-id>`, or the web multi-select of all active projects. The first project is home for the shared chief and conversation. Go rejects an empty or duplicate list and inactive projects; paused projects are allowed. Several projects allocate and join one member each without an architect selection turn. One project retains the ordinary workflow. The list stays fixed until ratification; changing it requires abandoning and handing in again. After ratification, amend and cancel retain their existing rules. A selected project judged to need no change is escalated to the owner rather than dropped.

The design agent uses the `chief_of_staff` profile, overrides and sandbox configuration, with no separate `design` binding. Its tools are exactly `file_read`, `read_repo`, `draft_brief` and `set_title`, without write, VCS or credential access. `set_title` sets the open session's title and nothing else. It sees a catalog of every active project's name, upstream and in-brief status. Brief projects' charter and entity map are in context; other active projects' are files in its view. `read_repo` reads any active project and refuses others.

A session's brief starts with its own project, including older briefs without a list. Optional `draft_brief.projects` replaces the list under the hand-in validation rules; omitting it retains the list. The session's own project can be removed while another remains. An owner request confirms the change; inferred changes require a readiness question and owner agreement. The web's read-only Brief panel shows the latest revision, project names with home marked and collapsed text above Start, updating live. Design read returns `brief_revision`, `brief`, `projects` and, once the agent has set one, `title`; CLI read prints the revision and list, and CLI start reads and prints them before sending the pin. Start sends the displayed/read `brief_revision`; a stale pin returns `conflict` and allocates nothing. The web surfaces the conflict and refreshes the panel.

Start durably binds the handed brief text, revision and ordered project list. Recovery completes that exact start once with stable members, or leaves it safely retryable without allocation. The session's project retains the conversation and continued shared chief even when it is not a member. See [design sessions](cli.md#design-sessions).

The shared design is called the **workgroup spec**. Stored document paths such as `project-specs.json`, API fields such as `project_spec` and `project_spec_revision`, and the CLI flag `--project-spec-revision` keep their names. These names do not introduce another project grouping model.

Edges are recorded in the ratified workgroup graph, not configured in TOML.
A contract edge resolves on its ratified contract; hard-block and merge-order
edges resolve only on the upstream PR's detected or owner-recorded merge.
Merge is the only release signal: no tag, package-version or ecosystem settings
exist. Expected waits are status only, without inbox entries or notifications.
Every draft publication, PR update and ready action still requires its own
workstream's owner delivery approval and owner signing. Integration-gate edges
remain an open question pending an owner ruling on the combined-test mechanism;
they are unsupported and the architect must not propose them.

Amend and cancel are requested in the workgroup conversation and decided by
one owner-approved impact summary. Contract changes cascade to dependents;
unchanged units stay untouched, changed completed units get revisions, removed
sections get removal units and new sections get new units. Active work stops
at a unit boundary; open PRs keep their branch and PR and need fresh individual
delivery approval, while merged changes need a follow-up workstream. Cancel
asks whether open PRs are closed or left as drafts, includes dependent impact
and never reverts merged work. These are recorded operations, not configuration
switches. See [amend and cancel](cli.md#amend-and-cancel).

## Root and identity

`Options.Root` takes precedence over the default root. An explicit root is
self-contained: its top-level file is `<root>/config.toml`. The default layout
follows the XDG base directories:

| File | Default | Base directory |
| --- | --- | --- |
| top-level `config.toml` | `~/.config/osmia/config.toml` | `$XDG_CONFIG_HOME/osmia` |
| root | `~/.local/share/osmia` | `$XDG_DATA_HOME/osmia` |

Each XDG variable is used only when it is an absolute path; there is no
Osmia-specific environment variable. The default top-level file may be a
symlink, for example into a dotfiles repository; it is read and edited at its
target. Relative explicit roots resolve against the process working
directory. `~` and `~/` use the user's home; `Options.Home` supplies an absolute
home for embedding and tests. Other users' tilde syntax is rejected. Paths are
absolute and cleaned, and existing symlink ancestors are resolved without requiring
the final directory to exist.

Project and repo IDs are `p_` followed by 32 lowercase hex digits; workstream IDs use `w_`, and workgroup IDs use `g_`.
`NewProjectID` and `NewWorkstreamID` generate random 128-bit keys. Persist the key
once; changing a display name, upstream or clone does not change it. Parsing accepts
only the canonical format, so traversal, absolute names and case aliases cannot
become identities. `CheckProjectIDs`/`CheckWorkstreamIDs` reject duplicates with
`ErrCollision`; constructors also accept existing IDs to check. Generation does
not reserve a key: persistence callers must reserve it atomically and retry
generation on collision. The traces reserve project and workstream
identities when creating their directories: a project's own trace directory
for its ID, and a workstream's own trace directory for its ID. Workstream keys are not a
configuration list; they belong to persisted workstream manifests.

```text
<root>/config.toml                             top-level file of an explicit root; the default is ~/.config/osmia/config.toml
<root>/runtime.json                            also holds archive records of permanently archived workstreams
<root>/osmia.sock
<root>/tailnet/                                embedded Tailscale node state, when listen.tailnet is set
<root>/project-add.json                       journal of one interrupted project registration
<root>/notifications.json                     ledger of owner notifications, once notify.webhook has been set
<root>/slot-intervals.json                    capacity slots held or waited for at the last stop, once telemetry is on; a restart sends their spans with the original start
<root>/service-intervals.json                 the pool limit intervals and uptime of the running service, while [telemetry] is on
<root>/projects/<repo-id>/config.toml
<root>/projects/<repo-id>/trace.db             project charter, entity map and lasting decisions
<root>/projects/<repo-id>/osmia.lock           the trace's process lock
<root>/workstreams/<repo-id>/<workstream-id>/  the workstream's own trace.db and osmia.lock, separate from projects/<repo-id>/
<root>/workgroups/<workgroup-id>/              trace.db and osmia.lock: cross-repo record, workgroup spec, shared chief and group decisions
<root>/workstreams-staging/                    a new workstream's trace is built here before an atomic rename publishes it
<root>/workstreams-deleting/<repo-id>/      a workstream's trace archive cleanup has moved aside, durably marked for removal
<root>/branches/<repo-id>/<workstream-id>  the workstream's feature branch, a workspace of the clone
```

A root left over from a version that had the Beekeeper, the service's former
service-wide assistant, may still carry a `<root>/beekeeper/` directory,
alongside the layout above. The service no longer reads or writes it; it can
be deleted.

Each trace is one SQLite database, `trace.db`, beside its write-ahead log
(`trace.db-wal`) and shared-memory index (`trace.db-shm`). No other trace file
exists on disk. A trace still kept as a Git repository is migrated to its
database the next time the service opens its project; see
[trace migration](#trace-migration).

`Root` provides checked helpers for these paths. Managed state paths reject
symlink aliases, including aliases within the root that could collapse two
identities; an explicit root's `config.toml` is managed the same way. The clone
and root must be separate, non-nested directories, including after resolving
symlinks. Relative clone paths resolve from the project configuration
directory; missing clones are accepted for configuration purposes. Loading
configuration writes nothing in the clone. Helpers check existing paths, not future filesystem changes;
state writers must protect against concurrent symlink replacement.

### Project and workgroup migration

At startup, each repo of a multi-repo project becomes an independent project, keeping its existing `p_` ID. Each split-off charter copies the group's current rules and numbers as a charter revision naming its source project and source charter revision. The owner edits each charter separately afterwards. Startup removes `group_id` from every configuration file, preserving every other byte, including comments. It is no longer a supported configuration field. The split is crash-safe and idempotent: interrupted startup resumes without duplicate charter revisions. Existing workgroups keep their identity, members, approvals, revision pins and shared chief without another ratification.

Existing one-repo projects keep their ID, paths, charter and settings. The project/workgroup migration puts every non-session workstream, including building, delivered, abandoned and archived records, in its own workgroup with the existing chief thread. Exploratory sessions keep their kind and thread outside workgroups and remain excluded from factory passes. Traces, rulings, decisions and archive records are preserved without owner action. Repeated starts do not duplicate records or require moving existing member traces.

An in-flight design start keeps its recorded selection. A start with no selection waits with an inbox entry for the owner's choice, validated like hand-in before continuing. New hand-ins and design starts record selection durably with the handed input.

The commands `osmia project add-repo` and `osmia project repos` and the web Add repo control are unavailable: use `project add` for each repo, `GET /v1/config` for the projects and the hand-in project list for shared work. The grouping registration route `POST /v1/projects/repos` is not served. Intake uses no architect selection turn, `tools/design-selection.json` or `deliver_affected_repos` tool. This migration is separate from trace migration below.

### Trace migration

A trace with a `.git` directory and no `trace.db` is a Git trace, and is
migrated to its database before its project opens or is scheduled; other
projects keep running meanwhile. `osmia serve` and `osmia doctor` both run the
migration. It covers the project's trace and every workstream trace of the
project, and each cross-repo workgroup's trace with its host workstream. For
each trace it:

1. takes the Git trace's lock, `.git/osmia.lock`;
2. finishes an interrupted publication left in `.git/osmia-publication.json`;
3. records pending hand edits as owner revisions: a project's `charter.md`, and
   a workstream's `spec.md` and `plan.json` once the draft passes the same
   validation an edit through the API does. An invalid draft stops that
   workstream's migration, and the project does not open, with an error naming
   the files and the problems; fix or revert the file and restart, and nothing
   is lost;
4. imports HEAD into `trace.db.import` in the same directory: every JSONL line
   as a record, in the order the trace read its files, the workflow, and
   `project.json`, `workstream.json`, role notes and inbox numbering as files.
   Every other file HEAD holds must be the latest revision of a document, or
   the migration stops naming it. Bytes in the work tree that were never
   committed are dropped, and each dropped path is logged;
5. verifies the import record by record against HEAD, with the decoded
   workflow, the files and the database's integrity;
6. checkpoints, closes, flushes and renames the import to `trace.db`. From then
   on `trace.db` is the only authority, and a `.git` beside it is ignored;
7. removes `.git` and every other file of the directory except `trace.db`,
   `osmia.lock` and, in a project's trace, `config.toml`.

A crash before the rename discards `trace.db.import` and starts again; a crash
after it resumes the cleanup. A trace still on the shared layout, with a
`workstreams/` directory inside the project's trace, or with
`.git/osmia-migration-reclaim.json`, is refused with an error naming it.
Workstreams already in `workstreams-deleting/` are deleted as they are.
Progress, completion and any failure are logged, naming the project and
workstream and never trace content or credentials.

## Top-level config.toml

This minimal configuration supplies an agent model and no project yet:

```toml
version = 1
active_projects = []

[profiles.default]
agent = "claude"
model = "your-model-name"
```

Only IDs in `active_projects` are loaded; the list may be empty or absent, and
the service then starts without a project until `osmia project add` registers
one. With several IDs the service runs every listed one-repo project at once,
each with its own trace, reconciliation loop, lander and drift cadence. Role capacity
(`capacity.*`), budgets and the factory-level pause are shared by all of them:
a turn in flight on any project takes a slot of its role kind. Each repo
counts its own slots in use, waiting turns and daily spend from its own trace
and publishes them, so the shared limits and the status's capacity and daily
budget follow each repo's latest pass. Each repo's
`capacity.per_workstream` applies to its own workstreams. Runtime project-scoped
controls apply only to that project's members, including in cross-project workgroups.
A paused project or one at its capacity or `git_operations` limit holds only
its own member; the others continue as far as graph edges allow. Additional directories
may hold archived
traces and are not scanned or activated. Duplicate IDs are errors. The
top-level file, and every active repo's file, must exist and specify
`version = 1`; one repo file that fails to load fails the whole load. Unknown versions, keys (including empty unknown
tables and case variants), duplicate TOML keys, malformed TOML and incorrect
types are errors.

The following optional settings show their defaults:

```toml
# The workspace backend of new workstreams: "auto", "git" or "jujutsu".
workspaces = "auto"

[listen]
# Default: <resolved-root>/osmia.sock, including when the root is overridden.
# A relative value resolves against the root; ~/ is also supported.
socket = "osmia.sock"
# Optional loopback host:port serving the same API over TCP; empty disables.
web = ""
# Optional hostname under which the service joins your tailnet and serves the
# same API on HTTP port 80 and HTTPS port 443; empty disables.
tailnet = ""
# Also serve HTTPS using Tailscale-issued certificates; restart to change.
tailnet_https = true

[capacity]
masons = 4
reviewers = 2
committee = 3
checks = 2
per_workstream = 2

[shed]
max_rounds = 3
max_bounces = 3

[committee]
perspectives = ["correctness", "integration", "scope"]
# Empty: every member runs on roles.committee's profile.
profiles = []

[mason]
max_clean_turns = 3

[loop]
max_sessions = 30
stall_after = "45m"

[events]
window = "5s"
progress_window = "15m"

[notify]
# Optional absolute http or https URL that each new owner decision is posted
# to as plain text; empty sends nothing.
webhook = ""

[skills]
# How stale a skill clone may get before a turn that needs it pulls it:
# "never", "always" or a duration.
refresh = "24h"

[transcribe]
# The full OpenAI-compatible transcription endpoint recorded audio is posted
# to; empty turns Whisper transcription off.
url = ""
# The model name sent with each request.
model = "large-v3-turbo"
# The environment variable holding an optional bearer key.
api_key_env = "WHISPER_API_KEY"

[telemetry]
# The OpenTelemetry collector traces are sent to, as host[:port]; empty (the
# default) turns telemetry off and builds no exporter.
endpoint = ""
# "grpc" (default) or "http/protobuf".
protocol = "grpc"
# Connect without TLS.
insecure = false
# The environment variable holding optional export headers, as
# comma-separated name=value pairs.
headers_env = ""

[projects]
# The GitHub user account new project repositories are created under; it must
# be the account GITHUB_TOKEN authenticates as. Empty keeps creation off.
repo_owner = ""
# The absolute directory new clones are made in; ~/ is expanded. Empty keeps
# creation off.
clone_dir = ""

[roles.mason]
profile = "default"
sandbox = "none"
skills = []
```

A top-level file left over from a version that had the Beekeeper may still
carry a `[beekeeper]` section. The loader accepts and ignores it: it loads
without error and has no effect, so the section can be deleted at your
convenience.

The socket must be a direct child of the resolved root with a `.sock` suffix,
separate from managed state. Its absolute path may contain at most 103 bytes
for portability across Unix socket implementations. An existing path must be a
Unix socket; the loader neither binds nor removes it. `listen.web` is
`host:port`, where the host is `localhost` or a loopback address such as
`127.0.0.1` or `[::1]`, and the port is a number from 0 to 65535 (0 binds a
free port). An empty host, any other address or hostname, and a missing or
named port are errors that name `listen.web`. The listener has no
authentication: anyone who can connect to the host's loopback interface can
use the API ([web listener](running.md#web-listener)). A browser on the
same machine opens the [page](running.md#web-page) at `http://<listen.web>/`. `listen.tailnet` is
one DNS label of 1 to 63 lowercase letters, digits and hyphens that neither
starts nor ends with a hyphen, such as `osmia`; anything else is an error that
names `listen.tailnet`. With it set, the service joins your tailnet through
embedded Tailscale, keeps the node's state in `<root>/tailnet`, and logs in
with the auth key in the `TS_AUTHKEY` environment variable or, without one,
logs a login URL on first run, in its [structured log](running.md#service-log).
The auth key never belongs in these files.

`listen.tailnet_https` defaults to `true`: with `listen.tailnet` set, the node
also serves HTTPS on port 443 at its full MagicDNS address,
`https://<host>.<tailnet>.ts.net/`. Set `listen.tailnet_https = false` to opt out.
Changing it requires a restart, just like `listen.tailnet`. Enable **HTTPS
certificates** in the tailnet admin console before using it. Issuing a
certificate publishes the node's `*.ts.net` name in public Certificate
Transparency logs: the name becomes public, but access does not. HTTPS setup
or listener failures leave HTTP on port 80 serving; status reports HTTPS as
unavailable with a reason, and a failed certificate request is retried (see
[Tailnet listener](running.md#tailnet-listener)). HTTP does not redirect to
HTTPS.

Dictation in conversation composers records in the browser, which allows
microphone capture only over HTTPS or localhost, and transcribes the recording
through the Whisper server named by [`[transcribe]`](#optional-whisper-transcription).
See [dictation](running.md#dictation) for use in conversation composers.

Tailnet membership is the boundary: there is no in-app authentication
([tailnet listener](running.md#tailnet-listener)). All capacity and shed values
must be positive integers. `events.window` is a positive Go duration: how long
an undelivered event of a workstream waits before the service delivers it, with
every other ready event, as one chief-of-staff turn. `events.progress_window`
is how long an event of routine progress waits instead: a unit starting,
passing its checks, being approved or sent back, or landing, or the workstream
changing state. It goes out sooner with any other event. It must be no shorter
than `events.window`, and defaults to the longer of `15m` and `events.window`.
The notice that asks the chief of staff to title work started from a design is not routine progress: it waits `events.window`. While an event turn of a workstream is queued or running, its new events wait
for the next turn. A workstream cap may exceed global mason capacity:
the global pool still limits concurrent execution.

`capacity.checks` must be positive, defaults to 2 and limits service Dagger check
runs across all projects and workstreams, including unit and session
runs. A run waits cancellably for a free slot in arrival order before selecting
and running checks. Waiting does not consume `checks_timeout` or count as an
incomplete run.
Check slots are independent of the model-role and per-workstream limits, so
other work continues while checks wait. Reloading applies a changed limit to
new starts immediately; executing runs finish even when the limit is lowered.

`mason.max_clean_turns` bounds the continuations of a mason turn that ended
without an accepted report: the clarify turn after a clean ending without a
tool call, the turn after a refused `done`, which states the refusal reason, and
the turn after a failure, which says what ended the turn. A card problem
never refuses a report, so a refusal comes only from the report itself. Past the
bound the default is to contest the unit, with a notice that names the reason;
the bound sets the router's default and does not stop it, so Jev may instead
give the mason another turn with a fresh allowance, and a departure records its
reason. It also bounds the reminders a mason, or a drift mason,
gets when it reports done with conflict markers left; past it the default is to
contest the unit or hold the drift rebase, with the same departures.
`loop.max_sessions` is how many agent
sessions a workstream may run without progress before the loop guard pauses
it, and it stays a hard limit for the moves Jev routes: every request to Jev is
one of those sessions, and a paused workstream is offered no target that starts
agent work. A change of state of the feature, a unit, the shed, an amendment,
the final review or the publication, a drift rebase that moved the branch, or
anything you do counts as progress; a routed exit that moves nothing, a hold or
a followed default that stays where it is, does not. The pause is soft, attributed to the loop
guard, and listed in the inbox; resuming the workstream starts the count over.
It must not be negative, and `0` turns the guard off.

`loop.stall_after` is a Go duration: how long a unit or a workstream may go
without progress before the stall watchdog raises a `stalled` exit for it to
the transition router, which asks Jev only what to do about it. The default is
`45m`; `"0"` turns the watchdog off, and a negative value is refused. Progress
is the same as the loop guard counts, except that nothing the chief of staff
does counts and neither does a drift rebase. The watchdog raises nothing while
something legitimate explains the quiet: an open owner card about the subject,
a pause that covers the subject, a running or queued turn younger than
`stall_after`, an operation in flight, or a dependency wait on an upstream that
has itself progressed within `stall_after` or is explained. A card of a
workstream explains a unit's quiet only when it holds the whole workstream's
work. Workgroup changes being drafted, debated, presented or applied are
watched the same way. An operation in flight explains it only while its latest
claim, observation or retry is younger than `stall_after` and its consecutive failures
are below the limit at which it is given up. A hold explains it only when its
own cause is one of those. A router hold whose window passes is a stall. The
same window is how long after its first failure a landing, rebase or
publication that keeps failing is retried before it is ended; with `"0"` that
window stays `45m`, so operations still end.

`workspaces` picks the backend of the workspaces the agents work in for each
new workstream: its feature branch, units and drift resolution. The project's
repository stays Git either way. `auto` uses Jujutsu workspaces on the clone's
Git store when a `jj` of the supported release or later is on the service's
`PATH`, and Git worktrees otherwise; `git` always uses Git worktrees; and
`jujutsu` always uses Jujutsu, so without a supported `jj` a new workstream
does not start and status says why. Any other value is an error that names
`workspaces`. A workstream keeps the backend it was created on until it is
delivered or abandoned, so a changed value, reloaded or not, applies to new
workstreams alone.

`shed.max_rounds` caps the rounds the service debates a workstream's spec and
plan for on its own. Debate that reaches it
concludes with its dissent open: the cap
approves nothing. The service reads it when a round's reply is recorded, so a
changed value applies to debates still running. It also bounds how many
further rounds the owner may ask for after a
conclusion; from the first such request the rounds the owner asked for replace
the cap as the debate's round limit.

The architect pool has no factory-wide limit and no
`capacity.*` key: each runs at most one turn per workstream. The capacity view
(the status API's `capacity` and the chief of staff's `capacity` tool) shows
them as unbounded, with no numeric `limit`, and a note saying so; only the
mason, reviewer, committee and checks pools report a numeric `limit`, which
comes from `config.toml` and applies on reload. An unbounded pool is not zero
capacity and has no setting to raise.

`capacity.committee` is the size of each workstream's committee, not a pool
shared across workstreams: a workstream that
enters the shed for debate gets that many committee
members, and all of them run at the same time in every round, whatever other
workstreams run and outside `capacity.per_workstream`. Two workstreams in the
shed run two committees at once. The committee is fixed when the workstream
enters the shed, so a later change applies to workstreams that enter after it.

`committee.perspectives` and `committee.profiles` give committee members
different review focuses and agents. Member *n* takes entry (*n*−1) modulo
the list's length from each list, so the two lists cycle independently and
a member keeps its assignments across rounds:

```toml
[committee]
perspectives = ["correctness", "integration", "scope"]
profiles = ["claude", "codex-fast"]
```

A perspective is `correctness` (charter compliance, contradictions and
verifiable acceptance), `integration` (fit with the project's recorded decisions,
the existing code and the order of units) or `scope` (expansion beyond the
handed design and unit size). It adds that focus to the member's shed system
prompt, in debate and amendment rounds. Every member still applies the charter,
fit, size and acceptance tests and may raise any objection. The list must not be
empty, and any other name is an error that names the entry.

Each entry of `committee.profiles` must name a configured profile. A member
runs every turn of its thread on its assigned profile: shed rounds, answers to
its questions, continuations after a hard pause and, for member 1, the final
review. With a provider limit in force, the member uses the first profile in
its assigned profile's fallback chain whose provider is not limited, and the
committee role's effective profile when there is none. An empty list, and an
owner override of the committee's profile, which wins while it is set, run
every member on the committee role's profile. With `roles.committee.sandbox =
"claude"`, every profile in each assigned profile's fallback chain must use
Claude, and a custom `roles.committee.image` must support all of their agents.
Reload applies both lists to turns queued after it.

`roles.chief_of_staff.events_profile` optionally names the profile of the chief
of staff's turns that deliver service events, so they can run on a lighter
model than the turns that answer you, which use `roles.chief_of_staff.profile`.
It must name a configured profile and is accepted only on the chief of staff.
An owner override of the chief of staff's profile applies to both while it is
set.

`notify.webhook` is an absolute `http` or `https` URL with a host, such as an
[ntfy](https://ntfy.sh) topic; a relative URL, one without a host and any other
scheme are errors that name `notify.webhook` without quoting the URL. With it
set, every entry that opens in the inbox is posted to it once
([notifications](running.md#notifications)). The URL is shown in
`/v1/config`, so treat a URL that embeds a token like any other value there.

Profiles use lowercase names starting with a letter and containing letters,
digits, `_` or `-`, at most 64 characters. Each profile requires `agent` (`claude`,
`codex` or `opencode`) and a nonblank `model`. Model availability is checked by the
provider at execution time, not through network access during loading.

| Profile key | Default | Validation/meaning |
| --- | --- | --- |
| `effort` | `medium` | `low`, `medium`, `high`, `xhigh`, `max`; provider interpretation remains backend-specific |
| `fallback` | empty | Another named profile; unknown references and cycles are rejected, including unused profiles |
| `timeout` | `45m` | Positive Go duration; explicit empty values are invalid |
| `max_turns` | `0` | Nonnegative; zero means no turn limit; only Claude supports a nonzero value through the pinned adapter |

Roles are `architect`, `committee`, `chief_of_staff`, `foreman`, `mason` and
`reviewer`. Every omitted role binding defaults to profile `default`.
If there is no profile named `default`, bind all six roles explicitly. A leftover
`[roles.librarian]` table is accepted and ignored, and the service warns that
it has no effect. Role keys:

| Role key | Default | Validation/meaning |
| --- | --- | --- |
| `profile` | `default` | Must reference an existing named profile |
| `sandbox` | `none` | `none` (host execution), `claude`, `container` or `sbx` (Docker Sandboxes) |
| `image` | empty | Required for `container`; optional agent-specific template for `sbx`; forbidden with host modes |
| `cpus` | unset | `sbx` only: a positive integer, the CPUs of each of the role's sandboxes; see [Sandbox size](#sandbox-size) |
| `memory` | unset | `sbx` only: a positive integer and the unit `m` or `g`, such as `512m` or `4g`, the memory of each of the role's sandboxes; see [Sandbox size](#sandbox-size) |
| `skills` | `[]` | Skills by git reference, given to the role's Claude turns; see [Role skills](#role-skills) |

A `claude` sandbox requires Claude in every profile in the role's fallback chain.
No arbitrary mounts, credentials, environment, tools or capability grants are
accepted from these files; a role's `skills` add only the read-only skill cache
and the tool that loads skills. Sandbox settings describe requested execution. A turn
runs in any mode the platform can confine; one it cannot (for example `claude`
on Linux) fails before launch with core's reason. Successful configuration
loading does not imply that execution is available.

### Docker Sandboxes (`sbx`)

Select `sbx` per role, independently of its profile:

```toml
[roles.mason]
profile = "default"
sandbox = "sbx"
# Optional; omit to use sbx's template for the resolved agent.
# image = "example/osmia-claude:1"

[roles.committee]
sandbox = "sbx"
```

All six roles support this mode with `claude`, `codex` or `opencode` profiles.
An omitted `image` lets each fallback profile select its agent's default template.
A custom template must support every agent in that role's fallback chain. A
`[roles.classifier]` table is accepted and ignored, so a configuration file that
has one still loads.
Reload applies role settings to subsequent turns; an active turn keeps its grants.

### Sandbox size

`cpus` and `memory` in `[roles.<role>]` size each sandbox the role creates. They
are valid only with `sandbox = "sbx"`: with `none`, `claude` or `container`,
loading fails with an error naming `roles.<role>.cpus` or
`roles.<role>.memory`. `cpus` is a positive integer. `memory` is a positive
integer followed by `m` or `g`, such as `512m` or `4g`. A value in any other
form fails the load, naming the field. Osmia checks the format only; `sbx`
enforces its own limits, such as its 512 MiB minimum, when it creates the
sandbox and reports a refusal as the turn's failure.

```toml
[roles.mason]
sandbox = "sbx"
cpus = 2
memory = "4g"
```

The values apply to every turn of the role, fallback profiles included. A reload
applies changed values to sandboxes created afterwards; a turn already running
keeps the sandbox it has.

**Osmia sets no defaults.** A role without `cpus` or `memory` gets exactly the
`sbx create` command line it would get without these keys, so `sbx` chooses its
own size: for memory, half the host's memory, between 512 MiB and 32 GiB. Every
sandbox of a role holds that much, and the mason, reviewer and committee pools
can run several at once, so an unsized setup can ask for more than the host has.
`osmia doctor` warns about that; see
[Doctor](cli.md#doctor).

Starting values for a 16 GB host whose Dagger engine is remote, to adjust to
your capacity settings and workload:

```toml
[roles.mason]
sandbox = "sbx"
cpus = 2
memory = "4g"

# The read-only roles: architect, reviewer, committee,
# chief_of_staff and foreman.
[roles.reviewer]
sandbox = "sbx"
cpus = 2
memory = "2g"
```

That is `2g` for read-only roles, `4g` for masons and `cpus = 2`. With the
default capacity, the four masons, two reviewers, three committee members and
one architect hold 16 + 4 + 6 + 2 = 28 GiB at most, more than
such a host has, so lower `capacity.masons`, `capacity.reviewers` and
`capacity.committee` or the values to fit; doctor shows the total. These are
not defaults, only a place to start: Osmia applies none of them.

A local Dagger engine needs room of its own. It runs on the host beside the
sandboxes, and its cache and the builds it runs take memory the sandboxes do not
leave, so size the sandboxes smaller or use a remote engine. If a mason turn
fails because its guest ran out of memory, raise the mason's `memory` (and
reduce `capacity.masons` if the host cannot hold more).

**Building the Osmia template**

From the Osmia checkout, run:

```sh
./scripts/build-sandbox.sh
```

The script requires a working `sbx` installation and the Dagger CLI on the
host's `PATH`. It pulls the latest Codex template, which ships Codex, removes
Docker Engine and the Docker CLI from it, installs the latest Claude Code with
its native installer and the Dagger CLI release reported by the host's
`dagger version`, verifies the tools, and saves the local template as
`osmia:latest`. Tags such as `:latest` are supported for templates. Each run
refreshes both agents and replaces that local tag. The temporary build sandbox
has no workspace mounts and is removed when the script exits, including when it
refuses to save.

The template has no Docker Engine: no `dockerd` or `containerd` runs in a
sandbox made from it, and it has no Docker CLI. There is one template for every
role and no Docker variant. Masons validate only through Dagger, which reaches
the Dagger engine over TCP through agentcore's host forward for the
sandbox, not through a Docker daemon in the guest. A guest Docker stack would
only cost host memory in every sandbox. Masons use this template; a mason
`image` that carries Docker is the owner's own choice, outside what Osmia builds
or supports.

Before saving, the script proves inside the build sandbox, as root and as the
agent user, that the template is Docker-free, and otherwise it exits non-zero
without saving, names what it found and still removes the build sandbox. It
refuses when:

- a `dockerd`, `containerd` or containerd shim process runs;
- a `docker`, `dockerd`, `containerd` or related executable remains;
- a Docker daemon answers `/_ping` at the default sockets, the `DOCKER_HOST`
  address, an address in a Docker context, `127.0.0.1:2375` or any
  docker- or containerd-named socket in the guest; or
- a probe cannot run or cannot tell. Only a connection that cannot be made
  counts as absence; an unreadable `/proc`, a socket that answers something
  else or an address scheme the probe cannot ask is a refusal.

The reachability probe asks each socket or address directly with `curl`. It
does not use the Docker CLI, whose absence would prove nothing. The script also
keeps its version checks for Codex, Claude Code and the host's Dagger release.

Under each role that should use this template, set `sandbox = "sbx"` and
`image = "osmia:latest"` in the existing role configuration, then run
`osmia reload`. `image` is configured per role, not inherited from a profile.
The template supports Codex and Claude profiles, including fallback chains.
Docker may warn about using a
Codex-based template with Claude; both agent executables are installed. Build
under the OS account running Osmia so the service can access the local template.
Existing sandboxes keep their installed versions.

The template installs the Dagger CLI; engine access still requires the role's
[Dagger configuration](#dagger-for-masons). Set its `version` to the host's
Dagger release so turns use the CLI baked into the template; rebuild the
template after upgrading the host's Dagger CLI.

Follow [Docker Sandboxes setup](https://docs.docker.com/ai/sandboxes/get-started/),
including initial network policy and credential approvals, before unattended
turns. Put `sbx` on the service's `PATH` and prepare it under the OS account
running Osmia:

```sh
sbx version
sbx login
sbx secret set anthropic      # for Claude; use the provider your profiles need
```

For Codex or OpenCode, configure the appropriate provider credentials using
[Docker's credential setup](https://docs.docker.com/ai/sandboxes/configuration/credentials/).
Provider secrets belong to the sandbox proxy, not Osmia's role configuration.
Osmia does not forward host provider tokens, GitHub credentials or SSH agents to
the VM. Keep delivery credentials in the service, and do not configure a GitHub
proxy secret for an Osmia sandbox.

Each turn's authenticated MCP listener uses a fresh loopback port, advertised to
the VM as `host.docker.internal` on Linux and macOS. Osmia explicitly grants the
listener's name and port to core. After creating the sandbox, core allows that
port for that sandbox alone, before any backend probes or execution. A policy
failure prevents the agent from starting. Cleanup removes the port rule before
removing the sandbox, including after failure or cancellation. The listener stays
owned by Osmia.

No global `localhost` network allowance is required. Broader operator policy
remains in effect; per-sandbox rules do not narrow it. Configure other permitted
destinations through [sbx network policy](https://docs.docker.com/reference/cli/sbx/policy/allow/network/).
Osmia grants no native web or fetch tools. Implementation shell commands follow the selected sandbox's network policy.

The sandbox mounts the scoped view and session paths, honoring read-only
access. When startup requires it, core supplies a separate temporary primary
workspace so Docker Sandboxes can write its agent instructions outside protected
inputs. The agent still runs in its granted working directory, and cleanup removes
the temporary workspace. The sandbox's shared skills are disabled; a role's own
[skills](#role-skills) are mounted read-only. Native reading tools are enabled;
file writers receive editing tools, and implementation turns also receive a shell.
A turn with a read-only view starts in an empty scratch directory, and its
instructions name the view's path. The `file_read` tool reads a file in the view
or lists a directory's entries, with `.` naming the view's root.
VCS executables and metadata remain unavailable. No arbitrary mounts or
inherited credentials are exposed, and only a mason configured with `dagger`
reaches a Dagger engine. Core creates and removes
the sandbox for each attempt, including cancellation. If the service is forcibly
killed, a sandbox may remain; its name is recorded in the turn's session directory
as `sandbox-name`, and `sbx rm --force <name>` removes it and its rules.
Core logs cleanup failures; a failed rule removal still attempts sandbox removal.

Unit and final reviewers receive read-only files from the exact candidate commit
and writable scratch space.

Before a unit is reviewed, the service runs `dagger check --progress=report --fail-fast --parallel 4` on
a separate, fresh export of its candidate, made a Git root of its own so Dagger
finds the project's workspace. The copy and temporary home are removed
afterward. Selected links with different filters need separate Dagger views:
links combine only when a single address selects exactly their checks, and
otherwise run in sequential commands. Every command uses `--parallel 4`, so
at most four checks run at once. One run holds one capacity slot, one result
and the combined output for all commands. A failed or incomplete part stops
later parts after the service's one rerun resolves any failed check. A refusal
is judged on one command: even after earlier commands passed, refused links
trigger a full run under the same timeout. The finished selection describes
that full run with reason `refused`; its output retains the earlier reports.
The run is bounded by the project's `checks_timeout` and is
recorded in the trace as `units/<unit>/checks-<n>.json`, with the candidate,
its base and diff, the checks it ran and why, the command, its exit status and
a diagnostic excerpt of at most 16 KiB. The complete combined stdout/stderr is
recorded atomically beside it as `units/<unit>/checks-<n>-output.txt`, named by
`output_path` and `output_revision` in the run. Failed-check blocks retain test
names, assertions and stack traces; passing blocks and rerun commands are omitted. Long check links
are abbreviated only in excerpts. Up to 32 failed check blocks share the
excerpt budget, with both ends retained when diagnostics are too large.
Additional failures remain in the captured output and the complete failed-link list.
`truncated` marks omitted diagnostics, and line references locate each block
in the captured output. Unrecognized or infrastructure output keeps a bounded
excerpt of both ends. Failure classification reads the complete output.
Agents can read the captured text through `factory_context` with `start` and
`lines`, continuing oversized lines with `offset` and `next_offset`.
A check that fails is rerun once by its own link. One that passes is
[flaky](smart-checks.md#flaky-checks): it counts as passing, and the run's
`flaky` list, the feed and the reviewer's evidence name it.
Passing checks send the unit to review, and its reviewer receives the
result instead of running checks. Failing checks, which Dagger's report names,
send it back to its mason with the failures and output; the send-back counts
toward `shed.max_bounces`. At that limit the default is to contest the unit,
and Jev may instead send it back to its mason, run the checks again or have it
reviewed again, recording its reason; a contested unit stays contested until the
owner or chief of staff rules review or revise. Recording the ruling also moves
the unit: review sends the same candidate to reviewing with the ruling’s note;
revise sends it to implementing with one mason turn carrying the failures and
note. Recorded rulings and pending role turns are reconciled after restart.
A run that reports no result, because the engine is
unreachable, the run timed out or the project has no Dagger checks, leaves the
unit checking, tells the chief of staff and runs again after ten minutes. After
three such runs of one candidate in a row the unit is contested instead: a
ruling of review runs its checks again, and revise returns it to its mason. One
run at a time per workstream; a pause holds runs not yet started.

Before the final read, the service waits for the Dagger Cloud statuses of the
pushed commit instead of running checks: it reads nothing for 2 minutes after
the push, then reads the commit's combined statuses at `pull_request_poll` and
records the completed result as `final/checks-<k>.json` for final review `k`,
with the source `pull_request`, the pull request and push, the status URL, the
counts, the failed checks with their `dagger trace` ids and the one rerun of
failed checks. A run with no completed "Run Dagger Checks" status within
`checks_timeout` of the push, or with cancelled or unaccounted checks, is
recorded as incomplete and read again after a wait that doubles with each
further run, up to an hour. The final reader's prompt carries the result; neither
it nor a unit reviewer runs checks. A completed result is read back instead of
read again. The one `dagger cloud checks rerun --failed --pr <N>` of a failure,
with `--org` set from `dagger.organization` for a project without a fork, runs from the workstream's checkout and receives PATH, a temporary HOME and
the Dagger Cloud token `cloud_token_env` names, without engine, provider,
GitHub or SSH credentials. Final reviews take no check slot.
There is no command fallback.

Reviewers read diffs through the `workstream_diff` tool rather than their
prompts, which list the changed files with added and removed line counts. A unit
reviewer reads its pinned candidate against its base, and the service refuses a
diff whose digest differs from the one the review was prepared with. A drift
reviewer reads two changes, named by `change`: `feature`, the feature branch's
change before the rebase, and `resolution`, the resolved candidate's change on
upstream. The final reviewer reads the branch against the upstream commit it was
rebased onto. Each call returns the whole diff by default, `files_only` for the
file list, `paths` for chosen files or directories, and `start` and `end` for
the hunks touching those lines of the named paths. Binary content is not shown,
and a response over 64 KiB is cut with a truncation flag.

The chief of staff receives a read-only view of its workstream's documents at
their latest revisions: `spec.md`, `plan.json`, the handed input, the shed
rounds, and the amendment, unit, final review and delivery records once they
exist. The service stages the view afresh for each turn, under
`chief_of_staff/<project>/<workstream>` in the root, and leaves out its own
tool-call and inspection records.

Missing CLI, login, template or policy requirements fail the turn without falling back to host
execution.

### Dagger engine

The top-level `[dagger]` section selects the service's engine:

```toml
[dagger]
engine = "managed"
version = "v0.20.5"
cloud_token_env = "OSMIA_DAGGER_CLOUD_TOKEN"
organization = "my-org"
```

| Key | Default | Validation/meaning |
| --- | --- | --- |
| `engine` | `"auto"` | `auto`, `managed`, `cloud` or an explicit `docker-container://<container>`, `tcp://<host>:<port>` or `unix://<absolute socket path>` address |
| `version` | Host Dagger CLI release, resolved at runtime | Expected engine release, such as `v0.20.5` or `0.20.5`; a mismatch is reported rather than enforced |
| `cloud_token_env` | Unset | Environment variable name holding the dedicated Dagger Cloud token; required for `engine = "cloud"` and for `check_fallback = "cloud"`, and used for the `dagger cloud checks rerun` of a pull request's failed Cloud checks, which cannot be requested without it |
| `organization` | Unset | The Dagger Cloud organization the token belongs to. A project without a `fork` passes it as `--org` to its `dagger cloud checks rerun` commands, which an organization token needs; a project with a fork runs its reruns without `--org`, because its pull request's checks run in the upstream's organization. Must be a Dagger Cloud organization name |
| `check_fallback` | `""` (off) | `""` or `"cloud"`. With `"cloud"`, a check run that cannot finish on the service's engine is retried on the Dagger Cloud engine; see [Cloud fallback for checks](#cloud-fallback-for-checks). Any other value fails to load, naming `dagger.check_fallback` |

`auto` lets the host CLI choose an engine. `managed` selects Osmia's
`osmia-dagger-engine` container at
`docker-container://osmia-dagger-engine`. An explicit address selects an
engine the owner operates. `cloud` selects a Dagger Cloud engine. A mason in
`sbx` runs Dagger on the same engine; see [Dagger for masons](#dagger-for-masons).

Cloud authentication is independent of engine selection: a token can
authenticate checks using `auto`, `managed` or an explicit address as well.
The setting names an environment variable, never the token itself. Loading
configuration does not read the variable or contact an engine. Only the
service's check client and, under `engine = "cloud"`, each mason sandbox
receive the token; under any other engine no sandbox does. Code under check can probably read the check
client's environment, for example through `env://`, and with it the Cloud
token. Use a dedicated, narrowly scoped token.

#### Cloud fallback for checks

`check_fallback = "cloud"` opts in to retrying check work that cannot finish on
the service's engine on the Dagger Cloud engine. It reuses the token named by
`cloud_token_env`, which must be set: configuration fails to load with
`check_fallback = "cloud"` and no `cloud_token_env`. It adds no timeout and no
credential setting; the operation's time budget is the project's
`checks_timeout`. The default `""` leaves check runs on the configured engine
with a single `checks_timeout`.

The key does nothing when `engine` is already `cloud`: checks keep running on
that Cloud engine, with no separate local attempt, escalation or routing.
Masons stay on the configured engine and do not receive the token. How unit
and session check runs use the fallback is described under
[Cloud fallback](smart-checks.md#cloud-fallback).

#### Another Docker host

Set `DOCKER_HOST` in the environment of the process running `osmia serve` to
place the managed engine on another Docker daemon. For example, with
`dagger.engine = "managed"`:

```sh
DOCKER_HOST=tcp://engine-host.example.ts.net:2375 osmia serve
# Or, when SSH works without an agent:
DOCKER_HOST=ssh://user@engine-host.example.ts.net osmia serve
```

For a service manager, set the variable in its service environment and restart
Osmia. Changing `DOCKER_HOST` requires a service restart; `osmia reload` does not
change the process environment. Osmia ensures the container on the selected
host only. The old host's container and cache are left alone, and the new host
has its own cache. First creation allows time for an image pull, within a
finite action deadline. An unreachable daemon fails startup or reload;
Osmia does not create a replacement engine after a connection failure.

Lifecycle commands, engine probes, check listings and runs, and mason engine
connections use the same host. This also applies to an explicit
`docker-container://<container>` engine. `osmia engine status` and `osmia doctor`
name the Docker host, omitting URL user information. Explicit `tcp://` and
`unix://` Dagger engines and Cloud check clients receive no `DOCKER_HOST`.
`auto` retains host CLI selection and its local Docker Desktop socket fallback.

A named `DOCKER_CONTEXT` alongside `DOCKER_HOST` is refused for managed and
explicit container engines, before connecting. Unset either variable, or use
`DOCKER_CONTEXT=default`, then restart the service. A `DOCKER_CONTEXT` set on its
own continues to select the daemon for lifecycle commands.

The check client receives only the Docker host address from Docker's and SSH's
environment: no `SSH_AUTH_SOCK`, `DOCKER_CONFIG`, `DOCKER_CONTEXT`,
`DOCKER_TLS_VERIFY` or `DOCKER_CERT_PATH`. Its HOME remains temporary, so host
Docker configuration, SSH key files and TLS client certificates are not made
available there. An `ssh://` host must work without an SSH agent, for example
through Tailscale SSH. Code under check can probably read `DOCKER_HOST`, such as
through `env://`; avoid putting credentials in it. Mason sandboxes receive the
forwarded engine endpoint, without `DOCKER_HOST` or a Docker socket.

The machine running Osmia must already be able to reach the daemon. Osmia does
no tailnet login, dialing or name resolution of its own for this setting and
does not use `listen.tailnet` to reach it. A plain `tcp://` Docker daemon has no
authentication of its own and grants control of its host: restrict access with
tailnet access rules and do not expose it publicly.

`DOCKER_HOST` applies to every Docker command the service runs. Roles using
`sandbox = "container"` would also run on the remote daemon, where the service
host's bind-mount paths do not exist. Use `sbx` for mason sandboxes with the
service forwarding the engine connection.

### Dagger for masons

A mason in `sbx` runs the project's Dagger checks and functions while it
works, on the engine the top-level `[dagger]` section selects for the
service's own checks. A mason in any other sandbox gets no Dagger. A
`check_fallback` does not apply to masons.

```toml
[roles.mason]
sandbox = "sbx"

[dagger]
engine = "managed"   # or cloud, docker-container://<container>, tcp://<host>:<port>, unix:///<socket>
version = "v0.20.5"  # required for a host engine: the CLI release installed in the sandbox
```

With an sbx mason, configuration fails to load when `dagger.engine` is `auto`
(also the default with no `[dagger]` section), because a sandbox cannot follow
the host CLI's own engine choice, and when a host engine has no
`dagger.version`. A `[roles.mason.dagger]` section left from an earlier
configuration loads with a warning and has no effect.

A Dagger-enabled mason's view holds an empty Git repository at its root, made
on the host before the sandbox starts, so the CLI finds `dagger.toml` there. It
has no commits, remote or hooks and is never captured. From the view root the
mason runs plain `dagger check`, `dagger check -l`, the Go check's
`--go-package`, `--go-test` and `--env` flags and `dagger generate`, and lints
through the link `dag+check://?check=lint&staticcheck-project=.`. Dagger is the
mason's only validation route.

#### Dagger Cloud

Under `engine = "cloud"` the mason's sandbox gets `DAGGER_ENGINE=cloud` and
`DAGGER_CLOUD_TOKEN`, which holds the value of the variable `cloud_token_env`
names, both passed to the sandbox by name. Nothing is forwarded and no host
port is allowed. The sandbox's own Dagger CLI runs the checks, so the mason's
`image` must include it; `scripts/build-sandbox.sh` installs the host's
release. The sbx network policy must allow Dagger Cloud. A turn fails before
its agent starts when the token's variable is unset in the service's
environment. The agent can read the token, so use a dedicated token you accept
exposing to masons. Mason turns on Dagger Cloud do not hold back an engine
restart or prune.

#### Host engines

Each mason turn on a host engine asks its sandbox for `dagger version`. A
template that already has `dagger.version` keeps it, so baking the CLI into the
mason's `image` saves an install per turn. Otherwise the turn installs that
release with the Dagger install script, and the sandbox network policy must
allow `dl.dagger.io`. A failed installation fails the turn before the agent
starts.

The CLI reaches the engine through `_EXPERIMENTAL_DAGGER_RUNNER_HOST`. A
`docker-container` or `unix` engine is forwarded from a fresh loopback port that
belongs to the turn, and a `tcp` engine on the host's loopback is reached at its
own port. The sandbox reaches either one as `host.docker.internal`, and the port
is allowed for that sandbox alone. Cleanup removes the rule and the forward along
with the sandbox. Other roles never receive the engine.

`managed` is the simplest host engine. Another is the one your own Dagger CLI
provisions: run any Dagger command on the host, such as `dagger core version`,
and name its container, `dagger-engine-<release>`. Each connection to a `docker-container` engine runs
`docker exec -i <container> buildctl dial-stdio`, as the Dagger CLI does, with
the service's Docker environment. The sandbox receives no Docker socket.

A `tcp` or `unix` address must be reachable from the host. With Docker Desktop,
publish a loopback port from an engine container you start, because Docker
Desktop does not carry connections to a container's Unix socket back to the
host:

```sh
docker run -d --name osmia-dagger-engine --privileged --restart unless-stopped \
  -p 127.0.0.1:1234:1234 -v osmia-dagger-engine:/var/lib/dagger \
  registry.dagger.io/engine:v0.20.5 \
  --addr unix:///run/dagger/engine.sock --addr tcp://0.0.0.0:1234
```

Where Docker runs natively, bind-mount the engine's socket directory instead,
for example `-v /run/osmia-dagger:/run/dagger`, and configure
`engine = "unix:///run/osmia-dagger/engine.sock"`. The account running Osmia
must be able to open that socket.

### Role skills

A role may name skills by git reference. Osmia clones each one into
`<root>/skills/` and gives the role's Claude turns a generated plugin that holds
only the skills:

```toml
[roles.reviewer]
skills = [
  "https://github.com/acme/skills",                 # whole repository
  "https://github.com/acme/skills#skills/review",   # one directory inside it
  "https://github.com/acme/review-skill@v1.2.0",    # pinned tag or branch
  "git@github.com:acme/private-skills.git",         # ssh works too
]
```

A reference is `<git-url>[@<ref>][#<sub/dir>]`. The selected directory is either
a single skill, with `SKILL.md` at its root, or a skills collection, with a
`skills/` directory. Anything else is an error when a turn needs it. A Claude
Code plugin repository works through its `skills/` directory, but its hooks, MCP
servers, commands and agents never reach a session. A sub-directory must stay
inside the repository. Two references of one role may not end in the same
name, since that name identifies the generated plugin.

A turn of a role with skills mounts the skill cache read-only in every
sandbox, and Claude's `Skill` tool joins its native tools. Codex and OpenCode
turns receive no skills, so a role whose fallback runs another agent loses its
skills for those turns.

Osmia clones a missing reference when a turn needs it, with the service's own
`git` and credentials and without prompting. A failed clone fails the turn. A
clone last fetched more than `skills.refresh` ago is pulled
(`git pull --ff-only`) first: `"always"` pulls before every turn and `"never"`
never pulls. A failed pull is logged and the turn uses the clone it has; a
reference pinned to a tag cannot be pulled, which is the point of pinning.
To pick up a change sooner, set `refresh = "always"` and reload, or delete the
clone under `<root>/skills/repos/`. The references are shown in `/v1/config`,
so do not embed credentials in a URL.

## Project registration

`osmia project add <name> --upstream OWNER/REPO [--fork OWNER/REPO] --clone PATH`
(API: `POST /v1/projects`) registers a project with the running service. The
client never writes configuration; the service validates the request before
writing anything, then:

1. generates a fresh project ID and journals the registration in
   `<root>/project-add.json`;
2. writes `projects/<id>/config.toml` with `name`, `upstream`, `fork`, the
   absolute `clone`, `base_branch` (default `main`) and
   `landing = "commit-per-unit"`; capacity is inherited from the top level;
3. seeds the local entity map from the clone's
   tracked CODEOWNERS and directory structure, and creates the project's
   trace database, `projects/<id>/trace.db`, with
   a [charter](charter.md) template and the seed as the first
   revisions of the charter and `kb/entities.json`;
4. adds the ID to `active_projects` in the top-level `config.toml` as a text
   edit, so the owner's comments, ordering and formatting survive;
5. activates the project (opens the trace and starts reconciliation) and
   removes the journal. Registration creates no workstream and requests no
   other work.

Validation refuses, each with its own message: a blank name, an upstream or a supplied fork
that is not `owner/repository`, an invalid base
branch, a relative clone path, a clone that does not exist, is not a directory
or has no `.git` entry, and a clone nested with the root either way.
Registration never writes to the clone; seeding only reads it. The service
writes to the clone once a workstream is ratified, when its
sealing fetches upstream, creates the feature branch and
its workspace, and forgets that workspace alone when its directory is gone.

Registration is recoverable. If the service stops at any step, the journal makes
the next start finish the registration with the same ID, or `osmia project add`
run again finishes it. A retry never creates a second trace and an
interrupted registration never blocks a retry: the same request returns the
finished project; a different request waits until the pending registration finishes. A trace whose database never committed its
manifest is discarded and redone, keeping only `config.toml`; one with a
manifest is kept.

Registration can add distinct projects while other projects run. Each active
registration needs its own clone. Repeating an active project's exact
registration returns it without change. Existing project runtimes and overrides
are retained.

`osmia project add` creates exactly one one-repo project with its own charter. `--fork` and `--base-branch` are optional. The project pane offers the same creation operation. To coordinate repositories, select their independent projects when handing in a feature or confirming a design brief's project list.

### The entity map and retrieval

Factory turns receive governing rules and decisions with references to the
project's entity map. They retrieve the entity map and informational project
notices with `project_context`, rather than carrying that material in every
prompt. The tool searches current records, lists at most 20 matches per page,
and reads at most 16 KiB per call. Reads identify the revision; subsequent pages
can pin it, and older revisions remain available. Other workstreams' records are
not exposed by this tool. Workstream documents remain available through
`factory_context` and each role's scoped file view.

The entity map, `kb/entities.json`, is computed by the service alone. It holds
one entity per directory of the repository, named from CODEOWNERS and the
directory structure, plus the root entity `repo` covering the whole repository,
and no agent writes it. A unit footprint names entities by ID or alias, or names
raw path globs such as `cmd/tool/**` that may match no file yet; an entry is a
raw glob when it names no entity and holds a `/` or a glob character. Registration records the first
revision from the clone's tracked files. After that the service re-seeds the map
from the project's upstream default branch when an architect draft starts and
again just before a sealing resolves its plan's footprints, and before the owner's plan edit, a ratification or an amendment draft is validated: it fetches the
upstream ref first, so a directory that landed upstream after drafting is
visible, and reads the directory listing and the CODEOWNERS blob from the fetched
commit without checking anything out. A commit is seeded once for the whole
project, however many workstreams trigger it, and never a commit older than the
last one seeded. Each seed is computed from the tree alone, so an entity whose
directory is gone leaves the map. A new revision is recorded only when the map
changes. A failed fetch or seed is logged once per distinct reason and does not
block drafting or sealing, which go on with the latest recorded map.

`osmia project remove <project-id>` (API: `DELETE /v1/projects`) removes the
active repo ID from `active_projects` as a text edit, stops admitting new work,
and waits for in-flight operations to finish before closing its runtime; other active projects keep running. Removal is refused while that project has any unresolved member in any workgroup. The project's and every workstream's traces, and the owner's clone, are not deleted. Adding the
same upstream again afterwards generates a new project ID and new trace
repositories; the old ones stay untouched under `projects/<old-id>/` and
`workstreams/<old-id>/` and are
never reused, so archived history is immutable and every add is a fresh start.

The service's loaded configuration changes only for the registration added or
removed: `/config` reports no `restart_required` or `reload_required` diagnostic
for these edits.
Without a project, `/config` and `/runtime` carry a `no_project` diagnostic and
project-scoped overrides are rejected as referencing an inactive project.

## Project config.toml

Store this file at `projects/<project-id>/config.toml`; `osmia project add`
writes it. The directory ID is the identity; there is no second configurable ID
to disagree with it.

```toml
version = 1
name = "My repo"                    # optional display metadata, default empty
upstream = "upstream/repository"    # required owner/repository, not a URL
fork = "my-account/repository"      # optional; omit to push feature branches to upstream
clone = "~/src/repository"          # required; may not yet exist
base_branch = "main"
landing = "commit-per-unit"
upstream_rebase = "6h"              # default; "0" disables scheduled drift rebases
pull_request_poll = "10m"           # default; minimum 5m between pull request checks
checks_timeout = "15m"              # default; how long one run of a candidate's checks may take
flake_fixes = false                 # default; true turns flake-fix workstreams on for this project

[capacity]
per_workstream = 2                  # defaults to top-level capacity.per_workstream
git_operations = 4                  # concurrent Git/GitHub operations in this project
```

Repository references contain two nonempty owner/repository components using
ASCII letters, digits, `.`, `_` and `-`, beginning with a letter or digit, without
a `.git` suffix. Fork and upstream must differ ignoring case. `base_branch` follows
Git branch-name constraints. `landing` accepts `commit-per-unit` (default) or
`squash`: it chooses whether an approved workstream is
published with each unit's commit or as one commit. Repo `capacity.per_workstream` is a positive integer overriding the
global default.

Repo `capacity.git_operations` is a positive integer, default 4. Seal, build,
land, rebase, drift and publish share this per-project limit with remote polls.
An operation waiting for capacity stays pending while the pass admits other
eligible work. Each workstream's workflow operations run in order; different
workstreams can land concurrently onto their own feature branches. A landing
waits only for its own workstream's rebases.
Reload applies a new limit to admission; operations already running finish.

Publication requires the service host's Git `user.name`, `user.email` and
`user.signingkey`, with repository-local overrides respected. Git's `gpg.format`
and signing-program settings select OpenPGP, SSH or X.509 signing. Signing is
required even if `commit.gpgsign` is false. Configure `landing = "squash"` for
one signed commit per workstream. The service must be able to use the key or
signing agent; runtime agents do not receive it. Missing identity or signing
failure refuses publication before pushing. Correct the configuration and
approve delivery again to retry. Local workstream commits remain unsigned.

`upstream_rebase` is a Go duration string: how long after a workstream's
latest drift rebase or final rebase (or, before either, its sealing) the
foreman schedules its next drift rebase onto
upstream. It defaults to `"6h"`. `"0"` (or `"0s"`) disables scheduled drift
rebases; `osmia rebase` and `osmia project rebase` still ask for them. A
drift rebase whose conflict resolution review sent back `shed.max_bounces`
times, or whose drift mason or reviewer turn failed or ended without a
verdict, is held: the workstream takes no scheduled drift rebase until
`osmia rebase` or `osmia project rebase` asks for one or the chief of staff hands it back, and the inbox lists it until
then. A value that does not
parse as a Go duration, a TOML number, a negative duration, or a nonzero
duration shorter than `1m` is rejected.

`pull_request_poll` is a Go duration string of at least `5m`, defaulting to
`"10m"`. It governs base refreshes (delivered workstreams' own pull requests
and dependency integration), delivered unresolved archives' pinned pull requests,
incoming hard-block and merge-order edges, the final review's reads of a pushed
commit's Dagger Cloud statuses (after the 2 minutes that follow a push), and the
lookup of an assembled workstream's draft pull request that you may have marked
ready on GitHub. Failed lookups also wait for the
interval; they supply no merge or close evidence. Zero, negative and shorter
durations, invalid duration strings and TOML numbers are rejected; polling
cannot be disabled.

`osmia rebase` and `osmia project rebase` (and their web controls) make each
covered workstream's next base refresh and incoming-edge check due immediately
once the owner's drift request is recorded. Subsequent checks wait for the
interval again, and the requested drift rebase proceeds as usual. Recorded
merged or closed own-PR outcomes, resolved archives and fully merged incoming
edges still stop their respective polling.

Reload applies the new interval to each item's last attempt, retaining its
cooldown even when the interval is unchanged. Reload alone never triggers an
early check. Archive and failed-edge attempt timing is kept in memory and may
reset on a process restart; durable observations and outcomes remain intact.

`checks_timeout` is a Go duration string from `1m` to `24h`: how long one run
of a unit candidate's checks may take, and how long after a push, or after a
rerun was requested, a final review waits for a completed "Run Dagger Checks"
status. It defaults to `"15m"`; raise it for a repo whose `dagger check` runs
longer. A unit run that outlasts it did not complete: the unit stays checking
and its checks run again later. A final review's pull request status that is
still pending after it is recorded as an incomplete run, and the statuses are
read again later. Listing the check links for a Jev selection has its
own five-minute bound. A change applies to runs that start after a reload.

`flake_fixes` is a boolean, `false` by default; clear the key to return to it.
It turns flake-fix workstreams on for the project. A check that fails and then
passes when rerun by its own link already counts as a pass in unit and
final-review check runs, and with the default that is all that happens. With
`flake_fixes = true` each such run is also recorded as a flake, the originating
workgroup's chief of staff is told, and the chief of staff may open a separate
workstream, through the dedicated flake-fix tool, to find and fix the flakiness's
cause, or decline with a recorded reason. The setting is off by default because
a flake-fix workstream is an ordinary workstream whose pull requests go out
under the owner's name: the owner opts a project in rather than having it
happen to every project. The owner still ratifies what such a workstream builds
and approves its delivery. The setting never changes whether any check run
passes or fails. It is validated like the other project settings: a value that
is not `true` or `false` fails at load naming `flake_fixes`, and with the key
left out the setting is off.

A `classifier` key in a project file is accepted and ignored, so a file that has
one still loads.

### Reading and saving a project's settings

`GET /v1/projects/config/<project-id>` and `PUT /v1/projects/config/<project-id>`
read and save one active project's `projects/<project-id>/config.toml`. An
inactive or unknown project is refused (`not_found`). Both follow the web and
tailnet listener rules: a save needs `Content-Type: application/json`, and every
request needs a `Host` naming the listener.

**Read.** The editable snapshot is the file on disk. The read decodes the
current bytes once and returns, for each effective setting (`name`, `upstream`,
`fork`, `clone`, `base_branch`, `landing`, `upstream_rebase`,
`pull_request_poll`, `checks_timeout`, `capacity.per_workstream`,
`capacity.git_operations`, `hearsay_scope`, `hearsay_declare_repo`,
`flake_fixes`, `hearsay_entities`):

- `value`: what the file gives it, or would give it. A setting the file leaves
  out shows its default; `capacity.per_workstream` shows the loaded top-level
  `capacity.per_workstream`.
- `default`: true when the file leaves the setting out. `hearsay_entities` is a
  default while it has no entries.
- `effective`: the value in the loaded configuration.
- `pending`: true when `value` differs from `effective`, that is, the file holds
  an edit that has not been applied. A reload or a save applies it.

`pin` identifies exactly the bytes the values and markers describe, so the
three always belong to the same snapshot. The `path` names the file.
`clone` is shown as the loader resolves it. When the file does not load, the
read still answers: `error` carries the loader's error for the file (its path,
the field and the reason, never the file's text), `settings` hold only the
values in effect, and there is no `pin`. A save is refused while the file does
not load; fix it by hand.

**Save.** The request carries the `pin`, the settings to change in `set` (an
omitted setting is unchanged) and `clear`, the names of settings whose key is
removed so the default or inherited value applies again.
`set.hearsay_entities` sets entries by local entity ID and
`clear_hearsay_entities` removes entries. `set.hearsay_declare_repo` and
`set.flake_fixes` are booleans; `clear` removes them so the default, `false`,
applies again. An empty string for `name`, `fork`
or `hearsay_scope` clears it. `upstream` and `clone` are required
and cannot be cleared. A save runs under the same lock as a reload and a
project add or removal:

1. If the file's bytes no longer match the `pin`, the save is refused with
   `conflict` and writes nothing. A new read returns the edited file's values
   and pin.
2. The candidate file is a text edit of the pinned bytes: only the changed keys
   are set, added or removed. Every other byte stays as it was, including
   comments, key order, an ignored `[preview]` table and a hand edit that has
   not been applied. A new key is placed with its siblings or in a new
   `[capacity]` or `[hearsay_entities]` table at the end of the file. A layout
   the edit cannot change safely, such as `capacity` written as an inline table,
   is refused and the file is edited by hand.
3. The candidate is validated with the loader's rules. An invalid value is
   refused as `validation` with the offending setting in `error.field` and the
   loader's message, for example `checks_timeout: must be a Go duration from 1m
   to 24h`.
4. The running service prepares to adopt the candidate as a reload does: the
   project's stages, the Dagger engine and the runtime state. A failure is
   refused with its reason.
5. Only then is the file replaced atomically, keeping its mode, and the saved
   project's settings adopted without a reload. The configuration event follows.

Every refusal, up to and including a failure writing the file, leaves the file's
bytes, its directory (no temporary file) and the loaded configuration unchanged.
A successful save adopts only the saved project: pending edits to the top-level
file or to other projects' files stay unapplied. The whole saved file of this
project takes effect, so a hand edit the read showed as pending is kept and
applied rather than reverted. `GET /v1/config` reports the new values, the
drift entry for the file is `unchanged` and a restart reads the same values back
from the file.

The response is the snapshot of the saved bytes with its new `pin`, plus
`outcome`:

- `applied`: the file holds the saved values and the service uses them.
- `saved_not_applied`: a failure after the file was replaced, such as the write
  not finishing cleanly, kept the values from taking effect. This is not a
  refusal: the file holds the saved values at the returned `pin`, the drift view
  shows the file as `changed`, a read flags the saved values as `pending`, and
  `reason` says what failed. A reload or a later save with the returned pin
  applies them.

The web page's **Project management** view reads and saves a project's settings through these two endpoints; see [running](running.md) for how it shows defaults, not-yet-applied hand edits, refusals, stale saves and a saved-but-not-applied outcome.

### Previews

Previews need no project configuration. The chief uses `inspect_code` on the
member's committed Dagger configuration and module source to choose a Service
call and HTTP port for each `preview` request. The tool requires a non-empty
`call` argument list and a `port` from 1 to 65535. `path` defaults to `/` and
must start with `/`; `timeout` defaults to `5m` and must be from `1m` to `30m`.

The service passes accepted arguments unchanged to `dagger call`, without a
shell. It refuses CLI options, provider schemes, client environment variable
names, paths outside the exported source (including symlinks), and host-writing
or interactive chain steps. It checks assignment values, quote removal,
comma-separated elements and surrounding whitespace as Dagger reads them.
Checks fail closed, so even a legitimate value containing `:` can be refused;
the chief can use a module default or explain the limitation. Only a verified
engine Service ID reaches the capture command.

A project file may contain an ignored `[preview]` table with `call`, `port`,
`path` and `timeout` keys. These values have no effect, are absent from the
effective configuration and are not validated. Loading and reloading leave
the file's bytes untouched. Unknown keys elsewhere, and unknown keys within
the table, are refused. The owner configures no per-project preview service.

## Configuration and restart behavior

Loading accepts profiles, bindings, capacity, budgets, shed limits, repository identity
and landing preferences as declarative inputs. Review slot
configuration is `capacity.reviewers`; no separate review-policy schema is defined.
Runtime profile overrides, pauses, priorities and archived workstreams belong
in `runtime.json`, never these files.
`osmia config` shows whether each file on disk differs from what is loaded
([disk drift](running.md#disk-drift)), and `osmia reload` applies edited files
to a running service after validating all of them ([reload](running.md#reload)). The root, `listen.socket`,
`listen.web`, `listen.tailnet`, `listen.tailnet_https` and the `[telemetry]` table keep their loaded
values until the service restarts. Role skills and `skills.refresh` apply to turns that start after a
reload, and `[transcribe]` to transcriptions that start after one. The `active_projects` list applies live: additions start their retained
traces and removals drain in-flight operations before stopping.

The optional `[budget]` table accepts `per_session`, `per_unit` and `per_day` as
positive decimal USD strings. `per_session` caps known spend on each new turn.
When a building or assembled workstream's known trace cost exceeds `per_unit`,
the service files one amendment request for that workstream and continues its
normal workflow. The request states the known spend as a lower bound and counts
attempts whose cost is unknown. When known spend on the service host's local
calendar day reaches `per_day`, the service pauses factory dispatch until the
next local day.
Missing values impose no cap. Unknown cost does not establish that a cap was reached.

Representative errors include the file and offending field:

```text
<root>/config.toml: profiles.default.fallback: unknown profile backup
<root>/config.toml: roles.mason.image: container requires an explicit image
<root>/projects/<id>/config.toml: clone: clone and Osmia root must be separate, non-nested directories
```

Malformed TOML and values of the wrong type name the last key read and the line
(`<path>: capacity.masons: value has the wrong type at line 3`), never
the file's text; an unreadable file is `<path>: cannot be read`. On any error `Load` returns `nil`, never a usable partial configuration.

## Optional Hearsay context

Omit `[hearsay]` for complete file-based operation. To enable external context,
configure the API endpoint and environment references for both the owner token
and the delegated agent tokens. The service resolves secrets when making a call;
configuration responses, agent environments, and local trace records contain no
credential values.

```toml
[hearsay]
url = "https://memory.example"
principal = "owner"
token_env = "HEARSAY_OWNER_TOKEN"

[hearsay.agents.worker]
id = "osmia-worker"
token_env = "HEARSAY_WORKER_TOKEN"
[hearsay.agents.orchestrator]
id = "osmia-chief"
token_env = "HEARSAY_CHIEF_TOKEN"
[hearsay.agents.observer]
id = "osmia-observer"
token_env = "HEARSAY_OBSERVER_TOKEN"
```

### Entity ids

Reads name Hearsay entities by id: `<namespace>:<name>` with a lowercase
namespace and no whitespace, such as `work:<work source>:<project-id>` or
`code:<repository>:<entity>`. A bare scope name such as `osmia-ab12` is not an
entity id.

A project with a configured work source (`[hearsay.publish] work_source`) reads
`work:<work source>:<project-id>` with no further setting, and a turn tied to a
workstream also reads `work:<work source>:<project-id>#<workstream-id>`. Records
the owner decided are served on the same work-source entity, so no separate
owner-source entity is read. Without a work source and without an override
below, a project stays in file mode.

Two optional keys in the project's `config.toml` change which entities are read:

```toml
# Replaces the project's default entity.
hearsay_scope = "work:osmia-work:p_0123456789abcdef0123456789abcdef"

# Entities a unit footprint's local entity maps to, replacing the default
# for turns whose footprint names them.
[hearsay_entities]
"internal.storage" = "code:example/repo:internal.storage"
```

- `hearsay_scope` is an entity id. When set, it replaces the project default.
  A workstream-tied turn still also reads the workstream's entity on the
  configured work source, if there is one.
- `hearsay_entities` maps a local entity id to a Hearsay entity id. Without a
  mapped footprint entity, the project entity is read.
- A value in either key that is not an entity id, such as an old scope name,
  fails at load with an error naming the field (`hearsay_scope` or
  `hearsay_entities`). Osmia does not migrate old values; change them to entity
  ids.

Settings apply through reload. Hearsay must configure the named principals with
the worker, orchestrator, and observer classes and grants intersected with the
owner's access. Each role reads through its class agent acting for the owner
principal (`hearsay.principal` and `hearsay.token_env`, plus the agent's own
id and token). The publisher's token is never used for a read.

### Publishing: `[hearsay.publish]`

Omit `[hearsay.publish]` and Osmia sends nothing to Hearsay's connectors
listener. Adding it names where the service posts project records and as whom.
It needs the `[hearsay]` table above, whose `principal` is the owner the records
name as their author.

```toml
[hearsay.publish]
work_source = "osmia-work"            # the work source projects and workstreams are posted to
owner_source = "osmia-owner"          # the paired source that holds what the owner decided
hooks_url = "https://hooks.example"   # Hearsay's connectors listener, not the API url
agent = "osmia-publisher"             # the dedicated publisher agent's id
token_env = "HEARSAY_PUBLISHER_TOKEN" # the variable holding the publisher agent's token
```

All five keys are required. Each is checked at load, and a missing or invalid
key, or one this table does not define, fails with an error naming it, for
example `hearsay.publish.hooks_url`:

- `work_source` and `owner_source` are Hearsay source ids of up to 64
  lowercase letters, digits, hyphens or underscores, and they differ.
- `hooks_url` is an HTTP(S) URL without credentials, query or fragment. It
  differs in role from `hearsay.url`: Hearsay serves hooks on a separate
  listener.
- `agent` is the publisher agent's id. It is a dedicated agent: it differs from
  `hearsay.principal` and from every `hearsay.agents.*.id`.
- `token_env` names an environment variable, never a token value. It differs
  from `hearsay.token_env` and from every `hearsay.agents.*.token_env`.

The publisher's token value is registered for redaction when the configuration
loads, so it does not appear in the service log, in a load or call error, or in
a configuration response, which carries only the variable's name. The work
source also supplies the default read entity described under
[Entity ids](#entity-ids).

#### Declaring a project's GitHub repo

A project is posted to Hearsay without `repos`, with the owner named as its
author: only the publisher and the owner can read its records. A project opts in
to declaring its GitHub repo on its Hearsay project in its `config.toml`:

```toml
hearsay_declare_repo = true
```

The default is `false`; clear the key to return to it. With the opt-in, the
project is posted with its `upstream` repository declared, and its records become
visible to everyone who can read that repository in Hearsay. For a public
repository that is everyone Hearsay knows, so leave it off for a project whose
decisions should stay private. Roles read a private project through their class
agents acting for the owner; opting in is not needed for that. The setting is
validated like the other project settings: a value that is not `true` or
`false` fails at load naming `hearsay_declare_repo`.

Bundles preserve local charter, documents, entities and rulings. External
context is separately labelled, limited to 8,000 bytes across scope bundles,
and fetched under a three-second deadline. Missing credentials, invalid
responses and outages produce `degraded` context with the local bundle intact.
Status reports `file`, `hearsay`, or `degraded`; a configured service starts
unverified (`degraded`) until a bundle succeeds. Credentials are not forwarded
through HTTP redirects.

The service exposes Hearsay read tools within role turns: `get_bundle`,
`search`, `traverse`, `resolve`, `stance_history`, `get_l1` and `get_l0`.
`traverse` follows Hearsay's typed graph edges from entities, so a role can
reach the decision log and spec documents around a project or workstream entity.
Exploratory sessions receive the same read tools at the observer class and no
write tool. Workers and the chief also receive
`assert`, which proposes an agent learning and does not ratify an owner ruling. The endpoint, human token and agent tokens remain service-owned.
Live Hearsay sources, identities and authority policy are configured by the
operator in Hearsay; `osmia project memory` generates them for a project (see
[Hearsay integration](hearsay.md#prepare-hearsays-configuration)).

Hearsay watches use the orchestrator identity and the project's default entity. Their opaque cursors, seen event
identities and chief notices commit together in the local trace. Remote failures
retain the last cursor and leave local work running. Changed connection or
identity settings select a separate cursor; the first watch begins at Hearsay's
current position. Existing history remains available through bundles and read
tools. Feedback on a delivered workstream can produce a notice or question; it
cannot reopen implementation.

When `fork` is omitted, Osmia pushes its feature branch to `upstream` and
opens the draft pull request there against `base_branch`. A fork equal to upstream
(case-insensitive) is normalized to omission. The clone can have just an
`origin` remote matching upstream. Existing configurations with a separate
fork continue to push there. Osmia never pushes to the configured base branch
or merges the pull request.

For dependent workstreams without a fork, the pull request initially targets
the parent's feature branch. After the parent integrates, fresh review and
owner delivery approval authorize retargeting that same pull request to the
project's base branch with the approved description. Interrupted updates are
reconciled against the recorded request before retrying.

## Project creation

A design session can describe a brand-new project, and Osmia creates its
repository, clones it and registers it. That stays off until the top-level
`[projects]` table sets both keys:

```toml
[projects]
# The GitHub user account the repository is created under.
repo_owner = "octocat"
# The directory the new clone goes in, as <clone_dir>/<repo>.
clone_dir = "~/src"
```

`repo_owner` is a GitHub user account name: up to 39 letters, digits and single
hyphens. It must be the account `GITHUB_TOKEN` authenticates as, because
GitHub's create-repository call always creates under the token's own account;
an organization is not supported. The service confirms the account before it
records or creates anything and refuses on a mismatch. `clone_dir` must be an
absolute directory, or start with `~/`, and must not overlap the Osmia root.
Both keys are validated whether or not creation is on, and an invalid one fails
the load with an error naming the field, such as `projects.repo_owner` or
`projects.clone_dir`. Setting only one key leaves creation off.
`osmia config` and `GET /v1/config` report the table.

New repositories are private unless the owner confirms a public one in the
design session, and each is initialised with only a README.

`GITHUB_TOKEN` needs permission to create repositories:

- A classic token needs the `repo` scope.
- A fine-grained token needs Administration (or repository creation) write
  permission plus Pull requests and Metadata, with access to All repositories,
  because the repository does not exist when the token is chosen.

The token is used only for GitHub API calls. Clone, fetch and push use the
host's own git configuration, SSH or HTTPS, as they do for every registered
project; the clone uses the SSH URL GitHub returns. Osmia adds no git
credential handling of its own. `osmia doctor` reports whether creation is
configured, whether the token's account is `repo_owner` and whether the token
can create repositories, reading `X-OAuth-Scopes` for a classic token. A
fine-grained token reports no scopes, so doctor says it cannot confirm the
permission rather than passing. Doctor makes no claim about git credentials.

## Optional Whisper transcription

Conversation composers can turn a spoken recording into text through a Whisper
server you run, such as an OpenAI-compatible `/v1/audio/transcriptions`
endpoint reachable over your tailnet. Transcription is off until `url` is set;
with it off, the composers show no microphone and the service contacts no
server.

```toml
[transcribe]
# The full transcription endpoint audio is posted to. Empty (the default)
# turns transcription off.
url = "http://whisper.<tailnet>.ts.net:8000/v1/audio/transcriptions"
# The model name sent with each request.
model = "large-v3-turbo"
# The environment variable holding an optional bearer key.
api_key_env = "WHISPER_API_KEY"
```

`url` must be an `http` or `https` URL with a host and without credentials, a
query or a fragment; plain `http` to a tailnet address is expected. `model`
names a model in up to 128 characters without whitespace, and `api_key_env`
names an environment variable. The settings are validated whether or not
transcription is on; an invalid one fails the load with an error naming the
field, such as `transcribe.url`. They apply through reload: the next
transcription after `osmia reload` uses the new endpoint and model, with no
restart. `osmia config` and `GET /v1/config` show the three settings, which is
how the page learns whether transcription is on.

### Where audio goes

The page records in the browser and uploads each clip to the Osmia service
(`POST /v1/transcribe`, a JSON body carrying the clip base64-encoded with its
MIME type). The service posts it to `url` as a multipart request, the audio as
`file` with its MIME type and a matching filename extension, and `model`, and
answers the page with the transcribed text. The browser never contacts the
Whisper server, so the page's content security policy is unchanged. The
service does not store the clip or the text, and neither reaches the trace,
the log, an event or a file under the root.

The service adds `Authorization: Bearer <key>` exactly when the variable named
by `api_key_env` is set in the service's environment. The key is read from
the environment for each request, never stored in configuration, and it appears
in no response, error or log record. The service does not follow redirects
from the Whisper server, so a clip and the key go only to the configured URL.

A clip over 25 MiB is refused without being forwarded. A request to the Whisper
server may take two minutes at most. An unreachable server, a non-success
status, a reply that is not a `{"text": ...}` object and a timeout each answer
a transcription error with no text. On the web and tailnet listeners the route
follows the other writes: the `Host` must name the listener and the request
body must be JSON.

## Optional telemetry

Osmia can export traces of its own work to an OpenTelemetry collector over OTLP.
Telemetry is off until `endpoint` is set; with it off the service builds no
exporter and makes no connection. Only traces are exported: no metrics, and
the service keeps no local store of them. The [telemetry guide](telemetry.md)
lists every span and attribute and gives queries for the questions the traces
answer.

```toml
[telemetry]
# The collector, as host[:port] (and a base path for http/protobuf). The
# port defaults to 4317 for grpc and 4318 for http/protobuf. Empty (the
# default) turns telemetry off.
endpoint = "collector.<tailnet>.ts.net:4317"
# "grpc" (the default) or "http/protobuf".
protocol = "grpc"
# Connect without TLS. The default connects with TLS.
insecure = false
# The environment variable holding optional export headers, as
# comma-separated name=value pairs with percent-encoded values.
headers_env = "OSMIA_OTLP_HEADERS"
```

`endpoint` is a host, with an optional port, without a scheme, credentials, a
query or a fragment; with `http/protobuf` it may carry a base path such as
`collector:4318/otlp`, and spans are posted to `<base path>/v1/traces`.
`insecure` alone chooses between TLS and a plain connection. `protocol` must
be `grpc` or `http/protobuf`, and `headers_env` names an environment variable.
The settings are validated whether or not telemetry is on; an invalid one
fails the load with an error naming the field, such as `telemetry.protocol`.

Only this table configures the exporter. The service never reads an `OTEL_*`
environment variable: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES` and the rest change neither where spans go nor the
resource attributes, which are `service.name` (`osmia`) and the service
version. Setting any of them has no effect. The table applies at start; a
reload that changes it reports `telemetry` as needing a restart and keeps the
running settings.

The headers come from the variable `headers_env` names, read once at start.
Typically they carry a collector credential, so the value is a secret: it is
never stored in configuration, and it appears in no response (`osmia config`
and `GET /v1/config` show only the variable's name), error or log record. A
variable that is named but unset sends no headers; one that is not
`name=value` pairs stops telemetry from starting, and the service logs an
error naming the variable and runs with telemetry off.

Telemetry is best effort and never slows or fails the service's work. Spans
wait in a bounded in-memory queue and are exported in the background; when
the collector is slow, down or refusing, the queue fills and the newest spans
are dropped, and a failed export is logged at most once a minute without any
header value. On shutdown the service flushes the queue, waiting at most five
seconds. Spans carry identifiers, roles, models, states and outcomes only;
prompts, replies, titles, reasons and check output are never attached to
them.

While telemetry is on, the running service keeps `service-intervals.json` in
the root: the start of the run, the pool limit intervals it has open and the
last time it recorded being alive (every 30 seconds). A clean shutdown sends
those intervals' spans and removes the file. If the service stopped without
shutting down, the next start sends the spans of that run's open pool limit
intervals and its uptime, ended at the last time it was seen alive. With
telemetry off the file is never written.

## Jev

Jev is TypeSafe's System One model. Osmia asks it bounded typed questions
inside turns and the service's check runs. Jev is required: there is no switch
to turn it off. Without its API key Jev is unconfigured: the service still
starts, every judgment takes its fallback, and check selection runs every
check. `osmia doctor` fails its Jev check until the key is set. The settings
show their defaults:

```toml
[jev]
# Base URL of a TypeSafe-compatible API; requests go to <url>/v1/systemone.
url = "https://openrouter.ai/api"
# The model asked. An alias moves when a new version ships; name a versioned
# model your provider accepts to keep tuned thresholds stable.
model = "~typesafe/jev-latest"
# The environment variable holding the API key.
api_key_env = "OPENROUTER_API_KEY"
# How long one request may take, at most 1m.
timeout = "10s"
# The probability at or above which breadth keeps a top-level check and
# ranking selects a link, above 0 and at most 1. Tune against a versioned model.
check_threshold = 0.5
# The confidence at or above which Jev's pick on a decision is binding, above
# 0 and at most 1. One value serves every decision kind.
decision_threshold = 0.9
# When no check reaches the threshold, run this many highest-scored links.
# Non-negative integer; 0 (default) runs every check instead.
check_fallback_picks = 0
```

The settings are validated and apply through reload. A leftover
`enabled = true` or `enabled = false` from a configuration written when Jev was
optional still loads and has no effect; `false` logs a warning when the
service loads or reloads it. Remove the line. The service reads the key when it makes a request; configuration
responses, sessions and trace records never contain it, and it is not sent
through a redirect. A judgment's state, such as a unit candidate's diff for
smart checks, is sent to the configured provider.

Every judgment takes its fallback when the key is missing, the request times out, is rate limited, fails or is refused, the
response does not answer the questions as asked, or the answers fall below the
judgment's own threshold. A transient failure is retried once. A rate limit,
or three transient failures in a row, cools Jev down for every judgment for 30
seconds, doubling with each further episode up to 15 minutes or as long as the
provider's `Retry-After` asks within that cap; judgments fall back without a
request meanwhile.

A clean mason turn without an outcome is classified by Jev before the code
rules. Jev chooses the class and the sentence of the response that supports it,
which becomes the recorded evidence. A class below its threshold, a stricter one
for `gave_up`, or one no sentence supports falls back to the code rules, as does
an outage, a cool-down or a missing key. The turn's classification records `by`
as `jev` or `rule`, and a Jev class names its judgment, which the chief of
staff's notice cites. A fallback leaves an unclear ending a clarify
continuation, which counts toward `mason.max_clean_turns` like any other
classified turn, so the unit is contested once the bound is reached.

Unit and smart session check runs use three stages. `dagger list checks --format=link` supplies top-level checks;
`dagger list checks --all --format=link` supplies expanded links, with scoped
listings attributing them to top-level checks. Breadth asks which top-level
checks the change could affect and uses `jev.check_threshold` to keep them.
If none reaches it, all are kept. Depth chooses the granularity for each kept
collection; ranking judges the resulting links and runs those at or above
`jev.check_threshold`. A lower threshold keeps and runs more checks and is
less likely to miss an affected one. Lists still exceeding the fixed 1024
cap are narrowed to collection items, such as a Go package's tests rather
than each test, or fall back to every check if they cannot narrow safely.
There is no cap setting or separate breadth threshold. Each stage gets its own
`jev.timeout`; three sequential requests can take roughly 30 seconds at the
10-second default before falling back, in addition to listing time.

A failed breadth stage or top-level or scoped listing restores the full
candidate path for ranking. A failed depth stage keeps breadth's result at
the finest level. A failed expanded listing or ranking judgment runs every
check; during cool-down each stage falls back immediately.
When no ranking link reaches the threshold, `check_fallback_picks` above 0
runs up to that many highest-scored candidates from the same answers,
however low the scores. Equal scores follow sorted candidate order. Fewer
scored candidates run all scored links; no usable score or 0 (the default)
runs every check. A pick may cover a whole collection item. Picks can miss an
affected check that a full run would catch. An explicit full session run
stays full. The selection, summary and feed distinguish fallback picks.
The setting is validated, reported in effective configuration and applied
through reload. Without the key, selection skips the listing and runs every
check. Each stage records the acceptance
values it applies; a changed value creates a new judgment.

[Smart checks](smart-checks.md) describes the selection end to end: what Jev
is asked, what a run records, and `go run ./cmd/smartcheck`, which runs the
same selection on any Git working tree.

The chief of staff investigates a question, a contested unit, a committee
objection, a plan-only amendment, a discovery or a final review's blocking
finding first and then calls `decide` with the kind, the subject and a short
evidence summary. The service checks from the subject's records that the chief
may decide it today (a conflict between member charters, the owner's own
objection and an amendment that changes approved intent are refused, with no
request and no judgment), builds the state from the records and asks Jev to
pick: answer, route as an amendment or escalate a question; review, revise or
escalate a contested unit; dismiss or escalate an objection; approve or decline
a plan-only amendment; assign, decline or retain a discovery; and, for each
blocking finding of the current final review (subject `<review>:<finding>`, as
the final review notice names it), whether it needs code in the next plan
revision or is left to the owner. Jev never reads code or uses tools. A pick at
or above `[jev] decision_threshold` is binding: the outcome tool (`answer`,
`route_amendment`, `escalate`, `resolve_contested`, `resolve_objection`,
`decide_amendment`, `record_discovery`, `triage_finding`) then records only that
outcome, with the chief's own text, and the decision is shown as decided by
Jev. A pick below the threshold, or a fallback, leaves the decision with the
chief. The outcome tools refuse a subject `decide` has not been called for; a
disposition the owner gave and the chief relays needs none. The result belongs
to the subject occurrence (the question, the contest, the objection, the
amendment, the discovery or the finding of one final review), is reused when
`decide` is called again, and survives a restart. A finding triaged as needing
code is stated in the next plan revision the chief files with `route_amendment`,
and one left to the owner is shown on the delivery card with the chief's
reason. When the service refuses the transition a binding pick calls for, the
item goes to the owner's inbox with Jev's judgment (a discovery as a
`discovery` entry, an amendment as its amendment card, a finding on the
delivery card) and Jev is not asked again.

Each judgment is recorded under its workstream in
`judgments/<id>.json`, one revision when it starts and one with its result,
and its usage is a ledger cost under the `jev` role. A judgment asked again
with the same inputs, after a restart included, returns the recorded decision.
`osmia status` reports Jev as `unconfigured`, `ready` or `degraded`, with the
latest failure and any cool-down, and its spend appears as the `jev` provider
in today's usage. `osmia doctor` fails when the key is not set.

### Resolution, retention and shared lifecycle

A workstream's own slice, spec, plan and workspaces leave active state and new
bundles when it resolves (merged or cancelled). Its trace remains until owner
archive. The workgroup's workgroup spec stays active for the shared chief and
every unresolved member until every member resolves, including a dependent
waiting for or repairing after an upstream merge. Only then does it leave
active views and bundles; workgroup spec history and owner decisions remain in
the trace until archive. A stored `delivered` publication state alone does not
mean merged: an open, unmerged PR shows **PR open** in the project pane.
A ready but unmerged PR keeps its member unresolved and the workgroup spec active.
Merge is detected by GitHub polling or recorded by the owner with `osmia merged`.
Resolution requires cancellation/abandonment or delivery with an owner merge
record or a recorded merged outcome matching the current publication's PR.
Closed but unmerged PRs, outcomes for earlier PRs and delivery without evidence
remain unresolved. One-member workgroups use the same rule; their existing
delivery cleanup remains separate from group resolution.

Every delivered member's current PR is observed at most once per configured
`pull_request_poll` interval,
with or without a same-repo base or downstream edges. Observation stops on a
matching merged or closed outcome. Failed lookups record nothing and retry
on the next cadence; a closed-without-merge outcome remains unresolved.
These are service rules, with no additional polling configuration.

Archive, by the owner or by the chief of staff, and eligible cleanup of a terminal member preserve the original
shared conversation, active workgroup spec and status for unresolved survivors,
including across restart and when the retired member was the conversation's
source or the controller's driver. Durable archive identity and verified
resolution evidence, followed by cleanup intent and confirmed removal, locate
the original authority without recreating a deleted member trace. Archive is
never a merge event and cannot release edges or grant seal, admission or
publication permission. Missing or corrupt live history, inactive or
misconfigured repos and incomplete joins yield errors or recoverable holds;
absence or an arbitrary API failure does not prove retirement or resolution.

Approved amend/cancel journals keep their exact original approval and document
pins. Survivor effects reconcile once through the original group authority.
If a remaining effect requires unavailable retired history, the journal and
pending effect stay recorded with an explicit recoverable diagnostic; they
are never discarded, declared applied or retargeted to newer revisions.
Shared inbox questions retain their original ordered origins and ruling links;
sufficient recorded evidence resumes eligible survivors once, while genuinely
unavailable ruling or relay effects remain pending.

### Permanent workstream archives

`osmia archive <workstream>` permanently archives delivered or abandoned work.
The chief of staff also archives a delivered workstream on its own, as itself,
once the own-pull-request outcome recorded for the workstream's current
publication is `merged`: every reconciliation pass picks the delivered,
unarchived workstreams with that evidence, including ones recorded before this
behaviour existed, and archives them through the same path, guards and record
as the owner's archive. It adds no pull request lookup. It writes nothing
and tries again on a later pass when the publication or its outcome changed
since the pick, a workgroup change holds the archive, or the workstream's
state changed. Open, closed-without-merge and unobserved pull requests,
an earlier pull request's merge, an owner-recorded merge without a pull
request of its own, abandoned workstreams, sessions and workstreams reopened
out of delivered are never archived this way, and an existing archive is never
replaced. The owner's archive stays available for every terminal workstream.
`runtime.json` keeps a small archive record outside the deleted storage: the
project, the workstream ID, the title, the outcome (delivered or abandoned),
the archive time and, where one exists, the delivery link (the pull request
URL, or the recorded upstream commit for an owner-recorded merge). The
record also pins `resolution_evidence` from the member's history as `merged`,
`cancelled` or `unresolved`; `resolved` is true only with merged or cancelled
evidence. Both fields are exposed in the API's runtime archive records. The
record's optional `actor` is `chief_of_staff` for an automatic archive and
absent for the owner's, including every record made before the chief of staff
archived; the API shows it as `archived_by` (`owner` or `chief_of_staff`). The
chief's conversation shows an entry saying the workstream was archived because
its pull request merged, with the pull request link, derived from the committed
archive record: it appears once, survives an interruption, and is absent for
a refused attempt and for an owner archive. A shared chief thread keeps it
after the member's cleanup; a solo workstream's conversation, with its entry,
goes when cleanup removes its trace. An
archived member without that evidence keeps the workgroup unresolved and its
workgroup spec active. The pane shows **blocked** with the wait
`archived member <id> has no pinned merge or cancel evidence; workgroup remains unresolved`.
An older `resolved` flag without evidence is re-derived from retained history
where possible; otherwise the hold remains visible. Archive and cleanup alone
never supply resolution evidence.

The service then deletes the workstream's own independent storage, its trace
directory and its temporary workspaces, including all check
stdout/stderr, documents, decisions and agent turns, from disk. An archive
cannot be restored or unarchived. Resolution removes active documents and temporary workspaces while retaining
the trace and target-repository branches. Publication may require further
dependent repair or approved rework; it never authorizes trace deletion.

The cleanup pass first fills in the title, outcome and delivery link in
`runtime.json`, for an archive recorded before this behaviour existed too,
while the trace is still present. It then waits for unfinished turns, pending
or unacknowledged operations and temporary workspace cleanup. A base's trace
also waits while another retained workstream depends on it as a base
(archived bases may leave together with their archived dependents). Once every wait clears, the
service records durable cleanup intent, closes the workstream's database and
moves its trace directory aside by one atomic rename. Directory listings no
longer show it as live, and its removal then runs without holding any trace
lock, so deleting even a
large trace never delays a read, a write or reconciliation of any other
workstream. The archive record is marked clean, `trace_deleted` in the API,
only once the storage is confirmed gone; a later pass reconciles whether an
earlier attempt was interrupted by a crash or is simply still in flight, so a
half-deleted trace is never mistaken for a live one. Previously archived
workstreams undergo this same cleanup automatically, once per archive. Other
workstreams and project records are preserved. Copies made outside Osmia's
root are outside this cleanup.
