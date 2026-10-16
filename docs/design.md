# Osmia design doc

A personal software factory for long-running feature work on repositories you own or contribute to. It is configured at the user level, keeps its factory state outside the repository it works on, is driven by a person through one aide, and shares one pool of agent capacity across several workstreams on several projects.

Status: design v0.3, 2026-10-15. This document is the source of truth for feature proposals and implementation; an issue does not override the design. Osmia uses `github.com/kpenfound/busybees/core` as a Go dependency and defines an optional Hearsay memory integration. Section 17 defines those boundaries and section 18 summarizes the feature guarantees.

---

## 1. Purpose

You want to land a large feature in a project like dagger. You are a contributor, so you cannot install a factory in the repository, label its issues, or push to its main branch. You have a design in your head or in a document, you want agents to plan it, argue about the plan, build it in pieces, review each piece, and hand you one feature branch on your repository or fork with a pull request you are happy to put your name on. You want to be asked when something needs your judgement, and otherwise left alone. You want the same machine working on three dagger features and a feature for another project at the same time, from one process, from your phone.

Osmia is that machine. It does not decide what to build. You hand it a feature. It does not run on the repository's issue tracker. It runs on a directory under your home. It does not push to anyone's main. It delivers a branch and a pull request, and stops.

### 1.1 Why a mason bee

Mason bees, the genus *Osmia*, have no hive. Each one moves into whatever cavity already exists, a reed, a hole in a wall, a tube someone left out, and gets to work. Orchards buy them by the tube and set them out where the pollination is needed, and a handful of them do the work of a hive. They are gentle and rarely sting.

That is the tool. It moves into an existing repository without changing it, is deployed where the work is, and is meant to be a good guest.

### 1.2 The shape of the design

Four choices give the design its shape.

1. The owner ratifies a feature's intent: required outcomes, acceptance criteria, exclusions, constraints and reserved tradeoffs. The factory proposes an execution plan alongside it; engineering choices in that plan remain revisable.
2. Units own coherent outcomes, with explicit boundaries and dependencies. Masons adapt implementation within their assignments. Reviewers judge outcomes using applicable evidence. The factory revises assignments and incorporates necessary discoveries without changing approved intent.
3. Every agent talks upward to the chief of staff, which investigates and resolves engineering blockers, coordinates revisions, and raises intent decisions or concrete operational limitations it cannot resolve. The owner approves publication.
4. The transitions are Go. Models investigate, propose and judge inside turns. The service owns scheduling, capacity, credentials, version control, durable revisions and recovery.

---

## 2. Vocabulary

### 2.1 Concepts

| Term | Meaning |
|---|---|
| project | One charter, one or more repos and their active workgroups. The main entry point for requesting work. |
| repo | One target repository in a project, with its own upstream, optional fork, clone, landing settings, caps, knowledge base and entity map. |
| workgroup | One feature request in a project: one or more workstreams and exactly one shared chief of staff. |
| brief | The distilled handoff from a design conversation, or the handed document on the fast path. Decisions, intent, tradeoffs and rejected options; it takes precedence over the attached transcript. |
| project spec | The committee-debated, owner-ratified workgroup design: behaviour, cross-repo contracts and numbered criteria. |
| workstream slice | The part of the project spec assigned to one repo's workstream. Contract sections are read-only to its roles. |
| edge | A typed dependency between workstreams in the workgroup's acyclic graph. |
| workstream | One repo, units, one feature branch and one pull request within a workgroup. Its own worker threads and trace. |
| charter | Your rules as a contributor to a project. One per project, human-owned, grows over time. |
| knowledge base | What the factory knows about a repo's architecture that the repository's own documents do not say. Prose by subsystem, a local entity map and trace decisions, enriched by Hearsay when enabled. |
| feature spec (spec) | What must be true when the feature is done: intended behaviour, what it must not do, and acceptance criteria as a numbered list so they can be cited. Drafted by the architect, ratified by you. Not a description of the codebase, and never enforced mechanically. |
| criterion | One numbered item in a spec's acceptance criteria. Cited in debate and review; a handle, not a key. |
| acceptance | What a unit's reviewer checks against its work to accept it: behaviour that must hold, tests added in the project's own conventions or kept passing, checks to run. Written as observable conditions in the current assignment; evidence demonstrates them. |
| plan | A versioned working graph of assignments that realizes approved intent. The factory may revise it as it learns. |
| unit | One piece of the plan: a task, its acceptance, the criteria it serves, the units it depends on, the code it will touch. Built by one mason, verified against its acceptance by one reviewer, landed as one commit on the feature branch. |
| footprint | The code entities a unit expects to touch, declared in the plan. Overlap between footprints is entanglement, so footprints decide which units may be built at once. A footprint never limits what the unit's mason may change. |
| seal | The pair of the upstream main commit and the spec's hash recorded at ratification. Moves when the feature branch rebases. |
| bundle | The context a session gets at the start of a turn: its task, applicable constraints and decisions, and references for retrieving project knowledge and evidence. |
| the shed | Bounded engineering debate for initial planning and focused revisions. It challenges material risks and assumptions; it does not require engineering unanimity. |
| amendment | A durable revision proposal. The chief of staff decides plan-only engineering revisions; spec changes and charter exceptions require the owner. |
| question | Anything a role cannot answer from its bundle. Goes to the chief of staff, which answers or asks you. |
| ruling | Your answer to a question. Recorded locally, rephrased for the asker, and also asserted to Hearsay when enabled. |
| trace | The complete record of a workstream: documents and their revisions, debate, questions, every agent turn, every transition, every cost. Kept in that workstream's own git repository under the Osmia root, outside every target repository. Repo traces hold KB, role notes and lasting decisions; the default repo holds the project charter. A cross-repo workgroup trace owns its shared chief thread, project spec and group decisions. |
| profile | A named agent configuration: which agent binary, which model, effort, fallback. Roles bind to profiles, and the binding can change while the factory runs. |
| capacity | The number of sessions of each role kind that may run at once, across every workstream and project. |
| the service | The one long-running Osmia process. Everything else, the web interface, the command line, the agents' tools, is a client of it. |

### 2.2 Roles

Roles take names from the trade. All but the owner are agents.

| Role | Job |
|---|---|
| owner | You. Hands in features, sits in the shed, ratifies specs, answers questions, edits the charter, takes delivery. Talks only to the chief of staff. |
| chief of staff | The one role facing the owner. Your assistant for the workgroup. Answers every member role's questions from the brief, project spec, slices, KB and repo state, with optional Hearsay context; rules on contested units on your behalf and escalates design changes or concrete operational limitations it cannot resolve, rephrases both ways, presents ratification, keeps the status that names the pull request's title. Never implements, reviews or dispatches. |
| architect | Selects affected repos from the brief and project context; drafts the project spec, graph, slices and plans, answers the committee in the shed and drafts amendment deltas. |
| committee | Adversarial reviewers. In the shed they argue against the spec and plan, citing the spec, the charter and the knowledge base. On a unit one of them is the reviewer, verifying the unit's work against its task and acceptance until the unit lands. At the end one reads the whole branch, at a high level, against the spec's intent and the charter. One role, three prompts. |
| mason | Does one unit's task until its acceptance holds. May ask questions. |
| foreman | Runs the landing. Merges approved units onto the feature branch, rebases units still in flight, keeps the branch current with upstream, resolves conflicts against the sealed spec, flags entanglement, pushes the branch and opens the pull request. Mostly code, with a session only for conflict resolution. |
| librarian | Maintains the knowledge base. Extracts it when a repo is added, folds in what masons learned when units land. |

---

## 3. Principles

1. **No factory state is committed to the target repository.** No configuration, no specification, no workflow labels, no comments by bots. The repository sees the feature's code and tests on a feature branch in your repository or fork and a pull request under your name. This rule concerns repositories Osmia works on; Osmia's own source repository carries its design and development configuration.
2. **Work is handed, not mined.** The factory builds what you give it. It has no backlog of its own and idles when you give it nothing.
3. **The owner talks to one role.** Every question, status, ratification and ruling passes through the chief of staff. No other agent addresses you and you address no other agent.
4. **The outer machine is Go.** States, transitions, readiness, entanglement checks, landing order, capacity and pause are code. What happens inside a turn is a model. Nothing on the scheduling path waits on a model.
5. **Agents never touch version control.** Sessions get a directory of files. The service creates workspaces, snapshots, rebases, merges and pushes.
6. **Agents are durable threads.** A role on a workstream is one persistent conversation that receives turns and keeps its context, not a fresh session each time.
7. **The trace is the record.** Files under the Osmia root hold everything that happened and supply context on their own. When enabled, Hearsay distils them into decisions and serves them back as additional memory. It is never required to make progress.
8. **One session, shared capacity.** One process runs every workstream on every project against one pool of slots. Focus is an ordering, pause is a switch, and neither restarts anything.
9. **Gates are transitions, not messages.** Agents message each other freely through fixed routes. The owner gates intent ratification, intent amendments and delivery. The factory owns engineering revisions. Contested units are gated too: the chief of staff rules on them for the owner and raises the ones it cannot resolve.

---

## 4. Documents

The project owns its charter and repo list. Each repo owns its knowledge base, entity map and repository settings. A workgroup holds its brief, project spec and graph; each member has its slice, spec and plan. All factory documents live under the Osmia root, outside target repositories. Codebase context comes from reading the repos and their KB, not from keeping delivered specs as standing project knowledge.

Each repo's dedicated trace holds its knowledge and role notes; the default repo's trace holds the project's authoritative charter. Each workstream has an independent trace. A cross-repo workgroup has a dedicated trace outside every member trace for its membership, project spec, shared conversation and group decisions. Direct one-repo workgroups keep their chief thread in the workstream's trace. When selection targets a different repo, the workgroup trace adopts the original conversation even if only one member is allocated. Nothing is authoritative in two places.

### 4.1 Charter

Your rules as a contributor to this project, in a page of numbered rules, so the committee can cite one: the scope you will and will not touch, dependency policy, how a change must be tested, what a pull request to this project must look like. It cites the repository's own contributor documents as binding, so the committee can veto against the project's contributing guide without you restating it.

You edit it directly. The chief of staff proposes an amendment when a ruling you gave is really a standing rule, and you ratify or decline. Each entry records the ruling that caused it. A charter change becomes a notice in every in-flight bundle on the project.

Keep factory settings out of it. Budgets, capacity and models are configuration. If they leak into the charter, agents start debating whether "two masons" is a principle.

### 4.2 Knowledge base

What a new contributor learns that the repository's documents do not say. It has three parts.

- **Prose by subsystem.** `kb/<subsystem>.md`: how the subsystem is built, how to run the tests that matter, what breaks when you touch what, and the decisions behind it. Turns receive references to these files and retrieve relevant sections through bounded project-context tools; they become pinned anchors when Hearsay is enabled. Hearsay caps anchors at four per scope, which is a useful pressure to keep them short.
- **The entity map.** The map from how people talk about the code to where it is: entities with stable local identifiers, aliases, path patterns, owners and part-of edges, seeded from CODEOWNERS and repository structure. Its local record is `kb/entities.json`; when Hearsay is enabled, these seeds are mapped into its code entity namespace. Footprints name these entities and remain resolvable without Hearsay.
- **Decisions.** Constraints and choices about each subsystem, recorded as rulings and decisions in the trace with provenance. When Hearsay is enabled, it also represents them as stances. Local bundles include the relevant decisions without requiring an external service.

The repository's `CLAUDE.md`, `AGENTS.md` and `CONTRIBUTING.md` are inputs the knowledge base must not repeat. Every role can retrieve relevant KB sections: the architect plans against the real architecture, the committee cites it, and the mason uses its footprint to select knowledge.

The librarian reconciles every pass into concise current explanations, removing superseded claims, duplicate material and historical refresh narratives. Detailed evidence and history stay in the trace and source ledger. Each prose file is limited to 8 KiB and total prose to 64 KiB; these are ceilings, not targets. The librarian can validate its staged output with `kb_check`, and the service independently validates it before publication. Invalid output leaves the recorded KB untouched.

It is written by the librarian: an extraction pass over the clone when a repo is added, and a pass each time a workstream goes quiet, with none of its units started and unmerged, as at assembly, or once it is delivered or abandoned. That pass folds in what every mason whose unit landed since the last one reported it learned. Refreshes are serial per repo, so its workstreams never race on them. A question the chief of staff had to answer from the code rather than the knowledge base is a gap, and it files the entry.

### 4.3 Project spec and workstream slices

What must be true when the feature is done: the intended behaviour, what it must not do, and acceptance criteria as a numbered list. The architect drafts the project spec from the brief, using the repos and their knowledge bases. It includes intended behaviour, cross-repo contracts and numbered acceptance criteria, alongside an acyclic workstream graph and a slice and plan for each affected repo. The committee debates these together; one owner ratification covers the project spec, graph and every slice. A one-workstream packet retains the same ratification steps. You edit it freely before ratification. After ratification it changes only by amendment.

This spec describes the request's intended outcome. The repository does not carry it and nobody upstream maintains it as ongoing codebase documentation. It is the statement of intent that everything downstream is argued against: the plan turns it into units with acceptance a reviewer can check, the mason builds its unit against it, and the final read judges whether the assembled feature delivers it. The flow is spec, then plan, then implementation, and the owner approves intent and an initial working plan before a mason starts. What is not enforced is coverage as arithmetic. Each unit's reviewer verifies the criteria it serves in detail; the final read judges the feature as a whole, and you decide at delivery.

A slice's contract sections are read-only to its workstream roles. A contract change from a question, revision or drift amendment becomes a project-level amendment; it is never applied locally to one slice. Units cite the slice criteria they implement.

