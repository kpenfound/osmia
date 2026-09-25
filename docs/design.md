# Osmia design doc

A personal software factory for long-running feature work on repositories you contribute to but do not own. It is configured at the user level, keeps its factory state outside the repository it works on, is driven by a person through one aide, and shares one pool of agent capacity across several workstreams on several projects.

Status: design v0.2, 2026-09-15. M1 implementation is under construction. This document is the source of truth for feature proposals and implementation. Milestones phase the work; an issue does not override the design. Osmia uses `github.com/kpenfound/busybees/core` as a Go dependency and supports an optional Hearsay memory integration. Section 17 defines those boundaries and section 18 records the implementation order.

---

## 1. Purpose

You want to land a large feature in a project like dagger. You are a contributor, so you cannot install a factory in the repository, label its issues, or push to its main branch. You have a design in your head or in a document, you want agents to plan it, argue about the plan, build it in pieces, review each piece, and hand you one branch on your fork with a pull request you are happy to put your name on. You want to be asked when something needs your judgement, and otherwise left alone. You want the same machine working on three dagger features and a feature for another project at the same time, from one process, from your phone.

Osmia is that machine. It does not decide what to build. You hand it a feature. It does not run on the repository's issue tracker. It runs on a directory under your home. It does not push to anyone's main. It delivers a branch and a pull request, and stops.

### 1.1 Why a mason bee

Mason bees, the genus *Osmia*, have no hive. Each one moves into whatever cavity already exists, a reed, a hole in a wall, a tube someone left out, and gets to work. Orchards buy them by the tube and set them out where the pollination is needed, and a handful of them do the work of a hive. They are gentle and rarely sting.

That is the tool. It moves into an existing repository without changing it, is deployed where the work is, and is meant to be a good guest.

### 1.2 The shape of the design

Four choices give the design its shape.

1. A feature starts as a written specification: what must be true when it is done, drafted from what you hand in, debated by adversarial reviewers with you in the room, and ratified by you. It lives beside the factory's state, never in the repository. The plan that breaks it into units says how each part of it will be shown to hold, before anything is built.
2. Units are built in parallel where the plan allows and land one at a time on one feature branch, each reviewed against the specification before it lands. When a reviewer has read the whole branch against the specification and you accept that report, the feature is delivered as a pull request from your fork.
3. Every agent talks upward to one role, the chief of staff, which answers what it can from the record and asks you the rest. You talk to the chief of staff and to nothing else.
4. The transitions are code. No model runs on the scheduling path. Agents are durable threads that receive turns and never hold a version control tool.

---

## 2. Vocabulary

### 2.1 Concepts

| Term | Meaning |
|---|---|
| project | A repository you contribute to, with your fork, your clone, your rules for it and the factory's knowledge of it. One directory under the Osmia root. |
| workstream | One feature on one project, from handing in to delivery. Its own branch, its own agents, its own record. |
| charter | Your rules as a contributor to a project. One per project, human-owned, grows over time. |
| knowledge base | What the factory knows about a project's architecture that the repository's own documents do not say. Prose by subsystem, a local entity map and trace decisions, enriched by Hearsay when enabled. |
| feature spec (spec) | What must be true when the feature is done: intended behaviour, what it must not do, and acceptance criteria as a numbered list so they can be cited. Drafted by the architect, ratified by you. Not a description of the codebase, and never enforced mechanically. |
| criterion | One numbered item in a spec's acceptance criteria. Cited in debate and review; a handle, not a key. |
| proof | The evidence that a criterion holds, named in the plan before the unit is built: a test added in the project's own conventions, an existing test, a scripted check recorded in the trace, or a reviewer's judgement where no test is practical. |
| plan | The directed graph of units that realises the spec. Data, produced by the shed, changed only by amendment. |
| unit | One piece of the plan: the criteria it addresses, the proof it will produce for each, the units it depends on, the code it will touch. Built by one mason, reviewed by one reviewer, landed as one commit on the feature branch. |
| footprint | The code entities a unit touches, declared in the plan and checked against the diff. Overlap between footprints is entanglement. |
| seal | The pair of the upstream main commit and the spec's hash recorded at ratification. Moves when the feature branch rebases. |
| bundle | The context a session gets at the start of a turn: the documents it needs, the unit, its questions and answers, local decisions and notices, and an optional Hearsay bundle for its scope. |
| the shed | Debate. Where a spec and plan go before ratification, and where amendments go. Named for bikeshedding, because cheap debate that repeats without social cost is the centre of the machine rather than its friction. |
| amendment | A change to a ratified spec or plan, raised from inside the workstream, debated in a short shed round, decided by you. |
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
| chief of staff | The one role facing the owner. Answers every other role's questions from the record and Hearsay, escalates what it cannot, rephrases both ways, presents ratification, keeps the status, drafts the pull request description. Never implements, reviews or dispatches. |
| architect | Drafts the spec and plan from what the owner handed in, answers the committee in the shed, redrafts on amendment. |
| committee | Adversarial reviewers. In the shed they argue against the spec and plan, citing the spec, the charter and the knowledge base. On a unit one of them is the reviewer, holding the review conversation until the unit lands. At the end one reads the whole branch against the spec and the charter. One role, three prompts. |
| mason | Builds one unit until its criteria hold and the proofs the plan named are in place. May ask questions and request amendments. |
| foreman | Runs the landing. Merges approved units onto the feature branch, rebases units still in flight, keeps the branch current with upstream, resolves conflicts against the sealed spec, flags entanglement, pushes the branch and opens the pull request. Mostly code, with a session only for conflict resolution. |
| librarian | Maintains the knowledge base. Extracts it when a project is added, folds in what masons learned when units land. |

