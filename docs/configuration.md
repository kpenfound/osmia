# Configuration

`internal/config.Load` reads and validates the entire declarative configuration.
It returns a configuration only when the top-level file and the active project's
file pass. It creates no directories, repositories, sockets or state and does
not launch agents. The local service owns startup and the Unix
socket. Project registration through `osmia project add` writes the project
file and edits `active_projects`; see [project registration](#project-registration).
A running service applies edited files through [reload](running.md#reload).
Migrations are separate work.

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

Project IDs are `p_` followed by 32 lowercase hex digits; workstream IDs use `w_`.
`NewProjectID` and `NewWorkstreamID` generate random 128-bit keys. Persist the key
once; changing a display name, upstream or clone does not change it. Parsing accepts
only the canonical format, so traversal, absolute names and case aliases cannot
become identities. `CheckProjectIDs`/`CheckWorkstreamIDs` reject duplicates with
`ErrCollision`; constructors also accept existing IDs to check. Generation does
not reserve a key: persistence callers must reserve it atomically and retry
generation on collision. The trace repository reserves project and
workstream identities when creating their directories. Workstream keys are not a
configuration list; they belong to persisted workstream manifests.

```text
<root>/config.toml                             top-level file of an explicit root; the default is ~/.config/osmia/config.toml
<root>/runtime.json
<root>/osmia.sock
<root>/tailnet/                                embedded Tailscale node state, when listen.tailnet is set
<root>/project-add.json                       journal of one interrupted project registration
<root>/notifications.json                     ledger of owner notifications, once notify.webhook has been set
<root>/projects/<project-id>/config.toml
<root>/projects/<project-id>/                  dedicated project trace repository
<root>/projects/<project-id>/workstreams/<workstream-id>/
<root>/branches/<project-id>/<workstream-id>  the workstream's feature branch, a workspace of the clone
```

`Root` provides checked helpers for these paths. Managed state paths reject
symlink aliases, including aliases within the root that could collapse two
identities; an explicit root's `config.toml` is managed the same way. The clone
and root must be separate, non-nested directories, including after resolving
symlinks. Relative clone paths resolve from the project configuration
directory; missing clones are accepted for configuration purposes. Loading
configuration writes nothing in the clone. Helpers check existing paths, not future filesystem changes;
state writers must protect against concurrent symlink replacement.

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
one. With several IDs the service runs every listed project at once, each with
its own trace, reconciliation loop, lander and drift cadence. Role capacity
(`capacity.*`), budgets and the factory-level pause are shared by all of them:
a turn in flight on any project takes a slot of its role kind. Each project's
`capacity.per_workstream` still applies to its own workstreams, and a project
pause holds only that project. Additional project directories may hold archived
traces and are not scanned or activated. Duplicate IDs are errors. The
top-level file, and every active project's file, must exist and specify
`version = 1`; one project file that fails to load fails the whole load. Unknown versions, keys (including empty unknown
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

[beekeeper]
# The Beekeeper's runtime session settings. It runs in its own shadow
# project, separate from every registered project's configuration.
name = "Beekeeper"
profile = "default"
sandbox = "none"
# image = "example/osmia-claude:1" # only with sandbox "container" or "sbx"


[roles.mason]
profile = "default"
sandbox = "none"
skills = []
```

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
prints a login URL on first run. The auth key never belongs in these files.
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

Before a unit is reviewed, the service runs `dagger check --progress=report --fail-fast` on
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
toward `shed.max_bounces`. A run that reports no result, because the engine is
unreachable, the run timed out or the project has no Dagger checks, leaves the
unit checking, tells the chief of staff and runs again after ten minutes. After
three such runs of one candidate in a row the unit is contested instead: a
ruling of review runs its checks again, and revise returns it to its mason. One
run at a time per workstream; a pause holds runs not yet started.

Before the final read, the service runs every check the same way on the
rebased feature branch commit, within `checks_timeout`, and records the run as
`final/checks-<k>.json` for final review `k`, with the commit, command, exit
status, failed checks and the diagnostic excerpt. The complete captured output
is recorded with it as `final/checks-<k>-output.txt`, accessible the same way.
The final reader's prompt
carries the result; neither it nor a unit reviewer runs checks. A final run
that reports no result is recorded as incomplete, and the reader is told the
checks show nothing about the commit. A retry of the review reads the recorded
run back instead of running the checks again. The check client inherits only
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
   tracked CODEOWNERS and directory structure, and creates the trace repository with
   a [charter](charter.md) template in `charter.md` and the seed as the first
   revisions of the charter and `kb/entities.json`;
4. adds the ID to `active_projects` in the top-level `config.toml` as a text
   edit, so the owner's comments, ordering and formatting survive;
5. creates the librarian's workstream and thread in the trace and requests
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
run again finishes it. A retry never creates a second trace repository and an
interrupted registration never blocks a retry: the same request returns the
finished project; a different request waits until the pending registration finishes. A trace initialization that never committed is
discarded and redone; one with history is kept. The extraction is requested
once; a retry finds it in the trace and does not request another.

Registration can add distinct projects while other projects run. Each active
registration needs its own clone. Repeating an active project's exact
registration returns it without change. Existing project runtimes and overrides
are retained.

`osmia project remove <project-id>` (API: `DELETE /v1/projects`) removes the
active ID from `active_projects` as a text edit, stops admitting new work,
and waits for in-flight operations to finish before closing its runtime; other active projects keep running. The trace directory and the owner's clone are not deleted. Adding the
same upstream again afterwards generates a new project ID and a new trace
repository; the archived trace stays untouched under `projects/<old-id>/` and is
never reused, so archived history is immutable and every add is a fresh start.

The service's loaded configuration changes only in its project after add and
remove: `/config` reports no `restart_required` or `reload_required` diagnostic
for these edits.
Without a project, `/config` and `/runtime` carry a `no_project` diagnostic and
project-scoped overrides are rejected as referencing an inactive project.

## Project config.toml

Store this file at `projects/<project-id>/config.toml`; `osmia project add`
writes it. The directory ID is the identity; there is no second configurable ID
to disagree with it.

```toml
version = 1
name = "My project"                 # optional display metadata, default empty
upstream = "upstream/repository"    # required owner/repository, not a URL
fork = "my-account/repository"      # optional; omit to push feature branches to upstream
clone = "~/src/repository"          # required; may not yet exist
base_branch = "main"
landing = "commit-per-unit"
upstream_rebase = "6h"              # default; "0" disables scheduled drift rebases
checks_timeout = "15m"              # default; how long one run of a candidate's checks may take
classifier = "default"              # optional; omitted by default

[capacity]
per_workstream = 2                  # defaults to top-level capacity.per_workstream
```

Repository references contain two nonempty owner/repository components using
ASCII letters, digits, `.`, `_` and `-`, beginning with a letter or digit, without
a `.git` suffix. Fork and upstream must differ ignoring case. `base_branch` follows
Git branch-name constraints. `landing` accepts `commit-per-unit` (default) or
`squash`: it chooses whether an approved workstream is
published with each unit's commit or as one commit. Project `capacity.per_workstream` is a positive integer overriding the
global default.

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
times is held: the workstream takes no scheduled drift rebase until
`osmia rebase` or `osmia project rebase` asks for one or the chief of staff hands it back, and the inbox lists it until
then. A value that does not
parse as a Go duration, a TOML number, a negative duration, or a nonzero
duration shorter than `1m` is rejected.

`checks_timeout` is a Go duration string from `1m` to `24h`: how long one run
of a unit candidate's checks, or of a final review's checks, may
take. It defaults to `"15m"`; raise it for a project whose `dagger check` runs
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

Each enabled project names its Hearsay scope in project `config.toml`:

```toml
hearsay_scope = "example"

[hearsay_entities]
"internal.storage" = "example-storage"
```

The optional entity mapping selects scope bundles for unit footprints; without
mapped footprint entities, the project scope is used. An empty project scope
keeps that project in file mode. Settings apply through reload. Hearsay must
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

### Permanent workstream archives

`osmia archive <workstream>` permanently archives delivered or abandoned work.
The archived list keeps the title and workstream ID, with the terminal state and
archive time. The service deletes the entire workstream trace, including all
check stdout/stderr, documents, decisions and agent turns, from disk and the
trace repository's Git history. An archive cannot be restored or unarchived.
Delivery and abandonment alone remove temporary workspaces while retaining the
trace and target-repository branches.

The cleanup pass first saves the title in `runtime.json`, then waits for
unfinished turns, pending operations and temporary workspace cleanup. A base's
trace also waits while another retained workstream depends on it. Cleanup
retries failures and resumes interrupted deletion after restart. Previously
archived workstreams undergo this cleanup automatically, once per archive;
completion is recorded in `runtime.json`. Other workstreams and project records
are preserved. Copies made outside Osmia's root are outside this cleanup.