Retention has two levels. When a workstream resolves (merged or cancelled), its own slice, spec, plan and workspaces leave active state and new bundles. The project spec remains active for the shared chief and every unresolved member until every member resolves, including dependents waiting on or repairing after an upstream merge. Then it leaves active state. At both levels traces, project-spec history, rulings and owner decisions are retained until owner archive; delivery or resolution never deletes them. Active worker bundles are refused for resolved members, and the chief's staged view omits their own spec and plan. The historical documents remain available through trace context readers.

A member resolves only through recorded cancellation/abandonment, or delivery with merge evidence. Merge evidence is an owner's outside-factory merge record (`osmia merged`) or a recorded merged outcome whose PR number matches the current publication. Ready PRs, drafts, closed but unmerged PRs, outcomes for earlier PRs and delivery without evidence remain unresolved and keep the project spec active. Publishing, marking ready, archiving and cleanup do not establish a merge. This rule also applies to one-member workgroups; their existing delivery cleanup is separate from workgroup resolution.

### 4.4 Plan

The plan is the factory's working approach, not the definition of workstream scope. Each unit owns a coherent outcome described by its task, observable acceptance criteria, spec criteria served, dependencies and scheduling footprint. Optional constraints record the owner's hard limits, boundaries describe neighboring responsibilities and interfaces, and guidance records adaptable implementation suggestions, assumptions and file-level pointers. An explicit method in the owner's spec remains binding; a suggested method in guidance does not. A constraint never names files or line-level changes: a mason that needs a different file than the plan expected simply changes it.

Start with one unit when the profile can complete and review the outcome. Split for useful parallelism, reviewability or practical capacity, not file count or subsystem count alone, and size each unit so one mason turn can complete it. A unit can span implementation layers. When a feature has several units, one of them owns connecting their work in the running product, with acceptance that exercises the whole path rather than each part against fixtures. Detail future work progressively. Bounded investigation is valid work whose outcome is evidence and a revised approach.

Masons adapt implementation and make the necessary supporting changes their outcome needs, wherever those are. They consult neighboring assignments and ask for coordination before taking over another unit's outcome; touching code another unit's footprint names is not that, and needs no revision. The chief of staff can commission a revision without an existing question. The architect proposes revisions and focused debate tests them. The chief decides a plan-only revision with recorded reasoning against approved intent. Go requires the spec to remain identical, preserves merged history, validates the graph and footprints, versions assignments, and invalidates affected reviews. Changed tasks, acceptance, constraints, boundaries or dependencies return affected work to implementation. A revision waits until affected running workers have returned before it takes effect. Started identities are retained; completed units change through follow-up work. Unstarted work can be split, combined or removed.

Newly discovered necessary work is recorded under discoveries/ with its criterion and a durable disposition: open, assigned to an unmerged unit, declined with a reason, or retained as an optional suggestion. Workers record discoveries without ending their turns; the chief coordinates dispositions. Assigned work remains necessary until its unit lands. Open discoveries and pending revisions prevent assembly and delivery. Completing the initial unit list is insufficient when necessary work remains unresolved.

In-flight workstreams adopt current factory authority through an idempotent reconciliation recorded in the trace. Adoption and the chief's reassessment notice are committed together, so acknowledged historical blockers can be investigated again without duplicate notices after restart. New workstreams record their policy at intake. Delivered and abandoned workstreams are untouched. Every subsequent role turn, including a queued or resumed turn, receives current operating instructions alongside its original task. Adoption preserves sealed documents, completed work, explicit owner holds, escalations and decisions; it neither rewrites plans nor grants ratification or publication. Any restructuring uses the ordinary revision workflow.

### 4.5 Where they live

```
~/.config/osmia/config.toml      profiles, capacity, budget, Hearsay, listen addresses
<root>/                          default ~/.local/share/osmia
  runtime.json                   profile overrides, pause states, workstream priority, archive records
  skills/                        clones of the skills roles name, and their generated plugins
  projects/<repo-id>/            one git repository per repo: durable knowledge and configuration
    config.toml                 upstream, fork, clone, landing, repo caps; group_id for additional repos
    charter.md                   authoritative project charter in the default repo's trace
    kb/<subsystem>.md
    kb/entities.json             local footprint entities and path mappings
    notes/<role>.md              a role's private craft memory
  workstreams/<repo-id>/<id>/    one git repository per workstream: that workstream's complete trace
    handed/                      what you gave, never modified
    spec.md                      every revision in the repository's history
    plan.json
    shed/round-<n>/              one file per committee member, the architect's reply, your rulings
    amendments/<n>/              request, draft spec and plan, affected set, its round, the decision
    questions/<n>/               as asked, the decision, what you were sent, what you said, what went back
    units/<u>/                  bundle, checks-<n>.json, review/, landing.json
    agents/<id>/log.jsonl        every turn's request and final response, with provenance
    events.jsonl                 every transition, with who and why
    ledger.jsonl                 cost per session
  workgroups/<workgroup-id>/     cross-repo membership, project spec, shared chief thread and decisions
  workstreams-staging/           a workstream's trace is built here, then published by one atomic rename
  workstreams-deleting/<project-id>/  a trace archive cleanup moved aside, durably marked for removal
```

`workstreams/<repo-id>/` is separate from `projects/<repo-id>/` and every other repo's: a workstream's trace never shares a parent directory with the project tree. `workstreams-staging/` and `workstreams-deleting/<project-id>/` are working areas of the service, not a stable public layout: a directory listing of `workstreams/<project-id>/` never observes a workstream before its trace is complete, and never observes one once cleanup has begun removing it.

The defaults follow the XDG base directories: the top-level configuration is `osmia/config.toml` under `$XDG_CONFIG_HOME`, and the root is `osmia` under `$XDG_DATA_HOME`, falling back to `~/.config` and `~/.local/share`. Everything else Osmia keeps is in the root. An explicit `--root` is self-contained and holds its own `config.toml`.

Each workstream's own trace repository's history is its decision trail. `osmia trace` walks it in either direction: criterion to unit to turns to review to commit, or commit back to the ruling that shaped it. Each repo trace keeps its knowledge decision trail. The default repo's trace owns charter revisions, and the cross-repo workgroup trace keeps the shared project-spec and conversation history.

When the service opens a project whose trace still uses the older shared layout, every workstream recorded inside the project's own repository, it migrates each retained workstream into its own dedicated trace repository automatically, before the project is opened or scheduled; other projects keep running meanwhile. Migration is resumable: a crash at any step, and a restart mid-copy, both resume or complete on the next start without losing or duplicating a record, and a workstream is never authoritative in two places at once. For each workstream it builds the new repository from the workstream's subtree of the shared trace, verifies that it holds exactly the same records, latest content and revision history before relying on it, and only once every workstream is verified reclaims the obsolete shared storage: it rewrites the project's own history to drop the workstream paths and prunes the objects that history alone held, while keeping the charter, knowledge base, notes and decision history intact. An archived workstream not yet cleaned up is migrated the same way as any other, so archive cleanup still finds a real trace for it; its archive record is kept either way. Migration logs its start, each workstream's copy, verification and switch, reclamation, completion and any failure or reason, naming the project and workstream and never transcript content or credentials.

Startup also wraps each existing one-repo project as a default project containing that repo, retaining its ID, paths and authoritative charter. Every existing non-session workstream, including building, delivered, abandoned and archived records, appears in its own one-workstream workgroup with its existing chief thread. Exploratory sessions appear under the project outside all workgroups, with their session kind, thread and exclusion from factory passes unchanged. This project/workgroup migration requires no owner action, preserves traces, rulings, decisions and archive records, and is resumable and idempotent across crashes and repeated starts.

State updates and their outbox entries form one recoverable commit: after a restart both are visible or neither is. Appends alone are not a transaction. Durable operation identifiers connect an intended side effect to its result, so recovery can inspect a workspace, branch or existing pull request before retrying. Each consecutive failed attempt of an operation doubles the wait before the next, up to five minutes, so a persistent failure does not become a retry storm. A change that must write both project knowledge and a workstream's trace, such as a charter revision and its notice, a librarian refresh point, or a ruling promoted to a project decision, stays recoverable as a whole: after a restart neither store ever shows half of it. Writes to one workstream's trace do not serialize behind another workstream's, beyond the project's own brief bookkeeping of which workstreams exist. Revisions and records remain available after delivery or abandonment; removing a project from active configuration does not delete its trace or the owner's clone.

---

## 5. Workgroup and workstream lifecycle

Every feature request runs as a workgroup. Handing a document in from the project pane or `osmia handin <project-id> <doc>` initially creates a one-workstream workgroup in the named repo with that document as its brief. With two or more active repos, the architect selects the affected repos through the same production start path as design intake; Go validates and records that selection before allocating one member per selected repo. A single selection targets that repo and keeps the ordinary ratification packet; several selections get one all-member packet. No design session or repo-selection approval is required. The one-repo path needs only ratification, final-review acceptance and per-workstream delivery approval; a graph is shown only for several members or edges.

The owner can discuss a feature in a project's design session before handing it in. The design agent has the project charter and read-only repo context, asks readiness questions about material choices, and stores its brief draft in `tools/design-brief.json`. Starting from that brief is one owner action. The handed brief and reference transcript become immutable, and the architect selects the affected repos from the project. Go validates the selection, allocates one member per affected repo and creates each member's handed input. The architect drafts the member specs and plans and the shared project spec, contracts and graph; the committee receives that shared draft for debate.

When selection allocates members, the original conversation, from either the design session or the ordinary hand-in, becomes the workgroup's single chief thread in its workgroup trace. Its identity, messages, request-response links and bounded recovery state continue; historical member traces stay in place. The original address and each member's conversation and feed reach that same thread. After a resolved member is owner-archived, surviving members keep using that authority and its active project spec, including after authorized cleanup and restart. Archive records preserve verified resolution and group identity; cleanup records intent before deleting history. Missing history alone proves neither resolution nor merge. Consolidated inbox entries preserve their original question and ruling links in the group trace. A remaining effect that needs unavailable retired history stays pending with a recoverable diagnostic. For several selected repos, the existing ratification action approves the entire packet. A durable group decision pins the project spec and every member revision; preparations reconcile after interruption without another approval. Until every pinned seal completes, the entire group remains pending and no member can build or release a contract edge. Final-review acceptance, per-workstream delivery approval and owner signing keep their existing gates.

### 5.1 Feature states

```
handed -> sketched -> in-shed -> ratified -> building -> assembled -> delivered

revisions adapt the plan in building or assembled; the owner decides intent changes

terminal: delivered, abandoned
```

| State | Meaning | Handled by |
|---|---|---|
| handed | You gave the factory a document, a design, an issue link, or a paragraph. Copied under `handed/`, never modified. | owner |
| sketched | The architect has drafted the spec and plan against the handed design, the charter and the knowledge base. | architect |
| in-shed | Committee members read the spec and plan in parallel and object with citations. The architect answers once per round. Rounds are capped by `max_shed_rounds`. You may object, rule, edit the spec directly, or ratify as handed and skip debate for small work. | committee, architect, owner |
| ratified | Your ratification of intent, with any charter or owner objections explicitly resolved and the current execution plan recorded. The seal is recorded: upstream main commit and spec hash. Footprints are taken from the plan, entanglement advisories issued, the feature branch created. | owner, foreman |
| building | Units move through their own states below. Amendments may arrive. | mason, committee, foreman |
| assembled | Every unit has merged. The service records which merged units serve each criterion. One committee member reads the whole branch at a high level against the spec's intent and the charter, and reports findings, each blocking or advisory. The chief of staff triages blocking findings into a plan revision or leaves them to you; you can accept them as they are at delivery. | committee, chief of staff, owner |
| delivered | The branch is pushed to your repository or fork and the pull request opened under your name, with a description the chief of staff drafted and you approved; or you merged the branch into upstream yourself and recorded it. Terminal. | foreman, chief of staff, owner |

The stored workflow state `delivered` records publication as well as an owner-recorded merge. It is distinct from workgroup resolution and the project pane's labels: an open, unmerged PR is **PR open**; **delivered (merged)** requires merge evidence, whether live or archived. Neither draft nor ready publication resolves a member or releases a merge edge. Cancellation uses the recorded abandoned terminal state and displays **cancelled**. A workgroup resolves exactly once, when every member is merged or cancelled. Archive and cleanup do not supply a merge or turn unresolved publication into resolution. Archive records preserve the verified merge commit and reference, or the current pull request lookup identity, so a dependent can observe its upstream merge after archive, cleanup and restart. Retained history remains readable to the edge resolver; a missing merge record never becomes a release signal.

The service observes every delivered member's own current PR, with or without a same-repo base or downstream edges, at most once per minute. A matching merged or closed outcome stops observation. Open responses and failed lookups record no outcome; failures retry on the next cadence. A closed-without-merge outcome stops observation but leaves the member unresolved. This observation is distinct from dependency-edge release.

Within the ordinary build loop feature states move forward. An approved workgroup amendment can reopen affected open-PR work on its existing branch (section 5.4). An assembled feature whose final review finds something blocking remains assembled. Nothing is built from a finding on its own: the chief of staff groups the blocking findings that need code into one plan revision, whose units use the normal implementation, review and landing loop, and the final review runs again after they land. The chief leaves a finding that is mistaken or not worth building to you, and you can accept any blocking finding when you approve delivery. Once final reviews of a workstream have found something blocking three times, further revisions of its assembled plan are yours to decide. Changes to ratified intent require an owner amendment; changes to the execution plan use a factory-approved revision. Neither a failed final review nor the end of a debate cap authorises delivery.

The owner may abandon an undelivered workstream. Abandonment stops new turns, cancels active turns while retaining their recorded work, releases capacity and preserves the trace. It does not delete the owner's branch or close an existing pull request. Delivered and abandoned workstreams are terminal.

The owner may permanently archive a terminal workstream. Archive keeps a small archive record outside the deleted storage: the project, the workstream ID, the title, the outcome (delivered or abandoned), the archive time and, where one exists, the delivery link, the pull request URL or the recorded upstream commit for an owner-recorded merge. Archive cannot be undone; the UI states this before submission.