---

## 3. Principles

1. **No factory state is committed to the target repository.** No configuration, no specification, no workflow labels, no comments by bots. The repository sees the feature's code and tests on a branch on your fork and a pull request under your name. This rule concerns repositories Osmia works on; Osmia's own source repository carries its design and development configuration.
2. **Work is handed, not mined.** The factory builds what you give it. It has no backlog of its own and idles when you give it nothing.
3. **The owner talks to one role.** Every question, status, ratification and ruling passes through the chief of staff. No other agent addresses you and you address no other agent.
4. **The outer machine is Go.** States, transitions, readiness, entanglement checks, landing order, capacity and pause are code. What happens inside a turn is a model. Nothing on the scheduling path waits on a model.
5. **Agents never touch version control.** Sessions get a directory of files. The service creates workspaces, snapshots, rebases, merges and pushes.
6. **Agents are durable threads.** A role on a workstream is one persistent conversation that receives turns and keeps its context, not a fresh session each time.
7. **The trace is the record.** Files under the Osmia root hold everything that happened and supply context on their own. When enabled, Hearsay distils them into decisions and serves them back as additional memory. It is never required to make progress.
8. **One session, shared capacity.** One process runs every workstream on every project against one pool of slots. Focus is an ordering, pause is a switch, and neither restarts anything.
9. **Gates are transitions, not messages.** Agents message each other freely through fixed routes. The owner gates ratification, contested units, amendments and delivery.

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

It is written by the librarian: an extraction pass over the clone when a project is added, and a pass at every unit landing that folds in what the mason reported it learned. Landing is serial, so several workstreams on one project never race on it. A question the chief of staff had to answer from the code rather than the knowledge base is a gap, and it files the entry.

### 4.3 Feature spec

What must be true when the feature is done: the intended behaviour, what it must not do, and acceptance criteria as a numbered list. The architect drafts it from what you handed in, in the vocabulary the knowledge base gives the project. You edit it freely before ratification. After ratification it changes only by amendment.

This is not a spec in the sense of a description of the codebase's behaviour that the code is held to. The repository does not carry it, nobody upstream maintains it, and it goes stale the day the feature is delivered. It is the statement of intent that everything downstream is argued against: the plan says how each criterion will be shown, the mason builds to it, the reviewer reads the diff against it, and the final read reports on it. The flow is spec, then proof, then implementation, and the order is enforced by the plan being ratified before a mason starts. What is not enforced is coverage as arithmetic. Whether a criterion has been shown to hold is a reviewer's judgement, recorded with its evidence, and yours at delivery.

### 4.4 Plan

The directed graph of units. For each unit: the criteria it addresses, how each will be shown to hold, the units it must wait for, and the code entities it will touch. The shed produces it with the spec, and you ratify both together. Naming the proof before the build is the point: a criterion nobody can say how to show is a criterion nobody understands yet, and the shed catches that. Units whose footprints are disjoint and whose dependencies are met may run in parallel. How finely the feature is cut is the factory's concern, not yours, as long as it all lands on one branch.

### 4.5 Where they live

```
<root>/                          default ~/.osmia
  config.toml                    profiles, capacity, budget, Hearsay, listen addresses
  runtime.json                   profile overrides, pause states, workstream priority
  projects/<project-id>/         one git repository per project
    config.toml                  upstream, fork, clone path, landing style, per-project caps
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
      units/<u>/                 bundle, review/, landing.json
      agents/<id>/log.jsonl      every turn's request and final response, with provenance
      events.jsonl               every transition, with who and why
      ledger.jsonl               cost per session
```

The project repository's history is the decision trail. `osmia trace` walks it in either direction: criterion to unit to turns to review to commit, or commit back to the ruling that shaped it.

State updates and their outbox entries form one recoverable commit: after a restart both are visible or neither is. Appends alone are not a transaction. Durable operation identifiers connect an intended side effect to its result, so recovery can inspect a workspace, branch or existing pull request before retrying. Revisions and records remain available after delivery or abandonment; removing a project from active configuration does not delete its trace or the owner's clone.

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
| delivered | The branch is pushed to your fork and the pull request opened under your name, with a description the chief of staff drafted and you approved. Terminal. | foreman, chief of staff, owner |

