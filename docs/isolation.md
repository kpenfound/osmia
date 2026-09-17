# Turn isolation

`internal/isolation.Turns` is the service-owned preparation path for
`internal/thread.Runner`. The thread claims a durable request first, then the
preparation path selects resources using its immutable scope. Workspace selection,
file copying, MCP hosting, execution verification and cleanup failures are captured
in the owned turn response and survive reopening the trace repository.

The service supplies a workspace provider, a view directory under the Osmia root,
role grants, a scope selector, an MCP host factory and an execution engine. These
are Go dependencies, not repository or profile configuration. This path is an
in-process integration; the local API dispatches no workstream turns. The
librarian's [knowledge-base extraction](knowledge-base.md#extraction) is the
service's own use of it.

## Files and lifetime

The provider lease belongs to the service and must exclude concurrent source
writers while copying. The selector names explicit relative files or directories.
Each turn gets a fresh private copy in service storage disjoint from the provider
workspace. Selecting a directory recursively includes its files, excluding `.git`,
`.jj`, `.hg` and `.svn` at every depth. Explicit metadata selection, traversal,
symlinks and special files fail acquisition. Executable owner bits are preserved;
group/world access is removed. No hard links to source files are created.

The runtime receives only the copied directory, never the provider workspace,
lease or VCS handle. Built-in `file_read` and `file_write` handlers use `os.Root`
and reject absolute paths, traversal, symlinks and metadata components. The
read-only view also rejects writes in the handler. Files added by a writable role
remain inside that turn's view. The optional service `Capture` callback can inspect
or snapshot output before cleanup, including partial output from failed execution.
There is no automatic copy-back, commit, branch landing or delivery.

The service closes the execution boundary, MCP host, view and provider lease, in
that order, using a non-cancelled cleanup context. Cleanup errors accompany the
turn result. Views and MCP credentials are fresh on every turn, including resumed
threads; backend state is not an authority to reuse a prior grant. The thread
runner's resume check is forwarded to the execution engine through the same
executor; an engine that cannot verify a saved session selects replay.

## Capabilities

The service must explicitly grant a role its tool names and write/execute/network
permissions. A missing role grant fails. The M1 workspace-write ceiling permits
`mason` and `librarian`; other known roles are read-only and have no execute or
fetch permission. Some tools belong to one role: `set_status` is removed from
every other role's grant, so only `chief_of_staff` can see it. The selector's
optional `Narrow` request intersects the grant. Neither prompts nor profiles
grant tools. The full workflow role-tool catalogue is outside this M1
implementation.

The service registry classifies each tool as read, write, memory, execute, fetch
or VCS. Memory tools change only service-owned records bound to the turn, such as
private role memory or the [workstream status](trace.md#workstream-status), and
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
The service generates `OSMIA_MCP_TOKEN` for a turn's authenticated HTTP MCP host.
There is no environment inheritance, shell expansion or lookup of host provider,
GitHub, SSH-agent or cloud/delivery credentials. Unresolved credential references
fail closed. Provider transport/authentication is the execution engine's concern;
it must not give tools general network access or delivery credentials.

## Container verification

`coreadapter.CoreExecutor` runs a turn through busybees/core with `agent.Grants`
built from the verified isolation alone:

| Grant | Value |
|---|---|
| `Mounts` | the exact canonical view path with the view's access; the session directory read-only; for a read-only view, a writable `work` directory inside the session directory, where the session starts, because core runs every session in a writable directory |
| `Env` | the names of the service environment, which is the complete environment |
| `Tools` | one `mcp__osmia_<i>` server per scoped endpoint and no built-in tool |
| `VCS` | not granted |

Only `container` execution is accepted. Core's host boundaries cannot keep these
grants: `none` needs the whole filesystem writable and VCS granted, and `claude`
reads the whole filesystem. Extra mounts and domain overrides are rejected. The
view is checked again for symlinks, special files and VCS metadata before
construction.

The executor first asks the engine's `Verify` for the turn core would run and
refuses it unless it matches the grants: VCS not granted and `gh`, `git`, `hg`,
`jj` and `svn` shadowed by stand-ins; no built-in tools; no write directories;
an environment of the service variables plus the container's `HOME`; and binds
of the granted mounts only, including the view. Core then verifies the request
again before it starts anything, and refuses a variable, tool, MCP server or
mount the grants do not name, VCS access, and a writable mount holding VCS
metadata. A request field the grants do not describe (VCS environment, skills,
container-use environment, network domains, a different sandbox or image, a
different allow list, or an MCP entry other than a service-authenticated HTTP
endpoint) is refused before verification. Every refusal is an `UnsupportedError`
returned before the runtime starts.

`*agent.Runner` is the production engine. The hermetic engine fixture verifies
requests with core's own container boundary and records them; it tests
construction, permission checks and durable failures, not actual OS sandboxing.
`os.Root`, MCP filtering and profile flags are application restrictions and are
not, by themselves, an OS security sandbox.