The archive also pins `resolution_evidence` from the member's history as `merged`, `cancelled` or `unresolved`; `resolved` is true only with merge or cancel evidence. The service polls an unresolved delivered archive's exact pinned current PR on a one-minute cadence, even without a downstream dependency observer and after cleanup and restart. A matching observed merge or durable dependent edge merge receipt updates that evidence. Polling resumes on restart and stops once merge evidence is recorded; failed lookups supply no evidence and retry on the next cadence. An archived member without merge or cancel evidence keeps its workgroup unresolved and the project spec active. The pane shows it as **blocked**, naming the missing pinned merge or cancel evidence. An older `resolved` flag without evidence is re-derived from retained history where available; otherwise the unresolved hold stays visible. Retirement alone is never resolution, and missing history is never recreated to claim it.

Delivery and abandonment alone preserve the trace and remove temporary workspaces. Archive cleanup then deletes the workstream's independent storage, its own dedicated trace repository and its temporary workspaces, without rewriting or pruning any Git history. Cleanup is deferred, with a logged reason, while the workstream has unfinished turns, pending or unacknowledged operations, or a temporary workspace still being cleaned; while any retained workstream still depends on it as a base (an archived base can leave together with its archived dependents); or until the librarian refresh covering its landed units, and any knowledge gap filed against it, has incorporated the workstream's learnings into project knowledge. Once every deferral clears, the service records durable cleanup intent before moving the trace repository aside by one atomic rename. Its removal then runs without holding any repository lock, so deleting even a large trace never delays a read, a write or reconciliation of any other workstream. The archive record is marked clean only once the storage is confirmed gone; a later pass reconciles whether an earlier attempt was interrupted by a crash or is simply still in flight, so a half-deleted trace is never mistaken for a live one. Previously archived workstreams, recorded before this behaviour existed, receive the same cleanup automatically, with their title, outcome and delivery link filled in while the trace is still present. Other workstreams, project records and target-repository branches remain intact.

Each archive cleanup pass with archives awaiting cleanup, and each trace removal it leaves running in the background, reports itself through the backend activity record (section 10.1), so cleanup's progress is visible while it runs and a failed removal stays visible until a later pass retries it. Projects with no archives awaiting cleanup emit no cleanup activity events.

### 5.2 Unit states

```
planned -> ready -> implementing -> checking -> reviewing -> approved -> merged
                        ^              |            |
                        +--------------+------------+ checks failed or changes requested, bounded by max_bounces

any state may also be: waiting (a question is open), contested (send-backs, a mason clean-turn decision or a move)
any started unit that has not merged may be moved by the owner or the chief of staff
```

| State | Meaning | Handled by |
|---|---|---|
| planned | In the plan, dependencies not yet merged. | scheduler |
| ready | Every unit it depends on has merged. Eligible for a slot. | scheduler |
| implementing | A mason works in the unit's workspace until its task is done and its acceptance holds, then ends its turn with the outcome: what the work does and how it checked the acceptance. | mason |
| checking | The service runs the project's Dagger checks on a fresh export of the exact candidate before review. Passing checks send it to review with their result. Failing checks send it back to implementing with the failures and their output, as a send-back counted with review's toward `max_bounces`. A run that does not complete holds the unit here, tells the chief of staff and runs again later; three in a row contest it. | service |
| reviewing | The unit's reviewer reads the unit's task and acceptance, the mason's outcome, the recorded check result and the unit's diff against the feature branch, with the sealed spec as background, verifies the acceptance and decides. The reviewer runs no checks. Material findings send it back to implementing with the findings. The same reviewer re-reviews the revised candidate, always an exact commit. | committee |
| approved | The reviewer is satisfied. Waiting for the foreman. | scheduler |
| merged | Squashed to one commit on the feature branch, message generated from the unit's title and the criteria it serves. Units still in flight are rebased. | foreman |
| waiting | A sub-state of any of the above: the unit's role asked a question and its turn ended. Nothing else on the unit moves until the answer arrives. Other units continue. | chief of staff, owner |
| contested | Send-backs by failed checks and review reached `max_bounces`, the mason gave up or exhausted its clean-turn bound, a role's turn failed on every retry, a reviewer's turn ended without a verdict, its reviews were refused as stale, its check runs did not complete or its role's turns were interrupted too many times in a row, or a move held it for you. The chief of staff rules on it first (section 6.6), except on a unit moved here, which is yours; what it cannot resolve is raised to you with its findings. Nothing on the unit moves until a ruling. | chief of staff, owner |

Waiting and contested preserve the underlying unit stage and any candidate under discussion. An answer or ruling resumes that stage through a recorded transition; it does not bypass review or landing checks. A ruling of review on a unit contested by failed checks sends the candidate to its reviewer with those failures as evidence; it does not make them pass.

The state machine can still leave a unit stuck or wrong: checks that never complete, a block the service reports and cannot clear, a stage that needs to run again, a send-back you disagree with. So you can move any started unit that has not merged, from implementing, checking, reviewing, approved, waiting or contested, to implementing, checking, reviewing, approved or contested, with a note. The move and its note are recorded together. A move to implementing gives the mason a turn with the note and a fresh clean-turn allowance in its existing workspace. A move to checking runs the checks again on the recorded candidate, even if a run of it already completed. A move to reviewing gives the reviewer a fresh review of the recorded candidate with the note, and the review still starts from a completed check run of that candidate. A move to approved records your approval of the recorded candidate in place of the reviewer's verdict, and the foreman lands it with the usual landing checks. A move to contested holds the unit for you. A move into the state the unit is in restarts that stage. Planned and ready units are the scheduler's, a merged unit has landed and is changed only through a follow-up, and a unit does not move while the foreman is landing or rebasing it. A move from waiting leaves the question open; its answer still reaches the role. A unit waits only on a question its role's latest turn asked, so a question asked before a move, or while the unit was contested, does not send it back to waiting once the move's turn follows it.

A check run is bound to its candidate, the feature branch commit it was built on and the diff between them, and recorded under the unit with the checks it ran, why they were chosen, the command, its exit status, a bounded diagnostic excerpt and its complete captured output, including failures and runs that did not complete. The service extracts failed-check diagnostics deterministically and classifies failures from the complete output. Agents can retrieve the captured output in chunks through factory_context; excerpt limits never discard the underlying evidence. A review starts only from a completed run of the exact candidate it reviews; a candidate that changed, through a revision or a rebase, is checked again first. With the Jev boost on, a judgment chooses the checks the change can affect from the project's check links, `dagger check` runs those, and otherwise every check runs (section 9.6). Passing checks are evidence for the review and never an approval. A mason contest has no candidate, so the owner can only return it to implementing with a note and a fresh clean-turn allowance. Plan dependencies must reference existing units and form an acyclic graph. Invalid plans cannot be ratified, and an amendment must preserve those properties.

### 5.3 The shed

The shed challenges the initial intent and engineering approach, and holds focused debate for revisions. Members review from correctness, integration and scope perspectives and cite evidence. The architect answers objections and can revise the draft. Charter violations and explicit owner objections block ratification. Fit, unit size and acceptance-method disagreements are engineering advice; they receive an architect response but need not converge unanimously. The chief presents the intent, recommended approach, assumptions and material dissent for ratification.

Debate is bounded. Further engineering work uses a focused revision or investigation rather than repeating a full committee merely to obtain agreement. A small feature may skip initial debate at the owner's request. Owner ratification remains required.

### 5.4 Revisions and amendments

The chief routes a question or commissions a revision for new evidence, discovered work, ownership changes or dependencies. The architect drafts against the recorded spec and plan, and a bounded shed round examines the proposal. The chief may decide a plan-only revision with an explanation of how it preserves intent. The owner decides changes to the spec, constraint exceptions and owner-reserved tradeoffs. A model's assertion cannot authorize a spec change: the service compares the proposed spec with the current approved text. Charter and owner objections remain owner decisions.

The existing documents govern until a revision is applied. Revisions are pinned to the source seal and refused when it has changed. Application versions the documents and records affected units atomically with the seal; subsequent recoverable steps update assignments, invalidate reviews and notify workers. Active affected turns finish before resealing. Changed approved intent invalidates the corresponding evidence and requires fresh review. A rejected proposal preserves the existing documents and returns its reason to the requester. Nothing edits target code as a side effect of a plan revision.

The owner can request an amend in the workgroup's chief-of-staff conversation,
including the continued design chat. The architect and committee debate only
the delta against the approved project spec and slices. The service compares
the proposed text and presents one impact summary with the proposed intent,
affected workstreams and unit assignments. That summary is the amendment's
only owner approval. Contract changes re-evaluate dependents along the graph.
Unchanged units retain their state and commits; changed completed units get
revision units, removed sections get code-removal units and new sections get
new units. Reworked units wait for their revised dependencies.

A member that has not started receives its new slice before building. Active
members stop at a safe unit boundary before rework. An open, unmerged pull
request retains its feature branch and is updated on the same pull request
only after fresh review and that member's own delivery approval. Changed
sections of a merged member become a new follow-up member in the same
workgroup; merged branches and historical decisions remain intact.

The owner can also cancel a workgroup or a member in that conversation. One
impact summary includes cancellation of its transitive dependents and asks
whether open pull requests should be closed or left as drafts. Cancellation
holds unstarted landing and publication operations while repository effects
already begun reconcile to a whole-unit boundary. Unlanded work remains on
unit branches through the ordinary abandonment cleanup. Merged work is kept,
including a pull request that merges while cancellation is being applied.

### 5.5 Delivery

Every PR requires its workstream's own owner delivery approval of the description and commit message. This applies to a draft, an update to an open PR and marking it ready, including post-merge dependency updates. There is no workgroup-wide approval or pre-authorized publication.

At assembled, the foreman rebases the feature branch onto upstream main one last time, the service runs every project check on the rebased commit, the committee's final read runs against that with the check result as evidence, and the service drafts the pull request title and description from the trace: the title is the workstream's title (the status goal you see for it, or, before one is set, a fallback from the sealed spec's first heading or the handed issue's title), and the description a short summary of the sealed spec, closing the GitHub issue the workstream was handed in from when one is recorded. You see the title and approve the description, or edit it; the title is not edited separately. The foreman pushes the branch to your repository or fork and opens the pull request as you. Per repo, the branch lands either with one commit per unit, the default because reviewers on a large project want reviewable commits, or squashed to one.

The final read is high level. Each unit was already reviewed in detail against its own task and acceptance, and the checks cover style, formatting and lint. The service gives the reader every criterion with the merged units that serve it; a criterion no merged unit serves is blocking. The reader follows how the units' work connects in the running product and checks the whole against the spec's intent and the charter. It records a summary and findings. A finding is blocking only when the feature as a whole fails the spec's intent, its units do not work together, or it breaks a charter rule; anything else is advisory, shown to you and building nothing. A later final review starts from the previous one: it reads what changed since, says whether each earlier blocking finding is resolved, and raises a new blocking finding only where a change since then breaks the feature or a criterion plainly fails.

The final read never reads a commit whose checks did not complete. Such a run is recorded, and the checks run again after a wait that doubles with each further run, up to an hour. Once three runs in a row have not completed, the final review shows in the inbox with the last failure and when the checks run next. It takes no decision: the owner fixes the cause, and the entry closes once a run completes.

You approve delivery with the findings in view. Approving a report with blocking findings accepts them as they are, and the approval records what it accepted; without that acceptance, a report with blocking findings authorises nothing.

Local snapshots, unit commits and the delivery candidate presented to the owner may use Osmia's unsigned identity. With `landing = "squash"`, the delivery candidate combines the whole workstream into one commit on the reviewed base. The owner can edit the delivery commit message as well as the PR description. Squashing and owner review do not require signing credentials.

After owner approval, the service performs a pre-publication step before any push: it amends the delivery commit using the owner's Git identity and signing configuration on the service host, including configuration scoped to the project's clone. It sets both author and committer to the owner and uses the owner-approved commit message, removing model or runtime-agent co-author trailers such as `Co-authored-by: Claude ...` while preserving human co-author trailers. It adds the owner's `Signed-off-by` trailer, then signs with the owner's configured key and signing format so the signature covers the final message. With `landing = "commit-per-unit"`, it applies the same requirements to every outgoing unit commit. Delivery approval authorizes this attribution, message cleanup, sign-off and signing. Missing identity, an unavailable key or a signing failure blocks publication, with no unsigned fallback. Runtime agents never receive signing credentials.

Ordinary delivery completes implementation; a workgroup dependent can still repair its post-merge update within its slice, and an approved amendment can reopen an unmerged member. Approval is tied to the final reviewed branch revision, the PR title and description you saw, and the delivery commit message. If any changes before publication, the affected review and approval must be refreshed; a status goal set after approval does not change the title an already approved delivery publishes. The authorized pre-publication amendment changes commit IDs but must preserve the approved tree and base, and the approved message apart from agent co-author cleanup and the owner's sign-off; it does not require another owner approval. The trace links the reviewed history and delivery candidate to the signed delivery history. A change to the content or base requires refreshed review and approval. The service durably records the signed delivery commits before pushing. Publication is resumable: a retry reuses those commits and finds the recorded branch and any already-created pull request instead of signing again or opening another one. Without a GitHub token the service attempts no publication: it neither pushes nor asks GitHub anything, and the publication shows in the inbox at once, waiting for a restart with the token. A publication whose last three attempts failed shows in the inbox with its last failure, including the host's explanation of a refusal, and when it is next attempted. It takes no decision: the owner fixes the cause, such as a token without pull request access, and the entry closes once an attempt publishes. These requirements also apply to delivery maintenance for dependent workstreams and owner-requested drift checks of delivered pull requests.