The feature states move forward. Amendments revise the documents without moving the feature backward. An assembled feature whose final review finds gaps remains assembled while explicit follow-up units use the normal implementation, review and landing loop; its final review runs again after those units land. Changes to the ratified intent or plan still require the amendment gate. Neither a failed final review nor the end of a debate cap authorises delivery.

The owner may abandon an undelivered workstream. Abandonment stops new turns, cancels active turns while retaining their recorded work, releases capacity and preserves the trace. It does not delete the owner's branch or close an existing pull request. Delivered and abandoned workstreams are terminal.

### 5.2 Unit states

```
planned -> ready -> implementing -> reviewing -> approved -> merged
                        ^              |
                        +--------------+ changes requested, bounded by max_bounces

any state may also be: waiting (a question is open), contested (review bounces or a mason clean-turn decision)
```

| State | Meaning | Handled by |
|---|---|---|
| planned | In the plan, dependencies not yet merged. | scheduler |
| ready | Every unit it depends on has merged. Eligible for a slot. | scheduler |
| implementing | A mason works in the unit's workspace until its criteria hold and the proofs the plan named are in place and passing, then ends its turn with the outcome and a report per criterion. | mason |
| reviewing | The unit's reviewer runs the review over the unit's diff against the feature branch, with the sealed spec as the reference, and decides. Findings above the severity bar send it back to implementing with the findings. The same reviewer re-reviews the revised candidate, always an exact commit. | committee |
| approved | The reviewer is satisfied. Waiting for the foreman. | scheduler |
| merged | Squashed to one commit on the feature branch, message generated from the criteria it addresses. Units still in flight are rebased. | foreman |
| waiting | A sub-state of any of the above: the unit's role asked a question and its turn ended. Nothing else on the unit moves until the answer arrives. Other units continue. | chief of staff, owner |
| contested | Review bounces reached `max_bounces`, or the mason gave up or exhausted its clean-turn bound. Raised to you through the chief of staff with the reason. Nothing on the unit moves until you rule. | owner |

Waiting and contested preserve the underlying unit stage and any candidate under discussion. An answer or ruling resumes that stage through a recorded transition; it does not bypass review or landing checks. A mason contest has no candidate, so the owner can only return it to implementing with a note and a fresh clean-turn allowance. Plan dependencies must reference existing units and form an acyclic graph. Invalid plans cannot be ratified, and an amendment must preserve those properties.

### 5.3 The shed

The shed runs for a new spec and plan, and in a shorter form for amendments. It applies two tests, and one judgement.

- **Charter compliance is a veto.** A committee member citing a charter rule the spec violates kills that part of the spec. No quorum needed. The architect must redraft or the owner must overrule.
- **Fit is a judgement.** The committee says whether the plan realises the handed design without painting the project into a corner against the decisions the knowledge base holds. That is advice for you, not a veto.
- **Size is a split test.** A unit that addresses too many criteria or touches too much of the code gets split by what it addresses, not by estimated effort. A criterion whose proof the plan cannot name gets sent back to the architect.

Committee members run in parallel on the same revision of the spec and plan. The architect answers once per round. Consensus means zero dissent, never a vote and never a self-reported confidence. Debate may conclude early when dissent is resolved. The round cap limits automatic debate; reaching it does not turn remaining objections into approval. The chief of staff presents the spec, plan, dissent record and recommendation. You may request a redraft or further bounded debate, abandon the work, or explicitly overrule the remaining objections and ratify. Every overrule, including a charter veto, is recorded against the document revision. A small feature may skip debate at your explicit request, but still requires your ratification of both documents.

You are in the shed on purpose. This is the one place the design makes you a blocker, because a wrong spec built on for a week costs more than a day's wait.

### 5.4 Amendments

A big feature hits a constraint mid-unit that nobody saw at planning. Without an amendment path the mason either improvises or stalls. So a mason or a reviewer may file an amendment request, scoped to the sealed spec: which criteria, what change, why. It gets one short shed round with the same rules, the chief of staff presents it, and you decide. The automatic amendment debate is capped at one round, within `shed.max_rounds`; further debate requires a new owner decision. The round, architect reply and owner packet are recorded under `amendments/<n>/`. The unit that raised it waits. Others continue unless the change touches their footprint, in which case they are notified in their next bundle or, if the meaning of a criterion they address changed, sent back to implementing.

An amendment is not a feature-state cycle. The feature stays in building, or assembled if final review has begun. The approved amendment versions the spec and/or plan, updates the seal if the spec changes, and identifies affected units and proofs. It does not directly edit code. A rejected request leaves the prior documents in force and the requester receives the ruling. Any approval or final report based on changed criteria is invalidated before further landing or delivery.

