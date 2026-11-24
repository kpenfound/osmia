package delivery

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

var viewPath = regexp.MustCompile(`final/[A-Za-z0-9._/-]+`)

func mentionedPaths(text string) []string {
	var out []string
	for _, p := range viewPath.FindAllString(text, -1) {
		if p = strings.TrimRight(p, ".,;:"); !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func checkRecordHeader(repo *trace.Repository, id string, revision int) trace.Header {
	return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: checksActor, Cause: "review"}
}

func TestFinalCheckEvidenceNamesOnlyFilesTheReaderViewHolds(t *testing.T) {
	t.Parallel()
	pullRequest := FinalCheckRun{Review: 1, Operation: "review", Commit: reviewedCommit, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: pushedCommit, StatusURL: "https://dagger.cloud/acme/checks/github.com/acme/widgets@" + pushedCommit + "?check=3", Counts: &api.CheckCounts{Total: 3, Passed: 3}}
	with := func(f func(*FinalCheckRun)) FinalCheckRun {
		run := pullRequest
		f(&run)
		return run
	}
	cases := []struct {
		name  string
		run   FinalCheckRun
		files []string
	}{
		{name: "pull request passed", run: with(func(r *FinalCheckRun) { r.Status = "passed" }), files: []string{"final/checks-1.json"}},
		{name: "pull request failed", run: with(func(r *FinalCheckRun) {
			r.Status = "failed"
			r.Counts = &api.CheckCounts{Total: 3, Passed: 2, Failed: 1}
			r.FailedChecks = []api.CheckFailure{{Name: "test:a", TraceID: "trace-a"}}
			r.Failed = []string{"test:a"}
		}), files: []string{"final/checks-1.json"}},
		{name: "pull request incomplete", run: with(func(r *FinalCheckRun) {
			r.Status, r.Error = ChecksIncomplete, "the completed run is not a pass: the run reports no checks"
		}), files: []string{"final/checks-1.json"}},
		{name: "pull request continued", run: with(func(r *FinalCheckRun) { r.Status, r.Counts, r.StatusURL = ChecksContinued, nil, "" }), files: []string{"final/checks-1.json"}},
		{name: "maintenance review", run: FinalCheckRun{Review: 1, Operation: "review", Commit: reviewedCommit, ExitCode: -1, Error: "a review of a delivered workstream pushes nothing, so no pull request checks ran for it"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, repo := deliveryTrace(t)
			var docs []trace.Document
			if c.run.Status != "" {
				data, err := json.MarshalIndent(c.run, "", "  ")
				fixturekit.Must(t, err)
				docs = append(docs, trace.Document{Header: checkRecordHeader(repo, FinalChecksDocument(1), 1), Path: FinalChecksPath(1), Content: string(data) + "\n"})
			}
			if len(docs) > 0 {
				fixturekit.Must(t, repo.RecordDocuments(context.Background(), docs))
			}
			staged, err := FinalCheckFiles(repo, stream, FinalReport{Review: 1, Commit: reviewedCommit})
			fixturekit.Must(t, err)
			var view []string
			for name := range staged {
				view = append(view, name)
			}
			slices.Sort(view)
			if !slices.Equal(view, c.files) {
				t.Fatalf("the view holds %v, want %v", view, c.files)
			}
			evidence := FinalCheckEvidence(c.run)
			for _, path := range mentionedPaths(evidence) {
				if _, ok := staged[path]; !ok {
					t.Errorf("the evidence names %s, which the view lacks %v:\n%s", path, view, evidence)
				}
			}
			if listed := slices.Sorted(slices.Values(FinalCheckPaths(c.run))); !slices.Equal(listed, c.files) {
				t.Errorf("the prompt lists %v, the view holds %v", listed, c.files)
			}
			if c.run.Status != "" && staged[FinalChecksPath(1)] == "" {
				t.Errorf("the record is empty in the view")
			}
			if c.run.Source == api.CheckSourcePullRequest && c.run.StatusURL != "" && !strings.Contains(evidence, c.run.StatusURL) && c.run.Status != ChecksContinued {
				t.Errorf("the evidence lacks the status link %s:\n%s", c.run.StatusURL, evidence)
			}
			if c.run.Status == "" && len(mentionedPaths(evidence)) != 0 {
				t.Errorf("a result with no record names files: %s", evidence)
			}
		})
	}
}

func TestFinalCheckFilesHoldOnlyTheLatestRecordOfTheReviewedCommit(t *testing.T) {
	t.Parallel()
	_, repo := deliveryTrace(t)
	record := func(revision int, commit, status string) trace.Document {
		data, err := json.Marshal(FinalCheckRun{Review: 1, Commit: commit, Status: status})
		fixturekit.Must(t, err)
		return trace.Document{Header: checkRecordHeader(repo, FinalChecksDocument(1), revision), Path: FinalChecksPath(1), Content: string(data)}
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{record(1, reviewedCommit, ChecksIncomplete)}))
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{record(2, reviewedCommit, "passed")}))
	files, err := FinalCheckFiles(repo, stream, FinalReport{Review: 1, Commit: reviewedCommit})
	fixturekit.Must(t, err)
	if len(files) != 1 || !strings.Contains(files[FinalChecksPath(1)], `"status":"passed"`) {
		t.Fatalf("files %v", files)
	}
	other, err := FinalCheckFiles(repo, stream, FinalReport{Review: 1, Commit: pushedCommit})
	fixturekit.Must(t, err)
	none, err := FinalCheckFiles(repo, stream, FinalReport{Review: 2, Commit: reviewedCommit})
	fixturekit.Must(t, err)
	if len(other) != 0 || len(none) != 0 {
		t.Fatalf("another commit's record %v, another review's %v", other, none)
	}
}

