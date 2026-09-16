# M1 runtime overrides

`internal/runtime.Open` takes a configuration from `config.Load` and the active
project's known persisted workstream IDs. Without an active project the list is
empty, every project reference is stale, and project-scoped overrides are
rejected until `Resolve` supplies the registered project. It opens `runtime.json` under that
configuration's resolved root. The record repository supplies workstream IDs;
names and directory discovery are not identity authorities. An absent file means
empty overrides and is not created until the first successful mutation. The root
must already exist. The service owns one store and closes it on shutdown.

The version 1 JSON representation is:

```json
{
  "version": 1,
  "pauses": [
    {
      "target": {"scope": "factory"},
      "mode": "soft",
      "reason": "Travelling",
      "source": "operator"
    },
    {
      "target": {
        "scope": "workstream",
        "project": "p_0123456789abcdef0123456789abcdef",
        "workstream": "w_0123456789abcdef0123456789abcdef"
      },
      "mode": "hard",
      "source": "operator"
    }
  ],
  "priorities": [
    {
      "project": "p_0123456789abcdef0123456789abcdef",
      "workstreams": ["w_0123456789abcdef0123456789abcdef"]
    }
  ],
  "profiles": {"mason": "default"}
}
```

A pause target is `factory` (no IDs), `project` (project ID only), or `workstream`
(both IDs). One record per target is allowed. Modes are `soft` and `hard`; M1
records only the `operator` source and an optional free-form reason. These records
have no dispatch or cancellation effects in M1.

Priority contains a unique ordered subset of workstream IDs, scoped to a project.
An empty array is allowed; null is not. Unlisted workstreams have no explicit
preference. There is one priority record per project. Profiles map global role
names to named profiles; bindings must satisfy the role's sandbox requirements,
including the selected profile's fallback chain.

`SetPause`, `SetPriority` and `SetProfile` validate new overrides against the current
resolver input before writing. The corresponding `Clear` operations remove the
exact key, including a stale key. They never rewrite `config.toml`. `Resolve`
copies a newly validated configuration and identity list into the resolver without
changing runtime state or files; changing the root requires reopening. This is a
repository operation, not live reload orchestration.

`Snapshot` returns the entire stored state plus reference diagnostics. `Effective`
returns valid pauses and priorities, and every configured role binding with valid
runtime overrides applied. Both return independent copies. Clearing a profile
reveals the current configured binding. M1 configuration defines no pause or
priority settings, so their defaults are unpaused and no explicit ordering.
The effective pause list preserves all scopes; consumers can see every applicable
pause rather than losing a factory pause behind a workstream record.

## Invalid and stale state

Malformed JSON, duplicate object keys, unknown fields or schema versions, invalid target shapes, modes,
sources, empty bindings and duplicate pause/priority identities reject the whole
file: `Open` returns no store and leaves the file intact. Failed mutations leave
both the previous snapshot and file authoritative.

Well-formed references unavailable in the current configuration or identity list
are retained as stale data. `Open` returns reference diagnostics alongside the store; every `Snapshot` and
`Effective` call also reports a field
path and reason for each stale reference. Consumers must surface these diagnostics.
A load cannot distinguish a removed identity from an unknown manually entered one;
both follow this same explicit policy. New mutations cannot introduce unknown
references. Stale pause/profile entries do not affect effective values; stale
priority members are excluded while valid members retain their relative order.
Other valid overrides remain effective and survive subsequent mutations. Restoring
the exact referenced key makes it effective again; no name-based retargeting occurs.
Clearing stale keys is the explicit way to discard them.

## Persistence boundary

Mutations hold one store lock through validation, encoding and persistence; readers
see the prior or new complete snapshot. JSON uses indentation, sorted records and
sorted profile keys for deterministic inspection. Writes use exclusive, mode-0600
temporary files in the same directory, file sync, atomic rename and directory sync
(where supported). A flushed copy of the previous file allows rollback if directory
sync fails after rename. Encoding, write, file-sync, rename and directory-sync
errors are returned without acknowledging the mutation. Failure of rollback itself
is reported as requiring disk reconciliation; filesystem failure cannot guarantee
successful restoration. Abrupt process or machine failure during an unacknowledged
rename may leave either complete version. Successful commits have passed the
supported durability operations.

The store pins a root directory handle and opens runtime files without following
symlinks. Replacing a parent path cannot redirect writes outside that root. External
file changes cause subsequent mutations to fail until the store is reopened.
Cross-process mutation and multiple writable stores are unsupported by the store:
the [local service](service.md) enforces sole ownership with a lifetime root lock. Temporary rollback files are not an event log or a
record/outbox transaction. M4 scheduling, provider/budget pauses and lifecycle
controls are separate work.
