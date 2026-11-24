package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/trace"
)

var finalCheckPathPattern = regexp.MustCompile(`^final/checks-[0-9]+\.json$`)
var unitSelectionPathPattern = regexp.MustCompile(`^units/[^/]+/checks-[0-9]+-selection\.json$`)

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
	for _, d := range docs {
		if d.Revision != latest[d.ID] {
			continue
		}
		switch {
		case unitSelectionPathPattern.MatchString(d.Path):
			var start execution.UnitCheckSelection
			if err := decode(d, &start); err != nil {
				return nil, err
			}
			put(checkDocumentID(start.Unit, start.Run), FeedEntry{Kind: "unit_checks", At: d.At, UnitChecks: &FeedUnitChecks{TraceURL: start.TraceURL, Unit: start.Unit, Run: start.Run, State: "running", Selection: FeedSelection(&start.Selection), StartedAt: start.StartedAt}})
		case sessionCheckStartPattern.MatchString(d.Path):
			var start SessionCheckStart
			if err := decode(d, &start); err != nil {
				return nil, err
			}
			put(sessionCheckDocumentID(start.Run), FeedEntry{Kind: "session_checks", At: start.StartedAt, SessionChecks: &api.FeedSessionChecks{Run: start.Run, Commit: start.Commit, Kind: start.Kind, State: "running", Selection: FeedSelection(start.Selection), TraceURL: start.TraceURL, StartedAt: start.StartedAt}})
		}
	}
	for _, d := range docs {
		switch {
		case execution.CheckPathPattern.MatchString(d.Path) && d.Revision == latest[d.ID]:
			var run UnitCheckRun
			if err := decode(d, &run); err != nil {
				return nil, err
			}
			key := checkDocumentID(run.Unit, run.Run)
			entry := FeedEntry{Kind: "unit_checks", At: d.At, UnitChecks: &FeedUnitChecks{TraceURL: run.TraceURL, Unit: run.Unit, Run: run.Run, State: run.Status, Selection: FeedSelection(run.Selection), Failed: run.Failed, Flaky: run.Flaky, Error: run.Error, StartedAt: run.StartedAt, FinishedAt: &run.FinishedAt}}
			if i, ok := indexes[key]; ok {
				entry.At = out[i].At
				entry.UnitChecks.StartedAt = out[i].UnitChecks.StartedAt
				if entry.UnitChecks.TraceURL == "" {
					entry.UnitChecks.TraceURL = out[i].UnitChecks.TraceURL
				}
				if entry.UnitChecks.Selection == nil {
					entry.UnitChecks.Selection = out[i].UnitChecks.Selection
				}
			}
			put(key, entry)
		case finalCheckPathPattern.MatchString(d.Path):
			var run FinalCheckRun
			if err := decode(d, &run); err != nil {
				return nil, err
			}
			entry := FeedEntry{Kind: "final_checks", At: d.At, FinalChecks: &FeedFinalChecks{TraceURL: run.TraceURL, Review: run.Review, State: run.Status, Source: run.Source, PullRequest: run.PullRequest, Push: run.Push, StatusURL: run.StatusURL, Counts: run.Counts, FailedChecks: run.FailedChecks, Reruns: run.Reruns, Failed: run.Failed, Flaky: run.Flaky, Error: run.Error, FinishedAt: &run.FinishedAt}}
			put(fmt.Sprintf("%s-result-%d", d.ID, d.Revision), entry)
		case sessionCheckRecordPattern.MatchString(d.Path) && d.Revision == latest[d.ID]:
			var run SessionCheckRun
			if err := decode(d, &run); err != nil {
				return nil, err
			}
			entry := FeedEntry{Kind: "session_checks", At: run.FinishedAt, SessionChecks: &api.FeedSessionChecks{Run: run.Run, Commit: run.Commit, Kind: run.Kind, State: run.Status, Selection: FeedSelection(run.Selection), Failed: run.Failed, Flaky: run.Flaky, Error: run.Error, TraceURL: run.TraceURL, StartedAt: run.StartedAt, FinishedAt: &run.FinishedAt}}
			if i, ok := indexes[d.ID]; ok && entry.SessionChecks.TraceURL == "" {
				entry.SessionChecks.TraceURL = out[i].SessionChecks.TraceURL
			}
			put(d.ID, entry)
		case d.Path == finalReportPath:
			var report FinalReport
			if err := decode(d, &report); err != nil {
				return nil, err
			}
			review := &api.FeedFinalReview{Review: report.Review, State: report.Outcome, Reader: report.Reader, Turn: report.Turn, Failure: report.Failure, Blocking: delivery.CountSeverity(report.Findings, FindingBlocking)}
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

func FeedSelection(selection *CheckSelection) *FeedCheckSelection {
	if selection == nil {
		return nil
	}
	return &FeedCheckSelection{FallbackPicks: selection.FallbackPicks, Mode: selection.Mode, Selected: len(selection.Links), Judged: selection.Candidates, Listed: selection.Listed, Reason: selection.Reason, Detail: selection.Detail, Description: checkrun.DescribeSelection(*selection)}
}