func TestFinalPromptCarriesTheCheckResultBeforeTheCriteria(t *testing.T) {
	t.Parallel()
	run := FinalCheckRun{Review: 2, Commit: reviewedCommit, Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: pushedCommit, Status: "passed", StatusURL: "https://dagger.cloud/acme/checks/github.com/acme/widgets@" + pushedCommit + "?check=3", Counts: &api.CheckCounts{Total: 3, Passed: 3},
		StartedAt: fixturekit.DemoStart, FinishedAt: fixturekit.DemoStart.Add(time.Minute)}
	report := FinalReport{Review: 2, Commit: reviewedCommit, Upstream: &seal.Base{Remote: "origin", Branch: "main", Commit: "abc"}}
	coverage := []FinalCriterion{{Criterion: "spec#1", Evidence: "unit one"}}
	prompt := FinalPrompt(report, run, nil, coverage, FinalReport{})
	evidence := FinalCheckEvidence(run)
	section, view, criteria := strings.Index(prompt, "Check result:\n"+evidence), strings.Index(prompt, "Your view holds:"), strings.Index(prompt, "Every criterion and the merged units")
	if section < 0 || view < 0 || criteria < 0 || section > criteria {
		t.Fatalf("check result at %d, view list at %d, criteria at %d:\n%s", section, view, criteria, prompt)
	}
	if strings.Count(prompt, evidence) != 1 || strings.HasSuffix(strings.TrimSpace(prompt), strings.TrimSpace(evidence)) || strings.Contains(prompt[criteria:], "Dagger Cloud ran the project's checks") {
		t.Fatalf("the check result is not only in its own section:\n%s", prompt)
	}
	if list := prompt[view:criteria]; !strings.Contains(list, "- final/checks-2.json:") || strings.Contains(list, "-output") {
		t.Fatalf("the view list %q", list)
	}
	if !strings.Contains(prompt, run.StatusURL) {
		t.Fatalf("the prompt lacks the status link:\n%s", prompt)
	}
	maintenance := FinalCheckRun{Review: 2, Commit: reviewedCommit, ExitCode: -1, Error: "a review of a delivered workstream pushes nothing, so no pull request checks ran for it"}
	if list := FinalPrompt(report, maintenance, nil, coverage, FinalReport{}); strings.Contains(list, "final/checks") {
		t.Fatalf("a maintenance review lists a check file:\n%s", list)
	}
}