The architect drafts a proposed revision from the sealed documents, request, charter and context. The proposal keeps the sealed spec and plan in force until the owner decides. The trace records the proposed spec, plan and an affected set of criteria, units and proofs under the request. Invalid drafts leave the sealed documents untouched.

### 5.5 Delivery

At assembled, the foreman rebases the feature branch onto upstream main one last time, the committee's final read runs against that, and the chief of staff drafts the pull request description from the trace. You approve the description, or edit it. The foreman pushes the branch to your fork and opens the pull request as you. Per project, the branch lands either with one commit per unit, the default because reviewers on a large project want reviewable commits, or squashed to one.

Delivery is where Osmia stops implementation on that workstream. Approval is tied to the final reviewed branch revision and the PR description you saw. If either changes before publication, the affected review and approval must be refreshed. Publication is resumable: a retry finds the recorded branch and any already-created pull request instead of opening another one.

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

Every question lands in the trace as `questions/<n>/`: the question as asked, the decision, what you were sent, what you said, what went back, and what it changed. With Hearsay enabled, your answer is also asserted under your principal, which authority makes a ratified stance. The local ruling is authoritative and durable before that assertion is attempted.

### 6.3 Waiting

Nothing on the scheduling path waits on a model, and a session blocked on a tool call for the hours you are asleep would hold a slot and hit its timeout. So asking is non-blocking. Calling `ask` records the question and ends the turn with the outcome `waiting`. The workspace is intact. The unit parks in `waiting`, its slot is freed, and the rest of the factory carries on. When the answer arrives it is delivered as the asker's next turn, with the thread's context intact, so from the asker's side it asked and then it heard back.

The chief of staff runs as a singleton per workstream, one turn at a time, so its record stays coherent and it can batch. A question waits with no timeout. It sits in the inbox until you answer. The chief of staff never answers on your behalf under a deadline: you asked to be asked, and a wrong guess silently built on is worse than a parked unit.

### 6.4 Status

The chief of staff keeps a status per workstream, rewritten fresh whenever the answer to one of three questions changes: what is being worked toward right now, what happened since you last looked that changes the picture, and who is doing what.

- **Goal.** One sentence. Changes when the objective changes, not when a step completes.
- **Attention.** Only a concrete action or decision you must take now, with enough context to act. Otherwise empty.
- **Note.** A few sentences on what changed that matters and where things stand. Translated, not compressed: what a worker's result means for the feature, not its identifiers.
- **Agents.** One line per active agent in your own words.

Identifiers stay out: commit hashes, branch names, file paths, session ids, model names. The status is the thing you read for a few seconds after time away, and it is the top of every view.

### 6.5 Events

The service tells the chief of staff what happened: a unit finished, a review came back, a question was raised, a landing succeeded, upstream moved. Events are written to a durable outbox in the same transaction as the state change, coalesced over a short window, and delivered as one turn, retried until delivered. Only the chief of staff receives events. Workers receive turns from the scheduler and nothing else, and are never told to wait for another agent.

Events are information, not authorisation. The chief of staff does not dispatch, restart, replace or route around an agent. Transitions are the scheduler's.

---

## 7. Version control and landing

### 7.1 Fork and upstream

A project is a clone of your fork with upstream as a second remote. The canonical base is upstream's main. Your fork is the push target. Osmia never pushes a protected branch and never merges a pull request.

### 7.2 Agents hold no tool

Sessions get a plain directory of files. The service performs every version control operation: create workspace, snapshot, rebase, squash, merge, push. A mason improvising a rebase is not acceptable, and an agent that never holds the tool cannot damage history. Enforcement belongs to the execution boundary: agents receive neither VCS tools nor writable VCS metadata nor inherited GitHub credentials. A core profile flag or prompt alone is insufficient. Read-only roles cannot acquire execution or write capabilities through repository-provided tools.

### 7.3 Workspaces

The feature branch is one workspace on the project's clone. Each unit gets a workspace of its own descending from the feature branch. The first implementation is git worktrees behind a workspace interface. Jujutsu is the second implementation behind the same interface, colocated so plain git still works for reading history: change IDs that survive rebases, snapshot on every command so an interrupted session never loses work, stored rather than blocking conflicts, and an operation log the service can restore from. The design does not depend on it, and the switch is made when rebases and interrupted sessions start to hurt.

### 7.4 Landing a unit

On approval the foreman squashes the unit's workspace to one commit on the feature branch with a message generated from the criteria it addresses, then rebases every unit still in flight. A conflict goes to a mason session in the conflicting unit's workspace with the sealed spec and the instruction that the tree carries conflict markers to resolve against it. A session never discovers conflict markers by accident. Reviewers never see them.

An approval records the reviewed candidate, its base and the governing spec and plan revisions. Landing first checks that those inputs are still current. If a rebase changes the candidate or its base, that unit returns to review after any conflict resolution and before landing. The foreman never silently transfers approval to a different candidate. Active file writers must finish or be stopped and snapshotted before their workspace is rebased; the scheduler owns this coordination.

