# Configuration

`internal/config.Load` reads and validates the entire declarative configuration.
It returns a configuration only when the top-level file and every active
repo configuration pass. It creates no directories, repositories, sockets or state and does
not launch agents. The local service owns startup and the Unix
socket. Registration through `osmia project add` or `project add-repo` writes
the repo file and edits `active_projects`; see [project registration](#project-registration).
A running service applies edited files through [reload](running.md#reload).
Startup performs the recoverable migrations described below.

## Projects, repos and workgroups

A project owns one charter and one or more repos. Each repo owns its upstream,
optional fork, clone, base branch, landing style, caps, KB and entity map.
Workgroups represent feature requests; their workstreams each target exactly
one repo and share one chief of staff. Footprints name entities from that repo.
Briefs, project specs, slices and all other factory state stay outside target
repositories. File-based context supports the complete flow without Hearsay.

The default repo's `p_…` ID is also the project ID. Additional repos have their
own `p_…` IDs and set `group_id` to that default ID in their configuration.
`active_projects` lists repo registrations, including the default. The shared
charter is a document in the default repo's trace,
`projects/<default-repo-id>/trace.db`, which the owner edits with
`osmia edit charter <project-id>` or in the web UI; each repo keeps its KB and
entities in its own trace. API project views expose the repo list;
compatibility fields such as `upstream` at the top level describe the default
repo. Use `osmia project repos <project-id>` to see the members.

Charter reads and edits through any repo use that same default-repo authority,
including worker bundles and owner-ratified rules. Added repos have no separate
charter file. Any charter revisions retained in a member repo are historical
records and do not supply its active policy.

The design agent uses the `chief_of_staff` role's profile, overrides and sandbox
configuration. There is no separate `design` role binding. Its grants restrict
it to the project charter, each repo's KB and read-only source access; it has
no VCS, delivery credentials, source writes or gate tools. The service records
its draft brief outside the repos. One owner start hands that brief to the
architect, which selects affected repos; Go validates and allocates one member
per selected repo. The same conversation becomes the shared chief thread.
One ratification seals the project spec, graph and slices together. See
[design sessions](cli.md#design-sessions) for commands.

Ordinary file, issue and stdin hand-ins use the same affected-repo selection
path when the project has two or more active repos. A single selected repo
keeps ordinary single-member ratification; several selected repos share one
all-member packet. A project with one active repo goes directly to ordinary
drafting. Selection adds no configuration or owner approval. See
[handing in work](design.md#142-handing-in-a-workstream) for input, base and
skipped-debate behavior.

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
<root>/projects/<repo-id>/config.toml
<root>/projects/<repo-id>/trace.db             dedicated repo knowledge trace; default repo holds project charter
<root>/projects/<repo-id>/osmia.lock           the trace's process lock
<root>/workstreams/<repo-id>/<workstream-id>/  the workstream's own trace.db and osmia.lock, separate from projects/<repo-id>/
<root>/workgroups/<workgroup-id>/              trace.db and osmia.lock: cross-repo record, project spec, shared chief and group decisions
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

At startup, each existing one-repo project becomes a default project holding
that one repo, retaining its ID and paths. Its charter is the project charter.
Every existing non-session workstream appears under it as a one-workstream
workgroup whose chief is its existing thread. This includes building,
delivered, abandoned and archived records. Existing exploratory sessions
stay outside workgroups, retain their session kind and thread identity and
remain excluded from factory passes. They are never converted to features.
Traces, rulings, decisions and archive records are preserved without owner
action. Migration is resumable and idempotent: an interrupted start finishes
on the next start, and repeated starts create no duplicates. This is separate
from the trace migration below; neither requires moving existing member
traces to a workgroup trace.

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
one. With several IDs the service runs every listed repo at once, each with
its own trace, reconciliation loop, lander and drift cadence. Additional repos
share the project of their `group_id`; that default repo must also be active. Role capacity
(`capacity.*`), budgets and the factory-level pause are shared by all of them:
a turn in flight on any project takes a slot of its role kind. Each repo
counts its own slots in use, waiting turns and daily spend from its own trace
and publishes them, so the shared limits and the status's capacity and daily
budget follow each repo's latest pass. Each repo's
`capacity.per_workstream` applies to its own workstreams. Runtime project-scoped
controls retain the selected repo registration's scope. Additional directories
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
# same API on port 80; empty disables.
tailnet = ""

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
Tailnet membership is the boundary: there is no in-app authentication
([tailnet listener](running.md#tailnet-listener)). All capacity and shed values
must be positive integers. `events.window` is a positive Go duration: how long
an undelivered event of a workstream waits before the service delivers it, with
every other ready event, as one chief-of-staff turn. `events.progress_window`
is how long an event of routine progress waits instead: a unit starting,
passing its checks, being approved or sent back, or landing, or the workstream
changing state. It goes out sooner with any other event. It must be no shorter
than `events.window`, and defaults to the longer of `15m` and `events.window`.
While an event turn of a workstream is queued or running, its new events wait
for the next turn. A workstream cap may exceed global mason capacity:
the global pool still limits concurrent execution.

`capacity.checks` must be positive, defaults to 2 and limits service Dagger check
runs across all projects and workstreams, including unit, final-review and session
runs. A run waits cancellably for a free slot in arrival order before selecting
and running checks. Waiting does not consume `checks_timeout` or count as an
incomplete run.
Check slots are independent of the model-role and per-workstream limits, so
other work continues while checks wait. Reloading applies a changed limit to
new starts immediately; executing runs finish even when the limit is lowered.

`mason.max_clean_turns` also bounds the reminders a mason, or a drift mason,
gets when it reports done with conflict markers left; past it the unit is
contested or the drift rebase is held. `loop.max_sessions` is how many agent
sessions a workstream may run without progress before the loop guard pauses
it: a change of state of the feature, a unit, the shed, an amendment, the
final review or the publication, a drift rebase that moved the branch, or
anything you do counts as progress. The pause is soft, attributed to the loop
guard, and listed in the inbox; resuming the workstream starts the count over.
It must not be negative, and `0` turns the guard off.

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
verifiable acceptance), `integration` (fit with the knowledge base's decisions,
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

Roles are `architect`, `committee`, `chief_of_staff`, `foreman`, `librarian`,
`mason` and `reviewer`. Every omitted role binding defaults to profile `default`.
If there is no profile named `default`, bind all seven roles explicitly. Role keys:

| Role key | Default | Validation/meaning |
| --- | --- | --- |
| `profile` | `default` | Must reference an existing named profile |
| `sandbox` | `none` | `none` (host execution), `claude`, `container` or `sbx` (Docker Sandboxes) |
| `image` | empty | Required for `container`; optional agent-specific template for `sbx`; forbidden with host modes |
| `dagger` | unset | Mason only, with `sbx`: the Dagger CLI `version` and host `engine` given to implementation turns; see [Dagger for masons](#dagger-for-masons) |
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

[roles.librarian]
sandbox = "sbx"

[roles.committee]
sandbox = "sbx"
```

All seven roles support this mode with `claude`, `codex` or `opencode` profiles.
An omitted `image` lets each fallback profile select its agent's default template.
A custom template must support every agent in that role's fallback chain. The
classifier uses the mason's sandbox and template with its own configured profile.
Reload applies role settings to subsequent turns; an active turn keeps its grants.

**Building the Osmia template**

From the Osmia checkout, run:

```sh
./scripts/build-sandbox.sh
```

The script requires a working `sbx` installation. It pulls the default Codex
template, installs the latest Codex and Claude Code plus Dagger CLI
`1.0.0-beta.17`, verifies the tools, and saves the local template as `osmia:latest`.
Tags such as `:latest` are supported for templates. Each run refreshes both agents
and replaces that local tag. The Dagger version is a constant in the script. The
temporary build sandbox has no workspace mounts and is removed when the script
exits.

Under each role that should use this template, set `sandbox = "sbx"` and
`image = "osmia:latest"` in the existing role configuration, then run
`osmia reload`. `image` is configured per role, not inherited from a profile.
The template supports Codex and Claude profiles, including fallback chains
and the classifier when configuring the mason. Docker may warn about using a
Codex-based template with Claude; both agent executables are installed. Build
under the OS account running Osmia so the service can access the local template.
Existing sandboxes keep their installed versions.

The template installs the Dagger CLI; engine access still requires the role's
[Dagger configuration](#dagger-for-masons). Keep its configured version aligned
with the script's pin.

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

Before a unit is reviewed, the service runs `dagger check --progress=report --failfast` on
a separate, fresh export of its candidate, made a Git root of its own so Dagger
finds the project's workspace. The copy and temporary home are removed
afterward. The run is bounded by the project's `checks_timeout` and is
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
Passing checks send the unit to review, and its reviewer receives the
result instead of running checks. Failing checks, which Dagger's report names,
send it back to its mason with the failures and output; the send-back counts
toward `shed.max_bounces`. At that limit, the unit stays contested until the
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

Before the final read, the service uses the same [smart-check selection](smart-checks.md)
on the rebased feature branch commit against its upstream base and runs the
selected checks, or every check on a fallback, within `checks_timeout`. It
records the run as `final/checks-<k>.json` for final review `k`, with the commit,
selection, command, exit status, failed checks and the diagnostic excerpt. The complete captured output
is recorded with it as `final/checks-<k>-output.txt`, accessible the same way.
The final reader's prompt carries the selection and result; neither it nor a
unit reviewer runs checks. A final run that reports no result is recorded as
incomplete, and the read waits for checks to complete. Retries wait longer
after each incomplete run, up to an hour, and reuse the recorded judgment for
the same review, commit and selection inputs, including after a restart. A
completed run is read back instead of running the checks again. The check client inherits only
PATH and service engine selection, not provider, GitHub or SSH credentials.
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

### Dagger for masons

A mason in `sbx` can run the project's Dagger checks and functions while it
works. Configure the CLI release and the host engine it runs against:

```toml
[roles.mason]
sandbox = "sbx"

[roles.mason.dagger]
version = "v0.20.5"                                 # the Dagger CLI release; match the engine's
engine = "docker-container://dagger-engine-v0.20.5"  # or tcp://<host>:<port>, unix:///<socket>
```

| Key | Validation/meaning |
| --- | --- |
| `version` | Required. A Dagger release such as `v0.20.5` or `0.20.5`, installed in each mason sandbox whose template lacks it |
| `engine` | Required. `docker-container://<container>`, `tcp://<host>:<port>` or `unix://<absolute socket path>` of an engine on the host |

Each mason turn asks its sandbox for `dagger version`. A template that already
has the configured release keeps it, so baking the CLI into the mason's `image`
saves an install per turn. Otherwise the turn installs that release with the
Dagger install script, and the sandbox network policy must allow `dl.dagger.io`.
A failed installation fails the turn before the agent starts.

The CLI reaches the engine through `_EXPERIMENTAL_DAGGER_RUNNER_HOST`. A
`docker-container` or `unix` engine is forwarded from a fresh loopback port that
belongs to the turn, and a `tcp` engine on the host's loopback is reached at its
own port. The sandbox reaches either one as `host.docker.internal`, and the port
is allowed for that sandbox alone. Cleanup removes the rule and the forward along
with the sandbox. The classifier and other roles never receive the engine.

The simplest engine is the one your own Dagger CLI provisions: run any Dagger
command on the host, such as `dagger core version`, and name its container,
`dagger-engine-<release>`. Each connection to a `docker-container` engine runs
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
skills for those turns. The classifier, which shares the mason's sandbox,
receives none.

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
5. creates the librarian's workstream, its own trace database and
   its thread, and requests
   the first knowledge-base extraction as a
   durable operation;
6. activates the project (opens the trace and starts reconciliation, which
   runs the extraction) and removes the journal.

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
manifest is kept. The extraction is requested
once; a retry finds it in the trace and does not request another.

Registration can add distinct projects while other projects run. Each active
registration needs its own clone. Repeating an active project's exact
registration returns it without change. Existing project runtimes and overrides
are retained.

`osmia project add` creates a project with exactly one default repo.
`osmia project add-repo <project-id> --upstream OWNER/REPO --clone PATH`
(API: `POST /v1/projects/repos`) uses the same registration journal to add a
repo to that project, with its own config, clone, KB and entity map, and the
project's existing charter. `--fork` and `--base-branch` are optional on both
commands. `osmia project repos <project-id>` lists the repo views returned by
`GET /v1/config`. There is no separate repo-list endpoint. The project pane
offers project creation and adding a repo with these same service operations.

### Knowledge maintenance and retrieval

Factory turns receive governing rules and decisions with references to project
knowledge. They retrieve KB prose, the entity map and informational project
notices with `project_context`, rather than carrying that material in every
prompt. The tool searches current records, lists at most 20 matches per page,
and reads at most 16 KiB per call. Reads identify the revision; subsequent pages
can pin it, and older revisions remain available. Private role notes and other
workstreams' records are not exposed by this tool. Workstream documents remain
available through `factory_context` and each role's scoped file view.

The librarian maintains concise current explanations on every extraction and
refresh. It removes superseded claims, duplicate explanations, routine status
and refresh histories, retaining applicable constraints and non-obvious pitfalls.
It references detailed evidence rather than copying it into the KB. The service
keeps provenance and historical revisions in the trace and source ledger.

Each KB prose file has an 8 KiB ceiling, and all prose together has a 64 KiB
ceiling; the entity map is excluded. The librarian is instructed to aim well
below these limits and can call `kb_check` on its isolated output to find and
repair problems before finishing. The service also validates at publication;
invalid output leaves the recorded KB unchanged. Existing KB documents are not
silently truncated: a successful librarian pass must consolidate them.

`osmia project remove <project-id>` (API: `DELETE /v1/projects`) removes the
active repo ID from `active_projects` as a text edit, stops admitting new work,
and waits for in-flight operations to finish before closing its runtime; other active projects keep running. The project's and every workstream's traces, and the owner's clone, are not deleted. Remove additional repos before removing the default repo; a default with other
active repos cannot be removed. Adding the
same upstream again afterwards generates a new project ID and new trace
repositories; the old ones stay untouched under `projects/<old-id>/` and
`workstreams/<old-id>/` and are
never reused, so archived history is immutable and every add is a fresh start.

The service's loaded configuration changes only for the registration added or
removed: `/config` reports no `restart_required` or `reload_required` diagnostic
for these edits.
Without a project, `/config` and `/runtime` carry a `no_project` diagnostic and
project-scoped overrides are rejected as referencing an inactive project.

## Repo config.toml

Store this file at `projects/<repo-id>/config.toml`; `osmia project add` or
`osmia project add-repo` writes it. The directory ID is the identity; there is no second configurable ID
to disagree with it.

```toml
version = 1
name = "My repo"                    # optional display metadata, default empty
# group_id = "p_0123456789abcdef0123456789abcdef" # additional repo: its project ID
upstream = "upstream/repository"    # required owner/repository, not a URL
fork = "my-account/repository"      # optional; omit to push feature branches to upstream
clone = "~/src/repository"          # required; may not yet exist
base_branch = "main"
landing = "commit-per-unit"
upstream_rebase = "6h"              # default; "0" disables scheduled drift rebases
pull_request_poll = "10m"           # default; minimum 5m between pull request checks
checks_timeout = "15m"              # default; how long one run of a candidate's checks may take
classifier = "default"              # optional; omitted by default

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
waits only for its own workstream's rebases and pending knowledge refresh.
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
and incoming hard-block and merge-order edges. Failed lookups also wait for the
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
of a unit candidate's checks, or of a final review's checks, may
take. It defaults to `"15m"`; raise it for a repo whose `dagger check` runs
longer. A run that outlasts it did not complete: the unit stays checking and its
checks run again later, and a final review's reader is told its checks did not
complete. Listing the check links for a Jev selection has its
own five-minute bound. A change applies to runs that start after a reload.

`classifier` names a top-level profile for clean mason turns with no accepted
outcome. When omitted, code heuristics classify the response without a model
call. An unknown profile is rejected. The classifier inherits the mason's
sandbox, uses an empty read-only workspace and receives no service tools or native write, shell or web tools. A Claude
sandbox requires a Claude classifier profile. Its attempts are limited to two,
each with a 30 second maximum timeout. Invalid or unavailable answers retain
the code classification. With the Jev boost on, a Jev judgment classifies the
response first, and the classifier runs only when that judgment falls back.

## Configuration and restart behavior

Loading accepts profiles, bindings, capacity, budgets, shed limits, repository identity
and landing preferences as declarative inputs. Review slot
configuration is `capacity.reviewers`; no separate review-policy schema is defined.
Runtime profile overrides, pauses, priorities and archived workstreams belong
in `runtime.json`, never these files.
`osmia config` shows whether each file on disk differs from what is loaded
([disk drift](running.md#disk-drift)), and `osmia reload` applies edited files
to a running service after validating all of them ([reload](running.md#reload)). The root, `listen.socket`,
`listen.web` and `listen.tailnet` keep their loaded values until the service
restarts. Role skills and `skills.refresh` apply to turns that start after a
reload. The `active_projects` list applies live: additions start their retained
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

Each enabled repo names its Hearsay scope in its repo `config.toml`:

```toml
hearsay_scope = "example"

[hearsay_entities]
"internal.storage" = "example-storage"
```

The optional entity mapping selects scope bundles for unit footprints; without
mapped footprint entities, the project scope is used. An empty repo scope
keeps that repo in file mode. Settings apply through reload. Hearsay must
configure the named principals with the worker, orchestrator, and observer
classes and grants intersected with the owner's access.

Bundles preserve local charter, documents, entities and rulings. External
context is separately labelled, limited to 8,000 bytes across scope bundles,
and fetched under a three-second deadline. Missing credentials, invalid
responses and outages produce `degraded` context with the local bundle intact.
Status reports `file`, `hearsay`, or `degraded`; a configured service starts
unverified (`degraded`) until a bundle succeeds. Credentials are not forwarded
through HTTP redirects.

The service exposes Hearsay read tools within role turns. Workers and the chief
also receive `assert`, which proposes an agent learning and does not ratify an
owner ruling. The endpoint, human token and agent tokens remain service-owned.
Live Hearsay sources, identities, authority policy, entities and anchors are
configured by the operator in Hearsay.

Hearsay watches use the orchestrator identity. Their opaque cursors, seen event
identities and chief notices commit together in the local trace. Remote failures
retain the last cursor and leave local work running. Changed connection or
identity settings select a separate cursor; the first watch begins at Hearsay's
current position. Existing history remains available through bundles and read
tools. Feedback on a delivered workstream can produce a notice or question; it
cannot reopen implementation.

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

## Optional Jev boost

The Jev boost asks Jev, TypeSafe's System One model, bounded typed questions
inside turns and the service's check runs. One setting turns it on or off for
every judgment; it is off by default, and with it off Osmia makes no Jev
request and needs no Jev credential. The other settings show their defaults:

```toml
[jev]
enabled = false
# Base URL of a TypeSafe-compatible API; requests go to <url>/v1/systemone.
url = "https://openrouter.ai/api"
# The model asked. An alias moves when a new version ships; name a versioned
# model your provider accepts to keep tuned thresholds stable.
model = "~typesafe/jev-latest"
# The environment variable holding the API key.
api_key_env = "OPENROUTER_API_KEY"
# How long one request may take, at most 1m.
timeout = "10s"
```

The settings are validated whether or not the boost is on, and apply through
reload. The service reads the key when it makes a request; configuration
responses, sessions and trace records never contain it, and it is not sent
through a redirect. A judgment's state, such as a unit candidate's diff for
smart checks, is sent to the configured provider.

Every judgment falls back to the workflow as it runs without Jev when the key
is missing, the request times out, is rate limited, fails or is refused, the
response does not answer the questions as asked, or the answers fall below the
judgment's own threshold. A transient failure is retried once. A rate limit,
or three transient failures in a row, cools Jev down for every judgment for 30
seconds, doubling with each further episode up to 15 minutes or as long as the
provider's `Retry-After` asks within that cap; judgments fall back without a
request meanwhile.

With the boost on, a clean mason turn without an outcome is classified by Jev
before the `classifier` profile and the code rules. Jev chooses the class and
the sentence of the response that supports it, which becomes the recorded
evidence. A class below its threshold, a stricter one for `gave_up`, or one no
sentence supports falls back to the classifier, then the code rules. The turn's
classification records `by` as `jev`, `classifier` or `rule`, and a Jev class
names its judgment, which the chief of staff's notice cites.

With the boost on, a unit's check run lists the project's checks with
`dagger list checks --all --format=link` and asks Jev which of them the
candidate's changed files can affect, then runs only those links. A project
with more than 128 check links is asked about its collections' items, such as
a Go package's tests rather than each test. When Jev selects no check, or any
judgment fallback applies, the run checks everything, and the run records why.

[Smart checks](smart-checks.md) describes the selection end to end: what Jev
is asked, what a run records, and `go run ./cmd/smartcheck`, which runs the
same selection on any Git working tree.

With the boost on, the chief of staff's turn that delivers a question asks
Jev, before the session starts, whether the question's honest answer would
change the sealed spec or plan, whether answering it as it proposes would
contradict one of the owner's rulings or the project's notices, and whether it
asks for a standing project rule. The prompt ends with each answer that
reaches its threshold as a signal naming its judgment and the outcome it
argues for, and the turn's response records that text as `advice`. The chief
of staff still chooses the outcome and explains a choice against a signal. A
fallback, or answers below every threshold, leaves the prompt as it is with
the boost off. A question's judgment is reused when the question is delivered
again, and names the question, so its signals can be compared with the
question's outcome.

While the boost is on, each judgment is recorded under its workstream in
`judgments/<id>.json`, one revision when it starts and one with its result,
and its usage is a ledger cost under the `jev` role. A judgment asked again
with the same inputs, after a restart included, returns the recorded decision.
`osmia status` reports the boost as `disabled`, `unconfigured`, `ready` or
`degraded`, with the latest failure and any cool-down, and its spend appears
as the `jev` provider in today's usage. `osmia doctor` warns when the boost is
on without its key.

### Resolution, retention and shared lifecycle

A workstream's own slice, spec, plan and workspaces leave active state and new
bundles when it resolves (merged or cancelled). Its trace remains until owner
archive. The workgroup's project spec stays active for the shared chief and
every unresolved member until every member resolves, including a dependent
waiting for or repairing after an upstream merge. Only then does it leave
active views and bundles; project-spec history and owner decisions remain in
the trace until archive. A stored `delivered` publication state alone does not
mean merged: an open, unmerged PR shows **PR open** in the project pane.
A ready but unmerged PR keeps its member unresolved and the project spec active.
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

Owner archive and eligible cleanup of a terminal member preserve the original
shared conversation, active project spec and status for unresolved survivors,
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
`runtime.json` keeps a small archive record outside the deleted storage: the
project, the workstream ID, the title, the outcome (delivered or abandoned),
the archive time and, where one exists, the delivery link (the pull request
URL, or the recorded upstream commit for an owner-recorded merge). The
record also pins `resolution_evidence` from the member's history as `merged`,
`cancelled` or `unresolved`; `resolved` is true only with merged or cancelled
evidence. Both fields are exposed in the API's runtime archive records. An
archived member without that evidence keeps the workgroup unresolved and its
project spec active. The pane shows **blocked** with the wait
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
(archived bases may leave together with their archived dependents), and until
the librarian refresh covering its landed units has incorporated the
workstream's learnings into project knowledge. Once every wait clears, the
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
