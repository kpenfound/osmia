package delivery

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kinds"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/trace"
)

// ClimbResults reads a concluded Hill Climb workstream's results from its
// trace: the baseline, the final before and after the one rebase, and the
// attempts the climb kept. It reports false until the conclusion has recorded
// both finals.
func ClimbResults(repository *trace.Repository, stream config.WorkstreamID, spec climb.Spec) (kinds.ClimbResults, bool, error) {
	rec, found, err := climb.ReadConclusion(repository, stream)
	if err != nil || !found || !rec.Concluded() {
		return kinds.ClimbResults{}, false, err
	}
	attempts, err := climb.ReadAttempts(repository, stream)
	if err != nil {
		return kinds.ClimbResults{}, false, err
	}
	r := kinds.ClimbResults{Goal: spec.Goal, Metric: spec.Metric.Name, Unit: rec.Unit, Direction: string(spec.Metric.Direction), MinImprovement: spec.Metric.MinImprovement,
		Baseline: rec.Baseline, BaselineCommit: rec.BaselineCommit, Attempts: len(attempts), UpstreamBranch: rec.Target.Branch, UpstreamCommit: rec.Target.Commit,
		Before: kinds.ClimbFinal{Commit: rec.Pre.Commit, Score: rec.Pre.Score, Valid: rec.Pre.Valid, Reason: rec.Pre.Reason},
		After:  kinds.ClimbFinal{Commit: rec.Post.Commit, Score: rec.Post.Score, Valid: rec.Post.Valid, Reason: rec.Post.Reason}}
	epoch := 1
	if approval, ok, err := climb.ReadApproval(repository, stream); err != nil {
		return kinds.ClimbResults{}, false, err
	} else if ok {
		epoch = approval.EpochNumber()
	}
	for _, a := range attempts {
		if a.Outcome != climb.OutcomeKept || a.Scores == nil {
			continue
		}
		if a.EpochNumber() != epoch {
			r.Earlier = append(r.Earlier, kinds.ClimbEarlier{Number: a.Number, Commit: a.Commit, Hypothesis: a.Hypothesis, SpecRevision: a.SpecRevision})
			continue
		}
		k := kinds.ClimbKept{Number: a.Number, Commit: a.Commit, Hypothesis: a.Hypothesis, Score: a.Scores.Primary, Change: a.Scores.Primary - a.Best, ChangePercent: a.ChangePct}
		if a.Change != nil {
			k.Change = *a.Change
		}
		r.Kept = append(r.Kept, k)
	}
	return r, true, nil
}

// kindSection is the pull-request section the workstream's kind writes: the
// results table of a concluded Hill Climb, and nothing for a kind with no
// section.
func kindSection(repository *trace.Repository, stream config.WorkstreamID, specContent string) (string, error) {
	kind, _, ok, err := intake.WorkstreamKind(repository, stream)
	if err != nil || !ok || kind.PullRequestSection == nil {
		return "", err
	}
	pr := kinds.PullRequest{Workstream: string(stream)}
	if kind.Name == kinds.HillClimb {
		spec, err := climb.Parse([]byte(specContent), climb.Defaults{})
		if err != nil {
			return "", fmt.Errorf("the sealed climb spec does not parse: %w", err)
		}
		results, found, err := ClimbResults(repository, stream, spec)
		if err != nil || !found {
			return "", err
		}
		pr.Climb = &results
	}
	return strings.TrimSpace(kind.PullRequestSection(pr)), nil
}

// climbEvidence is what a Hill Climb workstream's final reader finds under
// climb/ in its view: the results the pull request carries, the conclusion and
// every recorded attempt.
func (a *FinalReviewer) climbEvidence(stream config.WorkstreamID) (map[string]string, error) {
	files := map[string]string{}
	rec, found, err := climb.ReadConclusion(a.Repository, stream)
	if err != nil {
		return nil, err
	}
	if found {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return nil, err
		}
		files["climb/conclusion.json"] = string(data) + "\n"
	}
	attempts, err := climb.ReadAttempts(a.Repository, stream)
	if err != nil {
		return nil, err
	}
	for _, at := range attempts {
		data, err := json.MarshalIndent(at, "", "  ")
		if err != nil {
			return nil, err
		}
		files[trace.ClimbAttemptPath(at.Number)] = string(data) + "\n"
	}
	return files, nil
}