### 7.5 Upstream drift

Upstream main moves daily on a busy project. On a cadence per project, and on demand, the foreman fetches upstream and rebases the feature branch onto it, then every unit in flight onto that. A rebase that changes what a sealed criterion means files an amendment. The seal moves with the branch.

### 7.6 Entanglement and dependencies

Two units in one workstream are entangled when the plan makes one depend on the other or their code footprints intersect. Entangled units run in sequence. Two workstreams on one project have no shared spec, so their entanglement is code entity overlap, and the foreman warns when both are in the same subsystem before both pull requests are open.

Footprints resolve through the local entity map even without Hearsay. The review compares actual changed paths with the declared footprint; undeclared scope must be explained and the plan amended when necessary before approval. Unknown or ambiguous mappings cannot be treated as proof of disjointness. Within a workstream the scheduler serializes such units until their footprints are resolved. Across workstreams overlap is advisory by default; the owner can pause or reprioritise the affected work.

A workstream may declare another on the same project as its base. It then rebases onto that workstream's branch instead of upstream until the base change is integrated upstream, and its pull request is opened against that branch. This is how one large change lands as a sequence of pull requests. Stacking inside one workstream is not supported.

Delivery of a base means its pull request is open, not merged upstream, so descendants continue to use that branch after delivery. Once the base is integrated upstream, the service can rebase descendants onto upstream and update their PR targets, refreshing affected reviews. Base relationships must remain acyclic. An abandoned or unavailable base parks descendants for an owner decision; they are never silently moved to another base. Osmia never merges the upstream pull requests itself.

---

## 8. Scheduler

### 8.1 Controllers per state

The service runs one controller per role kind, each reconciling its own input state against the tracker and sharing one pool of slots.

| Controller | Watches | Capacity |
|---|---|---|
| architect | `handed`, amendment requests | one per workstream |
| shed | `in-shed`, amendment rounds | committee members per proposal, `max_shed_rounds` |
| mason | `ready`, `implementing`, conflict resolution | `capacity.masons`, plus a per-workstream cap |
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

Active workstreams have a priority order you set, or ask the chief of staff to set. A paused workstream, project or factory dispatches nothing new. Turns in flight finish, and a hard pause stops them too. Paused units stay where they are, their slots go to what is not paused, and resume picks up with nothing to reconcile. The chief of staff stays reachable while everything is paused, so "pause everything, I'm travelling" and "resume dagger only" are messages. Pause state persists across restart and is shown with who set it and why: you, the daily budget, or a provider's usage limit.

### 8.5 Cost, retries and degradation

A per-session cost cap protects the infrastructure. A per-unit cost is a signal: passing it files an amendment request saying the unit is bigger than planned. A daily budget across the factory pauses dispatch when reached. Failures are classified as infrastructure or behavioural: infrastructure failures retry, then fall to the profile's fallback; behavioural failures are outcomes and go back into the state machine. Streaks of failures show in the status so broken plumbing is visible rather than silently expensive.

---

## 9. Agents

### 9.1 Durable threads

An agent is a persistent, resumable thread: an id, a role, a workstream, a backend session id, an owned message log, and at most one active turn. A process runs only while a turn is in flight and is recovered automatically. Messages to an agent are turns, whether from the scheduler, the chief of staff or, for the chief of staff, from you. A message that arrives mid-turn queues as the next turn.

Durable threads are why a reviewer remembers what it found last round, a committee member argues round three better than a debate record could tell it, and a mason that asked a question picks up where it stopped.

### 9.2 The owned message log

Every turn's request and final response is captured by the service, per agent, in `agents/<id>/log.jsonl`, with provenance: what caused the turn and at what depth. The service does not read the agent binary's own transcript files. Their schema is unofficial and versioned, and owning the log is what makes a thread survive a profile switch: when the next turn cannot resume the backend session, it starts a fresh session with a bounded replay of the owned log. Chief-of-staff event and owner conversation turns also receive the current workstream bundle, latest stored status and open inbox escalations in their system prompt, so durable context remains available when bounded replay omits the tool calls that recorded it. One capture path per backend, since each exposes turn results differently.

### 9.3 Profiles

A profile names an agent binary, model, effort, optional fallback profile, timeout and turn limits. The `fallback` setting refers to another named profile, so a fallback can change the model or the agent binary. Unknown references and fallback cycles are configuration errors. Roles bind to profiles in configuration. The binding can be overridden per role while the factory runs, from the web interface or the command line, effective for every new turn on every workstream. Turns in flight finish on the profile they started with. A switch that stays on the same agent binary resumes the thread as it is. A switch across binaries starts the next turn fresh from the owned log.

When the service sees a provider's usage limit, the role falls to its profile's fallback automatically and the status says so. A manual override wins either way.

