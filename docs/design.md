# Osmia design doc

A personal software factory for long-running feature work on repositories you own or contribute to. It is configured at the user level, keeps its factory state outside the repository it works on, is driven by a person through one aide, and shares one pool of agent capacity across several workstreams on several projects.

Status: design v0.2, 2026-09-15. This document is the source of truth for feature proposals and implementation; an issue does not override the design. Osmia uses `github.com/kpenfound/busybees/core` as a Go dependency and defines an optional Hearsay memory integration. Section 17 defines those boundaries and section 18 summarizes the feature guarantees.

---

## 1. Purpose

You want to land a large feature in a project like dagger. You are a contributor, so you cannot install a factory in the repository, label its issues, or push to its main branch. You have a design in your head or in a document, you want agents to plan it, argue about the plan, build it in pieces, review each piece, and hand you one feature branch on your repository or fork with a pull request you are happy to put your name on. You want to be asked when something needs your judgement, and otherwise left alone. You want the same machine working on three dagger features and a feature for another project at the same time, from one process, from your phone.

Osmia is that machine. It does not decide what to build. You hand it a feature. It does not run on the repository's issue tracker. It runs on a directory under your home. It does not push to anyone's main. It delivers a branch and a pull request, and stops.

### 1.1 Why a mason bee

Mason bees, the genus *Osmia*, have no hive. Each one moves into whatever cavity already exists, a reed, a hole in a wall, a tube someone left out, and gets to work. Orchards buy them by the tube and set them out where the pollination is needed, and a handful of them do the work of a hive. They are gentle and rarely sting.

That is the tool. It moves into an existing repository without changing it, is deployed where the work is, and is meant to be a good guest.

### 1.2 The shape of the design

Four choices give the design its shape.

1. A feature starts as a written specification: what must be true when it is done, drafted from what you hand in, debated by adversarial reviewers with you in the room, and ratified by you. It lives beside the factory's state, never in the repository. The plan that breaks it into units is debated and ratified with it: each unit is a task for one builder with the acceptance its reviewer will check, settled before anything is built.
2. Units are built in parallel where the plan allows and land one at a time on one feature branch, each reviewed against its task and acceptance before it lands. Builders and reviewers work the plan as ratified; they do not reopen how the work is cut. When a reviewer has read the whole branch against the specification and you accept that report, the feature is delivered as a pull request from your repository or fork.
3. Every agent talks upward to one role, the chief of staff, which answers what it can from the record and asks you the rest. You talk to the chief of staff and to nothing else.
4. The transitions are code. No model runs on the scheduling path. Agents are durable threads that receive turns and never hold a version control tool.

---

## 2. Vocabulary

### 2.1 Concepts

| Term | Meaning |
|---|---|
| project | A repository you contribute to, with your optional fork, your clone, your rules for it and the factory's knowledge of it. One directory under the Osmia root. |
| workstream | One feature on one project, from handing in to delivery. Its own branch, its own agents, its own record. |
| charter | Your rules as a contributor to a project. One per project, human-owned, grows over time. |
| knowledge base | What the factory knows about a project's architecture that the repository's own documents do not say. Prose by subsystem, a local entity map and trace decisions, enriched by Hearsay when enabled. |
| feature spec (spec) | What must be true when the feature is done: intended behaviour, what it must not do, and acceptance criteria as a numbered list so they can be cited. Drafted by the architect, ratified by you. Not a description of the codebase, and never enforced mechanically. |
| criterion | One numbered item in a spec's acceptance criteria. Cited in debate and review; a handle, not a key. |
| acceptance | What a unit's reviewer checks against its work to accept it: behaviour that must hold, tests added in the project's own conventions or kept passing, checks to run. Written in the plan and settled in the shed. |
| plan | The directed graph of units that realises the spec. Data, produced by the shed, changed only by amendment. |
| unit | One piece of the plan: a task, its acceptance, the criteria it serves, the units it depends on, the code it will touch. Built by one mason, verified against its acceptance by one reviewer, landed as one commit on the feature branch. |
| footprint | The code entities a unit expects to touch, declared in the plan. Overlap between footprints is entanglement, so footprints decide which units may be built at once. |
| seal | The pair of the upstream main commit and the spec's hash recorded at ratification. Moves when the feature branch rebases. |
| bundle | The context a session gets at the start of a turn: the documents it needs, the unit, its questions and answers, local decisions and notices, and an optional Hearsay bundle for its scope. |
| the shed | Debate. Where a spec and plan go before ratification, and where amendments go. Named for bikeshedding, because cheap debate that repeats without social cost is the centre of the machine rather than its friction. Units are defined there and nowhere else. |
| amendment | A change to a ratified spec or plan, routed by the chief of staff from a question or filed after upstream moves, debated in a short shed round, decided by you. |
| question | Anything a role cannot answer from its bundle. Goes to the chief of staff, which answers or asks you. |
| ruling | Your answer to a question. Recorded locally, rephrased for the asker, and also asserted to Hearsay when enabled. |
| trace | The complete record of a workstream: documents and their revisions, debate, questions, every agent turn, every transition, every cost. Files in a git repository under the Osmia root. |
| profile | A named agent configuration: which agent binary, which model, effort, fallback. Roles bind to profiles, and the binding can change while the factory runs. |
| capacity | The number of sessions of each role kind that may run at once, across every workstream and project. |
| the service | The one long-running Osmia process. Everything else, the web interface, the command line, the agents' tools, is a client of it. |

### 2.2 Roles

Roles take names from the trade. All but the owner are agents.

| Role | Job |
|---|---|
| owner | You. Hands in features, sits in the shed, ratifies specs, answers questions, edits the charter, takes delivery. Talks only to the chief of staff. |
| chief of staff | The one role facing the owner. Your assistant for the workstream. Answers every other role's questions from the record and Hearsay, rules on contested units on your behalf, escalates what it cannot resolve or is unsure of, rephrases both ways, presents ratification, keeps the status, drafts the pull request description. Never implements, reviews or dispatches. |
| architect | Drafts the spec and plan from what the owner handed in, answers the committee in the shed, redrafts on amendment. |
| committee | Adversarial reviewers. In the shed they argue against the spec and plan, citing the spec, the charter and the knowledge base. On a unit one of them is the reviewer, verifying the unit's work against its task and acceptance until the unit lands. At the end one reads the whole branch against the spec and the charter. One role, three prompts. |
| mason | Does one unit's task until its acceptance holds. May ask questions. |
| foreman | Runs the landing. Merges approved units onto the feature branch, rebases units still in flight, keeps the branch current with upstream, resolves conflicts against the sealed spec, flags entanglement, pushes the branch and opens the pull request. Mostly code, with a session only for conflict resolution. |
| librarian | Maintains the knowledge base. Extracts it when a project is added, folds in what masons learned when units land. |

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
9. **Gates are transitions, not messages.** Agents message each other freely through fixed routes. The owner gates ratification, amendments and delivery. Contested units are gated too: the chief of staff rules on them for the owner and raises the ones it cannot resolve.

