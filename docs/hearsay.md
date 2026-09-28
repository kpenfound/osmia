# Hearsay integration

Osmia supports the complete local workflow without Hearsay. The optional API
adds scoped context, role tools and durable watches. The companion `osmia`
connector runs in Hearsay, reads committed trace snapshots, and writes L0 only.
Osmia has no build dependency on a Hearsay checkout.

## Prepare configuration

`osmia project memory <project-id>` returns JSON containing five Hearsay
configuration fragments, the project's `hearsay_scope` and `hearsay_entities`
settings, and anchor artifact handles. The fragments' JSON is valid YAML. This
read-only command uses the current local entity map and document revisions; it
makes no Hearsay requests and changes no configuration.

Review and merge the fragments into a Hearsay configuration directory. Preserve
existing principal grants when combining projects; merge their scopes and source
identities under the same principal rather than defining duplicate principals.
Map local CODEOWNERS handles to Hearsay principals before adding entity owners.
The generated code namespace is `code:<upstream>:<entity>`. Hearsay scope names are separate, bounded identifiers: those
names, not code entity IDs, belong in Osmia's `hearsay_entities` mapping.

The connector requires a read-only mount of the Osmia root at `/mnt/osmia`, or
another absolute path configured in each source's `settings.root`. Its source
containers explicitly list project IDs. Wildcards, symlink directories, missing
repositories and unsupported trace versions are refused. No source credentials
are needed. Do not mount the target clones or service credentials.

Each project has two private sources, `osmia-owner-<project-id-suffix>` and
`osmia-work-<project-id-suffix>`. Both ACLs name the owner's source-native `local`
identity. The owner source contains charter revisions and the original human
ruling. The work source contains drafts, agent contributions, source commits and
execution provenance. The generated authority policy allows the owner source's
`spec` artifacts to ratify stances and leaves agent contributions inferred. A
chief's relay never becomes a second owner ruling. Keep these sources separate
when editing authority policy.

Run `hearsay config validate --config <directory>`, configure its environment
secrets and start its connectors, distiller and assertion worker. Review the
produced L1 documents, then apply each desired anchor with:

```sh
hearsay gestures pin --config <directory> --principal <owner> \
  --artifact <source> <artifact>
```

The export lists charter, subsystem prose and workstream specifications for
anchoring. Hearsay authorizes and limits pins; export does not bypass these gates.
Rerun the export after extraction or new workstreams to discover additional
entities and anchors. Configure the API URL and token environment references in
Osmia, copy the reviewed scope mapping into the project configuration, and reload.
See [configuration](configuration.md#optional-hearsay-context).

## Replay contract

Artifacts are `<project>/<workstream>/<trace-kind>/<record-id>`; project records
have an empty workstream segment. An event native ID adds
`@<record-revision>-<permission-fingerprint>`. Artifact creation time comes from
the first recorded revision, and edit time from the revision being emitted.
The original owner ruling is exported once; its agent relay is not another
observation of the owner's decision. No immediate owner assertion is sent: the
committed connector path owns authoritative ruling ingestion and avoids a second,
unrelated agent-proposal identity.

Backfill and polling use the same conversion and bounded pages. A cursor pins a
project's Git commit, log file and record position. A lost sink response replays
the same event IDs. New trace commits are read in the next polling cycle, and
inactive and delivered workstreams remain ingestible. Dirty working files and
uncommitted transactions are invisible. Empty document revisions emit tombstones;
removing a configured project retracts its current documents. Bump
`permission_version` when restoring a prior ACL or re-adding a removed project,
so its returning records receive fresh observation IDs.

Session starts and ends, turns and tool calls retain execution provenance. They
are L0 audit material, not automatically distilled team knowledge. Private role
notes, runtime settings and watch-cursor documents are excluded. Hearsay bundle
content in turn/tool provenance cannot re-enter the knowledge pipeline as a
project ruling. Charter, spec, plan and subsystem prose are documents; question
threads and shed packets group contributions; landings are commits; transitions,
seals and rulings use Osmia extension kinds. GitHub's connector owns the actual
pull-request observations.

Watches commit each cursor together with deduplicated event handles and chief
notices. They cannot change feature state or restart delivered work. An outage
retains the cursor and uses local context. A changed endpoint, principal, agent
configuration or project scope starts a separate cursor. Tests use HTTP fixtures
and local trace repositories; deployment with real sources is an operator step.
