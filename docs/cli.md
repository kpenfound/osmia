# M1 command line

Build with `go build -o osmia ./cmd/osmia`, or install with
`go install ./cmd/osmia`. Prepare the [M1 configuration](configuration.md) first.

```sh
osmia serve --root ~/.osmia
# In another terminal:
osmia status --root ~/.osmia
osmia pause all --reason "Away for the weekend"
osmia resume all
osmia profiles
osmia profiles set mason default
osmia profiles clear mason
osmia status --json
```

## Commands and flags

- `serve [--root PATH]` validates and starts the foreground service. SIGINT and
  SIGTERM drain requests and release ownership and the socket. A live owner is
  refused; a provably stale socket is recovered automatically.
- `status` shows health, loaded configuration digest/root, diagnostics and
  effective runtime controls.
- `pause <all|project-id|workstream-id> [--hard] [--reason TEXT]` stores an
  operator pause; the default mode is soft.
- `resume <all|project-id|workstream-id>` clears that scope's pause. Parent pauses
  still apply; all effective pauses are displayed.
- `priority set <workstream-id>...` stores the active project's ordered list.
  `priority clear` removes that preference.
- `profiles` displays effective bindings and runtime diagnostics.
  `profiles set <role> <profile>` overrides a binding.
  `profiles clear <role>` restores its configured default.

Every command accepts `--root PATH`, defaulting to `~/.osmia`. Flags may occur
before or after positional arguments; value flags also accept `--flag=value`.
Duplicate and unknown flags are errors. `--help` prints the supported syntax.

Clients connect to `osmia.sock` under the root. If configuration specifies another
socket name, pass `--socket PATH` to each client command; relative paths resolve
against the root. For example, with `listen.socket = "local.sock"`:

```sh
osmia status --root ~/.osmia --socket local.sock --json
```

Clients never load configuration, runtime, trace or project files. They obtain
the active project and effective values over HTTP using the service client.
Changing socket configuration requires restarting the service and updating the
client's `--socket` argument.

Project and workstream arguments are persistent IDs (`p_` or `w_` followed by
32 lowercase hexadecimal digits), not display names. Workstream IDs must be known
to the service's record repository. The standalone M1 entry point does not
discover workstreams or provide onboarding. These commands store controls;
scheduler effects are unavailable in M1.

## Output and exit codes

All client commands accept `--json`. Status returns an object with `health`,
`configuration` and `runtime` API responses; profiles returns the runtime
response. Mutations return `mutation` (the API acknowledgement) and `runtime`
(the subsequent effective-state response). Output is one JSON value plus newline,
without progress text. Profile map keys are sorted in human output and JSON.
The responses are separate API requests, not an atomic snapshot.

Human mutation output identifies the affected scope and prints effective controls,
including defaults after clearing overrides. If the mutation succeeds but reading
the effective state fails, stderr says it was acknowledged; inspect status before
retrying. Failures leave stdout empty and write actionable diagnostics to stderr.
Raw configuration/parser and server error text is omitted from failure messages.

| Exit | Meaning |
| --- | --- |
| 0 | Success, help, or clean foreground cancellation |
| 1 | Invalid API response, local output or unexpected client failure |
| 2 | Invalid command, flags, arguments or root |
| 3 | Missing socket, connection failure or unavailable service |
| 4 | API malformed-input or validation rejection |
| 5 | API conflict, unsupported operation, restart required or internal failure |
| 6 | Foreground startup/service failure, including ownership conflict |

For exit 3, start the service and verify matching root/socket and permissions.
For exit 6, check configuration and runtime validity and permissions, stop any
existing owner, and ensure the socket path is unused or stale. Do not delete a
live-owned socket. Unsupported responses identify the M1 limit; restart-required
responses instruct the operator to stop and start the service.

Detached management, install/upgrade commands, completion, web/tailnet,
onboarding, hand-in, inbox, conversation, ratification, answer, reload and trace
navigation are unavailable. The command examples in the design describe the
eventual product; this reference lists the implemented surface.