---

## 4. Documents

Every project has two standing documents, and every workstream has two of its own. All are files under the Osmia root, versioned by a dedicated trace git repository for that project, and none is ever committed to the target repository. The trace repository and the target clone are separate repositories.

### 4.1 Charter

Your rules as a contributor to this project, in a page of numbered rules, so the committee can cite one: the scope you will and will not touch, dependency policy, how a change must be tested, what a pull request to this project must look like. It cites the repository's own contributor documents as binding, so the committee can veto against the project's contributing guide without you restating it.

You edit it directly. The chief of staff proposes an amendment when a ruling you gave is really a standing rule, and you ratify or decline. Each entry records the ruling that caused it. A charter change becomes a notice in every in-flight bundle on the project.

Keep factory settings out of it. Budgets, capacity and models are configuration. If they leak into the charter, agents start debating whether "two masons" is a principle.

### 4.2 Knowledge base

What a new contributor learns that the repository's documents do not say. It has three parts.

- **Prose by subsystem.** `kb/<subsystem>.md`: how the subsystem is built, how to run the tests that matter, what breaks when you touch what, and the decisions behind it. These files are read directly into local bundles and become pinned anchors when Hearsay is enabled. Hearsay caps anchors at four per scope, which is a useful pressure to keep them short.
- **The entity map.** The map from how people talk about the code to where it is: entities with stable local identifiers, aliases, path patterns, owners and part-of edges, seeded from CODEOWNERS and repository structure. Its local record is `kb/entities.json`; when Hearsay is enabled, these seeds are mapped into its code entity namespace. Footprints name these entities and remain resolvable without Hearsay.
- **Decisions.** Constraints and choices about each subsystem, recorded as rulings and decisions in the trace with provenance. When Hearsay is enabled, it also represents them as stances. Local bundles include the relevant decisions without requiring an external service.

The repository's `CLAUDE.md`, `AGENTS.md` and `CONTRIBUTING.md` are inputs the knowledge base must not repeat. Everyone reads it: the architect plans against the real architecture, the committee cites it, the mason's bundle carries the sections its footprint names.

It is written by the librarian: an extraction pass over the clone when a project is added, and a pass each time a workstream goes quiet, with none of its units started and unmerged, as at assembly, or once it is delivered or abandoned. That pass folds in what every mason whose unit landed since the last one reported it learned. Refreshes are serial, so several workstreams on one project never race on them. A question the chief of staff had to answer from the code rather than the knowledge base is a gap, and it files the entry.

### 4.3 Feature spec

What must be true when the feature is done: the intended behaviour, what it must not do, and acceptance criteria as a numbered list. The architect drafts it from what you handed in, in the vocabulary the knowledge base gives the project. You edit it freely before ratification. After ratification it changes only by amendment.

This is not a spec in the sense of a description of the codebase's behaviour that the code is held to. The repository does not carry it, nobody upstream maintains it, and it goes stale the day the feature is delivered. It is the statement of intent that everything downstream is argued against: the plan turns it into units with acceptance a reviewer can check, the mason builds its unit against it, and the final read reports on every criterion. The flow is spec, then plan, then implementation, and the order is enforced by the plan being ratified before a mason starts. What is not enforced is coverage as arithmetic. Whether a criterion has been shown to hold is the final reviewer's judgement, recorded with its evidence, and yours at delivery.

### 4.4 Plan

The directed graph of units. For each unit: a task, written as a ticket for one developer; its acceptance, what the reviewer will check against the work, including the tests it adds or must keep passing; the spec criteria it serves; the units it must wait for; and the code entities it will touch. Every criterion is served by at least one unit. The shed produces the plan with the spec, and you ratify both together. Writing the acceptance before the build is the point: a unit nobody can say how to accept is a unit nobody understands yet, and the shed catches that.

The shed is where units are argued over. Once the plan is ratified, a mason does its unit's task and a reviewer checks the unit's acceptance against the work, as a developer and a reviewer would with a ticket. Neither reopens how the work is cut. When the task itself looks wrong, they ask the chief of staff, which routes the question to an amendment when the honest answer changes the plan. Units whose footprints are disjoint and whose dependencies are met may run in parallel. How finely the feature is cut is the factory's concern, not yours, as long as it all lands on one branch.

### 4.5 Where they live

```
~/.config/osmia/config.toml      profiles, capacity, budget, Hearsay, listen addresses
<root>/                          default ~/.local/share/osmia
  runtime.json                   profile overrides, pause states, workstream priority
  skills/                        clones of the skills roles name, and their generated plugins
  projects/<project-id>/         one git repository per project
    config.toml                  upstream, optional fork, clone path, landing style, per-project caps
    charter.md
    kb/<subsystem>.md
    kb/entities.json             local footprint entities and path mappings
    notes/<role>.md              a role's private craft memory
    workstreams/<id>/
      handed/                    what you gave, never modified
      spec.md                    every revision in the repository's history
      plan.json
      shed/round-<n>/            one file per committee member, the architect's reply, your rulings
      amendments/<n>/            request, draft spec and plan, affected set, its round, the decision
      questions/<n>/             as asked, the decision, what you were sent, what you said, what went back
      units/<u>/                 bundle, checks-<n>.json, review/, landing.json
      agents/<id>/log.jsonl      every turn's request and final response, with provenance
      events.jsonl               every transition, with who and why
      ledger.jsonl               cost per session
```

The defaults follow the XDG base directories: the top-level configuration is `osmia/config.toml` under `$XDG_CONFIG_HOME`, and the root is `osmia` under `$XDG_DATA_HOME`, falling back to `~/.config` and `~/.local/share`. Everything else Osmia keeps is in the root. An explicit `--root` is self-contained and holds its own `config.toml`.

The project repository's history is the decision trail. `osmia trace` walks it in either direction: criterion to unit to turns to review to commit, or commit back to the ruling that shaped it.