Provider limits persist in `runtime.json` by agent backend. New turns of roles bound to a limited backend use the first profile in their fallback chain with an available backend. A role with no available fallback is paused with provider attribution. A reported reset time releases the limit when it passes; the owner can also clear a limit with `osmia profiles clear-limit <backend>`. A limit without a reset time remains until cleared. An owner profile override takes precedence while a limit is active.

### 9.4 What a session sees

A session starts in the unit's workspace, or a read-only clone for a committee member, with the Osmia MCP server and a bundle. It additionally receives a Hearsay MCP server when that integration is enabled and available.

The Osmia server, role-scoped:

| Tool | Roles | Purpose |
|---|---|---|
| `ask` | all but chief of staff | Raise a question. Ends the turn with outcome `waiting`. |
| `done` | all | End the turn with an outcome and a report. |
| `notes_read`, `notes_write` | all | The role's private craft memory for the project. |
| `amend` | mason, committee | File an amendment request against sealed criteria. |
| `object`, `concede` | committee | A shed contribution, citing the spec, the charter or the knowledge base. |
| `verdict` | committee | A review verdict with findings and severities. |
| `answer`, `escalate`, `route_amendment`, `propose_charter`, `set_status`, `notify` | chief of staff | The five outcomes of a question, the status, and a notice to in-flight bundles. |
| `pause`, `resume`, `prioritise`, `capacity` | chief of staff | The factory-wide controls. |
| `decide_amendment` | chief of staff | Record your decision on a presented amendment when you give it in a message. |

The Hearsay server: `get_bundle`, `resolve`, `stance_history`, `get_l1`, `get_l0`, `search`, `assert`, filtered by the role's agent class and your principal.

There is no tool that lists agents, messages an arbitrary agent, or creates one. Routes are fixed by role.

### 9.5 Sandboxes

A role runs on the host or in a container, per profile. In a container the workspace is bind-mounted and the MCP servers are reached over HTTP from the host. A read-only role has no tool that writes, runs or fetches.

---

## 10. The service

### 10.1 One process

One long-running process holds the scheduler, the threads, the event bus, the tracker and the runtime state. It serves an HTTP API and the web interface on a unix socket for the local command line, optionally on a loopback TCP address for a browser on the same machine, and, through embedded Tailscale, on your tailnet for everything else. Tailnet membership is the security boundary: no in-app authentication, one trusted user. The command line, the web interface and the chief of staff's factory tools all call the same handlers.

### 10.2 The API

- Workstreams and their status. An event stream.
- The conversation per workstream: send a message, list turns.
- The inbox: open questions across every workstream, answer one.
- Pause and resume at factory, project and workstream level. Priority order.
- Profile bindings: get, override, clear. Usage per provider.
- Configuration: what is loaded and its digest, reload, last error.
- Projects: add, remove, extract the knowledge base. Workstreams: hand in, abandon.
- Trace: walk a workstream's record.

### 10.3 Runtime state is not configuration

Profile overrides, pause states and priority live in `runtime.json` under the root, persisted so they survive a restart and untouched by a reload. Configuration says the defaults. The runtime says what you changed since. Clearing an override returns to the configuration.

### 10.4 Reload

Reload is explicit. It reads the top-level configuration and every project's `config.toml`, validates them whole, and applies them only if they pass. A bad file leaves the old configuration running and the error in the interface. Profiles, capacity, budgets, review settings, Hearsay settings and the project list apply live; a removed project drains and stops. Listen addresses, the tailnet identity and the root need a restart, and the interface says so. The charter and the knowledge base need no reload because bundles read them at turn time. The loaded digest is shown, so "did the reload take" is checkable.

### 10.5 Notifications

A new question, a contested unit, a delivery or a budget pause can go out through a configured webhook so the inbox reaches you without the page open. One key, optional.

---

## 11. Interfaces

### 11.1 Web

Built for a phone as much as a laptop. Embedded in the binary, one page, fed by the event stream.

- **Active work.** Every workstream with its goal, attention and note, its units by state, sessions running with their role and profile, and the capacity view: slots used per role kind, who is waiting, and every pause in force with its reason.
- **Inbox.** Every open question, contested unit and ratification packet across workstreams and projects, each with the chief of staff's rephrasing, the options and its recommendation, answered inline.
- **Conversation.** One per workstream, the thread with the chief of staff.
- **Controls.** Pause and resume at every level, priority order, the profile switcher with usage per provider beside it, and a reload button that lights when the file on disk differs from what is loaded.

### 11.2 Command line

The following is the full design; see the [M1 command line](cli.md) for the
implemented subset. Detached serving and the later lifecycle commands are
unavailable in M1.

```
osmia serve                          the service, foreground or detached
osmia project add dagger --upstream dagger/dagger --fork kpenfound/dagger --clone ~/github.com/dagger/dagger
osmia handin dagger ./design.md      a new workstream from a document, an issue URL, or stdin
osmia status [workstream]            the status, or every workstream's goal and attention
osmia inbox                          open questions
osmia answer <n> "..."               a ruling
osmia send <workstream> "..."        a message to the chief of staff
osmia pause|resume [all|<project>|<workstream>]
osmia profiles [set <role> <profile>|clear <role>]
osmia reload
osmia trace <workstream> [unit|criterion|commit]
osmia ratify <workstream>            after reading the packet
osmia amendment <workstream> <n> [approve|reject|round|overrule]
```

