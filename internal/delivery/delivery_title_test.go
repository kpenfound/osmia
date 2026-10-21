package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func draftFixture(t *testing.T) (*config.Config, *trace.Repository, *Controller, FinalReport) {
	t.Helper()
	cfg, repo := deliveryTrace(t)
	cfg.Project.Upstream = "dagger/dagger"
	drafts := New(Dependencies{PendingMergeOrderReferences: func(*trace.Repository, config.WorkstreamID) ([]string, error) { return nil, nil }})
	return cfg, repo, drafts, FinalReport{Spec: 1, Summary: "Resumable uploads", Commit: strings.Repeat("c", 40)}
}

// TestDraftDeliveryTitleIsTheWorkstreamStatusGoal shows the drafted title is
// the latest chief-of-staff status goal, not text taken from the final
// review's summary (spec#1).
func TestDraftDeliveryTitleIsTheWorkstreamStatusGoal(t *testing.T) {
	t.Parallel()
	cfg, repository, drafts, report := draftFixture(t)
	defer repository.Close()
	fixturekit.SetStatusGoal(t, repository, stream, "Ship resumable uploads to users", time.Now().UTC())
	title, _, err := drafts.DraftDelivery(cfg, repository, stream, report)
	fixturekit.Must(t, err)
	if title != "Ship resumable uploads to users" {
		t.Fatalf("title %q, want the status goal, not the final review's summary %q", title, report.Summary)
	}
}

// TestDraftDeliveryTitleFallsBackToSpecHeadingWithoutAGoal shows that a
// workstream with no status goal gets a meaningful fallback title (the
// sealed spec's first heading) before the bare workstream ID (spec#2).
func TestDraftDeliveryTitleFallsBackToSpecHeadingWithoutAGoal(t *testing.T) {
	t.Parallel()
	cfg, repository, drafts, report := draftFixture(t)
	defer repository.Close()
	title, _, err := drafts.DraftDelivery(cfg, repository, stream, report)
	fixturekit.Must(t, err)
	if title != "Resumable uploads" {
		t.Fatalf("title %q, want the spec's first heading", title)
	}
	if title == "Workstream "+string(stream) {
		t.Fatal("fell all the way back to the bare workstream ID")
	}
}

// TestDraftDeliveryBodyIsASpecSummaryWithNoWorkAccounting shows the drafted
// body is a short prose summary of the sealed spec, with no per-criterion
// evidence or gap lines, no landed-unit list and no reviewed commit hash
// (spec#3).
func TestDraftDeliveryBodyIsASpecSummaryWithNoWorkAccounting(t *testing.T) {
	t.Parallel()
	cfg, repository, drafts, report := draftFixture(t)
	defer repository.Close()
	_, body, err := drafts.DraftDelivery(cfg, repository, stream, report)
	fixturekit.Must(t, err)
	if !strings.Contains(body, "An interrupted upload resumes from its last acknowledged chunk") {
		t.Fatalf("body %q does not hold the spec's opening prose", body)
	}
	for _, unwanted := range []string{report.Commit, "spec#1", "spec#2", "Landed units", "resume.go", "dedupe.go", "Evidence:", "Gap:"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body %q still accounts for the work with %q", body, unwanted)
		}
	}
}

// TestDraftDeliveryClosesAnIssueInTheTargetRepository shows that a
// workstream handed in from a GitHub issue in the repository the pull
// request targets gets a "closes #N" reference (spec#4).
func TestDraftDeliveryClosesAnIssueInTheTargetRepository(t *testing.T) {
	t.Parallel()
	cfg, repository, drafts, report := draftFixture(t)
	defer repository.Close()
	fixturekit.RecordHanded(t, repository, stream, "https://github.com/dagger/dagger/issues/42", time.Now().UTC())
	_, body, err := drafts.DraftDelivery(cfg, repository, stream, report)
	fixturekit.Must(t, err)
	if !strings.Contains(body, "closes #42") {
		t.Fatalf("body %q does not close the same-repository issue", body)
	}
}

// TestDraftDeliveryClosesAnIssueInAnotherRepository shows that a workstream
// handed in from a GitHub issue in a different repository than the pull
// request targets gets a "closes OWNER/REPO#N" reference (spec#5).
func TestDraftDeliveryClosesAnIssueInAnotherRepository(t *testing.T) {
	t.Parallel()
	cfg, repository, drafts, report := draftFixture(t)
	defer repository.Close()
	fixturekit.RecordHanded(t, repository, stream, "https://github.com/acme/other/issues/7", time.Now().UTC())
	_, body, err := drafts.DraftDelivery(cfg, repository, stream, report)
	fixturekit.Must(t, err)
	if !strings.Contains(body, "closes acme/other#7") {
		t.Fatalf("body %q does not close the other-repository issue", body)
	}
}

// TestDraftDeliveryNoClosingReferenceForFileOrStdin shows that a workstream
// handed in from a file or stdin gets no closing issue reference (spec#6).
func TestDraftDeliveryNoClosingReferenceForFileOrStdin(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"file:/home/owner/design.md", "stdin"} {
		t.Run(source, func(t *testing.T) {
			cfg, repository, drafts, report := draftFixture(t)
			defer repository.Close()
			fixturekit.RecordHanded(t, repository, stream, source, time.Now().UTC())
			_, body, err := drafts.DraftDelivery(cfg, repository, stream, report)
			fixturekit.Must(t, err)
			if strings.Contains(body, "closes") {
				t.Fatalf("body %q closes an issue for a %s hand-in", body, source)
			}
		})
	}
}