State updates and their outbox entries form one recoverable commit: after a restart both are visible or neither is. Appends alone are not a transaction. Durable operation identifiers connect an intended side effect to its result, so recovery can inspect a workspace, branch or existing pull request before retrying. Each consecutive failed attempt of an operation doubles the wait before the next, up to five minutes, so a persistent failure does not become a retry storm. Revisions and records remain available after delivery or abandonment; removing a project from active configuration does not delete its trace or the owner's clone.

---

## 5. Workstream lifecycle

### 5.1 Feature states

```
handed -> sketched -> in-shed -> ratified -> building -> assembled -> delivered

amendments revise documents in building or assembled; the owner decides

terminal: delivered, abandoned
```

| State | Meaning | Handled by |
|---|---|---|
| handed | You gave the factory a document, a design, an issue link, or a paragraph. Copied under `handed/`, never modified. | owner |
| sketched | The architect has drafted the spec and plan against the handed design, the charter and the knowledge base. | architect |
| in-shed | Committee members read the spec and plan in parallel and object with citations. The architect answers once per round. Rounds are capped by `max_shed_rounds`. You may object, rule, edit the spec directly, or ratify as handed and skip debate for small work. | committee, architect, owner |
| ratified | Consensus or your explicit disposition of remaining objections, plus your ratification of the current spec and plan. The seal is recorded: upstream main commit and spec hash. Footprints are taken from the plan, entanglement advisories issued, the feature branch created. | owner, foreman |
| building | Units move through their own states below. Amendments may arrive. | mason, committee, foreman |
| assembled | Every unit has merged. One committee member reads the whole branch against the spec and the charter and reports, criterion by criterion, what shows it holds and what does not. The chief of staff presents the report with anything unshown on top, and you accept it or send the gaps back as units. | committee, owner |
| delivered | The branch is pushed to your repository or fork and the pull request opened under your name, with a description the chief of staff drafted and you approved; or you merged the branch into upstream yourself and recorded it. Terminal. | foreman, chief of staff, owner |

The feature states move forward. Amendments revise the documents without moving the feature backward. An assembled feature whose final review finds gaps remains assembled while explicit follow-up units use the normal implementation, review and landing loop; its final review runs again after those units land. Changes to the ratified intent or plan still require the amendment gate. Neither a failed final review nor the end of a debate cap authorises delivery.

The owner may abandon an undelivered workstream. Abandonment stops new turns, cancels active turns while retaining their recorded work, releases capacity and preserves the trace. It does not delete the owner's branch or close an existing pull request. Delivered and abandoned workstreams are terminal. The owner may archive a terminal workstream to take it out of the list of work, and unarchive it to bring it back. Archiving is a runtime setting, like a pause; it deletes nothing and does not change the feature state.

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

The state machine can still leave a unit stuck or wrong: checks that never complete, a block the service reports and cannot clear, a stage that needs to run again, a send-back you disagree with. So you can move any started unit that has not merged, from implementing, checking, reviewing, approved, waiting or contested, to implementing, checking, reviewing, approved or contested, with a note. The move and its note are recorded together. A move to implementing gives the mason a turn with the note and a fresh clean-turn allowance in its existing workspace. A move to checking runs the checks again on the recorded candidate, even if a run of it already completed. A move to reviewing gives the reviewer a fresh review of the recorded candidate with the note, and the review still starts from a completed check run of that candidate. A move to approved records your approval of the recorded candidate in place of the reviewer's verdict, and the foreman lands it with the usual landing checks. A move to contested holds the unit for you. A move into the state the unit is in restarts that stage. Planned and ready units are the scheduler's, a merged unit has landed and is changed only through a follow-up, and a unit does not move while the foreman is landing or rebasing it. A move from waiting leaves the question open; its answer still reaches the role.

A check run is bound to its candidate, the feature branch commit it was built on and the diff between them, and recorded under the unit with the checks it ran, why they were chosen, the command, its exit status and the end of its output, including failures and runs that did not complete. A review starts only from a completed run of the exact candidate it reviews; a candidate that changed, through a revision or a rebase, is checked again first. With the Jev boost on, a judgment chooses the checks the change can affect from the project's check links, `dagger check` runs those, and otherwise every check runs (section 9.6). Passing checks are evidence for the review and never an approval. A mason contest has no candidate, so the owner can only return it to implementing with a note and a fresh clean-turn allowance. Plan dependencies must reference existing units and form an acyclic graph. Invalid plans cannot be ratified, and an amendment must preserve those properties.

### 5.3 The shed

The shed runs for a new spec and plan, and in a shorter form for amendments. It applies two tests, and one judgement.

- **Charter compliance is a veto.** A committee member citing a charter rule the spec violates kills that part of the spec. No quorum needed. The architect must redraft or the owner must overrule.
- **Fit is a judgement.** The committee says whether the plan realises the handed design without painting the project into a corner against the decisions the knowledge base holds. That is advice for you, not a veto.
- **Size is a split test.** A unit that takes on too much for one mason or touches too much of the code gets split. A unit whose task is unclear, or whose acceptance a reviewer could not verify from the unit's work, gets sent back to the architect.

Committee members run in parallel on the same revision of the spec and plan. Members have distinct review perspectives, correctness, integration and scope, and may run on different profiles; both are assigned in turn by member number from configuration. A perspective focuses a member without limiting the objections it may raise. The architect answers once per round. Consensus means zero dissent, never a vote and never a self-reported confidence. Debate may conclude early when dissent is resolved. The round cap limits automatic debate; reaching it does not turn remaining objections into approval. The chief of staff presents the spec, plan, dissent record and recommendation. You may request a redraft or further bounded debate, abandon the work, or explicitly overrule the remaining objections and ratify. Every overrule, including a charter veto, is recorded against the document revision. A small feature may skip debate at your explicit request, but still requires your ratification of both documents.

The shed is the only place units are debated. After ratification the plan is the masons' and reviewers' ticket queue, not a further argument.

You are in the shed on purpose. This is the one place the design makes you a blocker, because a wrong spec built on for a week costs more than a day's wait.

### 5.4 Amendments

A big feature hits a constraint mid-unit that nobody saw at planning. Without an amendment path the mason either improvises or stalls. So a mason or a reviewer asks, and when the honest answer changes the sealed spec or plan, the chief of staff routes the question to an amendment request: which criteria or units, what change, why. Masons and reviewers do not file amendments themselves; the drift mason files one when an upstream change alters what a sealed criterion means (section 7.5). It gets one short shed round with the same rules, the chief of staff presents it, and you decide. The automatic amendment debate is capped at one round, within `shed.max_rounds`; further debate requires a new owner decision. The round, architect reply and owner packet are recorded under `amendments/<n>/`. The unit that raised it waits. Others continue unless the change touches their footprint, in which case they are notified in their next bundle or, if the meaning of a criterion they serve changed, sent back to implementing.

