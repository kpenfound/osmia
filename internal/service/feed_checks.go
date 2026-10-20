package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var finalCheckPathPattern = regexp.MustCompile(`^final/checks-[0-9]+\.json$`)
var finalCheckStartPathPattern = regexp.MustCompile(`^final/checks-[0-9]+-attempt-[0-9]+-started\.json$`)
var unitSelectionPathPattern = regexp.MustCompile(`^units/[^/]+/checks-[0-9]+-selection\.json$`)

// feedChecks joins durable starts with results. It reads final result revisions
// separately: retries replace that document, but each attempt remains visible.
func feedChecks(repository *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []FeedEntry
	indexes := map[string]int{}
	latest := map[string]int{}
	for _, d := range docs {
		latest[d.ID] = max(latest[d.ID], d.Revision)
	}
	put := func(key string, entry FeedEntry) {
		if i, ok := indexes[key]; ok {
			out[i] = entry
		} else {
			indexes[key] = len(out)
			out = append(out, entry)
		}
	}
	decode := func(d trace.Document, v any) error {
		if err := json.Unmarshal([]byte(d.Content), v); err != nil {
			return fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		return nil
	}
	// Starts are read first so their time and selection survive a later result.
	for _, d := range docs {
		if d.Revision != latest[d.ID] {
			continue
		}
		switch {
		case unitSelectionPathPattern.MatchString(d.Path):
			var start UnitCheckSelection
			if err := decode(d, &start); err != nil {
				return nil, err
			}
			put(checkDocumentID(start.Unit, start.Run), FeedEntry{Kind: "unit_checks", At: d.At, UnitChecks: &FeedUnitChecks{Unit: start.Unit, Run: start.Run, State: "running", Selection: feedSelection(&start.Selection), StartedAt: start.StartedAt}})
		case finalCheckStartPathPattern.MatchString(d.Path):
			var start FinalCheckAttempt
			if err := decode(d, &start); err != nil {
				return nil, err
			}
			put(finalCheckAttemptDocument(start.Review, start.Attempt), FeedEntry{Kind: "final_checks", At: start.StartedAt, FinalChecks: &FeedFinalChecks{Review: start.Review, Attempt: start.Attempt, State: "running", Selection: feedSelection(start.Selection), StartedAt: &start.StartedAt}})
		}
	}
	for _, d := range docs {
		switch {
		case checkPathPattern.MatchString(d.Path) && d.Revision == latest[d.ID]:
			var run UnitCheckRun
			if err := decode(d, &run); err != nil {
				return nil, err
			}
			key := checkDocumentID(run.Unit, run.Run)
			entry := FeedEntry{Kind: "unit_checks", At: d.At, UnitChecks: &FeedUnitChecks{Unit: run.Unit, Run: run.Run, State: run.Status, Selection: feedSelection(run.Selection), Failed: run.Failed, Error: run.Error, StartedAt: run.StartedAt, FinishedAt: &run.FinishedAt}}
			if i, ok := indexes[key]; ok {
				entry.At = out[i].At
				entry.UnitChecks.StartedAt = out[i].UnitChecks.StartedAt
				entry.UnitChecks.Selection = out[i].UnitChecks.Selection
			}
			put(key, entry)
		case finalCheckPathPattern.MatchString(d.Path):
			var run FinalCheckRun
			if err := decode(d, &run); err != nil {
				return nil, err
			}
			// Results without start evidence (including preparation failures and
			// legacy attempts) remain distinct by document revision.
			key := finalCheckAttemptDocument(run.Review, run.Attempt)
			entry := FeedEntry{Kind: "final_checks", At: d.At, FinalChecks: &FeedFinalChecks{Review: run.Review, State: run.Status, Selection: feedSelection(run.Selection), Failed: run.Failed, Error: run.Error, FinishedAt: &run.FinishedAt}}
			if i, ok := indexes[key]; ok && run.Attempt > 0 {
				entry.At = out[i].At
				entry.FinalChecks.Attempt = run.Attempt
				entry.FinalChecks.StartedAt = out[i].FinalChecks.StartedAt
				if selection := out[i].FinalChecks.Selection; selection != nil {
					entry.FinalChecks.Selection = selection
				}
			} else {
				key = fmt.Sprintf("%s-result-%d", d.ID, d.Revision)
			}
			put(key, entry)
		case d.Path == finalReportPath:
			var report FinalReport
			if err := decode(d, &report); err != nil {
				return nil, err
			}
			review := &FeedFinalReview{Review: report.Review, State: report.Outcome, Reader: report.Reader, Turn: report.Turn, Failure: report.Failure, Blocking: countSeverity(report.Findings, FindingBlocking)}
			for _, criterion := range report.Criteria {
				if strings.TrimSpace(criterion.Gap) != "" {
					review.Gaps++
				}
			}
			put(fmt.Sprintf("review-%d", report.Review), FeedEntry{Kind: "final_review", At: d.At, FinalReview: review})
		}
	}
	return out, nil
}

func feedSelection(selection *CheckSelection) *FeedCheckSelection {
	if selection == nil {
		return nil
	}
	return &FeedCheckSelection{Mode: selection.Mode, Selected: len(selection.Links), Judged: selection.Candidates, Listed: selection.Listed, Reason: selection.Reason, Detail: selection.Detail, Description: selection.describe()}
}
