# Smart checks

Before a unit is reviewed, the service runs the project's Dagger checks on the
unit's candidate. With the [Jev boost](configuration.md#optional-jev-boost)
on, smart checks run only the checks the change can affect: Jev judges, for
each check, how likely the change is to alter its result, and `dagger check`
runs the likely ones. Whenever that judgment cannot be used, every check runs,
exactly as with the boost off. The [design](design.md#96-jev-judgments) calls
this the check selection and defines what it may decide.

Smart checks pay off in a project whose Dagger checks are fine-grained, such
as one using Dagger collections, where each Go package's tests are an item a
check link can name.

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
4. **Narrow to candidates.** One judgment asks about at most 128 links. A
   project with more is asked about its collections' items instead: removing
   a link's last filter gives the link that covers it and its siblings, so
   `...?go-package=internal/trace&go-test=TestBuilt` and
   `...?go-package=internal/trace&go-test=TestChunks` become
   `...?go-package=internal/trace`. Each step collapses the item that removes
   the most links, so the largest collections are judged by package and small
   ones keep their own links. Links that cannot narrow to 128 run every check.
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
7. **Accept.** The links whose probability is at least 0.5 are selected. A
   response that selects none is declined, and the run checks everything.
8. **Run.** `dagger check --progress=report` runs with the selected links, or
   with none to run every check, bounded by the project's `checks_timeout`.

Only the unit check run selects. The check run before a final review runs
every check on the rebased feature branch.

## What a run records

The run's record, `units/<unit>/checks-<n>.json` in the workstream's trace,
holds its selection beside the command, exit status and output:

```json
"selection": {
  "mode": "selected",
  "links": [
    "dag+check://go/packages/tests/test?go-package=internal/trace",
    "dag+check://go/packages/tests/test?go-package=internal/service"
  ],
  "candidates": 37,
  "judgment": "judgment_4f1c..."
}
```

| Field | Meaning |
| --- | --- |
| `mode` | `selected` when Jev's links ran, `full` when every check ran |
| `links` | The links passed to `dagger check`, for a selected run |
| `candidates` | How many links Jev was asked about, after narrowing |
| `judgment` | The judgment record the selection rests on, also set on a full run whose judgment fell back |
| `reason`, `detail` | Why a full run ran every check |

A full run's `reason` is one of the service's own, decided before Jev is
asked:

| Reason | Cause |
| --- | --- |
| `disabled` | The boost is off |
| `unlisted` | `dagger list checks` failed or timed out |
| `no_checks` | The project lists no check links |
| `too_many_checks` | The links do not narrow to 128 |
| `change_too_large` | The changed files alone do not fit the state |

or the judgment's [fallback reason](../internal/jev/reason.go): `declined`
when no check reached 0.5, `unconfigured`, `cooling_down`, `timeout`,
`rate_limited`, `unavailable`, `rejected`, `unusable`, `interrupted`,
`unrecorded` or `cancelled`.

The judgment itself is recorded under the workstream as
`judgments/<id>.json`, with the request (or its digest above 64 KiB), the
configured and resolved model, every answer with its probability, the outcome
and usage. Its cost enters the ledger under the `jev` role. A declined
judgment keeps its answers for evaluation; they never select checks.

The selection is also summarized in one sentence, such as "Jev selected 2 of
37 check links for the changed files in judgment judgment_4f1c...", or "every
check ran: Jev did not select them (declined: no check reached probability
0.50)". That sentence appears in the unit's transition to reviewing and its
notice, in the reviewer's prompt with the check evidence, and in the verdict a
mason receives when checks fail.

The factory runs checks with `--progress=report --failfast`. It records the
complete combined output beside the check record in `checks-<n>-output.txt`
and names that document in `output_path` with its `output_revision`. The
agent-facing `output` holds up to 16 KiB of extracted failure diagnostics, excluding passing check blocks
and rerun commands. Oversized excerpts are marked `truncated`; agents can
read the captured output in chunks through `factory_context`. Classification
uses the complete output, independently of the excerpt limit.

## Restarts and new runs

A judgment is identified by the check run that asked, the task, its version
and the exact request. A run retried after a restart reads back the decision
it already used instead of asking again. A new run, for a revised or rebased
candidate, asks afresh. A judgment interrupted twice falls back as
`interrupted`.

## What Jev sees

Jev receives only the judgment's state and questions: the check links, the
changed file paths with their line counts, and the diff when it fits. With
the default settings this goes to OpenRouter. Jev holds no tool, no version
control and no delivery credential.

## Limits

A selection can miss a check whose result depends on something the diff does
not show, such as an environment variable, a remote module or a file read at
run time. The threshold trades running unaffected checks against missing an
affected one; passing checks are evidence for the review, never an approval.
Collapsed links run their whole item, so a project with many small
collections runs more than it would with each link judged.

## Code map

| Part | Code |
| --- | --- |
| Questions, state, narrowing, threshold and version | [checkselect](../internal/checkselect) |
| The check run, its record and the selection's fallbacks | [service](../internal/service): `unit_checks.go`, `smart_checks.go`, `review_checks.go` |
| Recording, reuse, retries and cool-down shared by every judgment | [jev](../internal/jev) |
| The System One API client | [systemone](../internal/systemone) |
| Changed files from a diff | [gitdiff](../internal/gitdiff) |
| The local command | [smartcheck](../cmd/smartcheck) |

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
`dagger check --failfast` with the selected links, stopping at the first
failure. When the factory would fall back, it says why and runs every check;
when nothing changed, it runs nothing. It exits
with `dagger check`'s status.

| Flag | Default | Effect |
| --- | --- | --- |
| `-v`, `--verbose` | off | Print the changed files, each candidate's probability and the selection to standard error |
| `--dry-run` | off | Print the selected links, or `every check`, instead of running them |
| `--base` | `main` | Compare against the merge base of this revision |
| `--threshold` | `0.5` | The probability a check needs to be selected |
| `-C` | `.` | Run in another working tree |
| `--model`, `--url` | the `[jev]` defaults | Which Jev model to ask, and where |
| `--api-key-env` | `OPENROUTER_API_KEY` | The variable holding the API key; the command stops when it is unset |

Arguments after `--` go to `dagger check`. With `-v`, the output looks like:

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

Unlike a factory run, the command records nothing, makes one request without
retry or cool-down, and runs Dagger in your checkout with your environment
rather than in a fresh export with a scrubbed one.

## Tune the selection

1. Run `smartcheck --dry-run -v` on representative branches and compare the
   probabilities with the checks each change really affects. Pin `--model` to
   a versioned model, since thresholds are tuned against one.
2. Try other thresholds with `--threshold` until misses and over-selection
   are acceptable.
3. Change `checkselect.Threshold`, or the questions and state in
   `checkselect`, and bump `checkselect.Version`, so earlier recorded
   decisions stay as they were made.
4. Update the selection tests in `internal/checkselect`,
   `internal/service/unit_checks_test.go` and `cmd/smartcheck`.