An amendment is not a feature-state cycle. The feature stays in building, or assembled if final review has begun. The approved amendment versions the spec and/or plan, updates the seal if the spec changes, and identifies the affected units. It does not directly edit code. A rejected request leaves the prior documents in force and the requester receives the ruling. Any approval or final report based on changed criteria is invalidated before further landing or delivery.

The architect drafts a proposed revision from the sealed documents, request, charter and context. The proposal keeps the sealed spec and plan in force until the owner decides. The trace records the proposed spec, plan and an affected set of criteria and units under the request. Invalid drafts leave the sealed documents untouched.

### 5.5 Delivery

At assembled, the foreman rebases the feature branch onto upstream main one last time, the service runs every project check on the rebased commit, the committee's final read runs against that with the check result as evidence, and the chief of staff drafts the pull request description from the trace. You approve the description, or edit it. The foreman pushes the branch to your repository or fork and opens the pull request as you. Per project, the branch lands either with one commit per unit, the default because reviewers on a large project want reviewable commits, or squashed to one.

Local snapshots, unit commits and the delivery candidate presented to the owner may use Osmia's unsigned identity. With `landing = "squash"`, the delivery candidate combines the whole workstream into one commit on the reviewed base. The owner can edit the delivery commit message as well as the PR description. Squashing and owner review do not require signing credentials.

After owner approval, the service performs a pre-publication step before any push: it amends the delivery commit using the owner's Git identity and signing configuration on the service host, including configuration scoped to the project's clone. It sets both author and committer to the owner and uses the owner-approved commit message, removing model or runtime-agent co-author trailers such as `Co-authored-by: Claude ...` while preserving human co-author trailers. It adds the owner's `Signed-off-by` trailer, then signs with the owner's configured key and signing format so the signature covers the final message. With `landing = "commit-per-unit"`, it applies the same requirements to every outgoing unit commit. Delivery approval authorizes this attribution, message cleanup, sign-off and signing. Missing identity, an unavailable key or a signing failure blocks publication, with no unsigned fallback. Runtime agents never receive signing credentials.

Delivery is where Osmia stops implementation on that workstream. Approval is tied to the final reviewed branch revision, delivery commit message and PR description you saw. If any changes before publication, the affected review and approval must be refreshed. The authorized pre-publication amendment changes commit IDs but must preserve the approved tree and base, and the approved message apart from agent co-author cleanup and the owner's sign-off; it does not require another owner approval. The trace links the reviewed history and delivery candidate to the signed delivery history. A change to the content or base requires refreshed review and approval. The service durably records the signed delivery commits before pushing. Publication is resumable: a retry reuses those commits and finds the recorded branch and any already-created pull request instead of signing again or opening another one. Without a GitHub token the service attempts no publication: it neither pushes nor asks GitHub anything, and the publication shows in the inbox at once, waiting for a restart with the token. A publication whose last three attempts failed shows in the inbox with its last failure, including the host's explanation of a refusal, and when it is next attempted. It takes no decision: the owner fixes the cause, such as a token without pull request access, and the entry closes once an attempt publishes. These requirements also apply to delivery maintenance for dependent workstreams.

You may instead merge an assembled feature's branch into upstream yourself, outside the factory, for example while the service runs without a GitHub token. The factory still delivers through a pull request; this path only records a merge you made. You record it as the owner: the service fetches upstream's base branch and checks that it already holds the changes of the feature branch tip, meaning their merge is clean and changes nothing, so fast-forward, merge, squash and rebase merges all count. One commit then records the merge, naming the branch tip and the upstream commit that holds it, and moves the feature to delivered with a notice for the chief of staff. Nothing is pushed and no pull request is opened. The merge needs no final review or delivery approval: merging was your delivery decision. It is refused for a feature that is not assembled, and while upstream lacks or conflicts with the branch. A publication waiting for a GitHub token is refused in the same commit. A publication the service could still carry out refuses the merge until the publication has an outcome. Descendants treat a base delivered this way as integrated upstream.

When Hearsay is enabled, upstream review feedback can later arrive through its GitHub connector. The chief of staff presents it as a notice or question (section 12.4). A delivered workstream is not reopened or amended. Additional code work requires you to hand in a new workstream, referencing the delivered trace and, when appropriate, its branch as the base. Without Hearsay you may hand in that feedback yourself.

---

## 6. The chief of staff and questions

### 6.1 One persona, one thread per workstream

You talk to the chief of staff. Underneath, each workstream has its own durable thread, because one thread across four features on two projects would spend its context on the wrong feature. The persona holds together through memory at three levels: how you decide, recorded in prior rulings and notes and enriched by your Hearsay stance history when enabled; the project's decisions; and the workstream's own record. Your messages go to the thread of the workstream you have selected. Factory-wide requests, pause this, reorder that, work from any thread because the tools are factory-wide.

### 6.2 What it does with a question

Every role has an `ask` tool. Its question always goes to the chief of staff, never to another agent. The chief of staff does one of five things, and the choice is recorded.

1. **Answers it** when the answer is derivable from the documents, the trace or Hearsay. The bundle for the question's scope arrives with current stances, conflicts and open questions, each with a pointer. A ratified stance answers a question, cited. Redirecting is a valid answer: "the code answers that, look here" is what a good chief of staff says to a builder who did not look.
2. **Escalates it** when answering would be a new decision, or contradicts something you already ruled. It rephrases for you: which unit, which criterion, what is blocked, the options, its recommendation. Several open questions are batched into one ask.
3. **Routes it as an amendment** when the honest answer changes the sealed spec or plan.
4. **Proposes a charter amendment** when your answer is a standing rule rather than a decision about this feature.
5. **Rephrases your answer** for the asker and decides its scope: local to the asker, or a notice in every in-flight bundle on the project when it applies wider than the question.

With the Jev boost on, the turn that delivers a question also carries advisory signals on it: whether the honest answer changes the sealed spec or plan, whether answering it would contradict a ruling, and whether it asks for a standing project rule (section 9.6). They inform the choice and never make it.

Every question lands in the trace as `questions/<n>/`: the question as asked, the decision, what you were sent, what you said, what went back, and what it changed. With Hearsay enabled, your answer is ingested under your principal through the owner source, which authority makes a ratified stance. The local ruling is authoritative and durable before external ingestion. An optional immediate assertion must reconcile to that same ruling.

### 6.3 Waiting

