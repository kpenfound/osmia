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

func TestDraftDeliveryOmitsPrivateCheckTraceLinks(t *testing.T) {
	t.Parallel()
	cfg, repo, drafts, report := draftFixture(t)
	defer repo.Close()
	const link = "https://dagger.cloud/private-org/traces/check123"
	fixturekit.SetStatusGoal(t, repo, stream, "Resumable uploads "+link, time.Now().UTC())
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	for _, doc := range docs {
		if doc.Path == "spec.md" {
			doc.Revision++
			doc.Content = "# Resumable uploads\n\nResume uploads. Check evidence: " + link + "\n"
			report.Spec = doc.Revision
			fixturekit.Must(t, repo.RecordDocuments(t.Context(), []trace.Document{doc}))
		}
	}
	title, body, err := drafts.DraftDelivery(cfg, repo, stream, report)
	fixturekit.Must(t, err)
	if strings.Contains(title, "dagger.cloud") || strings.Contains(body, "dagger.cloud") || !strings.Contains(body, "Resume uploads") {
		t.Fatalf("private delivery text: %q %q", title, body)
	}
	if _, reason, err := drafts.DeliveryGate(t.Context(), repo, stream, "Check evidence: "+link); err != nil || !strings.Contains(reason, "private Dagger Cloud") {
		t.Fatalf("publication gate: %q %v", reason, err)
	}
}
