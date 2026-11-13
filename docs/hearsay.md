# Hearsay integration

Osmia supports the complete local workflow without Hearsay. With Hearsay, it
does two independent things:

- **It pushes decisions.** Hearsay becomes the record of how decisions are made
  about each project Osmia runs: changes to a project's shape, feature
  decisions, the charter and the chief of staff's judgment calls. The service
  posts them to Hearsay's `work` push source. Factory internals stay in Osmia.
- **It reads.** Role turns and the chief's watch read Hearsay's decision log,
  spec documents and the rest of its context through the read API.

No project design, merge dependency, amend or cancel depends on Hearsay being
configured or available. Workgroup conversations, workgroup specs, questions and
owner decisions remain authoritative in Osmia's local trace, and Osmia has no
build dependency on a Hearsay checkout. Hearsay's own documents define the
request shapes and configuration this page relies on:
[the connector contract](https://github.com/kpenfound/hearsay/blob/main/docs/connector-contract.md)
and [configuration](https://github.com/kpenfound/hearsay/blob/main/docs/config.md).

## Push, and who decided

Only deterministic service code writes: lifecycle steps, the librarian's
committed extraction and the owner's own gates. No model turn writes to Hearsay,
and Osmia adds no agent tool for writing. Agents keep `assert` as it is.

Publishing is off unless `[hearsay.publish]` is configured
([configuration](configuration.md#publishing-hearsaypublish)). It names two
Hearsay `work` sources and the publisher agent that posts to them:

| Source | Holds | Authority |
|---|---|---|
| **work source** | `project`, `workstream` and `update` records, plus what the chief of staff, a committee or the librarian decided | Documents and decisions are **inferred** |
| **owner source** | `document` and `decision` records the owner stands behind, for the projects and workstreams the work source holds | Documents and decisions are **ratified** |

**Authority comes from the source, never from the author.** Everything the owner
decides goes to the owner source, with the owner named as `author` through the
publisher agent, which Hearsay lists as an `attributor`. Everything the chief,
a committee or the librarian decides goes to the work source and stays
inferred, even when it names the owner or says `decider: owner`. A chief's relay
of a ruling is never posted as a ruling: the ruling is posted once, from the
owner's own gate, to the owner source. Hearsay never reads the author, the
attributing poster or the decider to decide a tier.

## What is posted

**Shape of the project**

| Record | Kind | Source |
|---|---|---|
| A registered project, with the owner as `author`; its removal | `project` | work |
| A workgroup's project spec (its cross-repo contract), on each member project | `document` | owner |
| A project amendment to it, with the owner's ruling; posted once on each member project | `decision` | owner |
| A subsystem added, removed, renamed or moved under a different `part_of` parent in `kb/entities.json`, naming the workstream that caused it | `update` | work |

Nothing else from the knowledge base is posted: no subsystem notes, no
`kb/sources.json` and no KB entity as a live Hearsay code entity.

**Charter**

| Record | Kind | Source |
|---|---|---|
| Every charter revision, as a revision of one project document | `document` | owner |
| A charter proposal: the rule the chief drew from a ruling, and whether the owner ratified or declined it, with the owner's note | `decision` | owner |

**Feature decisions, per workstream**

| Record | Kind | Source |
|---|---|---|
| The workstream: what the owner asked for, its core state (`proposed`, `started`, `blocked`, `completed` or `abandoned`), Osmia's own state as `status`, the pull request URL and whether it merged, and the reason for a cancellation or abandonment. A workgroup member's description names the workgroup and the other members. The pull request body is not sent | `workstream` | work |
| Each sealed spec, as one revision of the workstream's spec document. An amendment produces a new seal and so a new revision | `document` | owner |
| An owner ruling on an escalated question, with the question and its escalation: the passage in tension, the conflict and the options | `decision` | owner |
| An amendment the owner decided, with the change, the reason and who decided | `decision` | owner |
| A final-review finding the owner accepted at delivery instead of having it fixed | `decision` | owner |
| A plan-only amendment, decided by the chief of staff | `decision` | work |

**Chief of staff decisions** (all `decision`, work source, inferred)

- Logged assumptions: the judgment calls the chief made without asking.
- The chief's answers to agents' questions about intent or scope. An answer that
  cites code counts as engineering and is not sent.
- Scope calls: a discovery declined or deferred, and a final-review follow-up
  pushed to later, each with its reason.

Every `decision` carries its context, outcome, rationale, rejected options,
decider (`owner`, `chief_of_staff` or `committee`), links where they exist (the
spec document, the pull request URL, the question id), and the project or
workstream it belongs to. When it revisits an earlier posted decision, such as a
later ruling on the same question or a new amendment decision on the same
subject, it names that decision in `supersedes`, and the earlier one is posted
first.

### Never sent

- Turn requests and responses, tool calls and conversations.
- Costs, check and test output, and images.
- Spec drafts, committee debate rounds and amendment requests before a decision.
- Plans and footprints, unit landings, mason reports and learnings, and
  per-unit review findings.
- Subsystem notes and `kb/sources.json`.
- Engineering answers that came from reading code.
- The chief's fixes to a stuck workstream (`move_workstream` and similar).
- Private role notes (`notes/`), CODEOWNERS handles, entity owners, runtime
  settings and cursors, including the publisher's digests.
- Anything that came out of a Hearsay bundle. Nothing flows from Hearsay back
  into what Osmia posts, and Hearsay posts nothing to Osmia.

The librarian's own workstream and exploratory sessions produce no posts.

## Delivery

**Reconcile from the trace.** There is no event queue. Each pass reads the
committed trace, computes every record, compares it with the digest of what was
last posted successfully and posts only what changed to
`<hooks url>/hooks/<source id>`, as the publisher agent. An unchanged artifact
sends nothing; a changed one is posted once. A parent (project, then workstream)
is posted before its children, and the owner source only accepts records for
parents the work source already holds, so a refused owner-source post is retried
on a later pass.

**Backfill.** The first pass after `[hearsay.publish]` is configured posts every
eligible artifact already in the committed trace. The backfill covers what the
trace holds at that moment: a workstream archived and cleaned before publishing
was configured is not reconstructed, so its seals and decisions are not posted
and the pass reports no error for it.

**Outages.** When Hearsay is unreachable or refuses a post, the digest stays
unrecorded, local work carries on unaffected and a later pass retries.
Hearsay's refusal of a duplicated id (a 409 that says where the id is held) is
treated the same way: nothing is marked posted. A Hearsay outage never stops,
delays or changes local work, including archive cleanup and project removal.

**Stable record ids.** Each record's id is derived from Osmia ids (project,
workstream, seal, question, amendment, discovery, follow-up and the like). The
same artifact has the same id after a service restart and on a repost, and a
reposted identical record has no effect in Hearsay. The ids are a contract:
Hearsay keeps a changed repost as a new revision of the same record, so a
document's revisions and a decision's supersession depend on them not changing.

## Privacy and read access

A project is posted to Hearsay **without `repos`**, with the owner named as the
project's author. Hearsay serves such a project's records only to the poster and
the named author, so they are readable by the owner and nobody else. Naming the
owner as `author` grants access only; it ratifies nothing, because authority
follows the source. Documents and decisions on both sources follow their
project's current access.

A project can opt in to declaring its GitHub repo on its Hearsay project
(`hearsay_declare_repo` in its `config.toml`, off by default). Its records then
become visible to everyone who can read that repository in Hearsay, which for a
public repository is everyone Hearsay knows. The opt-in is never needed to read
a private project, and Osmia does not use it, or the publisher's token, to work
around access.

Roles read through their own class credentials. Each of the worker,
orchestrator and observer class agents acts for the owner principal, sending the
owner's token with its own agent id and token, so its reach is the intersection
of the agent's grants and the owner's. The publisher agent is a dedicated
principal that only posts, and its token is never used for a read.

## Reading

A project with a configured work source reads `work:<work source>:<project-id>`
with no further setting, and a turn tied to a workstream also reads
`work:<work source>:<project-id>#<workstream-id>`. Owner-source records are served
on the work source's entity, so one read serves both sources and no
owner-source entity is read. `hearsay_scope` and `hearsay_entities` hold entity
ids; an old scope name fails at load
([configuration](configuration.md#entity-ids)). Without a work source and
without an override, Hearsay reads stay off.

On these entities `get_bundle` and `traverse` serve documents as `spec`
sections at their latest revision and the entity's decision log with each
decision's outcome and decider. A decision is served ratified when it arrived on
the owner source and inferred when it arrived on the work source. One that a
later decision names in `supersedes` is marked superseded by its successor. A
workstream's read also serves its project's decisions; a project's read does not
serve its workstreams'. A reader without access gets the answer an unknown scope
gets.

Roles receive the read tools `get_bundle`, `search`, `traverse`, `resolve`,
`stance_history`, `get_l1` and `get_l0`. `traverse` follows Hearsay's typed
graph from an entity, so a role can reach the decision log and spec documents
around a project or workstream. Exploratory sessions receive the read tools at
the observer class and no write tool, and the classifier receives no tools.
External context is labelled separately from the local knowledge base, which
every turn still receives word for word.

The chief of staff's watch reads the project's default entity with the
orchestrator identity. Each watch commits its cursor together with deduplicated
event handles and chief notices. A watch cannot change feature state or restart
delivered work. An outage retains the cursor and uses local context, and a
changed endpoint, principal, agent configuration or entity starts a separate
cursor.

## Prepare Hearsay's configuration

`osmia project memory <project-id>` prints reviewable Hearsay configuration for
the project as JSON. It is read-only: it uses the local trace and the Osmia
configuration, makes no Hearsay requests and changes no configuration. Its
fields are:

| Field | Meaning |
|---|---|
| `files` | Four fragments, `sources/osmia.yaml`, `principals/osmia.yaml`, `scopes/osmia.yaml` and `authority/osmia.yaml`. Their JSON is valid YAML |
| `work_source`, `owner_source` | The paired `work` sources. They are the `[hearsay.publish]` values when set, and otherwise `osmia-work` and `osmia-work-owner` |
| `publisher` | The agent that posts to both sources. It is `hearsay.publish.agent` when set, and otherwise `osmia-publisher` |
| `principal` | The owner principal the publisher names as `author`: `hearsay.principal` |
| `hearsay_scope` | The entity id the project reads: its `hearsay_scope` override, or `work:<work source>:<project-id>` |

The fragments define:

- **Sources.** The work source and the owner source, both of type `work`, with
  `containers: ["*"]`. The owner source's `owner_of` setting names the work
  source, which pairs them. On both, `posters` and `attributors` list only the
  publisher agent. Hearsay's default would let every agent with a token post, so
  the class agents are excluded explicitly.
- **Principals.** The owner as a `human` with the token variable of
  `hearsay.token_env`; the publisher as an `observer` agent with its
  `hearsay.publish.token_env`; and the `worker`, `orchestrator` and `observer`
  class agents with their configured ids and token variables. Each is granted
  the project's `work:` entity, and the class agents are readers only. The work
  source supplies every principal's identity by its id, so no identities are
  listed.
- **Scope.** One scope covering both sources, which is the scope Hearsay places
  the pair's documents and decisions under.
- **Authority.** One policy on that scope: `ratified_by.artifacts` is `spec` and
  `decision`, restricted by `ratified_by.sources` to the owner source, and hand
  ratification stays with the owner. Nothing on the work source ratifies.

Review the fragments and merge them into a Hearsay configuration directory. The
sources, scope and authority are the same for every project that posts through
the same pair: keep one copy. The principals are shared too: when combining
projects, keep one entry per principal and add each project's entity to its
`scopes`. Then validate and start Hearsay:

```sh
hearsay config validate <directory>
```

Set the environment variables the principals name (the owner's, the publisher's
and each class agent's token), and run the connectors listener with both
sources in one process: Hearsay's id checks across the pair rely on a lock
shared in that process. Point `[hearsay]` at the API and `[hearsay.publish]` at
the connectors listener in Osmia, and reload. See
[configuration](configuration.md#optional-hearsay-context).