Nothing on the scheduling path waits on a model, and a session blocked on a tool call for the hours you are asleep would hold a slot and hit its timeout. So asking is non-blocking. Calling `ask` records the question and ends the turn with the outcome `waiting`. The workspace is intact. The unit parks in `waiting`, its slot is freed, and the rest of the factory carries on. When the answer arrives it is delivered as the asker's next turn, with the thread's context intact, so from the asker's side it asked and then it heard back.

The chief of staff runs as a singleton per workstream, one turn at a time, so its record stays coherent and it can batch. A question waits with no timeout. It sits in the inbox until you answer. The chief of staff never answers on your behalf under a deadline: you asked to be asked, and a wrong guess silently built on is worse than a parked unit.

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

The chief of staff is your assistant for the workstream. It takes the workstream decisions you could take when that resolves an issue or a conflict, and raises to you only what it cannot resolve or is not sure of. The service holds it to the same rules as you, records it as the actor, and shows each of its actions in the workstream's conversation.

A contested unit goes to the chief of staff first. Its view shows each started unit's state, its contest and the rulings the contest takes, its recent transitions and block reasons, and its roles' latest turns with their outcomes and the tool calls the service refused. When it is confident, it rules review or revise with a note the resumed role receives. It escalates when it cannot tell what is wrong, when the fix needs a decision you have not made, or when the unit is contested again after its ruling. It may rule on two contests of a unit in a row; after that, the unit's contests are yours until you rule on one. A contest reaches your inbox when it escalates it, when it has no rulings left for the unit, or once it has seen the contest and left it undecided, so no contest waits unseen. You can rule on any contest yourself at any time.

When the state machine leaves a unit stuck or wrong outside a contest, the chief of staff moves it as you could (section 5.2), with a note the resumed role receives. Its moves and its rulings share one limit: two on a unit in a row, after which only a move to contested is open to it until you rule on or move the unit. A move to contested raises the unit to your inbox with its note, and the contest is yours. When you ask it to move a unit, it records the move as yours.

Ratification, amendments, charter changes and delivery stay yours. The chief of staff records those decisions only when you give them in a message.

---

## 7. Version control and landing

### 7.1 Fork and upstream

A project names an upstream repository, a local clone and an optional fork. The canonical base is upstream's configured base branch. When a fork is configured it receives feature branches and pull requests target upstream. Without a fork, feature branches and pull requests live in upstream. An explicitly identical fork is normalized to omission. The clone needs remotes matching the configured repositories by URL; a single origin remote is sufficient without a fork.

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

The feature branch is one workspace on the project's clone. Each unit gets a workspace of its own descending from the feature branch. Workspaces sit behind one interface with two backends: git worktrees, and Jujutsu colocated so plain git can still read history. Jujutsu adds change IDs that survive rebases, a snapshot on every command so an interrupted session never loses work, stored rather than blocking conflicts, and an operation log the service can restore from. The workflow does not depend on which backend a project uses.

A workspace lasts as long as the work it holds. A unit's workspace is removed once the unit merges, since its work is on the feature branch. When a workstream is delivered or abandoned, the service removes its remaining workspaces: it first commits the files of each unit that did not merge to that unit's branch, so abandonment keeps the work. Every branch stays, so each candidate the trace records remains in the clone. A workspace stays while a turn on it is unfinished or, on a finished workstream, while an operation is pending, and a unit's workspace stays while a replay is in progress in it.

### 7.4 Landing a unit

On approval the foreman squashes the unit's workspace to one commit on the feature branch with a message generated from the unit's title and the criteria it serves, then rebases every unit still in flight. A conflict goes to a mason session in the conflicting unit's workspace with the sealed spec and the instruction that the tree carries conflict markers to resolve against it. A session never discovers conflict markers by accident. Reviewers never see them.

An approval records the reviewed candidate, its base and the governing spec and plan revisions. Landing first checks that those inputs are still current. If a rebase changes the candidate or its base, that unit returns to checking and review after any conflict resolution and before landing. The foreman never silently transfers approval to a different candidate. Active file writers must finish or be stopped and snapshotted before their workspace is rebased; the scheduler owns this coordination.

### 7.5 Upstream drift

Upstream main moves daily on a busy project. On a cadence per project, and on demand, the foreman fetches upstream and rebases the feature branch onto it, then every unit in flight onto that. A rebase that changes what a sealed criterion means files an amendment. The seal moves with the branch.

A rebase that conflicts goes to a drift mason, and a reviewer reads the resolution against the sealed spec. Send-backs are bounded by `max_bounces`, as a unit's are. A resolution sent back that many times holds the drift rebase: the branch and the seal stay, and the workstream takes no further drift rebase, on the cadence or otherwise, until you ask for one or the chief of staff hands it back. A handback asks for the next drift rebase with a note its drift mason and reviewer receive, so the chief of staff can return the work to the role that can resolve it, as it moves a unit. It may hand back twice in a row before the drift rebase is yours. A drift rebase in progress holds back its own workstream's landings and the project's other drift rebases, never another workstream's units.

### 7.6 Entanglement and dependencies

Two units in one workstream are entangled when the plan makes one depend on the other or their code footprints intersect. Entangled units run in sequence. Two workstreams on one project have no shared spec, so their entanglement is code entity overlap, and the foreman warns when both are in the same subsystem before both pull requests are open.

Footprints resolve through the local entity map even without Hearsay. They schedule work; they do not bind it. A unit's review does not hold its diff to its footprint: a reviewer who finds the work wandering beyond its task says so as a finding. Unknown or ambiguous mappings cannot be treated as proof of disjointness. Within a workstream the scheduler serializes such units until their footprints are resolved. Across workstreams overlap is advisory by default; the owner can pause or reprioritise the affected work.

A workstream may declare another on the same project as its base. It then rebases onto that workstream's branch instead of upstream until the base change is integrated upstream. Its dependent pull request is opened in the push repository against that branch. Once the base is integrated, the service prepares an upstream pull request from the descendant's branch, refreshing the affected review and owner delivery approval before publishing it. With a separate fork, the trace retains both requests and their relationship. Without a fork, the service retargets the existing pull request to the configured base branch and updates its approved description after fresh review and owner delivery approval. The operation records the prior request identity and expected contents, reconciles interruptions before retrying, and refuses an unexpectedly edited or closed request. The trace retains both publication records and the request relationship. This is how one large change lands as a sequence of pull requests. Stacking inside one workstream is not supported.

