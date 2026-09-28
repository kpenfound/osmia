# Turn isolation

`internal/isolation.Turns` is the service-owned preparation path for
`internal/thread.Runner`. The thread claims a durable request first, then the
preparation path selects resources using its immutable scope. Workspace selection,
file copying, MCP hosting, execution verification and cleanup failures are captured
in the owned turn response and survive reopening the trace repository.

The service supplies a workspace provider, a view directory under the Osmia root,
role grants, a scope selector, an MCP host factory and an execution engine. These
are Go dependencies, not repository or profile configuration. The service runs
the librarian's [knowledge-base extraction](knowledge-base.md#extraction) and
[refresh](knowledge-base.md#refresh-after-landing), the
architect's drafting and [replies to the shed](service.md#the-architects-reply),
the committee's [shed rounds](service.md#the-shed-debate)
and chief-of-staff thread turns through it; see
[running turns](service.md#running-turns).

## Files and lifetime

The provider lease belongs to the service and must exclude concurrent source
writers while copying. The selector names explicit relative files or directories.
Each turn gets a fresh private copy in service storage disjoint from the provider
workspace. Selecting a directory recursively includes its directories and regular
files, excluding `.git`, `.jj`, `.hg` and `.svn`, symlinks and special files at
every depth; a view never holds a symlink. Explicitly selecting metadata, a
symlink or a special file, or a path through traversal or a symlink, fails
acquisition with no view left behind. Executable owner bits are preserved;
group/world access is removed. No hard links to source files are created.

The runtime receives only the copied directory, never the provider workspace,
lease or VCS handle. Built-in `file_read` and `file_write` handlers use `os.Root`
and reject absolute paths, traversal, symlinks and metadata components. The
read-only view also rejects writes in the handler. Files added by a writable role
remain inside that turn's view. The optional service `Capture` callback can inspect
or snapshot output before cleanup, including partial output from failed execution.
`Turns` itself copies nothing back and makes no commit, branch landing or
delivery; a [unit workspace](service.md#unit-workspaces) or a drift
[resolution workspace](service.md#drift-rebases) provides the `Capture` that
copies a mason turn's view back into the workspace.

The service closes the MCP host, view and provider lease, in that order, using a
non-cancelled cleanup context. Cleanup errors accompany the
turn result. Views and MCP credentials are fresh on every turn, including resumed
threads; backend state is not an authority to reuse a prior grant. The thread
runner's resume check is forwarded to the execution engine through the same
executor; an engine that cannot verify a saved session selects replay.

## Capabilities

The service must explicitly grant a role its tool names and write/execute/network
permissions. A missing role grant fails. The workspace-write ceiling permits
`mason` and `librarian`; other known roles are read-only and have no execute or
fetch permission. Some tools belong to one role: `set_status`, `prioritise`, `decide_amendment`, `decide_charter`, `answer`,
`escalate`, `relay_ruling`, `route_amendment` and `propose_charter` are removed from every
other role's grant, so only `chief_of_staff` can see them. `amend` is limited to
`mason` and `reviewer`, and `ask` is removed
from the `chief_of_staff` grant, so every other role can hold it and the chief
of staff cannot. `object`, `concede` and `final_report` are removed from every
grant but the `committee`'s, `reply` from every grant but the `architect`'s, and `verdict`
from every grant but the `reviewer`'s. The selector's
optional `Narrow` request intersects the grant. Neither prompts nor profiles
grant tools.

The service registry classifies each tool as read, write, memory, execute, fetch
or VCS. Memory tools change only service-owned records bound to the turn, such as
private role memory, the [workstream status](trace.md#workstream-status), the
architect's [delivered draft](service.md#architect-drafting) and its
[answers to a round](service.md#the-architects-reply) or a committee
member's [shed contributions](service.md#a-members-turn), and
enforce their own scope, so any granted role may use them. `Turns.Scoped` supplies
trusted handlers bound to the claimed turn's scope, such as the trace's
[private role notes](trace.md#private-role-notes). They join the same registry,
duplicate-name and grant checks as `Turns.Tools`; an error fails preparation.
Registration requires both a granted name and a permitted effect. Unknown effects
and VCS tools are excluded; unknown granted names and duplicate registered names
fail preparation. The MCP adapter independently rejects effects exceeding its
grant before opening a transport. Handlers are trusted service code and must
enforce their declared effect and scope; arbitrary discovered handlers cannot be
registered. An empty grant exposes no built-in tools or MCP endpoint.

Only literal `LANG`, `LC_ALL` and `TZ` values may be supplied as public environment.
The MCP transport generates a fresh bearer token for each turn, which the service passes as `OSMIA_MCP_TOKEN`.
There is no environment inheritance, shell expansion or lookup of host provider,
GitHub, SSH-agent or cloud/delivery credentials. Unresolved credential references
fail closed. Provider transport/authentication is the execution engine's concern;
it must not give tools general network access or delivery credentials.

A workspace's `VCS()` access, which names the metadata a provider keeps (the
clone's `.git` for a git worktree; the Jujutsu repository and the clone's `.git`
for a Jujutsu workspace), is never granted to a session. A session in a Jujutsu
workspace sees the workspace's files without the `.jj` directory or writable
Git metadata. The service exposes no VCS tool to runtime agents.

## Enforced execution

`coreadapter.CoreExecutor` runs a turn through a busybees/core enforcer with
`agent.Grants` built from the verified isolation and service-owned MCP endpoints:

| Grant | Value |
|---|---|
| `Mounts` | the exact canonical view path with the view's access; the session directory read-only; for a read-only view, a writable `work` directory inside the session directory, where the session starts, because core runs every session in a writable directory |
| `Env` | the names of the service environment, which is the complete environment |
| `Tools` | one `mcp__osmia_<i>` server per scoped endpoint and no built-in tool |
| `VCS` | not granted |
| `HostServers` | for sbx, each scoped MCP server and its exact host loopback port; absent for other modes |
| `DaggerEngine` | not granted |

The engine hands out the enforcer of the role's sandbox mode:
`coreadapter.NewEnforcer` builds `agent.NewHostNone` for `none`,
`agent.NewHostClaude` for `claude`, `agent.NewContainer` with the role's
image for `container`, and `agent.NewSbx` with the resolved backend and optional
template for `sbx`. A host mode with an image, a container without one,
extra mounts and domain overrides are rejected. The view is checked again for
symlinks, special files and VCS metadata before construction.

The enforcer's `Prepare` asks the platform whether it can hold the grants. When
it cannot (confined `claude` on Linux, or a platform without a confiner), core's
`ErrUnsupported` is returned, wrapped in `coreadapter.ErrUnsupported` with core's
reason, and recorded as the turn's failure. For sbx, core checks the grants and
requires the CLI to answer `sbx version` during preparation; the runner checks the configured sandbox
and backend before starting the model. The executor reads the prepared
session's policy and refuses the turn unless it matches the grants: the sandbox
and image are the role's; the view is always readable, and writable exactly
when the role writes files; the provider workspace the view was copied from is not
writable; VCS is not granted and `gh`, `git`, `hg`, `jj` and `svn` are denied;
no built-in tool is granted; and the MCP servers are exactly the scoped
endpoints. An sbx policy must also name the resolved backend and exact host
server grants, with no Dagger engine. A mismatch is an `UnsupportedError` with capability
`session policy`.

The session then admits the request against its grants and refuses a
variable, tool, MCP server or mount the grants do not name, VCS access, and a
writable mount holding VCS metadata. A request field the grants do not describe
(VCS environment or executable paths, container environment, skills, container-use environment, network domains, a
host Dagger engine, different sandbox or image, a different allow list, or an MCP entry other than a
service-authenticated HTTP endpoint) is refused before a session is prepared.
Core's refusals (`ErrNotGranted`, `ErrUnsupported`, `ErrNoGrants`, and
`ErrPolicyChanged` when the policy no longer describes the turn) are wrapped in
`coreadapter.ErrUnsupported` and are not retried. Every refusal is returned
before the runtime starts, and the session is released after every attempt.

`coreadapter.CoreEngine` is the production engine. The hermetic engine
fixtures hand out core's `enforcertest.Enforcer`, which checks grants and
requests with core's own code and starts no process; they test construction,
permission checks and durable failures, not actual OS sandboxing.
`os.Root`, MCP filtering and profile flags are application restrictions and are
not, by themselves, an OS security sandbox.

## Docker Sandbox boundary

An sbx turn uses the same Osmia grants as other modes. Core's public boundary
verifies the request, and its runner controls `sbx create`, backend configuration,
execution and removal. Its enforcer freezes a validated policy and rechecks
mount identities and grants before running. A released session cannot run again.
Root mounts and paths containing colons are refused. Only scoped
MCP tools are granted: no native shell, VCS, delegated host MCP process or host
Dagger engine. Backend inspection refuses effective tool configurations that
would widen those grants.

The VM holds filesystem isolation; read-only roles get a read-only view and a
separate writable scratch directory. Sandbox templates are trusted operator
inputs. Core's sbx VCS stand-ins deny command names on `PATH`, not executables
addressed by absolute path inside an image. Osmia therefore also withholds all
native execution tools, VCS metadata and delivery credentials; it does not rely
on those stand-ins alone. Do not treat this as executable masking throughout an
arbitrary template.

MCP uses an authenticated loopback listener through the sandbox proxy on every
host platform, with no Docker bridge discovery. The provider proxy owns model
credentials. Core allows the granted MCP ports for the sandbox alone, then
removes those rules during cleanup; no global localhost allowance is needed.
Other network policy is configured by the operator. See
[sbx configuration](configuration.md#docker-sandboxes-sbx) for setup. Tests use fake CLIs and backends in Dagger;
they verify grants, protocol construction, refusals and cleanup, not live VM
confinement or provider authentication.