// ClimbFinalSystemPrompt is the system prompt of the final reader of a Hill
// Climb workstream.
func ClimbFinalSystemPrompt(p config.Project) string {
	return fmt.Sprintf(`You are the committee member who gives a concluded Hill Climb its final read for the %[1]s project (%[2]s).

## Purpose and setting
The climb improved one measured number by attempts: the mason proposed each change, the service measured it and Go alone kept or discarded it by arithmetic, and a reviewer read every kept attempt when it landed. Go decides because a score is a measurement and the decision is arithmetic over it, so no one's reading of a result can change the outcome. The climb branch was then rebased once onto upstream and the evaluator measured the rebased commit again. You read the branch as a whole against the sealed climb spec before the owner decides whether it is delivered. You judge whether the branch delivers the climb spec's goal: the kept changes serve the goal without gaming the metric (special-casing the workload, caching across runs, skipping work the oracle does not check, reading the evaluator's inputs), the guardrails and protected paths the climb spec names are respected, the results the pull request reports match the attempt records, and no charter rule is broken. You never decide whether a score improved: the recorded scores are the service's measurements and Go's arithmetic, so a score that moved with the rebase is not a finding by itself. When the climb kept no attempt, the branch holds only the evaluator and the files it reads; judge it as that.

## Tools and gates
- Your view is read-only. The branch, the sealed climb spec and the attempt records are files in it, and the diff tool reads the change the branch makes to upstream.
- %[3]s records your report. It is the only way your findings reach the owner.
- %[4]s
- Upstream's own changes are not the climb's.

## Your part of the state machine
The final review runs once the climb has concluded and its branch was rebased and measured again. The chief of staff triages each blocking finding of your report, and the owner decides whether the work is delivered; advisory findings are noted for the owner to read.

## Output format
One report recorded with %[3]s: a summary and your findings. A finding is blocking only when the branch as a whole fails the climb spec's goal or breaks a charter rule, because the chief of staff must triage every blocking finding; anything else worth the owner's attention is advisory. Report nothing you cannot point at in the branch, so that the owner can check each finding.

## When you are done
You are done when %[3]s has recorded your report, with an empty findings list when there are none. End your turn then.`, p.Name, p.Upstream, FinalReportTool, finalCheckReviewInstructions)
}

// ClimbFinalPrompt is the prompt of a Hill Climb workstream's final review.
func ClimbFinalPrompt(report FinalReport, checks FinalCheckRun, driftNotes []string, previous FinalReport, results string) string {
	notes := ""
	if len(driftNotes) > 0 {
		notes = "\n" + strings.Join(driftNotes, "\n") + "\n"
	}
	earlier := ""
	if previous.Review > 0 {
		earlier = fmt.Sprintf("\nFinal review %d read %s earlier. Start from what changed since then, and do not repeat its advisory findings.\n", previous.Review, previous.Commit)
	}
	held := ""
	for _, path := range FinalCheckPaths(checks) {
		if path == FinalChecksPath(checks.Review) {
			held += fmt.Sprintf("- %s: the recorded check result of the section above, as the service recorded it.\n", path)
		} else {
			held += fmt.Sprintf("- %s: the complete captured check output that record names.\n", path)
		}
	}
	held += finalTraceEvidence(checks)
	return fmt.Sprintf(`Final review %d of the concluded climb branch at commit %s, rebased once onto %s/%s at %s. The climb spec is revision %d, sealed as seal %d; the charter is revision %d.

Check result:
%s

Results the pull request reports:
%s

Your view holds:
- branch/: every tracked file of the reviewed commit.
- climb-spec.json: the sealed climb spec: the goal, the evaluator, the metric, the guardrails, the protected paths and the stop rules.
- climb/conclusion.json and climb/attempts/<n>.json: the conclusion with the finals before and after the rebase, and every recorded attempt with its hypothesis, scores, outcome and measurement.
- charter.md: the owner's optional guidance for contributing to this project, in addition to the repository's own contributor documents; it may be empty.
- context.md: local decisions and notices, plus optional scoped external memory.
%s%s
Call %s with change branch to read the whole change the branch makes to upstream: files_only lists the changed files, and paths and lines narrow the diff.
%s
Read the branch as a whole against the climb spec: what the kept attempts changed, whether the evaluator and the files it reads are untouched, and whether the results above follow from the attempt records. Then call %s once with your summary and findings, an empty list when there are none, and end your turn.
`, report.Review, report.Commit, report.Upstream.Remote, report.Upstream.Branch, report.Upstream.Commit, report.Spec, report.Seal, report.Charter, FinalCheckEvidence(checks), results, held, notes, reviewtools.DiffToolName, earlier, FinalReportTool)
}
