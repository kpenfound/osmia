# Smart checks

Before a unit or assembled feature is reviewed, the service runs the project's
Dagger checks on the commit to be read. With the [Jev boost](configuration.md#optional-jev-boost)
on, Jev judges how likely the change is to alter each check's result, and
`dagger check` runs the likely ones. Configured fallback picks run the
highest-scored checks when none reaches the threshold. Whenever that judgment
cannot be used, every check runs, exactly as with the boost off. The [design](design.md#96-jev-judgments) calls
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
3. **List.** `dagger list checks --all --format=link` lists the export's
   checks, with collections expanded to their items, as links such as
   `dag+check://go/packages/tests/test?go-package=internal/trace&go-test=TestBuilt`.
   Listing is bounded by five minutes and 4 MiB of output.
   Options exclude broad links: a no-filter or check-name-only root that
   covers other listed links, and any link holding the same checks as such a
   root. This includes a module link in a one-module project or the package
   link of a collection's only package when narrower tests are listed. Both
   `dag+check://?check=test&...` and `dag+check://<path>?...` use coverage among
   listed links, independent of check names. Standalone checks, including
   no-filter and name-only links with nothing narrower listed, stay options.
   Links covering each other count as one check. A leaf covers no strictly
   narrower listed link. The remaining options together cover every leaf
   in the original listing.
4. **Narrow to candidates.** One judgment asks about at most 128 links. A
   project with more remaining options is asked about its collections' items
   instead: removing a link's last filter gives the link that covers it and
   its siblings, so
   `...?go-package=internal/trace&go-test=TestBuilt` and
   `...?go-package=internal/trace&go-test=TestChunks` become
   `...?go-package=internal/trace`. Each step collapses the item that removes
   the most links, so the largest collections are judged by package and small
   ones keep their own links. Every step skips broad parents, whether listed
   or created by narrowing, using the original listing's leaves to compare
   coverage. Narrowing never produces a collection root or bare
   `dag+check://`. Links that cannot narrow to 128 without a broad link run
   every check with reason `too_many_checks`, without asking Jev.
5. **Build the state.** The judgment's state holds a guide to reading check
   links, the changed files with their added and removed line counts, and the
   diff. The state is bounded to 72 KiB: a diff that does not fit is replaced
   by a note to judge from the changed files, and changed files that do not
   fit on their own run every check.
6. **Ask.** Each candidate is one Noul question, `check-<i>`: "Could the
   change in the state alter the result of the Dagger check `<link>`?", where
   yes means the check exercises a changed file or code, configuration or
   generated files the change affects. The service's `jev.Judge` sends it as
   task `check-selection`, with the request timeout, retry and cool-down every
   judgment shares.
7. **Accept.** The links whose probability is at least
   [`jev.check_threshold`](configuration.md#optional-jev-boost), 0.5 by
   default, are selected, regardless of the picks setting. When none reaches
   it, `jev.check_fallback_picks` (non-negative integer, default 0) above 0
   accepts a response with a usable Noul score and runs up to that many
   highest-scored candidates from the same answers. There is no minimum
   score and no second request. Ties follow sorted candidate order. Fewer
   scored candidates run all scored links; no usable score runs every check.
   With 0 or unset, a response that selects none is declined and runs every
   check, keeping the threshold-only judgment identity.
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
links to Dagger; every fallback runs all checks with the same reason a unit
run records. An owner's explicit full session run remains a full run.
If the run cannot read the change's diff, it stops before executing checks.

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
| `candidates` | How many links Jev was asked about, after removing broad links and narrowing |
| `listed` | Total links the project listed, before removing broad links or narrowing; absent when listing was skipped or failed, and in older records |
| `judgment` | The judgment record the selection rests on, also set on a full run whose judgment fell back |
| `reason`, `detail` | Why a full run ran every check |

A full run's `reason` is one of the service's own, decided before Jev is
asked:

| Reason | Cause |
| --- | --- |
| `disabled` | The boost is off |
| `unlisted` | `dagger list checks` failed or timed out |
| `no_checks` | The project lists no check links |
| `too_many_checks` | The remaining links cannot narrow to 128 without a broad link |
| `change_too_large` | The changed files alone do not fit the state |
| `refused` | Dagger refused one command's selected links before that command started any check; `detail` holds its error |

or the judgment's [fallback reason](../internal/jev/reason.go): `declined`
when no check reached the threshold and picks are disabled or have no usable
score, `unconfigured`, `cooling_down`, `timeout`,
`rate_limited`, `unavailable`, `rejected`, `unusable`, `interrupted`,
`unrecorded` or `cancelled`.

The judgment itself is recorded under the workstream as
`judgments/<id>.json`, with the threshold and positive configured picks value
it applied, the request (or its digest above 64 KiB), the configured and
resolved model, every answer with its probability, the outcome
and usage. Its cost enters the ledger under the `jev` role. A declined
judgment keeps its answers for evaluation; they never select checks.

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

## Restarts and new runs

A unit judgment is identified by the check run that asked, the task, its
version, the threshold, the configured picks value when above 0, and the exact
request. A unit run retried after a restart reads back
its selection record, including a full run's fallback, instead of listing or
asking again. Recording the same run again keeps one selection record. A new run, for a revised or rebased
candidate, asks afresh. A judgment interrupted twice falls back as
`interrupted`.

Final-review judgments are keyed by the review number and rebased commit,
not the attempt number. An incomplete run's retry or an attempt resumed after
a service restart lists the checks again and reuses the recorded judgment
for the same request, threshold and picks value. A new review, commit or
changed judgment input asks afresh. Each attempt keeps its own start and result evidence.

## What Jev sees

Jev receives only the judgment's state and questions: the check links, the
changed file paths with their line counts, and the diff when it fits. With
the default settings this goes to OpenRouter. Jev holds no tool, no version
control and no delivery credential.

## Limits

A selection can miss a check whose result depends on something the diff does
not show, such as an environment variable, a remote module or a file read at
run time. The threshold trades running unaffected checks against missing an
affected one. Fallback picks can miss an affected check that a full run
would catch, since even very low scores are ranked and only the top links
run. One pick may cover part of a collection, such as a package's tests in a
multi-package collection; it never names a whole collection or an equivalent
module root. Passing checks are evidence for the review, never an approval.
Collapsed links run their whole item, so a project with many small
collections runs more than it would with each link judged.
A collection with one package and more than 128 tests falls back to a full
run with `too_many_checks`, since its package link holds the whole collection.
Coverage compares exact paths and filter values: a path prefix does not
cover longer paths.

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

`go run ./cmd/smartcheck` asks the same judgment of any Git working tree, with
no Osmia service, configuration or trace:

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
stopping at the first failure. When the factory would fall back before
running a check, it says why and runs every check; when nothing changed, it
runs nothing. Links Dagger refuses stop the command with Dagger's error. It
exits with `dagger check`'s status.

| Flag | Default | Effect |
| --- | --- | --- |
| `-v`, `--verbose` | off | Print the changed files, each candidate's probability and the selection to standard error |
| `--dry-run` | off | Print the selected links, or `every check`, instead of running them |
| `--base` | `main` | Compare against the merge base of this revision |
| `--threshold` | the `[jev]` default, `0.5` | The probability a check needs to be selected |
| `--fallback-picks` | `0` | When none reaches the threshold, run up to this many highest-scored candidates; 0 runs every check, negatives exit 2 |
| `-C` | `.` | Run in another working tree |
| `--model`, `--url` | the `[jev]` defaults | Which Jev model to ask, and where |
| `--api-key-env` | `OPENROUTER_API_KEY` | The variable holding the API key; the command stops when it is unset |

Arguments after `--` go to `dagger check`, except `--parallel` (both separate
and `=` forms) and `-j`, which are refused: every run uses `--parallel 4`.
Listing failure exits with an error even with picks enabled, and an unchanged
working tree runs no checks. With `-v`, the output looks like:

```text
base: main (merge base 1a2b3c4d5e6f)
changed files:
- internal/trace/records.go (+12 -3)
checks: 214 links, asking about 37
jev: <model> answered in 1.84s (10234 input, 74 output tokens)
  ✓ 0.97 dag+check://go/packages/tests/test?go-package=internal/trace
  ✓ 0.71 dag+check://go/packages/tests/test?go-package=internal/service
    0.12 dag+check://release/version-round-trip
selected 2 of 37 at probability 0.50 or more
```

With `--fallback-picks=2`, when all scores are below 0.50, the verbose
summary instead says `no check reached probability 0.50; running the 2
highest-scored checks as fallback picks out of 37 candidates`. `--dry-run`
prints those links without running them.

Unlike a factory run, the command records nothing, makes one request without
retry or cool-down, does not rerun failed checks, and runs Dagger in your
checkout with your environment rather than in a fresh export with a scrubbed
one.

## Tune the selection

1. Run `smartcheck --dry-run -v` on representative branches and compare the
   probabilities with the checks each change really affects. Pin `--model` to
   a versioned model, since thresholds are tuned against one.
2. Try other thresholds with `--threshold` until misses and over-selection
   are acceptable.
3. Optionally try `--fallback-picks=2` to bound runs where none reaches the
   threshold. Compare the risk of missing an affected check with running
   every check. Set `check_threshold` and `check_fallback_picks` in `[jev]`
   and run `osmia reload`. Each judgment records both configured values
   (the picks value when above 0), and a changed value is a new judgment, so
   earlier decisions stay as they were made.
4. A change to the questions or state in `checkselect` also bumps
   `checkselect.Version` and updates the selection tests in
   `internal/checkselect`, `internal/execution/unit_checks_test.go` and
   `cmd/smartcheck`.