Every command is an API call. Nothing reads state files directly.

---

## 12. Hearsay

Hearsay is an optional external context and memory service. It is not an orchestrator, has no work-unit model, delivers no agent messages and synthesises nothing at read time. The state machine, the plan, the mailbox and the documents of record stay in Osmia. Hearsay ingests source records (L0), distils them into memory (L1), and serves scoped bundles of decisions and source pointers. It is under development independently; integration is the final planned milestone and does not block the earlier product.

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

Asserts are the fast path and the connector is the durable one: a ruling is written locally before assertion and later ingested and distilled to the same stance, so an outage loses nothing. Historical traces can be ingested when Hearsay becomes available. The first release and all milestones before M8 use this provider. M8 starts when Hearsay serves anchors, entity resolution, stance history, assert, agent classes, watch and replayable connector ingestion. Hearsay readiness does not delay Jujutsu or multi-project support.

---

## 13. Configuration

The supported M1 subset, defaults and stable directory identifiers are documented
in [M1 configuration](configuration.md). The examples below describe the full design.

### 13.1 User configuration

```toml
# ~/.osmia/config.toml
version = 1

[listen]
socket = "~/.osmia/osmia.sock"
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
[roles.committee]
profile = "claude"
[roles.chief_of_staff]
profile = "claude"
[roles.architect]
profile = "claude"
[roles.foreman]
profile = "codex-fast"
[roles.librarian]
profile = "codex-fast"

[shed]
max_rounds = 3
max_bounces = 3
```

### 13.2 Project configuration

```toml
# ~/.osmia/projects/<project-id>/config.toml
version = 1
name = "dagger"
upstream = "dagger/dagger"
fork = "kpenfound/dagger"
clone = "~/github.com/dagger/dagger"
base_branch = "main"
landing = "commit-per-unit"          # or "squash"
upstream_rebase = "6h"
hearsay_scope = "dagger"

[capacity]
per_workstream = 3                   # overrides the global default
```

Every key has a default, validation, a documented meaning and a test. The configuration carries a version, adding keys is not a breaking change, and renaming or removing one is a migration that rewrites the file text so comments survive.

---

## 14. Bootstrap

### 14.1 Adding a project

`osmia project add` records the upstream, fork and clone, creates the project trace repository under the root with an empty charter template, and runs the librarian's extraction pass: an inventory of subsystems with their tests and conventions as knowledge-base prose, and the local entity map that can later seed Hearsay. You then write the charter. The command refuses to hand in work to a project whose charter is empty, because debate has nothing to cite.

### 14.2 Handing in a workstream

`osmia handin` takes a file, an issue URL or stdin, copies it under `handed/`, creates the workstream and starts the architect. From there the lifecycle in section 5 runs, and you hear from the chief of staff when the packet is ready.

### 14.3 First run

The first release, at the end of M5, runs on git worktrees, without Hearsay, with a single project and the web interface on the tailnet. Multiple workstreams on that project are supported. The order of what comes after is Jujutsu behind the workspace interface (M6), multi-project operation and cross-workstream bases (M7), then the Hearsay connector, bundles and watches (M8). Hearsay can be made ready independently while Osmia progresses through the earlier milestones.

---

## 15. Security

- Tailnet membership is the boundary for the web interface and the API. The unix socket is the boundary locally, and the loopback interface is the boundary for the optional web listener, which binds no other address and refuses requests whose `Host` is not loopback or whose writes are not JSON. There is no in-app authentication and no multi-user isolation.
- Sessions never hold a version control tool, so a session cannot push, force-push or rewrite history.
- GitHub credentials reach the foreman's push and pull request calls as environment variables, never as arguments, and never reach a session.
- No factory state is committed to the target project. Its repository sees the feature's code and tests through the branch and pull request.
- Every bundle served by Hearsay is an audit event: who asked, on whose behalf, what was filtered.

---

## 16. Defaults and deferred choices

1. Per-role or per-workstream override of profiles. The design has per role, global. Per workstream would let one feature run on a cheaper provider, and complicates the switcher.
2. Several workstreams in one project when both touch the same subsystem. The default is an advisory, with owner pause and priority controls. Automatic refusal across workstreams is deferred. Within a workstream overlapping or unresolved footprints are serialized.
3. Committee membership. Per workstream in the design, with project memory arriving through Hearsay. Per project would accumulate arguments in the thread instead. Revisit when the first workstream has run.
4. What counts as a proof. The plan names one per criterion. New tests, existing tests, scripted checks recorded in the trace, and explicit reviewer judgement are supported forms. The charter determines which are sufficient for a project, and the reviewer records the actual evidence and limitations. There is no mechanical coverage score that substitutes for that judgement.
5. Unit-scoped parallelism. A unit is one mason. Whether a wide unit may fan out into several masons with a squash step, as an assembler, is deferred until a real feature needs it.
6. The spec format. Markdown with a numbered list of criteria, and nothing parsed from it beyond the numbers. The footprint is declared in the plan by the architect and checked against the diff at review, never derived from the spec.

