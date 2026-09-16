# M1 configuration

`internal/config.Load` reads and validates the entire declarative configuration.
It returns a configuration only when both files pass. It creates no directories,
repositories, sockets or state and does not launch agents. The
[local service](service.md) owns startup and the Unix socket. Onboarding,
reload and migrations are separate work.

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

This minimal configuration selects one project and supplies its agent model:

```toml
version = 1
active_projects = ["p_0123456789abcdef0123456789abcdef"]

[profiles.default]
agent = "claude"
model = "your-model-name"
```

Only IDs in `active_projects` are loaded. Exactly one is required. Additional
project directories may hold archived traces and are not scanned or activated.
Duplicate IDs are errors; two distinct active IDs report unsupported multi-project
operation. Both configuration files must exist and specify `version = 1`.
Unknown versions, keys (including empty unknown tables and case variants), duplicate
TOML keys, malformed TOML and incorrect types are errors.

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

[roles.mason]
profile = "default"
sandbox = "none"
```

The socket must be a direct child of the resolved root with a `.sock` suffix,
separate from managed state. Its absolute path may contain at most 103 bytes
for portability across Unix socket implementations. An existing path must be a
Unix socket; the loader neither binds nor removes it. All capacity and shed values
must be positive integers. A workstream cap may exceed global mason capacity:
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

## Project config.toml

Store this file at `projects/<project-id>/config.toml`. The directory ID is the
identity; there is no second configurable ID to disagree with it.

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

M1 loading accepts profiles, bindings, capacity, shed limits, repository identity
and landing preferences as declarative inputs. It does not implement automatic
fallback, debate, review scheduling, landing or multi-project dispatch. Review slot
configuration is `capacity.reviewers`; no separate review-policy schema is defined.
Runtime profile overrides, pauses and priorities belong in `runtime.json`, never
these files; see [runtime overrides](runtime.md) for M1 persistence and resolution.
Nothing here performs a live reload: changed settings require a new
load/service start in M1. Root and listen changes will still require a service
restart when live reload arrives.

The full design's `listen.tailnet`, `listen.web`, `budget`, `notify`, `hearsay`,
project `upstream_rebase` and `hearsay_scope` settings are rejected as unsupported
in M1, even if supplied empty. Budget, pause/priority scheduling effects and live reload belong
to M4, tailnet/web and notifications to M5, multi-project operation to M7, and
Hearsay to M8. Unsupported keys do not silently enable later behavior.

Representative errors include the file and offending field:

```text
<root>/config.toml: profiles.default.fallback: unknown profile backup
<root>/config.toml: roles.mason.image: container requires an explicit image
<root>/config.toml: active_projects: M1 requires exactly one active project; multi-project operation is unsupported
<root>/projects/<id>/config.toml: clone: clone and Osmia root must be separate, non-nested directories
```

Malformed TOML errors include the parser's location. Filesystem errors identify the
path. On any error `Load` returns `nil`, never a usable partial configuration.