Delivery of a base means its pull request is open, not merged upstream, so descendants continue to use that branch after delivery. Once the base is integrated upstream, the service can rebase descendants onto upstream and publish their upstream requests. For an already delivered descendant, this is a separate delivery-maintenance operation: the feature stays terminal and no implementation unit restarts. Conflicts or changed intent requiring implementation need a new owner-requested workstream. Base relationships must remain acyclic. An abandoned or unavailable base parks descendants for an owner decision; they are never silently moved to another base. A parked descendant enters your inbox. When its own base was abandoned after its changes reached upstream by any route, such as a merge outside the factory, you can move the descendant onto upstream: the service checks that merging the base commit the descendant holds into upstream changes nothing, then records upstream as the descendant's base, and its next drift rebase replays only the descendant's own commits there. Osmia never merges the upstream pull requests itself.

---

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
| foreman | `approved`, landing events, upstream cadence | one lander per project, landing is serial |
| librarian | project added, unit merged | one per project |
| chief of staff | its event outbox and your messages | one turn at a time per workstream |

### 8.2 Level-triggered

Controllers reconcile against the current state. Events only wake them early. A missed event costs latency and never correctness. Crash recovery is restart, re-read state, resume. A turn that was running when the service died is found by its session directory and its thread is given a turn saying so.

### 8.3 Finish before start

A freed slot is offered to review before implementation, implementation before debate, debate before drafting, so the factory finishes work rather than widening it. Within a stage, the slot goes to the highest-priority workstream with something ready, and round-robin breaks ties across workstreams and projects.

### 8.4 Capacity, priority and pause

Capacity is per role kind and global across every workstream on every project. A per-workstream work-in-progress cap keeps one feature from taking everything when it is alone.

Active workstreams have a priority order you set, or ask the chief of staff to set. A paused workstream, project or factory dispatches nothing new. Turns in flight finish, and a hard pause stops them too. Paused units stay where they are, their slots go to what is not paused, and resume picks up with nothing to reconcile. The chief of staff stays reachable while everything is paused, so "pause everything, I'm travelling" and "resume dagger only" are messages. Pause state persists across restart and is shown with who set it and why: you, the daily budget, a provider's usage limit, or the loop guard.

### 8.5 Cost, retries and degradation

A per-session cost cap protects the infrastructure. A per-unit cost is a signal: passing it files an amendment request saying the unit is bigger than planned. A daily budget across the factory pauses dispatch when reached.

Nothing the service repeats on its own repeats without a bound. Send-backs, clean turns, rounds and drift resolutions have their limits, and so do reviews the service refuses as stale, check runs that do not complete, reminders to remove conflict markers, continuations of interrupted turns and redelivery of notices the chief of staff's turns keep failing on. Each limit ends in a decision someone can take: a contested unit, a held drift rebase, or notices held until you message the chief of staff. Behind them all, the loop guard watches each workstream for sessions without progress, meaning a change of state of the feature, a unit, the shed, an amendment, the final review or the publication, a drift rebase that moved the branch, or anything you did. A workstream that runs `loop.max_sessions` of them pauses with the loop guard as its source and enters your inbox, and resuming it starts the count over. Failures are classified as infrastructure or behavioural: infrastructure failures retry, then fall to the profile's fallback; behavioural failures are outcomes and go back into the state machine. Streaks of failures show in the status so broken plumbing is visible rather than silently expensive.

---

## 9. Agents

### 9.1 Durable threads

An agent is a persistent, resumable thread: an id, a role, a workstream, a backend session id, an owned message log, and at most one active turn. A process runs only while a turn is in flight and is recovered automatically. Messages to an agent are turns, whether from the scheduler, the chief of staff or, for the chief of staff, from you. A message that arrives mid-turn queues as the next turn.

Durable threads are why a reviewer remembers what it found last round, a committee member argues round three better than a debate record could tell it, and a mason that asked a question picks up where it stopped.

### 9.2 The owned message log

Every turn's request and final response is captured by the service, per agent, in `agents/<id>/log.jsonl`, with provenance: what caused the turn and at what depth. The service does not read the agent binary's own transcript files. Their schema is unofficial and versioned, and owning the log is what makes a thread survive a profile switch: when the next turn cannot resume the backend session, it starts a fresh session with a bounded replay of the owned log. Chief-of-staff event and owner conversation turns also receive the current workstream bundle, latest stored status and open inbox escalations in their system prompt, so durable context remains available when bounded replay omits the tool calls that recorded it. One capture path per backend, since each exposes turn results differently.

### 9.3 Profiles

A profile names an agent binary, model, effort, optional fallback profile, timeout and turn limits. The `fallback` setting refers to another named profile, so a fallback can change the model or the agent binary. Unknown references and fallback cycles are configuration errors. Roles bind to profiles in configuration, and the committee's members may each be assigned a profile of their own. The binding can be overridden per role while the factory runs, from the web interface or the command line, effective for every new turn on every workstream. Turns in flight finish on the profile they started with. A switch that stays on the same agent binary resumes the thread as it is. A switch across binaries starts the next turn fresh from the owned log.

When the service sees a provider's usage limit, the role falls to its profile's fallback automatically and the status says so. A manual override wins either way.

Provider limits persist in `runtime.json` by agent backend. New turns of roles bound to a limited backend use the first profile in their fallback chain with an available backend. A role with no available fallback is paused with provider attribution. A reported reset time releases the limit when it passes; the owner can also clear a limit with `osmia profiles clear-limit <backend>`. A limit without a reset time remains until cleared. An owner profile override takes precedence while a limit is active.

### 9.4 What a session sees

A session receives a scoped plain-file view with the Osmia MCP server and a bundle. A mason works in a disposable writable copy; the service captures its changes into the unit workspace. A reviewer receives a read-only export of the exact candidate commit and separate writable scratch space. A reviewer's next turn replaces its export, and the export is discarded when the unit merges or the workstream finishes. Session records and authoritative repositories remain service-owned. A workstream's session directories, including each agent's own transcript, last until it is delivered or abandoned and its turns are done; they are then removed with its workspaces, and its trace keeps every turn's request and final response. It additionally receives a Hearsay MCP server when that integration is enabled and available.

The Osmia server, role-scoped:

| Tool | Roles | Purpose |
|---|---|---|
| `ask` | all but chief of staff | Raise a question. Ends the turn with outcome `waiting`. |
| `done` | all | End the turn with an outcome and a report. |
| `notes_read`, `notes_write` | all | The role's private craft memory for the project. |
| `amend` | drift mason | File an amendment request when an upstream change alters what a sealed criterion means. |
| `object`, `concede` | committee | A shed contribution, citing the spec, the charter or the knowledge base. |
| `verdict` | committee | A review verdict: the decision, how the reviewer verified the acceptance, and findings with severities and the action each asks for. |
| `workstream_diff` | unit, drift and final reviewers | Read the diff a review is pinned to: the whole diff, the changed files with line counts, chosen files or directories, or the hunks touching a line range. A unit review reads its candidate against its base, a drift review the feature branch's change before the rebase and the resolved candidate's change on upstream, and a final review the branch against upstream. Review prompts list the changed files instead of carrying diffs. |
| `answer`, `escalate`, `route_amendment`, `propose_charter`, `set_status`, `notify` | chief of staff | The five outcomes of a question, the status, and a notice to in-flight bundles. |
| `inspect_code` | chief of staff | Read a committed code excerpt; a cited answer queues a librarian knowledge-gap refresh. |
| `pause`, `resume`, `prioritise`, `capacity` | chief of staff | The factory-wide controls. |
| `decide_amendment` | chief of staff | Record your decision on a presented amendment when you give it in a message. |
| `resolve_contested` | chief of staff | Rule review or revise on a contested unit on your behalf, escalate it to you, or record the ruling you gave in a message. |
| `move_unit` | chief of staff | Move a started unit that has not merged to implementing, checking, reviewing, approved or contested on your behalf, or record the move you asked for in a message. |
| `hand_back_drift` | chief of staff | Hand a held drift rebase back on your behalf with a note its drift mason and reviewer receive, or record the handback you asked for in a message. |

The Hearsay server: `get_bundle`, `resolve`, `stance_history`, `get_l1`, `get_l0`, `search`, `assert`, filtered by the role's agent class and your principal.

There is no tool that lists agents, messages an arbitrary agent, or creates one. Routes are fixed by role.

### 9.5 Sandboxes

A role runs on the host or in a container, per profile. In a container the workspace is bind-mounted and the MCP servers are reached over HTTP from the host. Native tools follow role capabilities: reading tools for read-only roles, editing tools for file writers, and a shell only for implementation turns with execution permission. Native delegation, web tools and arbitrary MCP discovery are not granted. Read-only roles cannot edit their inputs or run arbitrary commands. A role may name skills by git reference. The service clones them into its cache and gives the role's Claude turns only their skills, read-only, with Claude's tool that loads them. A repository's hooks, MCP servers, commands and agents never reach a session, and other agent binaries receive no skills. The service runs the checks itself before every review that reads code: a unit candidate's before its unit review, and every check on the rebased feature branch before the final read. Both run `dagger check` on a fresh export of the pinned commit and discard the copy afterward. No reviewer runs checks or chooses a command, path or environment; the checks a unit runs are chosen by the service. Reviewers judge the work against its task, acceptance and criteria; style, formatting and lint belong to the project's checks. The check client receives a temporary home and engine connectivity, without inherited provider, delivery or signing credentials; no agent receives an engine endpoint. Results enter the trace and inform the review without granting approval or delivery.

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

### 10.2 The API

- Workstreams and their status. An event stream.
- The conversation per workstream: send a message, list turns.
- The inbox: open questions across every workstream, answer one.
- Pause and resume at factory, project and workstream level. Priority order.
- Profile bindings: get, override, clear. Usage per provider.
- Configuration: what is loaded and its digest, reload, last error.
- Projects: add, remove, extract the knowledge base. Workstreams: hand in, abandon, archive and unarchive.
- Trace: walk a workstream's record.

### 10.3 Runtime state is not configuration

Profile overrides, pause states and priority live in `runtime.json` under the root, persisted so they survive a restart and untouched by a reload. Configuration says the defaults. The runtime says what you changed since. Clearing an override returns to the configuration.

### 10.4 Reload

Reload is explicit. It reads the top-level configuration and every project's `config.toml`, validates them whole, and applies them only if they pass. A bad file leaves the old configuration running and the error in the interface. Profiles, capacity, budgets, review settings, Hearsay and Jev settings and the project list apply live; a removed project drains and stops. Listen addresses, the tailnet identity and the root need a restart, and the interface says so. The charter and the knowledge base need no reload because bundles read them at turn time. The loaded digest is shown, so "did the reload take" is checkable.

### 10.5 Notifications

A new question, a contested unit, a delivery or a budget pause can go out through a configured webhook so the inbox reaches you without the page open. One key, optional.

---

## 11. Interfaces

### 11.1 Web

Built for a phone as much as a laptop. Embedded in the binary, one page, fed by the event stream. Workstreams are listed beside the one selected, which takes the main area; each inbox entry shows with its workstream and is counted on that workstream's row, so what needs you is visible from the list.

- **Active work.** Every workstream with its goal and its units counted by state, and the capacity view: slots used per role kind, who is waiting, and every pause in force with its reason.
- **Inbox.** Every open question, contested unit and ratification packet across workstreams and projects, each with the chief of staff's rephrasing, the options and its recommendation, answered inline, and every publication that keeps failing, with its last failure.
- **Feed.** One per workstream, everything that happened to it in the order it happened: the thread with the chief of staff and the actions it took on your behalf, each status it wrote with its attention and note, each session that ran with its role, profile and outcome, and each change of the workstream's or a unit's state.
- **Controls.** Pause and resume at every level, priority order, the profile switcher with usage per provider beside it, and a reload button that lights when the file on disk differs from what is loaded.

Submitting a feed action, such as a ruling on a contested unit or ratifying a proposal, removes its card from the feed at once, before the server has responded. The card stays off the feed through any refresh that still lists the action as outstanding, since the backend may not have processed it yet, and comes back as actionable if the backend is still listing it 120 seconds after it acknowledged the submission, so it can never stay hidden for good. A failed submission (a non-success response or a network error) brings the card back at once with the error shown and the owner's entered values kept. Dismissing one card never hides or changes another. Every form that submits an owner action, including new project, archive and the feed's own action forms, clears to its default values once its submission succeeds; a failed submission leaves the form as the owner left it, with the error shown.

### 11.2 Command line

The following is the full design; see the [command line](cli.md) for supported
commands. The service runs in the foreground or with `serve --detach`, which
returns after the service acknowledges readiness. Detached output is appended
to the private `service.log` under the root. A failure that stops a running
service is written to that output, naming the project whose work failed, with
credentials redacted. `osmia stop` requests shutdown over
the local Unix socket.