---

## 17. Component boundaries

**`github.com/kpenfound/busybees/core`** is the reusable Go dependency for agent execution and backends, session sandbox primitives, review, retry classification, ledger and budget primitives, capacity, event wakeups, the MCP host and the workspace interface. Osmia consumes a pinned module version. Reusable extensions belong in that dependency; Osmia-specific workflow policy remains here. A sibling checkout must not be required to build or test Osmia.

**Osmia owns the workflow.** Its Go controllers own feature and unit state, the plan, seals, footprints, scheduling policy, questions and fixed message routes, role notes, prompts, ratification and delivery gates. Durable threads wrap the core runner with an owned log and turn queue. The core's wakeup bus does not replace the durable outbox, and its workspace or VCS-access flags do not by themselves enforce the isolation required in sections 7 and 15. Adapters must verify those contracts explicitly.

**Hearsay** is optional memory, reached through a provider boundary. Its connector executes in Hearsay's process and reads Osmia's trace. Bundles and watches augment the local record; Hearsay never becomes the workflow tracker, dispatcher or owner of the specification. Section 12 defines the integration and fallback contract.

---

## 18. Implementation milestones

Milestones run in order. Feature issues name outcomes and refer back to this document; their later work-item decomposition must preserve its constraints. The first release is M5. Hearsay is deliberately last so its development can proceed independently.

| Milestone | Feature outcomes | Exit condition |
|---|---|---|
| M1 — A durable local service | Core integration; personal service, configuration and local API; durable records and outbox; isolated durable threads. | A role receives successive turns and survives a restart with an inspectable record. |
| M2 — From a handed design to a ratified plan | Project onboarding and local knowledge; chief of staff and inbox; feature intake and planning; debate and ratification. | An owner can hand in a design and ratify a buildable plan using local context. |
| M3 — Deliver one feature end to end | Sequential implementation; exact-candidate review; serial landing and librarian updates; final review and delivery; trace navigation. | A small feature reaches an owner-approved pull request with a complete trace. |
| M4 — Run long-lived work reliably | Parallel workstreams and shared capacity; amendments and standing rulings; concurrent landing and drift; budgets, profiles, pause, reload and recovery. | Several workstreams on one project progress through interruptions, questions and conflicts without lost work or duplicate landings. |
| M5 — Operate Osmia from a phone | Embedded web interface; tailnet access; notifications and the product's installation, release and first-run experience. | The first release supports the full lifecycle and every owner decision from a phone, on one project with git worktrees and local context. |
| M6 — Jujutsu workspaces | Jujutsu provider; rebase and interruption recovery. | The complete lifecycle works with either workspace backend. |
| M7 — Several projects and dependent features | Multi-project operation; cross-workstream bases and dependent pull requests. | One service handles several projects and sequences of dependent features. |
| M8 — Project memory through Hearsay | Replayable connector; scoped memory bundles; ruling reconciliation and watches. | Memory enriches the workflow while outages lose no decisions and local operation remains supported. |

Repository bootstrap is completed separately from these milestones: importing this design, initializing the Go module and Dagger checks, configuring the development factory, writing contributor guardrails and adding the placeholder README are not implementation feature issues. The GitHub roadmap is at <https://github.com/kpenfound/osmia/milestones>.

---

## Appendix A. Glossary card

```
project      a repository you contribute to; fork, clone, charter, knowledge base
workstream   one feature on one project; its own branch, agents and trace
charter      your rules as a contributor; one per project; grows by ratified rulings
spec         what must be true when the feature is done; numbered criteria; ratified by you; amended in the shed
plan         DAG of units; criteria, the proof named for each, dependencies, entities; data
unit         one piece; one mason, one reviewer, one commit on the feature branch
footprint    code entities a unit touches; overlap is entanglement
proof        evidence a criterion holds; named in the plan, produced by the mason, judged by the reviewer
seal         (upstream main commit, spec hash); moves on rebase
the shed     debate; charter veto, size split, fit; consensus or recorded owner overruling, then ratification
question     ask -> chief of staff -> answer | escalate | amend | charter | rephrase
trace        the workstream's record; files in the project's git repository under the root
profile      agent, model, effort, fallback; bound per role; switchable while running

feature      handed -> sketched -> in-shed -> ratified -> building -> assembled -> delivered
unit         planned -> ready -> implementing -> reviewing -> approved -> merged
             (+ waiting, contested)
roles        owner, chief of staff, architect, committee, mason, foreman, librarian
```