You may instead merge an assembled feature's branch into upstream yourself, outside the factory, for example while the service runs without a GitHub token. The factory still delivers through a pull request; this path only records a merge you made. You record it as the owner: the service fetches upstream's base branch and checks that it already holds the changes of the feature branch tip, meaning their merge is clean and changes nothing, so fast-forward, merge, squash and rebase merges all count. One commit then records the merge, naming the branch tip and the upstream commit that holds it, and moves the feature to delivered with a notice for the chief of staff. Nothing is pushed and no pull request is opened. The merge needs no final review or delivery approval: merging was your delivery decision. It is refused for a feature that is not assembled, and while upstream lacks or conflicts with the branch. A publication waiting for a GitHub token is refused in the same commit. A publication the service could still carry out refuses the merge until the publication has an outcome. Descendants treat a base delivered this way as integrated upstream.

When Hearsay is enabled, upstream review feedback can later arrive through its GitHub connector. The chief of staff presents it as a notice or question (section 12.4). For a workgroup, an owner-approved amendment can rework an open, unmerged pull request on its existing branch; changed merged work instead gets a follow-up workstream (section 5.4). Other additional code work requires you to hand in a new workstream, referencing the delivered trace and, when appropriate, its branch as the base. Without Hearsay you may hand in that feedback yourself.

---

## 6. The chief of staff and questions

### 6.1 One persona, one thread per workgroup

Each workgroup has exactly one chief of staff, with no independent member chiefs. Every role's `ask` and each member's events reach that shared durable thread. Messages sent through any member reach it too. It reads the brief, project spec, slices, each repo's KB and code directly. Its persona also carries owner rulings, project decisions and optional Hearsay memory. Factory-wide controls work from any thread.

Questions from several members can remain open concurrently. Each answer resumes the original asking role and turn in its own repo's trace, preserving asking and response links through restart and bounded replay. One-workstream workgroups retain their existing chief thread. Shared-chief turns hold no exclusive trace lock across the model call, so owner conversation, feed and project-spec reads remain responsive.

### 6.2 What it does with a question

Every role has an `ask` tool. Its question always goes to the chief of staff, never to another agent. The chief of staff does one of five things, and the choice is recorded.

1. **Answers it** with an engineering decision within approved intent, supported by documents, code and evidence. The bundle for the question's scope arrives with current stances, conflicts and open questions, each with a pointer. A ratified stance answers a question, cited. Redirecting is a valid answer: "the code answers that, look here" is what a good chief of staff says to a builder who did not look.
2. **Escalates it** when it changes approved intent, relaxes a constraint, needs an owner-reserved tradeoff or contradicts an owner ruling. It rephrases for you: which unit, which criterion, what is blocked, the options, its recommendation. Several open questions are batched into one ask.
3. **Routes it as a revision** when assignments, dependencies or the spec must change. The chief decides engineering revisions; the owner decides intent changes.
4. **Proposes a charter amendment** when your answer is a standing rule rather than a decision about this feature.
5. **Rephrases your answer** for the asker and decides its scope: local to the asker, or a notice in every in-flight bundle on the project when it applies wider than the question.

Worker escalations state the spec section, conflict, options and recommendation. Only a contradiction or extension of agreed design needs an owner design ruling; the chief resolves engineering choices from the record. A genuine gap pauses only affected units or workstreams, and becomes one consolidated owner question with options and a recommendation. The ruling resumes all and only its paused origins. Judgment calls within intent are logged through `log_assumption`; each workstream's drafted PR description includes its local assumptions and the shared chief's judgments affecting it. The chief can name affected member IDs with `workstreams`, or omit that selector for a workgroup-wide judgment. Shared judgments stay in the workgroup trace and remain readable after restart.

With the Jev boost on, the turn that delivers a question also carries advisory signals on it: whether the honest answer changes the sealed spec or plan, whether answering it would contradict a ruling, and whether it asks for a standing project rule (section 9.6). They inform the choice and never make it.

Every question lands in the trace as `questions/<n>/`: the question as asked, the decision, what you were sent, what you said, what went back, and what it changed. With Hearsay enabled, your answer is ingested under your principal through the owner source, which authority makes a ratified stance. The local ruling is authoritative and durable before external ingestion. An optional immediate assertion must reconcile to that same ruling.

### 6.3 Waiting

Nothing on the scheduling path waits on a model, and a session blocked on a tool call for the hours you are asleep would hold a slot and hit its timeout. So asking is non-blocking. Calling `ask` records the question and ends the turn with the outcome `waiting`. The workspace is intact. The unit parks in `waiting`, its slot is freed, and the rest of the factory carries on. When the answer arrives it is delivered as the asker's next turn, with the thread's context intact, so from the asker's side it asked and then it heard back. An answer that arrives after the asker's unit merged stays recorded but is not delivered: the unit's workspace is gone, and a merged unit changes only through a follow-up.

The chief of staff runs as a singleton per workgroup, one turn at a time, so its record stays coherent and it can batch. A question waits with no timeout. It sits in the inbox until you answer. The chief of staff never answers on your behalf under a deadline: you asked to be asked, and a wrong guess silently built on is worse than a parked unit.

### 6.4 Status

The chief of staff keeps a status per workstream, rewritten fresh whenever the answer to one of three questions changes: what is being worked toward right now, what happened since you last looked that changes the picture, and who is doing what.

- **Goal.** One sentence. Changes when the objective changes, not when a step completes.
- **Attention.** Only a concrete action or decision you must take now, with enough context to act. Otherwise empty.
- **Note.** A few sentences on what changed that matters and where things stand. Translated, not compressed: what a worker's result means for the feature, not its identifiers.
- **Agents.** One line per active agent in your own words.

Identifiers stay out: commit hashes, branch names, file paths, session ids, model names. The status is the thing you read for a few seconds after time away. Its goal heads every view, and each revision appears in the workstream's feed beside what prompted it.

### 6.5 Events

The service tells the chief of staff what happened: a unit finished, a review came back, a question was raised, a landing succeeded, upstream moved. Events are written to a durable outbox in the same transaction as the state change, coalesced over a short window, and delivered as one turn, retried until delivered. Routine progress, such as a unit starting, passing its checks, being approved or landing, or the workstream changing state, asks for no judgment: it waits for a longer window, `events.progress_window`, or goes out with the next event that does. While an event turn is queued or running, new events wait for the next one rather than queue another turn. Event turns may run on a lighter profile than the turns that answer you. After the first failed delivery, the service retries immediately; repeated failures wait thirty seconds, doubling up to fifteen minutes. The delay is recovered from durable turn results, does not acknowledge undelivered events, and does not delay new events. Only the chief of staff receives events. Workers receive turns from the scheduler and nothing else, and are never told to wait for another agent.

Events are information, not authorisation. The chief of staff does not dispatch, restart, replace or route around an agent. Transitions are the scheduler's. What it may do is take a decision you could take, through the same tool, and the service applies it as it applies yours.

### 6.6 Acting for you

The chief of staff is your assistant for the workgroup. It takes the workstream decisions you could take when that resolves an issue or a conflict, and raises to you only what it cannot resolve or is not sure of. The service holds it to the same rules as you, records it as the actor, and shows each of its actions in the workstream's conversation.

A contested unit goes to the chief of staff first. Its view includes state, recent transitions, role outcomes, refused tool calls and candidate evidence. It investigates, rules review or revise with actionable reasoning, and commissions replanning or investigation when necessary. Repeated contests remain engineering blockers; escalation is an explicit decision about changed intent, a reserved tradeoff or an operational limitation it cannot resolve. The service budget and loop guard bound execution. The owner may intervene at any time.

When the state machine leaves a unit stuck outside a contest, the chief can return it to implementation, checking or review. It cannot bypass independent review by approving a candidate itself. A deliberate move to contested holds work for the owner with the recorded reason. Owner-requested moves and rulings retain owner attribution.

Intent ratification, spec amendments, charter changes and delivery remain owner decisions, recorded only when the owner gives them. Plan-only revisions are factory decisions and remain out of the owner inbox unless they require an owner exception, or final reviews of the assembled feature have already found something blocking three times. The chief triages a final review's blocking findings: it groups those that need code into one plan revision, and tells you about any it judges mistaken or not worth building, which you can accept at delivery.

The chief has an investigation and recovery mandate. It reads workstream documents and their history through factory_context, inspects a mason's recorded candidate or a service snapshot of its idle captured workspace with inspect_code's unit selector, retrieves referenced GitHub text with read_remote_file, and commissions bounded investigation through plan revisions. Remote sources are resolved to commits and recorded under sources/ with provenance and source citations; credentials remain in the service. Missing verification or factory capabilities are operational limitations, not intent decisions. Current assignments and evidence remain retrievable after context truncation. Assigned discoveries enter the unit's reviewer context and review identity; changing that work invalidates an older review.

### 6.7 Project design conversations

An owner can open a design conversation in a project before handing work to
the factory. The design agent receives the project charter and each repo's
knowledge base, with service-mediated read-only access to committed source
in every repo. It has no source-writing tools, version-control access,
delivery credentials or owner gates. File-based context supports the entire
conversation without Hearsay.

Its readiness pass asks only about expensive-to-undo decisions, genuine forks
without a signal, or tension with the owner's intent. Every question includes
a recommended answer. Other inferences are collapsed assumptions the owner
can override by replying. Its brief records decisions, intent and tradeoffs,
and rejected options. The owner starts a workgroup from the latest brief in
one action on the web page, API or command line. The attached transcript is
reference material; the brief wins. That same durable thread then continues
as the chief of staff's thread for status, questions and escalations. Each
answered escalation shows one owner-ruling action in that conversation,
with the owner's exact answer and its original recorded time. Relaying the
ruling to workers leaves that action unchanged.

For a brief affecting several repos, the architect selects affected project
repos. The service validates that selection and allocates one member per repo,
with immutable handed inputs. The architect drafts the member specs and plans
and the project spec, contracts and graph; the committee debates them. The
original design address continues to serve the shared conversation and the
ratification packet. One existing ratification approves the entire draft.
Members prepare their seals against that durable decision. Until every pinned
seal is complete, the group remains pending and none of its members can build
or release a contract edge. A restart reconciles these preparations without a
second owner approval. Each member keeps its own trace, feature branch and
per-workstream delivery approval and signing.


### 6.8 Sessions

A session is a workstream kind for exploratory work rather than a planned feature: a direct chat with one agent on its own worktree, with none of the rest of the factory around it. A session never runs the shed, the architect, a chief of staff, a mason, a reviewer, the committee or delivery; it never gets a spec, a plan or units; and the scheduler never runs the pre-review check, base refresh or drift-rebase passes for it. The chat is one durable thread and one turn at a time, but a session belongs to a registered project, has a worktree the agent can change, and runs on the profile the owner chose when opening it.

The owner opens a session by naming a registered project and a profile, the same kind the service already runs mason turns on for that project, and may add a title. An unknown project or an unknown or missing profile is refused, and nothing is recorded. Opening records the session's kind, its fixed profile, the base commit (the project's default branch head at opening time) and its branch, and creates a dedicated worktree at that base commit through the same workspace machinery a unit's worktree uses, under the service root and outside the target repository. Sessions appear in the project's workstream list marked as sessions, with their profile and a state of `idle`, `working` or `ended`.

The profile is fixed for the life of the session: opening is the only operation that takes one, and nothing changes it afterward. A resumed backend session belongs to one runtime, so working on another profile means opening another session. The first turn on a session starts a model session on the profile's runtime, which the service records for the session; every later turn resumes it instead of replaying the whole thread, with input limited to the owner's new message plus whatever the service itself recorded since the previous turn, such as a commit or a check result. That model-session state is kept per session under the service root, outside the worktree and the target repository, and survives a restart. If a runtime reports a session's model session missing or unresumable, the turn falls back: it starts a new model session on the same profile, seeds it with the thread's history through the same bounded window a chief-of-staff turn's replay gets, records the new model session for later turns, and adds a thread entry saying its context was rebuilt from history. A message to an idle session starts exactly one turn; a message sent while one is already running is refused as busy and recorded nowhere, since there is no message queue. The owner can stop a running turn, which is recorded as stopped with whatever worktree changes it had already made kept, returning the session to `idle` for the next message to resume the same model session.

The session's agent runs in a sandbox whose workspace is the worktree, so its changes persist into the next turn, with the same reading, writing and command-running tools a mason's sandbox has. Charter rule 5 still holds: the agent has no version-control access, no delivery credentials and none of the factory tools (`done`, `amend`, `ask`), so nothing it does can commit, check or push on its own. Those become the owner's controls instead. The owner can read the worktree's uncommitted changes against the session head and the branch's commits against the base commit. A commit takes a message, stages every current worktree change onto the session branch, moves the head and adds a thread entry naming the commit; it is refused, recording nothing, while a turn is running, on an empty message, when the worktree has no changes, or once the session has ended, and a retry carrying the same request key, including one that crosses a restart, produces exactly one commit rather than a second. A check runs on the session head, one at a time: when the service has Jev enabled it is a smart check, the same smart-check selection a unit's own check run uses, scoped to the changes between the session's base commit and its head, and otherwise it is the same full `dagger check` the pre-review check run uses. Either way the service records the commit checked, whether the run was smart or full, with its selected checks when smart, the status, exit code and output, and adds a thread entry summarising the result and naming the commit. Pushing sends the session branch to the project's remote under the session's own branch name with credentials the service holds; it never pushes to the default branch, never forces, and never opens a pull request, and is refused when the remote branch already carries commits the session branch does not. Ending is refused while a turn is running; once a session has ended, its messages, commits, checks and pushes are all refused, its worktree and stored model-session state are released, and its branch, commits, thread and trace stay readable, with the session listed as `ended`.

A session must never acquire a spec, a plan, units, a chief of staff or delivery, must never change its profile or run a turn on any other one, and must never replay its whole thread while the recorded model session can still be resumed. The owner opens it and works its commit, check, push and end controls from the command line (section 11.2) or any API client; the agent's own chat happens only through the web page and the API, never through the command line.

---

## 7. Version control and landing

### 7.1 Fork and upstream

