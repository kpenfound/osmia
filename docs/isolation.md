# Turn isolation

`internal/isolation.Turns` is the service-owned preparation path for
`internal/thread.Runner`. The thread claims a durable request first, then the
preparation path selects resources using its immutable scope. Workspace selection,
file copying, MCP hosting, execution verification and cleanup failures are captured
in the owned turn response and survive reopening the trace repository.

The service supplies a workspace provider, a view directory under the Osmia root,
role grants, a scope selector, an MCP host factory and an isolation engine. These
are Go dependencies, not repository or profile configuration. This M1 path is an
in-process integration; the local API does not dispatch agent turns.

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
runner's resume check is forwarded to the enforcing engine through the same
boundary executor; an engine that cannot verify a saved session selects replay.

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
fail closed. Provider transport/authentication is the enforcing engine's concern;
it must not give tools general network access or delivery credentials.

## Host and container verification

`coreadapter.BoundaryExecutor` accepts host (`none`) or container execution and
constructs one mount: the exact canonical view path, read-only for a read-only
grant. Extra mounts and domain overrides are rejected. The view is checked again
for symlinks, special files and VCS metadata before construction. The engine must
also enforce these restrictions during execution, including path-resolution races
and nested/alternate mounts; checking path strings alone cannot establish them.

The engine prepares a stopped boundary and inspects its established policy before
the adapter calls `Run`. The inspected mount set, permissions and environment must
match exactly. Mandatory restrictions cover VCS executables (including shell
indirection), host files, inherited environment, delivery credentials, tool
additions, configuration discovery and privilege escalation. Repository tool/skill
configuration can be present as selected data but must never be loaded as executable
configuration. Backend session/log directories are service-owned and are not
additional writable agent mounts. Model-provider and scoped MCP transport must not
be usable as a general fetch proxy. Missing engines, incomplete inspection or any
policy mismatch fail before the runtime starts.

**No production enforcing engine is supplied at the current core pin.**
`CoreExecutor` continues to reject launch because core inherits host environment
and does not expose the required enforcing backend/process boundary. The local
adapter is ready for an engine with verifiable restrictions; it does not make an
unrestricted core runner safe. The hermetic engine fixture tests construction,
permission checks, cleanup and durable failures, not actual OS sandboxing.
`os.Root`, MCP filtering and profile flags are application restrictions and are
not, by themselves, an OS security sandbox.