```
osmia serve [--detach]               the service, foreground or detached
osmia stop                           stop the local service
osmia doctor                         check what the service needs, and how to fix it
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia handin dagger ./design.md      a new workstream from a document, an issue URL, or stdin
osmia status [workstream]            the status, or every workstream's goal and attention
osmia inbox                          open questions
osmia answer <n> "..."               a ruling
osmia move <workstream> <unit> <state> "..."   a unit to another state, with a note
osmia send <workstream> "..."        a message to the chief of staff
osmia pause|resume [all|<project>|<workstream>]
osmia archive|unarchive <workstream> a delivered or abandoned workstream out of the list, or back
osmia profiles [set <role> <profile>|clear <role>]
osmia reload
osmia trace <workstream> [unit|criterion|commit]
osmia ratify <workstream>            after reading the packet
osmia merged <workstream>            an assembled branch you merged into upstream yourself, recorded delivered
osmia rebase <workstream>            a drift rebase of a building or assembled workstream now, outside its cadence
osmia amendment <workstream> <n> [approve|reject|round|overrule]
```

Every command is an API call except `serve` and `doctor`. Doctor checks what the service needs before one runs, so it reads the root and configuration the way `serve` does; no other command reads state files directly.

---

## 12. Hearsay

Hearsay is an optional external context and memory service. It is not an orchestrator, has no work-unit model, delivers no agent messages and synthesises nothing at read time. The state machine, the plan, the mailbox and the documents of record stay in Osmia. Hearsay ingests source records (L0), distils them into memory (L1), and serves scoped bundles of decisions and source pointers. The integration is optional; the complete local workflow must remain usable without it.

### 12.1 What lives there

- **Decisions.** Every ruling you give is a ratified stance under your principal. Every committee contribution is a stance on the unit's topic; a send-back is a stance change. A mason's learnings at landing are asserted as proposals and arrive as inferred stances. You ratify from the interface.
- **The entity map.** The knowledge base's map of subsystems, aliases, path patterns and owners is Hearsay's code entity namespace, seeded when a project is added. Footprints name these entities.
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

The service assembles each turn's bundle: its own part, spec, plan, unit, questions and answers, notices, and then Hearsay's bundle for the project scope filtered by the unit's footprint entities, one to two thousand tokens of pointers with one line each. Roles map to agent classes: architect, committee and mason are workers, the chief of staff is an orchestrator over several scopes with a watch, the foreman and librarian are observers, and you are the steward. The effective principal is always the agent's grants intersected with yours.

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

### 13.2 Project configuration

```toml
# ~/.local/share/osmia/projects/<project-id>/config.toml
version = 1
name = "dagger"
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

Every key has a default, validation, a documented meaning and a test. The configuration carries a version, adding keys is not a breaking change, and renaming or removing one is a migration that rewrites the file text so comments survive.

---

## 14. Bootstrap

### 14.1 Adding a project

`osmia project add` records the upstream, optional fork and clone, creates the project trace repository under the root with an empty charter template, and runs the librarian's extraction pass: an inventory of subsystems with their tests and conventions as knowledge-base prose, and the local entity map that can later seed Hearsay. You then write the charter. The command refuses to hand in work to a project whose charter is empty, because debate has nothing to cite.

### 14.2 Handing in a workstream

`osmia handin` takes a file, an issue URL or stdin, copies it under `handed/`, creates the workstream and starts the architect. From there the lifecycle in section 5 runs, and you hear from the chief of staff when the packet is ready.

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
4. What counts as acceptance. The plan writes it per unit in plain language: behaviour that must hold, tests added or kept passing, checks to run. The charter determines what a project requires, and the reviewer records how it verified the acceptance. There is no mechanical coverage score that substitutes for that judgement.
5. Unit-scoped parallelism. A unit is one mason. Whether a wide unit may fan out into several masons with a squash step, as an assembler, is deferred until a real feature needs it.
6. The spec format. Markdown with a numbered list of criteria, and nothing parsed from it beyond the numbers. The footprint is declared in the plan by the architect and used to schedule units, never derived from the spec.

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
| Durable local service | Core integration; personal service, configuration and local API; durable records and outbox; isolated durable threads. | A role receives successive turns and survives a restart with an inspectable record. |
| Planning and ratification | Project onboarding and local knowledge; chief of staff and inbox; feature intake and planning; debate and ratification. | An owner can hand in a design and ratify a buildable plan using local context. |
| Feature delivery | Sequential implementation; exact-candidate review; serial landing and librarian updates; final review and delivery; trace navigation. | A small feature reaches an owner-approved pull request with a complete trace. |
| Concurrency and recovery | Parallel workstreams and shared capacity; amendments and standing rulings; concurrent landing and drift; budgets, profiles, pause, reload and recovery. | Several workstreams on one project progress through interruptions, questions and conflicts without lost work or duplicate landings. |
| Phone operation and notifications | Embedded web interface; tailnet access; notifications and the product's installation, release and first-run experience. | The full lifecycle and every owner decision are available from a phone, with local context. |
| Jujutsu workspaces | Jujutsu provider; rebase and interruption recovery. | The complete lifecycle works with either workspace backend. |
| Multiple projects and dependent features | Multi-project operation; cross-workstream bases and dependent pull requests. | One service handles several projects and sequences of dependent features. |
| Project memory through Hearsay | Replayable connector; scoped memory bundles; ruling reconciliation and watches. | Memory enriches the workflow while outages lose no decisions and local operation remains supported. |

---

## Appendix A. Glossary card

```
project      a repository you contribute to; optional fork, clone, charter, knowledge base
workstream   one feature on one project; its own branch, agents and trace
charter      your rules as a contributor; one per project; grows by ratified rulings
spec         what must be true when the feature is done; numbered criteria; ratified by you; amended in the shed
plan         DAG of units; task, acceptance, criteria served, dependencies, entities; data
unit         one task; one mason, one reviewer checking its acceptance, one commit on the feature branch
footprint    code entities a unit expects to touch; overlap is entanglement; schedules, never binds
acceptance   what a reviewer checks to accept a unit; written in the plan, settled in the shed
seal         (upstream main commit, spec hash); moves on rebase
the shed     debate; charter veto, size and acceptance tests, fit; the only place units are argued; then ratification
question     ask -> chief of staff -> answer | escalate | amend | charter | rephrase
trace        the workstream's record; files in the project's git repository under the root
profile      agent, model, effort, fallback; bound per role; switchable while running

feature      handed -> sketched -> in-shed -> ratified -> building -> assembled -> delivered
unit         planned -> ready -> implementing -> checking -> reviewing -> approved -> merged
             (+ waiting, contested)
roles        owner, chief of staff, architect, committee, mason, foreman, librarian
```