Each repo names an upstream repository, a local clone and an optional fork. The canonical base is upstream's configured base branch. When a fork is configured it receives feature branches and pull requests target upstream. Without a fork, feature branches and pull requests live in upstream. An explicitly identical fork is normalized to omission. The clone needs remotes matching the configured repositories by URL; a single origin remote is sufficient without a fork.

Delivery records the resolved push repository, target repository and base branch before publication so retries preserve the approved destination. Osmia pushes only its feature branches, never the project's base branch or a protected branch, and never merges a pull request. Delivery credentials remain with the service.

### 7.2 Agents hold no tool

Sessions get a plain directory of files. The service performs every version control operation: create workspace, snapshot, rebase, squash, merge, push. A mason improvising a rebase is not acceptable, and an agent that never holds the tool cannot damage history. Enforcement belongs to the execution boundary: agents receive neither VCS tools nor writable VCS metadata nor inherited GitHub credentials. A core profile flag or prompt alone is insufficient. Read-only roles cannot acquire execution or write capabilities through repository-provided tools.

Role execution supports confined host modes (`none` and `claude`), containers,
and Docker Sandboxes (`sbx`). The same scoped files and tools apply in every
mode. An sbx role may select an agent-specific template; provider credentials
and network policy belong to the sandbox proxy, while delivery credentials stay
with the service. The runner allows each service-owned MCP listener's exact
port for that sandbox's lifetime; no global localhost allowance is required.
A sandbox that cannot enforce the requested grants fails the
turn without silently selecting another mode.
A sandbox outlives its turn only when the service stops during the turn; the
service removes such sandboxes when it next starts.

### 7.3 Workspaces

The feature branch is one workspace on the workstream's repo clone. Each unit gets a workspace of its own descending from the feature branch. Workspaces sit behind one interface with two backends: git worktrees, and Jujutsu colocated so plain git can still read history. Jujutsu adds change IDs that survive rebases, a snapshot on every command so an interrupted session never loses work, stored rather than blocking conflicts, and an operation log the service can restore from. The workflow does not depend on which backend a project uses.

A workspace lasts as long as the work it holds. A unit's workspace is removed once the unit merges, since its work is on the feature branch. When a workstream is delivered or abandoned, the service removes its remaining workspaces: it first commits the files of each unit that did not merge to that unit's branch, so abandonment keeps the work. Every branch stays, so each candidate the trace records remains in the clone. Workgroup dependent repair and approved open-PR rework use the same feature branch through the ordinary workspace loop; retiring temporary workspaces never authorizes deleting the trace. A workspace stays while a turn on it is unfinished or, on a finished workstream, while an operation is pending, and a unit's workspace stays while a replay is in progress in it.

### 7.4 Landing a unit

On approval the foreman squashes the unit's workspace to one commit on the feature branch with a message generated from the unit's title and the criteria it serves, then rebases every unit still in flight. A conflict goes to a mason session in the conflicting unit's workspace with the sealed spec and the instruction that the tree carries conflict markers to resolve against it. A session never discovers conflict markers by accident. Reviewers never see them.

An approval records the reviewed candidate, its base and the governing spec and plan revisions. Landing first checks that those inputs are still current. If a rebase changes the candidate or its base, that unit returns to checking and review after any conflict resolution and before landing. The foreman never silently transfers approval to a different candidate. Active file writers must finish or be stopped and snapshotted before their workspace is rebased; the scheduler owns this coordination.

### 7.5 Upstream drift

Upstream main moves daily on a busy project. On a cadence per repo, and on demand, the foreman fetches upstream and rebases the feature branch onto it, then every unit in flight onto that. A rebase that changes what a sealed criterion means files an amendment. The seal moves with the branch.

A rebase that conflicts goes to a drift mason, and a reviewer reads the resolution against the sealed spec. Send-backs are bounded by `max_bounces`, as a unit's are. A resolution sent back that many times holds the drift rebase: the branch and the seal stay, and the workstream takes no further drift rebase, on the cadence or otherwise, until you ask for one or the chief of staff hands it back. A handback asks for the next drift rebase with a note its drift mason and reviewer receive, so the chief of staff can return the work to the role that can resolve it, as it moves a unit. Engineering handbacks remain internal, with budget and loop controls bounding repeated attempts. A drift rebase in progress holds back its own workstream's landings and the project's other drift rebases, never another workstream's units.

The drift mason works in its own resolution workspace, which it may read, write and execute in; it has no version control tool, so the service records its work and goes on with the rebase. It may also ask its workstream's chief of staff about the drift rebase it resolves, read factory context, record a discovery, file an amendment citing the drift rebase's upstream commit, and call done. The drift reviewer reads a read-only export of the resolved candidate, with no write or execution capability; it may ask the chief of staff, read factory context, record a discovery, read the diff with `workstream_diff` and record its verdict. Waiting on the chief of staff's answer leaves the drift rebase pending, not held: the wait counts toward none of the interrupted-turn, marker-reminder or send-back limits, and the resolution resumes once the answer arrives, including across a restart. Conflicted paths, and any other change the rebased branch needs so that both upstream's and the feature branch's behaviour stand, are in scope of the drift rebase whatever the workstream's sealed spec, plan or unit footprints name; a resolved path outside the workstream's original scope is not, on its own, a reason to reject it. The reviewer checks that both sides are kept, that nothing of the feature branch's change was lost or altered, that no change goes beyond what the rebase needs, that no conflict marker remains, and that the sealed criteria still hold.

Delivered workstreams with an open pull request retain **Check for drift**. An explicit owner request checks that pull request and runs the same drift rebase and bounded conflict resolution against the sealed spec. Ordinary delivered workstreams receive no cadence-driven drift checks. Workgroup dependency polling still detects merges; merge waits are separate from drift maintenance. A changed branch or base requires a fresh final review and owner approval of the delivery commits and description before the service signs and pushes the branch with its existing remote-tip safety check. Publication updates the existing pull request, rechecking that it is open before pushing. An unchanged branch needs no new approval or push. Requests, conflict resolution, reviews, approvals and publication recover from the trace after restart. The feature stays delivered and no implementation units restart; a change to approved intent requires a new owner-requested workstream. A merged, closed, archived or owner-recorded-merge workstream cannot take this maintenance path.

### 7.6 Entanglement and dependencies

Two units in one workstream are entangled when the plan makes one depend on the other or their code footprints intersect. Entangled units run in sequence. Workstreams on the same repo can overlap in code entities, even when a workgroup supplies a shared spec, and the foreman warns when both are in the same subsystem before both pull requests are open.

Footprints name entities from the workstream's own repo and resolve through its local entity map even without Hearsay. They schedule work; they do not bind it. A mason changes whatever its outcome needs, and widening a footprint never needs a plan revision. A unit's review does not hold its diff to its footprint: a reviewer who finds the work wandering beyond its task says so as a finding, and a change outside the footprint that the task needs is not one. Unknown or ambiguous mappings cannot be treated as proof of disjointness. Within a workstream the scheduler serializes such units until their footprints are resolved. Across workstreams overlap is advisory by default; the owner can pause or reprioritise the affected work.

A workstream may declare another on the same repo as its base. It then rebases onto that workstream's branch instead of upstream until the base change is integrated upstream. Its dependent pull request is opened in the push repository against that branch. Once the base is integrated, the service prepares an upstream pull request from the descendant's branch, refreshing the affected review and owner delivery approval before publishing it. With a separate fork, the trace retains both requests and their relationship. Without a fork, the service retargets the existing pull request to the configured base branch and updates its approved description after fresh review and owner delivery approval. The operation records the prior request identity and expected contents, reconciles interruptions before retrying, and refuses an unexpectedly edited or closed request. The trace retains both publication records and the request relationship. This is how one large change lands as a sequence of pull requests. Stacking inside one workstream is not supported.

Delivery of a base means its pull request is open, not merged upstream, so descendants continue to use that branch after delivery. Once the base is integrated upstream, the service can rebase descendants onto upstream and publish their upstream requests. For an already delivered descendant, this is a separate delivery-maintenance operation: the feature stays terminal and no implementation unit restarts. Conflicts can use owner-requested drift resolution; changes to approved intent need a new owner-requested workstream. Base relationships must remain acyclic. An abandoned or unavailable base parks descendants for an owner decision; they are never silently moved to another base. A parked descendant enters your inbox. When its own base was abandoned after its changes reached upstream by any route, such as a merge outside the factory, you can move the descendant onto upstream: the service checks that merging the base commit the descendant holds into upstream changes nothing, then records upstream as the descendant's base, and its next drift rebase replays only the descendant's own commits there. Osmia never merges the upstream pull requests itself.

---

### 7.7 Workgroup edges across repositories

A workgroup graph must be acyclic and name only its member workstreams; invalid graphs cannot be ratified. It may carry contract (the preferred default), hard-block and merge-order edges. Merge is the only release signal. A contract edge resolves on ratification of its project spec. A hard-block edge starts no dependent unit until the upstream pull request merges. A merge-order edge permits both workstreams to build, but keeps the dependent pull request a draft until the upstream merges. The service polls GitHub and also accepts an owner-recorded merge. Agent completion, tags and package versions are not release signals. Workstreams never merge pull requests or push a base branch.

While waiting, roles receive the upstream branch or commit in their file-based context. After its own delivery approval and owner signing, a dependent may publish a draft that states and links every unresolved upstream pull request. If an upstream pull request is not yet known, publication waits instead. Waits are status, not inbox decisions or webhook notifications: for example, `waiting on owner: repo-a#123 merge`.

After a merge, an ordinary unit on the dependent's existing branch updates its dependency reference to the merged upstream commit. The dependent re-runs checks and final review. Blocking findings from that review become one follow-up unit that repairs them within its ratified slice; only a contradiction or extension of the design needs escalation. This does not change the separate same-repo base-dependency maintenance rule in §7.6. The dependent and shared chief of staff retain the ratified project spec and contracts while this work remains pending. Updating the open pull request or marking it ready requires a fresh per-workstream delivery approval and owner signing, never a pre-authorized publication.

## 8. Scheduler

### 8.1 Controllers per state

The service runs one controller per role kind, each reconciling its own input state against the tracker and sharing one pool of slots.

| Controller | Watches | Capacity |
|---|---|---|
| architect | `handed`, amendment requests | one per workstream |
| shed | `in-shed`, amendment rounds | committee members per proposal, `max_shed_rounds` |
| mason | `ready`, `implementing`, conflict resolution | `capacity.masons`, plus a per-workstream cap |
| checks | `checking` | one check run per workstream |
| reviewer | `reviewing`, `assembled` | `capacity.reviewers` |
| foreman | `approved`, landing events, upstream cadence | one lander per repo, landing is serial |
| librarian | repo added, unit merged | one per repo |
| chief of staff | every member's outbox and your messages | one turn at a time per workgroup |

### 8.2 Level-triggered

Controllers reconcile against the current state. Events only wake them early. A missed event costs latency and never correctness. Crash recovery is restart, re-read state, resume. A turn that was running when the service died is found by its session directory and its thread is given a turn saying so.

### 8.3 Finish before start

A freed slot is offered to review before implementation, implementation before debate, debate before drafting, so the factory finishes work rather than widening it. Within a stage, the slot goes to the highest-priority workstream with something ready, and round-robin breaks ties across workstreams and projects.

### 8.4 Capacity, priority and pause

Capacity is per role kind and global across every workstream on every project. A per-workstream work-in-progress cap keeps one feature from taking everything when it is alone. No repo reads another repo's trace to share a limit: each repo's passes count the slots it holds, the turns waiting for one and the day's spend from its own trace and publish them to the service, which enforces the shared limits and reports capacity and the daily budget from what the projects last published. A published total follows each project's next pass, so it can briefly overstate a slot that was just freed, never understate one in use.

Active workstreams have a priority order you set, or ask the chief of staff to set. A paused workstream, project or factory dispatches nothing new. Turns in flight finish, and a hard pause stops them too. Paused units stay where they are, their slots go to what is not paused, and resume picks up with nothing to reconcile. The chief of staff stays reachable while everything is paused, so "pause everything, I'm travelling" and "resume dagger only" are messages. Pause state persists across restart and is shown with who set it and why: you, the daily budget, a provider's usage limit, or the loop guard.

### 8.5 Cost, retries and degradation

A per-session cost cap protects the infrastructure. A per-unit cost is a signal: passing it files an amendment request saying the unit is bigger than planned. A daily budget across the factory pauses dispatch when reached.

Nothing the service repeats on its own repeats without a bound. Send-backs, clean turns, rounds and drift resolutions have their limits, and so do reviews the service refuses as stale, check runs that do not complete, reminders to remove conflict markers, continuations of interrupted turns and redelivery of notices the chief of staff's turns keep failing on. Final reviews are bounded too: once three have found something blocking, further revisions of the assembled plan are the owner's. Each limit ends in a decision someone can take: a contested unit, a held drift rebase, an amendment in your inbox, or notices held until you message the chief of staff. A chief's recovery instruction does not count as progress. Behind them all, the loop guard watches each workstream for sessions without progress, meaning a change of state of the feature, a unit, the shed, an amendment, the final review or the publication, a drift rebase that moved the branch, or anything you did. A workstream that runs `loop.max_sessions` of them pauses with the loop guard as its source and enters your inbox, and resuming it starts the count over. Failures are classified as infrastructure or behavioural: infrastructure failures retry, then fall to the profile's fallback; behavioural failures are outcomes and go back into the state machine. Streaks of failures show in the status so broken plumbing is visible rather than silently expensive.

---

## 9. Agents

### 9.1 Durable threads

An agent is a persistent, resumable thread: an id, a role, a workstream or shared workgroup scope, a backend session id, an owned message log, and at most one active turn. A process runs only while a turn is in flight and is recovered automatically. Messages to an agent are turns, whether from the scheduler, the chief of staff or, for the chief of staff, from you. A message that arrives mid-turn queues as the next turn.

