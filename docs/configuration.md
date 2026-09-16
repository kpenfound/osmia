# Configuration

`internal/config.Load` reads and validates the entire declarative configuration.
It returns a configuration only when the top-level file and the active project's
file pass. It creates no directories, repositories, sockets or state and does
not launch agents. The [local service](service.md) owns startup and the Unix
socket. Project registration through `osmia project add` writes the project
file and edits `active_projects`; see [project registration](#project-registration).
Reload and migrations are separate work.

## Root and identity

`Options.Root` takes precedence over the default `~/.osmia`; no environment
variable overrides it. Relative explicit roots resolve against the process working
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
generation on collision. The [trace repository](trace.md) reserves project and
workstream identities when creating their directories. Workstream keys are not a
configuration list; they belong to persisted workstream manifests.

```text
<root>/config.toml
<root>/runtime.json
<root>/osmia.sock
<root>/project-add.json                       journal of one interrupted project registration
<root>/projects/<project-id>/config.toml
<root>/projects/<project-id>/                  dedicated project trace repository
<root>/projects/<project-id>/workstreams/<workstream-id>/
```

`Root` provides checked helpers for these paths. Managed state paths reject
symlink aliases, including aliases within the root that could collapse two
identities. The clone and root must be separate, non-nested directories, including
after resolving symlinks. Relative clone paths resolve from the project configuration
directory; missing clones are accepted for configuration purposes. Nothing is
written in the clone. Helpers check existing paths, not future filesystem changes;
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
one. At most one ID is accepted until multi-project operation arrives in M7.
Additional project directories may hold archived traces and are not scanned or
activated. Duplicate IDs are errors; two distinct active IDs report unsupported
multi-project operation. The top-level file, and the active project's file, must
exist and specify `version = 1`. Unknown versions, keys (including empty unknown
tables and case variants), duplicate TOML keys, malformed TOML and incorrect
types are errors.

The following optional settings show their defaults:

```toml
[listen]
# Default: <resolved-root>/osmia.sock, including when the root is overridden.
# A relative value resolves against the root; ~/ is also supported.
socket = "osmia.sock"

[capacity]
masons = 4
reviewers = 2
committee = 3
per_workstream = 2

[shed]
max_rounds = 3
max_bounces = 3

[events]
window = "5s"

[roles.mason]
profile = "default"
sandbox = "none"
```

The socket must be a direct child of the resolved root with a `.sock` suffix,
separate from managed state. Its absolute path may contain at most 103 bytes
for portability across Unix socket implementations. An existing path must be a
Unix socket; the loader neither binds nor removes it. All capacity and shed values
must be positive integers. `events.window` is a positive Go duration: how long
the oldest undelivered event of a workstream waits before the service delivers
it, with every other ready event, as one
[chief-of-staff turn](service.md#event-delivery). A workstream cap may exceed global mason capacity:
the global pool still limits concurrent execution.

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
| `sandbox` | `none` | `none` (host execution), `claude` or `container` |
| `image` | empty | Required for `container`; forbidden with other modes |

A `claude` sandbox requires Claude in every profile in the role's fallback chain.
No arbitrary mounts, credentials, environment, tools or capability grants are
accepted from these files. Sandbox settings describe requested execution; even
`none` requires the service's full isolation contract. The current pinned core
runner cannot enforce that contract and rejects execution until an enforcing
boundary is supplied; see [the adapter boundary](core-adapter.md). Successful
configuration loading does not imply that execution is available.

## Project registration

`osmia project add <name> --upstream OWNER/REPO --fork OWNER/REPO --clone PATH`
(API: `POST /v1/projects`) registers a project with the running service. The
client never writes configuration; the service validates the request before
writing anything, then:

1. generates a fresh project ID and journals the registration in
   `<root>/project-add.json`;
2. writes `projects/<id>/config.toml` with `name`, `upstream`, `fork`, the
   absolute `clone`, `base_branch` (default `main`) and
   `landing = "commit-per-unit"`; capacity is inherited from the top level;
3. seeds the [local entity map](knowledge-base.md#seeding) from the clone's
   CODEOWNERS and directory structure, and creates the trace repository with
   a [charter](charter.md) template in `charter.md` and the seed as the first
   revisions of the charter and `kb/entities.json`;
4. adds the ID to `active_projects` in the top-level `config.toml` as a text
   edit, so the owner's comments, ordering and formatting survive;
5. creates the librarian's workstream and thread in the trace and requests
   the first [knowledge-base extraction](knowledge-base.md#extraction) as a
   durable operation;
6. activates the project (opens the trace and starts reconciliation, which
   runs the extraction) and removes the journal.

Validation refuses, each with its own message: a blank name, an upstream or fork
that is not `owner/repository`, a fork equal to the upstream, an invalid base
branch, a relative clone path, a clone that does not exist, is not a directory
or has no `.git` entry, and a clone nested with the root either way. Nothing is
ever written to the clone; seeding only reads it.

Registration is recoverable. If the service stops at any step, the journal makes
the next start finish the registration with the same ID, or `osmia project add`
run again finishes it. A retry never creates a second trace repository and an
interrupted registration never blocks a retry: the same request returns the
finished project, a different request while a project is active is refused with
the active project's ID. A trace initialization that never committed is
discarded and redone; one with history is kept. The extraction is requested
once; a retry finds it in the trace and does not request another.

Operation stays single-project until M7: adding another project while one is
active is refused, and the error names the active project. Repeating the active
project's exact registration returns it without change.

`osmia project remove <project-id>` (API: `DELETE /v1/projects`) removes the
active ID from `active_projects` as a text edit and closes the project's runtime
state. The trace directory and the owner's clone are not deleted. Adding the
same upstream again afterwards generates a new project ID and a new trace
repository; the archived trace stays untouched under `projects/<old-id>/` and is
never reused, so archived history is immutable and every add is a fresh start.

The service's loaded configuration changes only in its project after add and
remove: `/config` reports no `restart_required` diagnostic for these edits.
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
fork = "my-account/repository"      # required, distinct from upstream
clone = "~/src/repository"          # required; may not yet exist
base_branch = "main"
landing = "commit-per-unit"

[capacity]
per_workstream = 2                  # defaults to top-level capacity.per_workstream
```

Repository references contain two nonempty owner/repository components using
ASCII letters, digits, `.`, `_` and `-`, beginning with a letter or digit, without
a `.git` suffix. Fork and upstream must differ ignoring case. `base_branch` follows
Git branch-name constraints. `landing` accepts `commit-per-unit` (default) or
`squash`. Project `capacity.per_workstream` is a positive integer overriding the
global default.

## Milestone and restart behavior

Loading accepts profiles, bindings, capacity, shed limits, repository identity
and landing preferences as declarative inputs. It does not implement automatic
fallback, debate, review scheduling, landing or multi-project dispatch. Review slot
configuration is `capacity.reviewers`; no separate review-policy schema is defined.
Runtime profile overrides, pauses and priorities belong in `runtime.json`, never
these files; see [runtime overrides](runtime.md) for M1 persistence and resolution.
Nothing here performs a live reload: changed settings other than project
registration and removal require a new load/service start. Root and listen
changes will still require a service restart when live reload arrives.

The full design's `listen.tailnet`, `listen.web`, `budget`, `notify`, `hearsay`,
project `upstream_rebase` and `hearsay_scope` settings are rejected as unsupported
in M1, even if supplied empty. Budget, priority scheduling effects, hard pauses that stop turns in flight,
and live reload belong to M4, tailnet/web and notifications to M5, multi-project operation to M7, and
Hearsay to M8. Unsupported keys do not silently enable later behavior.

Representative errors include the file and offending field:

```text
<root>/config.toml: profiles.default.fallback: unknown profile backup
<root>/config.toml: roles.mason.image: container requires an explicit image
<root>/config.toml: active_projects: at most one active project is supported; multi-project operation is unsupported
<root>/projects/<id>/config.toml: clone: clone and Osmia root must be separate, non-nested directories
```

Malformed TOML errors include the parser's location. Filesystem errors identify the
path. On any error `Load` returns `nil`, never a usable partial configuration.
