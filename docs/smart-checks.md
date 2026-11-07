# Smart checks

Before a unit or assembled feature is reviewed, the service runs the project's
Dagger checks on the commit to be read. With the [Jev boost](configuration.md#optional-jev-boost)
on, Jev judges how likely the change is to alter each check's result, and
`dagger check` runs the likely ones. Configured fallback picks run the
highest-scored checks when none reaches the ranking threshold. Selection runs
in three stages: breadth, depth and ranking. A breadth or depth failure keeps
selection going with a fallback for that stage; a ranking failure runs every
check, exactly as with the boost off. The [design](design.md#96-jev-judgments) calls
this the check selection and defines what it may decide.

Smart checks pay off in a project whose Dagger checks are fine-grained, such
as one using Dagger collections, where each Go package's tests are an item a
check link can name.

Acceptance states observable behaviours and may name tests a unit adds as
evidence. It never requires a whole-suite pass or run and need not list existing
tests to keep passing: the factory selects and runs checks and catches regressions.
A passing service-owned check gate settles test passing for every acceptance
item or criterion, including whole-suite, scoped and named tests. Reviewers do
not raise a finding solely because a smart-check selection ran, a named test is
not visible in that selection, or they cannot run tests. They still judge whether
requested tests exist and exercise the behaviour.

## How a run selects its checks

1. **Diff and export.** The check run diffs the candidate against the feature
   branch commit it was built on with `git diff --binary --no-renames`,
   records the diff's SHA-256, and exports the candidate into a fresh
   directory under `<root>/checks`, made a Git root of its own so Dagger finds
   the project's workspace.
2. **Boost off.** With the boost off, the run checks everything without
   listing checks or asking Jev.
3. **List and attribute.** `dagger list checks --all --format=link` lists the
   export's expanded checks. `dagger list checks --format=link`, without
   `--all`, lists its top-level checks with collections unexpanded. Dagger
   defines those top-level checks; Osmia does not infer them from link names.
   Each listing is bounded by five minutes and 4 MiB of output.
   Options exclude broad links: a no-filter or check-name-only root that
   covers other listed links, and any link holding the same checks as such a
   root. This includes a module link in a one-module project or the package
   link of a collection's only package when narrower tests are listed. Both
   `dag+check://?check=test&...` and `dag+check://<path>?...` use coverage among
   listed links, independent of check names. Standalone checks with nothing
   narrower listed stay options. Links covering each other count as one
   check. A leaf covers no strictly narrower listed link.

   Membership comes from an additional scoped listing for each top-level
   check: `dagger list checks --all --format=link '<top-level-link>'`, with the
   same listing bounds. An expanded link belongs to a top-level check only
   when exactly one scoped listing returns it. For example, Dagger's
   `dag+check://?check=go/modules/packages/tests/test` root can contain
   `dag+check://?check=test&go-module=.&go-package=internal/trace&go-test=TestBuilt`.
   These links need not share a check name or path. A root whose attributed
   links differ from its own link is a collection, even with only one item;
   a standalone check maps to itself. Ambiguous or unmatched expanded links
   bypass breadth and depth and go straight to ranking. Attribution never
   drops an expanded option.
4. **Build the state and judge breadth.** All three stages share the link
   guide, process overview, changed files with added and removed line counts,
   and diff. The state is bounded to 72 KiB, including the process text. A diff
   that does not fit is replaced by a note to judge from the changed files;
   if that state still does not fit, every check runs.

   Task `check-breadth`, version 1, asks one Noul per top-level check whether
   the change could affect it or any of its items. Roots at or above
   [`jev.check_threshold`](configuration.md#optional-jev-boost), 0.5 by
   default, are kept. Their collections go on to depth; their standalone
   checks go on to ranking. A root below the threshold and all its items are
   skipped. If none reaches the threshold, all top-level checks are kept.
5. **Judge depth.** Task `check-depth`, version 1, asks one Choice per kept
   collection with at least two offerable levels. Levels follow the item
   filters' order in the expanded links: level *k* retains the first *k*
   item filters, preserving the check name. For Go this is usually
   `go-module` (when listed), then `go-package`, then `go-test`. A level whose
   links would be broad within the collection is not offered. The question
   still shows the whole hierarchy and why a level is not offered. With only
   one offerable level, that level is used without a question. Inconsistent
   filter orders get no question and keep the finest links.

   Jev's top choice is used without a probability threshold or deterministic
   override, including when it chooses the finest level. Links are cut to
   that level and deduplicated. This changes granularity without dropping a
   leaf: each resulting link covers every item beneath it.
6. **Narrow to candidates.** If the filtered list still exceeds 1024 links,
   narrowing collapses collection items, removing the last filter to cover
   a link and its siblings. For example, two tests in `go-package=internal/trace`
   become one package link. Each step collapses the item that removes the
   most links. Coverage is measured against the leaves of the original
   `--all` listing, including leaves breadth excluded. Narrowing skips broad
   parents, whether listed or created, and never produces a collection root
   or bare `dag+check://`. Links that cannot narrow to 1024 without a broad
   link run every check with reason `too_many_checks`, without asking ranking.
   The cap bounds request time and per-judgment cost; it is not an API limit
   and has no configuration key.
7. **Rank and accept.** Task `check-selection`, version 4, asks one Noul per
   remaining candidate whether the change could alter its result. Requests
   run sequentially, since each depends on the preceding answers. Each
   stage uses the service's `jev.Judge` with its own `jev.timeout`, retry and
   shared cool-down. Links whose ranking probability is at least
   `jev.check_threshold` are selected, regardless of the picks setting.
   When none reaches it, `jev.check_fallback_picks` (non-negative integer,
   default 0) above 0 accepts a response with a usable Noul score and runs up
   to that many highest-scored candidates from the same answers. There is no
   minimum score and no further request. Ties follow sorted candidate order.
   Fewer scored candidates run all scored links; no usable score runs every
   check. With 0 or unset, a response that selects none is declined and runs
   every check.
8. **Group.** Dagger evaluates one `dagger check` under a single filtered view
   of the workspace, so it refuses links whose filters differ, and the filters
   of one link AND together. Links combine into one only where that selects
   the same checks: when they differ in the values of one filter, or when one
   covers another. `go-package=a` and `go-package=b` become
   `go-package=a&go-package=b`, but `go-package=a&go-test=T` and `go-package=b`
   need separate runs, because together they would run only `T`.
9. **Run.** `dagger check --progress=report --fail-fast --parallel 4` runs once
   for each group, one after another, or once with no link to run every check.
   The first run that does not pass stops the rest and decides the result. All
   the runs share one export and the project's `checks_timeout`, and at most
   four checks run concurrently.
10. **Run everything if refused.** Refusal is judged on one command: it exits
    with an error before starting any check, without losing its engine
    session. A later command can be refused even after earlier commands
    passed. The same run then checks everything under the same
    `checks_timeout`; the finished selection is full with reason `refused`,
    while its output keeps the earlier reports and its start evidence keeps
    the chosen links. Refused or non-reporting reruns leave a failed check
    standing. A lost engine session is incomplete.

Unit, final-review and session check runs all use this selection. Before a
final review, the service judges the rebased feature branch commit against
the upstream base it sits on. Accepted judgments pass only their selected
links to Dagger; a full-run fallback records the same reason as a unit run.
An owner's explicit full session run remains a full run.
If the run cannot read the change's diff, it stops before executing checks.

Fallbacks apply at the stage that failed, including timeouts, missing
credentials, provider failures and unusable answers:

- A failed top-level or scoped listing, an unavailable listing capability, or
  a failed or unusable breadth answer restores the full expanded candidate
  path, removing broad links and narrowing before ranking. Depth is skipped.
- A failed or unusable depth answer preserves breadth's result: every leaf
  of the kept collections at its finest level, kept standalone checks and
  uncovered expanded links go on to narrowing and ranking. Dropped roots stay
  dropped.
- A failed ranking judgment runs every check. Ranking's no-selection case
  uses fallback picks when configured, otherwise it runs every check.
- A failed `--all` listing remains a full run with reason `unlisted`.
- During a Jev cool-down, each stage falls back immediately without a request.

When the check runner requests CI identity, the export carries the exact pinned
Git commit, its real branch name, and the configured upstream as a credential-free
HTTPS `origin`. This also applies to Git commits from Jujutsu workspaces. Only
existing tags on that candidate are copied. The runner verifies that the exported
files match the commit's tree before attaching its Git metadata. If identity
preparation fails, checks still run on the anonymous export; the check record's
optional `ci_identity_fallback` field explains why, and the service logs a warning.
Without that option the export remains anonymous.

## What a run records

As soon as a unit run chooses its checks, before `dagger check` starts, it
records `units/<unit>/checks-<n>-selection.json` in the workstream's trace.
This immutable start evidence holds the unit, run, operation, candidate, base,
report revision, diff digest, start time and selection. It has no result, so
an interrupted run remains started and unfinished. The finished run's record,
`units/<unit>/checks-<n>.json`, keeps the same selection beside the command,
exit status and output. The command lists each `dagger check` actually
started, including any failure reruns, in order and joined by `&&`; commands
stopped by an earlier failure are absent. The output holds the combined
reports and labels separate commands. There is one result document, one
captured output document and one check-capacity slot for the entire run.
Passing summaries keep every report heading. The trace link belongs to the
last command, including the failing part when it prints one; a part that
prints no trace leaves the link empty.
After a refusal, the finished record's selection and command describe the full
run, its output holds the refused run above it, and the start evidence keeps
the links Jev selected.
Final-review attempts record their selection in
`final/checks-<review>-attempt-<attempt>-started.json` immediately before
execution, and in `final/checks-<review>.json` beside the command and result.
Older final records without a selection remain readable.

The selection has this shape:

```json
"selection": {
  "mode": "selected",
  "links": [
    "dag+check://go/packages/tests/test?go-package=internal/trace",
    "dag+check://go/packages/tests/test?go-package=internal/service"
  ],
  "candidates": 37,
  "listed": 214,
  "judgment": "judgment_4f1c..."
}
```

| Field | Meaning |
| --- | --- |
| `mode` | `selected` when Jev's links ran, `full` when every check ran |
| `links` | The links Jev selected, for a selected run; the command shows how they were grouped |
| `fallback_picks` | Configured picks count on a selected run where no check reached the threshold; absent for ordinary selections, full runs and older records |
| `candidates` | How many links ranking was asked about, after breadth, depth, broad-link removal and narrowing |
| `listed` | Total links the project listed, before removing broad links or narrowing; absent when listing was skipped or failed, and in older records |
| `judgment` | The ranking judgment record, also set on a full run whose ranking judgment fell back |
| `breadth_judgment` | Optional breadth judgment record, including when that judgment fell back |
| `depth_judgment` | Optional depth judgment record; absent when no depth question was needed or breadth failed |
| `roots` | Optional top-level count, recorded when breadth succeeded |
| `kept_roots` | Optional list of top-level links kept by breadth |
| `levels` | Optional map from each kept collection's root link to its chosen level; `finest` when there is no consistent named hierarchy |
| `reason`, `detail` | Why a full run ran every check |

A full run's `reason` is one of the service's own:

| Reason | Cause |
| --- | --- |
| `disabled` | The boost is off |
| `unlisted` | The expanded `dagger list checks --all --format=link` listing failed or timed out |
| `no_checks` | The project lists no check links |
| `too_many_checks` | The remaining links cannot narrow to 1024 without a broad link |
| `change_too_large` | The state with the changed files and shared guides does not fit even without the diff |
| `refused` | Dagger refused one command's selected links before that command started any check; `detail` holds its error |

or the judgment's [fallback reason](../internal/jev/reason.go): `declined`
when no check reached the threshold and picks are disabled or have no usable
score, `unconfigured`, `cooling_down`, `timeout`,
`rate_limited`, `unavailable`, `rejected`, `unusable`, `interrupted`,
`unrecorded` or `cancelled`.

The optional stage fields can be absent; older selection records remain readable.
Each stage's judgment is recorded separately under the workstream as
`judgments/<id>.json`, with its task, version, exact request identity, and
any threshold or positive configured picks value it applied, the request (or
its digest above 64 KiB), the configured and resolved model, every answer with
its probability, the outcome and usage. Each stage's cost enters the ledger
separately under the `jev` role. A declined judgment keeps its answers for
evaluation; they never select checks.

A picks run uses mode `selected` and keeps its links, candidate count, listed
count and judgment, with `fallback_picks` marking the configured count.

The selection is also summarized in one sentence, such as "Jev selected 2 of
37 check links for the changed files in judgment judgment_4f1c...", or "every
check ran: Jev did not select them (declined: no check reached probability
0.50)". A picks summary says "no check reached the threshold; the 2 highest-scored
checks ran out of 37 candidate links in judgment judgment_4f1c...". The web
feed says that no check reached the threshold and the highest-scored checks
ran, retaining selected, judged and listed counts. That sentence appears in
the unit's transition to reviewing and its notice, in the reviewer's prompt with the check evidence, and in the verdict a
mason receives when checks fail. The final reader's prompt and the feed's
final-review check entry also describe the selection and its reason. Running
final attempts show their selection as soon as start evidence is recorded.

When breadth succeeds, the summary adds the kept and total top-level counts
and the chosen levels, such as "within 2 of 5 top-level checks, by go-package".

The selected and candidate counts describe the saving as "checking 2 of 37".
When removing broad links or narrowing reduces the list, `listed` also explains
that those 37 are narrowed from 214 listed links. An absent total stays
unknown; no total is inferred for older runs.

The factory runs checks with `--progress=report --fail-fast --parallel 4`. It records the
complete combined output beside the check record in `checks-<n>-output.txt`
and names that document in `output_path` with its `output_revision`. The
agent-facing `output` holds up to 16 KiB of extracted failure diagnostics, excluding passing check blocks
and rerun commands. Oversized excerpts are marked `truncated`; agents can
read the captured output in chunks through `factory_context`. Classification
uses the complete output, independently of the excerpt limit. A non-zero exit
with a failed check under the report's `CHECKS` heading is recorded as failed.
A non-zero exit without a failed check remains incomplete, including a session
shutdown error after all checks passed, and its reason repeats the last error
Dagger printed.

## Flaky checks

Dagger's report lists the checks that failed, under `RUN LOCALLY`. The factory
reruns each of them once by its own link, in the same export and within the
same `checks_timeout`, and records every report in the captured output:

- **It passes.** The check is flaky and counts as a pass. Its link is in the
  record's `flaky` list and the feed, and the transition, the thread entry and
  the reviewer's evidence name it. The excerpt keeps its failure's diagnostics,
  marked `[passed on rerun]`, so the flake can be found and fixed.
- **It fails.** The check failed. Its link is in `failed`, with the diagnostics
  of its latest failure. Other checks that passed their rerun are still flaky.
- **The rerun reports no result**, because it timed out or lost its engine.
  Nothing contradicts the failure, so the check stays failed.

`--fail-fast` can cancel checks that the report then leaves out, so a passed
rerun does not show that the whole run passed. The factory runs the same
selection again with `--skip` for each flaky link, which reuses cached passes,
runs what was cancelled and gives any new failure its own rerun. A flaky check
is skipped there because Dagger would otherwise replay its cached failure. If
it was the only check selected, Dagger reports that no checks matched and there
is nothing left to run.

Dagger caches a failed result under the address that ran it, and no
`dagger check` option bypasses that. A rerun is a fresh attempt only when the
check's link selects less than the run that failed, as a test's link does within
its package or the whole project. A check whose link is the address that failed,
such as a selected test run alone or a check with no filters, replays its
failure and stays failed.

## Cloud fallback

With `[dagger] check_fallback = "cloud"` and an `engine` other than `cloud`
([configuration](configuration.md#cloud-fallback-for-checks)), unit,
final-review and session check runs retry on the Dagger Cloud engine, with the
token named by `cloud_token_env`, the work that cannot finish locally. With the
key off, or with `engine = "cloud"`, none of this applies.

**What escalates to cloud.** A local attempt that is incomplete because it
reached its time budget, lost its engine session, or exited non-zero without
reporting a failed check. The same check operation retries immediately on cloud
what did not finish. Checks that the local output reports as passed stand and
are not rerun, even in an invocation that did not finish, including a run of
every check; only the rest of that invocation goes to cloud. If the local
output reports no passes for the unfinished invocation, the whole invocation
goes to cloud.

**What never goes to cloud.** A failed check, which is a ✘ in Dagger's report
that also fails its rerun; the run is failed as it is without the fallback. A
result that already passed or failed locally is not rerun on cloud. A
cancelled operation or a service shutdown, a failure to export the candidate,
and Dagger's refusal of the selected links trigger no cloud attempt. The
every-check run that follows a refusal is an ordinary local attempt, and can
itself escalate if it cannot finish. A flaky-check rerun stays on the engine
of the attempt it belongs to.

**Time budget.** One operation has one local budget and one cloud budget, each
`checks_timeout`. All local work shares the local budget: every local
invocation, local flaky reruns and the every-check run after a refusal. All
cloud work shares the cloud budget, which is independent of the time spent
locally: work routed by memory, escalated work and cloud flaky reruns. Neither
budget restarts per invocation, per group of links or per rerun, so an
operation finishes or is cut off within about twice `checks_timeout`. The cloud
attempt runs inside the capacity slot the operation already holds. If the
cloud attempt does not finish either, the run is incomplete and the unit retry,
contest and final-review backoff apply as they do without the fallback.

**Memory per workstream.** A link that needed cloud in a workstream, because it
was escalated or routed there in one of that workstream's unit or final-review
check run records, goes straight to cloud in later check runs of the same
workstream, without a local attempt. An every-check run that escalated is
remembered the same way. Links not remembered still run locally first. Memory
is derived from the check run records already in the workstream's trace, adds
no persisted state, and is not shared between workstreams. Records written
before the fallback lack its fields and count as local-only.

**What is recorded.** Unit and final-review check run records
(`units/<unit>/checks-<run>.json` and `final/checks-<k>.json`) carry an optional
`engines` field that lists which links or invocations ran locally, which ran on
cloud, and why: `timeout` for an escalation after a local timeout,
`engine_failure` for an escalation after an engine failure or an exit without a
reported failed check, or `memory` when routed by memory. A run with the
fallback off records only local invocations. The selection description, the
evidence given to unit reviewers and the final reader, and the pass, fail,
incomplete and contest transition reasons the chief of staff reads say when
cloud was used and what ran there. A session check run's thread entry says so
too. The output of a run that used cloud heads each engine's report with
`== local engine ==` or `== cloud engine ==`.

## Restarts and new runs

Each stage's unit judgment is identified by the check run that asked, its
task, version and exact request, and the acceptance values it applies:
breadth's threshold, or ranking's threshold and positive configured picks
value. Depth has no confidence threshold. A unit run retried after a restart reads back
its selection record, including a full run's fallback, instead of listing or
asking again. Recording the same run again keeps one selection record. A new run, for a revised or rebased
candidate, asks afresh. A judgment interrupted twice falls back as
`interrupted`.

Final-review judgments are keyed by the review number and rebased commit,
not the attempt number. An incomplete run's retry or an attempt resumed after
a service restart lists the checks again and reuses the recorded judgment
for each stage's same task, version, exact request and configured acceptance
values. The depth request follows deterministically from breadth, and ranking
from both, so the same change reuses all three judgments across restarts and
retries. A new review, commit or changed judgment input asks afresh. Each
attempt keeps its own start and result evidence.

## What Jev sees

All three requests carry the same state bundle: `checks` (the link guide),
`process` (the overview below), `changed_files` (paths and added/removed line
counts), and `diff` when it fits within 72 KiB. The link guide explains both
path and query-name links, filters and dependencies, and that collection
filters nest in order: `go-module` contains `go-package` (a directory), which
contains `go-test` (a function).

The shared `process` text is:

> Osmia is choosing which of a project's Dagger checks to run on a change, in three stages. Stage 1, breadth: for each top-level check, could the change affect it or any of its items at all? A no skips the check and everything in it. Stage 2, depth: for each collection kept by stage 1, at which level of its hierarchy should stage 3 judge it? Stage 3, ranking: for each remaining link, could the change alter its result? A yes runs it. Missing an affected check can let a regression through; running an unaffected one only costs time.

Each question uses structured instructions, separating the stage wording
from the data it refers to.

**Breadth (`check-breadth` v1).** Each top-level check gets a Noul,
`breadth-<i>`, with `stage` asking: "Stage 1 of 3, breadth. Could the change
in the state alter the result of the Dagger check `<link>`, or of any of its
items?" Instructions include `link`; collections also carry `items` (the
expanded count) and up to three `examples`.

- Yes: "The check, or at least one of its items, builds or exercises a changed
  file, or code, configuration or generated files the change affects. It goes
  on to stage 2 or 3 for a finer judgment."
- No: "Nothing the check or any of its items depends on is touched by the
  change. The check and every item in it are skipped with no further judgment,
  so answer no only when the change clearly cannot reach it."

**Depth (`check-depth` v1).** Each eligible kept collection gets a Choice,
`depth-<i>`, whose `stage` says: "Stage 2 of 3, depth. Stage 1 judged that the
change could affect the collection `<root>`. Choose the level at which stage
3 should judge its links."

Its `hierarchy` lists every level coarsest first, including levels not offered.
Each entry has `level` (filter name), `links` (count), `example`, `contains`
(the next level's name, or `none`), and `offered` (`true` or a reason such as
"covers the whole collection; already kept by stage 1"). Options contain only
offered levels, in that order, named after the filter. Their descriptions say
"Judge each of the `<n>` `<level>` links, such as `<example>`. Each runs every
`<contains>` item in it." The finest level omits the last sentence.

The `guidance` explains that each item contains every item at the levels
below it. Choosing a level never drops a check; it decides how finely ranking
divides the collection, and each selected link runs everything beneath it.
Too broad a level makes nearly the whole collection run; too fine a level
asks ranking to guess dependencies from names that the change does not
identify. Jev is told to choose the level whose items correspond most directly
to the changed files and directories. For Go that is usually `go-package`:
packages are directories, connected through imports; `go-module` is too broad
and `go-test` too fine. Finer levels suit changes identifying specific items;
coarser levels suit changes that cannot distinguish items. This is prompt
guidance; the code uses Jev's choice as given.

**Ranking (`check-selection` v4).** Each remaining link gets a Noul,
`check-<i>`, with `link` and `stage`: "Stage 3 of 3, ranking. Earlier stages
kept this check and chose how finely to divide its collection. Could the
change in the state alter the result of the Dagger check `<link>`?"

- Yes: "The check exercises a changed file, or code, configuration or generated
  files the change affects. It runs."
- No: "Nothing the check depends on is touched by the change. It is skipped."

With the default settings this state and these questions go to OpenRouter.
Jev holds no tool, no version control and no delivery credential.

## Limits

A selection can miss a check whose result depends on something the diff does
not show, such as an environment variable, a remote module or a file read at
run time. The threshold trades running unaffected checks against missing an
affected one. Fallback picks can miss an affected check that a full run
would catch, since even very low scores are ranked and only the top links
run. One pick may cover part of a collection, such as a package's tests in a
multi-package collection; it never names a whole collection or an equivalent
module root. Passing checks are evidence for the review, never an approval.
A breadth "no" skips every check in that collection without a finer judgment
(unless no root reaches the threshold, in which case all are kept). A mistaken
breadth exclusion can therefore miss a whole collection of affected checks.
A coarse depth runs whole items: selecting a package link runs every test in
that package. This can run more checks than judging each leaf individually.
A collection with one package and more than 1024 tests falls back to a full
run with `too_many_checks`, since its package link holds the whole collection.
Coverage compares exact paths and filter values: a path prefix does not
cover longer paths.

Each stage has its own `jev.timeout`. Three sequential requests can delay
fallback by roughly 3× a single request's timeout, about 30 seconds at the
10-second default, in addition to listing time. The 1024 cap bounds request
time and per-judgment cost, and cannot be configured.

## Code map

| Part | Code |
| --- | --- |
| Questions, state, narrowing, selection and version | [checkselect](../internal/checkselect) |
| The check run, its record and the selection's fallbacks | [service](../internal/service): `unit_checks.go`, `final_checks.go`, `session_controls.go`, `smart_checks.go`, `review_checks.go` |
| Recording and reuse shared by every judgment | [jev](../internal/jev) |
| The System One API client, retries and cool-down | agentcore `jev` and `jev/systemone` |
| Changed files from a diff | [gitdiff](../internal/gitdiff) |
| The local command | [smartcheck](../cmd/smartcheck) |
| The runner against the real Dagger CLI, run by the `dagger-cli-contract` check | `internal/checks/daggercli_test.go` |

## Run smart checks locally

`go run ./cmd/smartcheck` uses the same three-stage selection on any Git
working tree, with no Osmia service, configuration or trace:

```sh
export OPENROUTER_API_KEY=...
go run ./cmd/smartcheck                    # run the checks Jev selects
go run ./cmd/smartcheck -v                 # also explain the selection
go run ./cmd/smartcheck --dry-run -v       # explain without running checks
go run ./cmd/smartcheck --base origin/main -- --progress=report
```

It diffs the working tree, including uncommitted changes to tracked files but
not untracked files, against the merge base of `--base` (`main` by default),
lists the checks of the Dagger workspace at the repository root, and runs
`dagger check --fail-fast --parallel 4` for each group of selected links,
stopping at the first failure. Breadth and depth use the same stage-relative
fallbacks as the factory. A failed expanded listing or ranking answer says why
and runs every check; when nothing changed, it runs nothing. Links Dagger
refuses stop the command with Dagger's error. It exits with `dagger check`'s
status.

| Flag | Default | Effect |
| --- | --- | --- |
| `-v`, `--verbose` | off | Print changed files, top-level probabilities, collection hierarchies and offered levels, chosen levels and their probabilities, and ranking scores and selection to standard error |
| `--dry-run` | off | Print the selected links, or `every check`, instead of running them |
| `--base` | `main` | Compare against the merge base of this revision |
| `--threshold` | the `[jev]` default, `0.5` | The probability needed to keep a top-level check in breadth and select a link in ranking |
| `--fallback-picks` | `0` | When none reaches the threshold, run up to this many highest-scored candidates; 0 runs every check, negatives exit 2 |
| `-C` | `.` | Run in another working tree |
| `--model`, `--url` | the `[jev]` defaults | Which Jev model to ask, and where |
| `--api-key-env` | `OPENROUTER_API_KEY` | The variable holding the API key; the command stops when it is unset |

Arguments after `--` go to `dagger check`, except `--parallel` (both separate
and `=` forms) and `-j`, which are refused: every run uses `--parallel 4`.
A top-level or scoped listing failure restores the full ranking candidates;
an expanded listing failure runs every check, even with picks enabled. An
unchanged working tree runs no checks. With `-v`, output includes each
collection's hierarchy with counts, examples, nesting and offered status,
and the chosen level's probability. Levels chosen without a question say
`(no judgment needed)`; a depth failure says `(fallback)`. For example:

```text
base: main (merge base 1a2b3c4d5e6f)
changed files:
- internal/trace/records.go (+12 -3)
breadth: top-level checks
  ✓ 0.96 dag+check://?check=go/modules/packages/tests/test
  ✓ 0.62 dag+check://?check=release/version-round-trip
depth: dag+check://?check=go/modules/packages/tests/test hierarchy (coarsest first)
  go-module: 1 links, contains go-package, not offered: covers the whole collection; already kept by stage 1
    example: dag+check://?check=test&go-module=.
  go-package: 2 links, contains go-test, offered
    example: dag+check://?check=test&go-module=.&go-package=internal/service
  go-test: 4 links, contains none, offered
    example: dag+check://?check=test&go-module=.&go-package=internal/service&go-test=TestRun
depth: dag+check://?check=go/modules/packages/tests/test: chosen go-package (probability 0.83)
checks: 5 links, asking about 3
jev: <model> answered in 1.84s (10234 input, 74 output tokens)
  ✓ 0.97 dag+check://?check=test&go-module=.&go-package=internal/trace
  ✓ 0.71 dag+check://?check=test&go-module=.&go-package=internal/service
    0.12 dag+check://?check=release/version-round-trip
selected 2 of 3 at probability 0.50 or more
```

With `--fallback-picks=2`, when all scores are below 0.50, the verbose
summary instead says `no check reached probability 0.50; running the 2
highest-scored checks as fallback picks out of 3 candidates`. `--dry-run`
prints those links without running them.

The command sends up to three sequential requests, each with its own default
10-second timeout. Unlike a factory run, it records nothing, has no retry or
cool-down, does not rerun failed checks, and runs Dagger in your checkout
with your environment rather than in a fresh export with a scrubbed one.

## Tune the selection

1. Run `smartcheck --dry-run -v` on representative branches and compare the
   probabilities with the checks each change really affects. Pin `--model` to
   a versioned model, since thresholds are tuned against one.
2. Try other thresholds with `--threshold` until misses and over-selection
   are acceptable.
3. Optionally try `--fallback-picks=2` to bound runs where none reaches the
   threshold. Compare the risk of missing an affected check with running
   every check. Set `check_threshold` and `check_fallback_picks` in `[jev]`
   and run `osmia reload`. Breadth records its threshold; ranking records its
   threshold and the picks value when above 0. Changed acceptance values
   create new judgments, so earlier decisions stay as they were made.
4. A change to the questions, state or interpretation in `checkselect` bumps
   the affected stage versions (`BreadthVersion`, `DepthVersion` or `Version`)
   and updates the selection tests in `internal/checkselect`,
   `internal/execution/unit_checks_test.go` and
   `cmd/smartcheck`.