Durable threads are why a reviewer remembers what it found last round, a committee member argues round three better than a debate record could tell it, and a mason that asked a question picks up where it stopped.

### 9.2 The owned message log

Every turn's request and final response is captured by the service, per agent, in `agents/<id>/log.jsonl`, with provenance: what caused the turn and at what depth. The service does not read the agent binary's own transcript files. Their schema is unofficial and versioned, and owning the log is what makes a thread survive a profile switch: when the next turn cannot resume the backend session, it starts a fresh session with a bounded replay of the owned log. Chief-of-staff event and owner conversation turns also receive a project briefing with governing rules and decisions, references to retrievable knowledge, latest stored status and open inbox escalations, so durable context remains available when bounded replay omits the tool calls that recorded it. One capture path per backend, since each exposes turn results differently.

### 9.3 Profiles

A profile names an agent binary, model, effort, optional fallback profile, timeout and turn limits. The `fallback` setting refers to another named profile, so a fallback can change the model or the agent binary. Unknown references and fallback cycles are configuration errors. Roles bind to profiles in configuration, and the committee's members may each be assigned a profile of their own. The binding can be overridden per role while the factory runs, from the web interface or the command line, effective for every new turn on every workstream. Turns in flight finish on the profile they started with. A switch that stays on the same agent binary resumes the thread as it is. A switch across binaries starts the next turn fresh from the owned log.

When the service sees a provider's usage limit, the role falls to its profile's fallback automatically and the status says so. A manual override wins either way.

Provider limits persist in `runtime.json` by agent backend. New turns of roles bound to a limited backend use the first profile in their fallback chain with an available backend. A role with no available fallback is paused with provider attribution. A reported reset time releases the limit when it passes; the owner can also clear a limit with `osmia profiles clear-limit <backend>`. A limit without a reset time remains until cleared. An owner profile override takes precedence while a limit is active.

### 9.4 What a session sees

A session receives a scoped plain-file view with the Osmia MCP server and a bundle. A mason works in a disposable writable copy; the service captures its changes into the unit workspace. A reviewer receives a read-only export of the exact candidate commit and separate writable scratch space. A reviewer's next turn replaces its export, and the export is discarded when the unit merges or the workstream finishes. Session records and authoritative repositories remain service-owned. A workstream's session directories, including each agent's own transcript, last until it is delivered or abandoned and its turns are done; they are then removed with its workspaces, and its trace keeps every turn's request and final response. It additionally receives a Hearsay MCP server when that integration is enabled and available.

The Osmia server, role-scoped:

| Tool | Roles | Purpose |
|---|---|---|
| `ask` | worker roles | Raise a question to the shared chief of staff. Ends the turn with outcome `waiting`. |
| `log_assumption` | Chief of staff | Record an in-intent judgment for inclusion in the drafted PR description. |
| `done` | all | End the turn with an outcome and a report. |
| `notes_read`, `notes_write` | all | The role's private craft memory for the project. |
| `amend` | drift mason | File an amendment request when an upstream change alters what a sealed criterion means. |
| `object`, `concede` | committee | A shed contribution, citing the spec, the charter or the knowledge base. |
| `verdict` | committee | A review verdict: the decision, how the reviewer verified the acceptance, and findings with severities and the action each asks for. |
| `final_report` | final reviewer | The final read's summary and findings, each blocking or advisory, with the criteria it concerns and the action it asks for. |
| `workstream_diff` | unit, drift and final reviewers | Read the diff a review is pinned to: the whole diff, the changed files with line counts, chosen files or directories, or the hunks touching a line range. A unit review reads its candidate against its base, a drift review the feature branch's change before the rebase and the resolved candidate's change on upstream, and a final review the branch against upstream. Review prompts list the changed files instead of carrying diffs. |
| `answer`, `escalate`, `route_amendment`, `propose_charter`, `set_status`, `notify` | chief of staff | The five outcomes of a question, the status, and a retrievable informational project notice. |
| `inspect_code` | chief of staff, architect | Read committed project code or a unit candidate, with snapshot identity and an inspection citation. |
| `project_context` | all factory roles | List, search and read project charter, KB and informational notices in bounded pages, including historical revisions. |
| `kb_check` | librarian | Validate staged KB output and prose budgets without publishing it. |
| `factory_context` | chief of staff, architect, mason, unit reviewer, drift mason, drift reviewer | List or read workstream documents at current or historical revisions, independently of conversation replay. |
| `read_remote_file` | chief of staff, architect | Retrieve a GitHub repository file at a resolved commit and record its provenance and content. |
| `record_discovery` | chief of staff, mason, unit reviewer, drift mason, drift reviewer | Record necessary discovered work; the chief assigns, declines or retains suggestions with reasons. |
| `pause`, `resume`, `prioritise`, `capacity` | chief of staff | The factory-wide controls. |
| `request_change`, `decide_change` | chief of staff in an owner-message turn | Record the owner's workgroup amend/cancel request or impact decision; no publication or merge authority. |
| `decide_amendment` | chief of staff | Decide a plan-only engineering revision or relay an explicit owner decision on intent. |
| `resolve_contested` | chief of staff | Rule review or revise on a contested unit on your behalf, escalate it to you, or record the ruling you gave in a message. |
| `move_unit` | chief of staff | Move a started unit to implementation, checking or review, explicitly escalate it, or record an owner-requested move; approving directly requires the owner. |
| `hand_back_drift` | chief of staff | Hand a held drift rebase back on your behalf with a note its drift mason and reviewer receive, or record the handback you asked for in a message. |

The Hearsay server: `get_bundle`, `resolve`, `stance_history`, `get_l1`, `get_l0`, `search`, `assert`, filtered by the role's agent class and your principal.

There is no tool that lists agents, messages an arbitrary agent, or creates one. Routes are fixed by role.

### 9.5 Sandboxes

A role runs on the host or in a container, per profile. In a container the workspace is bind-mounted and the MCP servers are reached over HTTP from the host. Native tools follow role capabilities: reading tools for read-only roles, editing tools for file writers, and a shell only for implementation turns with execution permission. Native delegation, web tools and arbitrary MCP discovery are not granted. Read-only roles cannot edit their inputs or run arbitrary commands. Service-mediated inspection and remote-source tools provide evidence without granting native network, shell or credential access. A role may name skills by git reference. The service clones them into its cache and gives the role's Claude turns only their skills, read-only, with Claude's tool that loads them. A repository's hooks, MCP servers, commands and agents never reach a session, and other agent binaries receive no skills. The service runs the checks itself before every review that reads code: a unit candidate's before its unit review, and every check on the rebased feature branch before the final read. Both run `dagger check` on a fresh export of the pinned commit and discard the copy afterward. No reviewer runs checks or chooses a command, path or environment; the checks a unit runs are chosen by the service. Reviewers judge the work against its task, acceptance and criteria; style, formatting and lint belong to the project's checks. The check client receives a temporary home and engine connectivity, without inherited provider, delivery or signing credentials; no agent receives an engine endpoint. Results enter the trace and inform the review without granting approval or delivery.

A mason in a Docker Sandbox may also be given Dagger, so it can run the project's checks and functions while it builds rather than relying on review. The owner configures the Dagger CLI release and a host engine, which may be the engine container the owner's own Dagger CLI provisioned. The sandbox keeps a template's CLI at that release or installs it, and it reaches the engine through a port allowed for that sandbox alone. The engine has no delivery credentials, and no other role, including the classifier that shares the mason's sandbox, receives it.

### 9.6 Jev judgments

Some decisions inside a turn are small, bounded judgments rather than generation: which of four classes a mason's final response falls in, whether a question's honest answer would change the sealed spec or plan, whether a proposed answer appears to contradict a ruling, which of a project's checks a unit's change can affect. An optional Jev boost asks those of Jev, TypeSafe's System One model, which takes textual state and typed questions and returns typed answers with probability distributions: a Choice selects one supplied option, a Score rates against supplied levels, and a Noul is the probability that a proposition holds. Larger generative models remain responsible for implementation, planning, explanations and review.

One global setting turns the boost on or off for every judgment; there is no per-judgment switch. It is off by default. The service reaches Jev through a TypeSafe-compatible API, OpenRouter by default, with a credential referenced through an environment variable that no session receives. Jev holds no tool, no version control and no delivery credential; a judgment sees only the state its question needs.

Every judgment has a supported fallback, which is the workflow as it runs without Jev. A judgment falls back when the boost is off, its credential is missing, the provider times out, is rate limited, unavailable or refuses the request, the response does not answer the questions in the shape they require, or the judgment's own acceptance, such as a confidence threshold, declines the answers. Thresholds are per judgment and tuned against a pinned model version; confidence is derived from the answer's distribution and is never treated as a verified probability of being correct. Requests are bounded by a short timeout and at most one retry of a transient failure. A rate limit, or repeated outages, cools every judgment down together, so an outage falls back at once rather than producing a retry storm.

Judgments run inside turns, or inside the service's check runs, never on the scheduling path, and no answer bypasses an outcome tool, reviewer judgment, service-owned version control or an owner gate. An answer is advice to the code or the role that asked; it is not a transition.

The mason classification asks, of a mason turn that ended cleanly without an outcome tool, which of four classes its final response falls in (a question asked in prose, a completion claim, giving up, or unclear) and, for each class but unclear, which sentence of the response supports it. The answers are used only when the class reaches its threshold, which is stricter for giving up because that contests the unit, and a sentence supports it. That sentence is the recorded evidence; an unclear class records its probability instead, never an invented explanation. Otherwise the classifier profile, then the code rules, classify the response as they do without Jev. A Jev class has the effect of any other: a completion claim still needs `done`, a prose question still needs `ask`, and the clean-turn bound and contested-unit rulings apply. The classification records what classified it and the judgment it rests on.

The question assessment asks, of each open question a chief-of-staff turn delivers, whether its honest answer would change the sealed spec or plan, including a unit's task or acceptance; which of the owner's rulings on the workstream and the project's notices answering it as it proposes or assumes would contradict; and whether it asks for a standing rule for the project rather than a decision about this feature. Its state is the question, the asker's unit from the sealed plan, the sealed criteria and, when they fit, the sealed spec and the plan's units; the rulings are the newest that fit. The service asks it when the turn runs, before the chief's session starts. Each answer that reaches its threshold becomes a signal at the end of the turn's prompt, naming the outcome it argues for: `route_amendment`, `escalate`, or a notice or charter proposal. The turn records the advice it added. A fallback, or answers below every threshold, leaves the prompt without signals. A signal never blocks or requires an outcome tool and never answers for the owner; the chief chooses from the record and says why when it chooses against a signal. The judgment is asked once for a question and its inputs, so a turn that delivers the question again reads back the same signals. Each judgment names its question, so its signals can be evaluated against the question's outcome and what the owner did later.

The check selection asks, for each link `dagger list checks --all` gives the candidate, the probability that the change can alter that check's result, from the changed files and, when it fits, the diff. A project with more links than one judgment can ask about is asked about its collections' items instead, the largest collapsed first. The links at or above its threshold run; a judgment that selects none, or falls back, runs every check.

The trace records each judgment, while the boost is on, under its workstream: the turn that asked, the task and its question version, the source record revisions its state was built from, the request, the configured and resolved model versions, the answers with their distributions, the outcome and any fallback reason, and usage. A judgment is identified by its cause, task, version and exact request, so after a restart the workflow reads back the decision it already used instead of asking again, and changed inputs are a new judgment. A judgment records its start before its request; a start without a result is an interrupted attempt, distinguishable from an accepted one, and a judgment interrupted twice falls back. Usage enters the ledger under the `jev` role and counts toward budgets; a cost the provider does not report is unknown, as for an agent. Status reports the boost as disabled, unconfigured, ready or degraded with its latest failure.

---

## 10. The service

### 10.1 One process

One long-running process holds the scheduler, the threads, the event bus, the tracker and the runtime state. It serves an HTTP API and the web interface on a unix socket for the local command line, optionally on a loopback TCP address for a browser on the same machine, and, through embedded Tailscale, on your tailnet for everything else. Tailnet membership is the security boundary: no in-app authentication, one trusted user. The command line, the web interface and the chief of staff's factory tools all call the same handlers.

Reads the web interface calls on first load and on every refresh, such as the project list, the workstream list and a workstream's feed, answer without waiting for a factory operation's effect: an agent turn, check run, rebase, merge or delivery claims its project's operation lock up front, but the service releases the project's write lock before running the effect itself, so a read or an owner action against any workstream of that project takes only the brief write lock and is never held up by the effect in progress, on that workstream or any other in the same project. A read reuses its last scan of the trace whenever nothing has changed since, rather than re-walking it. When a commit lands between the separate reads a response assembles together, the next attempt retries from scratch, so it always returns state from one generation and never a mix of old and new; every commit still takes the write lock briefly, so a read can wait that long, but never for an operation's effect. All of this runs inside the one process described above; there is no separate API process.

The service also keeps one in-process activity record of what it is doing: every reconcile-controller operation (a seal, build, land, rebase, drift or publish, a unit's checks, an agent turn), and startup or project recovery, reports its start, its current phase and, when measurable, its progress, and finishes the entry on success or leaves it recorded as failed. The record is a small, separately locked structure, not the trace: reporting to it and reading it never wait on the trace's write lock or on the operation lock a factory effect holds, so it stays visible and prompt even while the effect it describes is still running or stuck. A fresh start of the same operation, keyed by its own stable identity, supersedes whatever that key last reported, so a repeatedly failing operation never grows the record and a stale failure never outlives the operation that caused it. A failure also expires on its own after a bounded time if nothing retries it under the same key, so an operation keyed by the trace event that requested it rather than by a retry-stable identity (an abandoned workstream, or a check run a later run number never supersedes) cannot leave the record, or health, reporting a failure forever. Only kinds whose own reporting is expected to keep moving (Git and workspace operations, recovery) count toward the no-progress stall; an agent turn or a check run reports only its start and finish and may legitimately run for minutes, so elapsed time alone never marks one of those degraded.

Every core read (status, a workstream's status, config, runtime, the inbox, a conversation, a feed, trace views and the event stream's resync) is bound to answer within a short fixed latency, whatever a Git operation, a check run, an agent turn, reconciliation or recovery is doing at the time, because it never waits on anything but a short, validation-sized critical section of the trace or runtime lock. Of these, status alone keeps a fallback for the case where a fresh read still cannot finish inside that bound: it answers with the last snapshot it already had, marked with an `X-Osmia-Stale: true` response header, rather than block the caller further; the refresh it started keeps running underneath and the next request sees its result once it lands. A read that does assemble fresh state never mixes two generations of the trace or the runtime store: it retries from scratch, rather than returning a partial mix, when a commit lands between the separate reads it combines. A request that is cancelled or times out stops the read's own blocking work where that work can be cancelled, rather than continuing to hold anything on the caller's behalf.

Every owner-action write (hand-in, ratification, a shed action, an amendment decision, an answer, a move, delivery approval, abandon, pause, priority and archive) answers promptly with one of three outcomes: accepted, meaning the command is durably recorded before the response is sent; rejected, with the reason, when the request or the current authoritative state refuses it outright; or temporarily unavailable, naming the specific backend operation the activity record (above) shows is in the way, when a resource the write needs is held by an operation still in flight. A long-running accepted action, such as a drift rebase or a delivery, is a durable operation whose progress and outcome are reported back through the API and the event stream the same way any other operation is, not invented as a parallel mechanism. Before a write takes effect it is checked against current authoritative state and, where the client's request names the revision or version it was shown, against that revision: a stale view is refused with no effect, never silently applied against newer state and never treated as still current because a snapshot read once said so. A command carries or derives a stable identity, such as a hand-in key or a trace transition's expected version, so resubmitting it, including after a restart recovers an interrupted write, reaches the same durable result rather than applying twice; an accepted command's effect is reconciled from that durable record before anything is retried, never dropped silently.

### 10.2 The API

- Workstreams and their status. An event stream, which also announces a change to the backend activity record.
- The backend activity record: every operation in flight or failed, with its kind, the project and workstream it concerns when relevant, its phase, its progress when measurable, the time of its last progress and any failure.
- Health, read apart from the trace so it always answers promptly: whether the API is available, which it is whenever this response is sent at all, separately from whether the backend is ready, not yet ready (startup or project recovery is in progress) or degraded (an activity entry has failed or made no progress for a time), naming the operation when it is not simply ready.
- The conversation per workstream: send a message, list turns.
- The inbox: open questions across every workstream, answer one.
- Pause and resume at factory, project and workstream level. Priority order.
- Profile bindings: get, override, clear. Usage per provider.
- Configuration: what is loaded and its digest, reload, last error.
- Projects and repos: create a one-repo project, add a repo, list repos through configuration, edit the shared charter; remove repo registrations and extract per-repo knowledge. Workstreams: hand in, abandon and permanently archive.
- Project design: open, list, converse, read the brief and start a workgroup in one owner action. The same thread continues for status, escalation and amend/cancel impact decisions.
- Sessions: open, list and read; send a message, stop a turn; read changes, commit, check, push and end (section 6.8).
- Trace: walk a workstream's record.

### 10.3 Runtime state is not configuration

Profile overrides, pause states and priority live in `runtime.json` under the root, persisted so they survive a restart and untouched by a reload. Configuration says the defaults. The runtime says what you changed since. Clearing an override returns to the configuration.

### 10.4 Reload

Reload is explicit. It reads the top-level configuration and every project's `config.toml`, validates them whole, and applies them only if they pass. A bad file leaves the old configuration running and the error in the interface. Profiles, capacity, budgets, review settings, Hearsay and Jev settings and the project list apply live; a removed project drains and stops. Listen addresses, the tailnet identity and the root need a restart, and the interface says so. The charter and the knowledge base need no reload because bundles read them at turn time. The loaded digest is shown, so "did the reload take" is checkable.

### 10.5 Notifications

A new question, a contested unit, a delivery or a budget pause can go out through a configured webhook so the inbox reaches you without the page open. One key, optional.

### 10.6 Service tests

`internal/service`'s own `TestMain` applies the shuffle default to that package's test binary before running it, on top of the Go check's global flags in `dagger.toml`, which stay the same for every package. Unless the caller passes `-test.shuffle` explicitly, it turns shuffling on, so the binary prints the seed it drew and a failing order-dependent run replays with `-test.shuffle=<seed>`. It leaves `-test.timeout` unchanged, so the Go check's 30-minute timeout applies to this package as it does to the others. The service's `checks_timeout`, which defaults to 15 minutes, bounds the whole check run. Independent top-level tests in the package run in parallel. Its browser tests start one headless Chromium process the first time a test needs it, shared for the rest of the package run; each test opens its own tab on that one browser, rather than a separate browser context, and closes it through `t.Cleanup` before its fixture tears down.

Writing a new test in this package follows the conventions its existing tests now hold to:

- Wait for the state an assertion checks, with a bounded deadline, instead of sleeping a fixed duration or counting a fixed number of ticks; drive reconciliation passes explicitly when the assertion depends on how many have run.
- Prove a negative only after observing the pass, turn or write that would have caused it, for example by blocking a reconciliation pass on a channel and confirming it ran before asserting that a second service lifetime never raced it.
- Release every browser context, fake server, event stream and service through `t.Cleanup`, never a bare `defer`: cleanups run in last-registered-first-run order, so a fixture created before the browser tab opens tears down after the tab closes, not before.
- Build git fixtures in the test's own temporary directory without hardlinked objects, so a transient hardlink from a local clone never trips the trace's refusal of hardlink aliases, which stays unchanged.
- Keep a test helper shared across test files safe for concurrent callers, since parallel tests can call it at the same time.
- Call `t.Parallel()` first in a new top-level test, and stay serial only when it depends on process-wide state, such as an environment variable set with `t.Setenv`, a package-level variable or hook, the working directory or a fixed port, named in a one-line comment on the test.
- Never retry a failed assertion or rerun a test until it passes; a flake is a bug to find, not a wait to lengthen.

---

## 11. Interfaces

### 11.1 Web

Built for a phone as much as a laptop. Embedded in the binary, one page, fed by the event stream. The project pane is the entry point: each project lists its repos, workgroups and member workstreams, plus design and exploratory sessions outside workgroups. It offers project creation, adding a repo, design chat and the document hand-in fast path. The Osmia heading returns to the project pane.

Workstream labels are planning, ready, building, paused, blocked, PR open, delivered (merged) and cancelled. Blocked work names the service's edge or merge wait without adding an inbox entry. An open pull request stays PR open until a recorded merge. A workgroup graph appears only for several members or recorded edges; a single member adds no graph or approval. Selecting a member opens its existing feed, conversation, units and inbox decisions, with a direct workstream link that survives reload. A design-session link opened after start follows the adopted conversation into a member view. Shared workgroup questions appear once in the project overview and are answerable from either member's view using their original inbox identity. Pause and priority controls retain each repo's scope across projects.

- **Active work.** Every workstream with its goal and its units counted by state, and the capacity view: slots used per role kind, who is waiting, and every pause in force with its reason.
- **Inbox.** Every open question, contested unit and ratification packet across workstreams and projects, each with the chief of staff's rephrasing, the options and its recommendation, answered inline, and every publication that keeps failing, with its last failure.
- **Feed.** One per workstream, everything that happened to it in the order it happened: the thread with the chief of staff and the actions it took on your behalf, each status it wrote with its attention and note, each session that ran with its role, profile and outcome, and each change of the workstream's or a unit's state.
- **Controls.** Pause and resume at every level, priority order, the profile switcher with usage per provider beside it, and a reload button that lights when the file on disk differs from what is loaded.
- **Backend activity.** A header button, marked whenever an operation is in flight and distinctly when one is blocked or has failed, opens a popover listing every entry in the activity record: its kind, the workstream or project it concerns, its phase and progress, and any failure. An entry whose last progress is old enough, or that carries a failure, is shown as blocked whether or not a fresh read has arrived since.

Submitting a feed action, such as a ruling on a contested unit or ratifying a proposal, removes its card from the feed at once, before the server has responded. The card stays off the feed through any refresh that still lists the action as outstanding, since the backend may not have processed it yet, and comes back as actionable if the backend is still listing it 120 seconds after it acknowledged the submission, so it can never stay hidden for good. A failed submission (a non-success response or a network error) brings the card back at once with the error shown and the owner's entered values kept. Dismissing one card never hides or changes another. Every form that submits an owner action, including new project, archive and the feed's own action forms, clears to its default values once its submission succeeds; a failed submission leaves the form as the owner left it, with the error shown.

### 11.2 Command line

The following is the full design; see the [command line](cli.md) for supported
commands. The service runs in the foreground or with `serve --detach`, which
returns after the service acknowledges readiness; that launcher's own
terminal output, the "ready" line and a readiness failure, stays
human-readable text. The service process itself writes only structured,
JSON-lines records to its own standard output and standard error, detached
output appended to the private `service.log` under the root; see the
[service log](running.md#service-log). A failure that stops a running
service is one such record, naming the project whose work failed, with
credentials redacted. `osmia stop` requests shutdown over
the local Unix socket.

```
osmia serve [--detach]               the service, foreground or detached
osmia stop                           stop the local service
osmia doctor                         check what the service needs, and how to fix it
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia project add-repo <project-id> --upstream owner/other --clone ~/src/other
osmia project repos <project-id>      list the project's repos
osmia design open <project-id>       discuss before handing off
osmia design start <design-id>       hand the brief to the architect once
osmia handin <project-id> ./design.md a workgroup from a document, an issue URL, or stdin
osmia status [workstream]            the status, or every workstream's goal and attention
osmia inbox                          open questions
osmia answer <n> "..."               a ruling
osmia move <workstream> <unit> <state> "..."   a unit to another state, with a note
osmia send <workstream> "..."        a message to the chief of staff
osmia pause|resume [all|<project>|<workstream>]
osmia archive <workstream>      permanently delete a terminal workstream’s trace; retain its title
osmia profiles [set <role> <profile>|clear <role>]
osmia reload
osmia trace <workstream> [unit|criterion|commit]
osmia ratify <workstream>            after reading the packet
osmia merged <workstream>            an assembled branch you merged into upstream yourself, recorded delivered
osmia session open <repo-id> --profile default   a session for exploratory work; chat happens on the web page or the API
osmia session commit <session> "..."          commit every worktree change onto the session's branch
osmia session check <session>                 check the session's head commit
osmia session push <session>                  push the session's branch to the project's remote
osmia session end <session>                   end the session, releasing its worktree
osmia rebase <workstream>            a drift rebase of building, assembled or delivered PR work now
osmia amendment <workstream> <n> [approve|reject|round|overrule]
```

Every command is an API call except `serve` and `doctor`. Doctor checks what the service needs before one runs, so it reads the root and configuration the way `serve` does; no other command reads state files directly.

---

## 12. Hearsay

Hearsay is an optional external context and memory service. It is not an orchestrator, has no work-unit model, delivers no agent messages and synthesises nothing at read time. The state machine, the plan, the mailbox and the documents of record stay in Osmia. Hearsay ingests source records (L0), distils them into memory (L1), and serves scoped bundles of decisions and source pointers. The integration is optional; the complete local workflow must remain usable without it.

### 12.1 What lives there

- **Decisions.** Every ruling you give is a ratified stance under your principal. Every committee contribution is a stance on the unit's topic; a send-back is a stance change. A mason's learnings at landing are asserted as proposals and arrive as inferred stances. You ratify from the interface.
- **The entity map.** The knowledge base's map of subsystems, aliases, path patterns and owners is Hearsay's code entity namespace, seeded when a repo is added. Footprints name these entities.
- **Anchors.** The charter, the knowledge base prose and the workstream's spec are pinned anchors in their scopes. A roadmap or design document you want every plan judged against is pinned the same way; there is no standing document for direction.
- **The outside.** For a project like dagger, the scope also ingests the repository's GitHub activity and its Discord channels, so a mason on the engine gets what the maintainers said about the engine, not only what your factory learned.

The role notes shrink to what is private to a role's craft. The chief of staff's memory of how you decide is your stance history.

### 12.2 The connector

Osmia is a Hearsay source. A connector plugin in Hearsay's process reads the project trace repositories under the root, events, message logs, documents and their revisions, and emits L0 events. Recording local state never depends on a Hearsay write. Optional immediate assertions are a separate fast path. Ids are derived from stable Osmia record and revision identifiers, and ingest is idempotent, so everything is replayable. Immediate assertions and later connector ingestion must identify the same ruling rather than create two decisions.

| Osmia record | L0 kind |
|---|---|
| session start and end, each turn, each tool call | `agent_session`, `agent_turn`, `tool_call` |
| shed round, question thread | `thread` with a `message` per contribution |
| charter, spec, plan, knowledge-base prose | `document`, one revision per change |
| unit merged, feature delivered | `commit`, then `pull_request` through the GitHub connector |
| state transitions, rulings, seals | `osmia.transition`, `osmia.ruling` extension kinds |

### 12.3 Bundles

The service assembles each turn's task context and a project briefing with applicable charter rules, decisions and owner-policy notices. KB prose, the full entity map and informational notice history remain available through bounded project-context retrieval. Optional scoped Hearsay pointers accompany the briefing, with detailed evidence available through its tools. Roles map to agent classes: architect, committee and mason are workers, the chief of staff is an orchestrator over several scopes with a watch, the foreman and librarian are observers, and you are the steward. The effective principal is always the agent's grants intersected with yours.

### 12.4 Watching

The chief of staff watches the project scope. A Discord thread that decides something about a subsystem an in-flight unit touches, or an upstream change that supersedes a stance the plan relied on, becomes an amendment request, notice or question. A maintainer's review on a delivered pull request becomes a notice or question and may lead to a new owner-requested workstream, as described in section 5.5. A watch never grants permission to change a ratified spec or restart terminal work.

### 12.5 Without Hearsay

Osmia runs without it. The null provider uses the charter, knowledge-base prose, local entity map, spec, plan, questions, notices, trace decisions and private role notes. Footprint checks and scheduling still work. File-based context is a supported operating mode, not an error; status reports it explicitly. If Hearsay is configured but unavailable, status reports degraded memory and the same local path continues to work.

The connector is the durable ruling path: a ruling is written locally and then ingested under the owner identity, so an outage loses nothing. Optional immediate assertions must reconcile to the connector identity before they can be enabled. Historical traces can be ingested when Hearsay becomes available. File-based context is the supported provider. Hearsay integration requires anchors, entity resolution, stance history, assert, agent classes, watch and replayable connector ingestion. Jujutsu and multi-project support must work without Hearsay.

---

## 13. Configuration

Supported settings, defaults and stable directory identifiers are documented
in [configuration](configuration.md). The examples below describe the full design.

### 13.1 User configuration

```toml
# ~/.config/osmia/config.toml
version = 1
workspaces = "auto"                  # new workstreams: jujutsu when jj is supported, else git

[listen]
socket = "~/.local/share/osmia/osmia.sock"
web = "127.0.0.1:8484"               # loopback host:port only; empty disables
tailnet = "osmia"                    # hostname on the tailnet; empty disables

[capacity]
masons = 4
reviewers = 2
committee = 3
per_workstream = 2

[budget]
per_session = "5.00"
per_unit = "40.00"                   # a signal, files an amendment
per_day = "150.00"                   # a pause

[hearsay]
# Optional; omit these settings for file-based context.
# url = "http://hearsay.local:8080"
# principal = "kyle"
# token_env = "HEARSAY_OWNER_TOKEN"
# Configure hearsay.agents.worker, orchestrator and observer with id and token_env.

[jev]
enabled = false                      # one switch for every Jev judgment
# url = "https://openrouter.ai/api"
# model = "~typesafe/jev-latest"
# api_key_env = "OPENROUTER_API_KEY"

[notify]
webhook = "https://ntfy.sh/..."

[profiles.claude]
agent = "claude"
model = "claude-fable-5-1"
effort = "high"
fallback = "codex-fast"

[profiles.codex-fast]
agent = "codex"
model = "gpt-5.5"
effort = "medium"

[roles.mason]
profile = "claude"
sandbox = "container"
skills = ["https://github.com/acme/skills#skills/tdd"]
[roles.committee]
profile = "claude"
[roles.chief_of_staff]
profile = "claude"
events_profile = "codex-fast"        # optional: the profile of event turns
[roles.architect]
profile = "claude"
[roles.foreman]
profile = "codex-fast"
[roles.librarian]
profile = "codex-fast"

[shed]
max_rounds = 3
max_bounces = 3

[loop]
max_sessions = 30                    # sessions without progress before a pause; 0 disables

[committee]
perspectives = ["correctness", "integration", "scope"]
profiles = ["claude", "codex-fast"]   # assigned in turn by member number
```

### 13.2 Repo configuration and project membership

```toml
# ~/.local/share/osmia/projects/<repo-id>/config.toml
version = 1
name = "dagger"
# Additional repos set group_id = "p_<default-repo-id>"; omit for the default repo.
upstream = "dagger/dagger"
fork = "kpenfound/dagger"             # optional; omit to push feature branches to upstream
clone = "~/github.com/dagger/dagger"
base_branch = "main"
landing = "commit-per-unit"          # or "squash"
upstream_rebase = "6h"
checks_timeout = "15m"               # one run of a candidate's checks
hearsay_scope = "dagger"

[capacity]
per_workstream = 3                   # overrides the global default
```

`active_projects` lists active repo IDs. The default repo's ID is also the project ID; additional repo configurations name it with `group_id`. The project charter is authoritative in the default repo's trace. Repository settings, KB and entities remain per repo. The design agent uses the `chief_of_staff` role's configured profile and sandbox; no separate design-role binding is required. Edges come from the ratified graph, with no release-signal, tag or package configuration.

Every key has a default, validation, a documented meaning and a test. The configuration carries a version, adding keys is not a breaking change, and renaming or removing one is a migration that rewrites the file text so comments survive.

---

## 14. Bootstrap

### 14.1 Adding a project

`osmia project add` records the upstream, optional fork and clone, creates the project trace repository under the root with an empty charter template, and runs the librarian's extraction pass: an inventory of subsystems with their tests and conventions as knowledge-base prose, and the local entity map that can later seed Hearsay. You then write the project charter. `osmia project add-repo <project-id>` registers another repo using its own clone and KB while sharing that charter; `osmia project repos <project-id>` lists them. Projects can be added while other projects run. The command refuses to hand in work to a project whose charter is empty, because debate has nothing to cite.

### 14.2 Handing in a workstream

`osmia handin` takes exactly one file, issue URL or stdin input, copies it immutably under `handed/`, creates a one-workstream workgroup in the named repo and moves it to `handed`. That input is the brief. The API and project pane use the same hand-in path; no design session is required.

With one active repo, the architect drafts the spec and plan directly. With two or more active repos, ordinary file, issue and stdin hand-ins use the same affected-repo selection and allocation path as a design start. The architect selects repo IDs from the project context. Go refuses an empty, duplicate or outside-project selection before allocation, then records a valid selection durably before allocating exactly one member per selected repo. If the selection turn finishes without a valid selection, the start is invalid and no members are allocated.

Selecting only the named repo keeps the original workstream. Selecting one other repo allocates one member there; both single selections retain ordinary single-member ratification. Several selected repos get the project spec, contracts, graph, slices and plans, committee debate and one all-member ratification through the existing action. Each member receives the immutable handed input. The original chief-of-staff conversation becomes the one shared thread; members have no independent chiefs. Selection, allocation, join, conversation adoption and sealing reconcile durably after interruption or restart without duplicate members or replacement groups.

`--skip-debate` applies to every member. An optional same-repo `--base` requires that repo in the selection and carries only to its member. There is no additional owner selection or approval step. From there the lifecycle in section 5 runs, and you hear from the chief of staff when the packet is ready. Every member retains its own final-review acceptance, delivery approval and owner signing.

### 14.3 First run

Git worktrees and Jujutsu share the workspace interface. The service supports multiple projects and workstreams, with a web interface on the tailnet and file-based context. Cross-workstream bases and the optional Hearsay connector, bundles and watches are separate capabilities; neither is required for independent workstreams to run and deliver features.

---

## 15. Security

- Tailnet membership is the boundary for the web interface and the API. The unix socket is the boundary locally, and the loopback interface is the boundary for the optional web listener, which binds no other address and refuses requests whose `Host` is not loopback or whose writes are not JSON. The tailnet listener refuses requests whose `Host` is neither an IP address nor one of the node's names, and writes that are not JSON. There is no in-app authentication and no multi-user isolation.
- Sessions never hold a version control tool, so a session cannot push, force-push or rewrite history.
- GitHub credentials reach the foreman's push and pull request calls as environment variables, never as arguments, and never reach a session.
- The service alone uses the owner's configured signing key or signing agent for owner-approved delivery commits. Private keys and signing credentials never enter runtime-agent sandboxes, context bundles or the trace.
- No factory state is committed to the target project. Its repository sees the feature's code and tests through the branch and pull request.
- Every bundle served by Hearsay is an audit event: who asked, on whose behalf, what was filtered.

---

## 16. Defaults and deferred choices

1. Per-role or per-workstream override of profiles. The design has per role, global. Per workstream would let one feature run on a cheaper provider, and complicates the switcher.
2. Several workstreams in one project when both touch the same subsystem. The default is an advisory, with owner pause and priority controls. Automatic refusal across workstreams is deferred. Within a workstream overlapping or unresolved footprints are serialized.
3. Committee membership. Per workstream in the design, with project memory arriving through Hearsay. Per project would accumulate arguments in the thread instead. Revisit when the first workstream has run.
4. What counts as acceptance. Acceptance states observable conditions; evidence demonstrates those conditions. Reviewers may use equivalent or stronger evidence and applicable historical evidence with an explanation, but cannot weaken approved outcomes. Shared quality obligations come from the charter and service checks. The charter determines what a project requires, and the reviewer records how it verified the acceptance. There is no mechanical coverage score that substitutes for that judgement.
5. Unit-scoped parallelism. A unit is one mason. Whether a wide unit may fan out into several masons with a squash step, as an assembler, is deferred until a real feature needs it.
6. The spec format. Markdown with a numbered list of criteria, and nothing parsed from it beyond the numbers. The footprint is declared in the plan by the architect and used to schedule units, never derived from the spec.
7. **Integration gate edge remains unresolved.** “Neither PR is marked ready until a combined test of both branches passes” needs an owner ruling on the combined-test mechanism. Integration-gate edges are not implemented and the architect must not propose them until that mechanism is accepted.

---

## 17. Component boundaries

**`github.com/kpenfound/busybees/core`** is the reusable Go dependency for agent execution and backends, session sandbox primitives, review, retry classification, ledger and budget primitives, capacity, event wakeups, the MCP host and the workspace interface. Osmia consumes a pinned module version. Reusable extensions belong in that dependency; Osmia-specific workflow policy remains here. A sibling checkout must not be required to build or test Osmia.

**Osmia owns the workflow.** Its Go controllers own feature and unit state, the plan, seals, footprints, scheduling policy, questions and fixed message routes, role notes, prompts, ratification and delivery gates. Durable threads wrap the core runner with an owned log and turn queue. The core's wakeup bus does not replace the durable outbox, and its workspace or VCS-access flags do not by themselves enforce the isolation required in sections 7 and 15. Adapters must verify those contracts explicitly.

**Hearsay** is optional memory, reached through a provider boundary. Its connector executes in Hearsay's process and reads Osmia's trace. Bundles and watches augment the local record; Hearsay never becomes the workflow tracker, dispatcher or owner of the specification. Section 12 defines the integration and fallback contract.

**Jev** is an optional judgment provider, reached through a TypeSafe-compatible API. Osmia owns the questions, their interpretation, the thresholds and the fallbacks; Jev answers typed questions and never owns a decision, a transition or a gate. Section 9.6 defines the contract.

---

## 18. Feature guarantees

The workflow builds on durable local state, planning and owner ratification before implementation and delivery. Concurrency, alternate workspace backends and multi-project operation preserve those guarantees. The web interface exposes the same owner decisions as the local API. Optional memory integration must preserve a complete file-based workflow.

| Feature | Scope | Required behavior |
|---|---|---|
| Durable local service | Core integration; personal service, configuration and local API; per-repo, workgroup and workstream trace repositories; durable records and outbox; isolated durable threads. | A role receives successive turns and survives a restart with an inspectable record. |
| Planning and ratification | Project onboarding and local knowledge; chief of staff and inbox; feature intake and planning; debate and ratification. | An owner can hand in a design and ratify a buildable plan using local context. |
| Feature delivery | Sequential implementation; exact-candidate review; serial landing and librarian updates; final review and delivery; trace navigation. | A small feature reaches an owner-approved pull request with a complete trace. |
| Concurrency and recovery | Parallel workstreams, each in its own trace repository, and shared capacity; amendments and standing rulings; concurrent landing and drift; trace migration and archive cleanup; budgets, profiles, pause, reload and recovery. | Several workstreams on one project progress through interruptions, questions and conflicts, and through another workstream's migration or archive cleanup, without lost work, duplicate landings or waiting on another workstream's storage. |
| Phone operation and notifications | Embedded web interface; tailnet access; notifications and the product's installation, release and first-run experience. | The full lifecycle and every owner decision are available from a phone, with local context. |
| Jujutsu workspaces | Jujutsu provider; rebase and interruption recovery. | The complete lifecycle works with either workspace backend. |
| Projects and workgroups | Multi-repo projects, shared charter, read-only design intake, briefs, shared chief, project specs and protected slices; merge-only edges, amendments, cancel and two-level retention. | One owner start and ratification cover cross-repo work; each PR has its own delivery approval, and shared context lasts until all members resolve. |
| Project memory through Hearsay | Replayable connector; scoped memory bundles; ruling reconciliation and watches. | Memory enriches the workflow while outages lose no decisions and local operation remains supported. |

---

## Appendix A. Glossary card

```
project      one charter and one or more repos
repo         upstream, optional fork, clone, settings, KB and entities
workgroup    one feature request; member workstreams and one shared chief
brief        design handoff or handed document; wins over its transcript
project spec workgroup design, contracts and numbered criteria
slice        one repo's share of the spec; contract sections read-only
edge         contract, hard block or merge order; concrete recorded resolution
workstream   one repo, units, one branch, one PR and its own trace
charter      your rules as a contributor; one per project; grows by ratified rulings
spec         what must be true when the feature is done; numbered criteria; ratified by you; amended in the shed
plan         revisable DAG of outcomes, acceptance, boundaries, guidance, dependencies and entities
unit         one task; one mason, one reviewer checking its acceptance, one commit on the feature branch
footprint    code entities a unit expects to touch; overlap is entanglement; schedules, never binds
acceptance   observable conditions; reviewers judge applicable evidence against them
seal         (upstream main commit, spec hash); moves on rebase
the shed     debate; charter veto, size and acceptance tests, fit; bounded engineering debate; owner intent ratification
question     ask -> chief of staff -> answer | escalate | amend | charter | rephrase
trace        repo knowledge, shared workgroup decisions and member history; independent repositories under the root
profile      agent, model, effort, fallback; bound per role; switchable while running

feature      handed -> sketched -> in-shed -> ratified -> building -> assembled -> delivered
unit         planned -> ready -> implementing -> checking -> reviewing -> approved -> merged
             (+ waiting, contested)
roles        owner, chief of staff, architect, committee, mason, foreman, librarian
```
