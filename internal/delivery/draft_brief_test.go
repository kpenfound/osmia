package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// recordFinalReportRevision records revision n of the final report document of
// member and returns the report the drafts present for it.
func recordFinalReportRevision(t *testing.T, repo *trace.Repository, member config.WorkstreamID, n int, report FinalReport) FinalReport {
	t.Helper()
	report.Review, report.Commit = n, strings.Repeat(fmt.Sprint(n), 40)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: FinalReportDocument, Revision: n, Project: repo.Project(), Workstream: member, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}, Path: "final/report.json", Content: "{}\n"}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	return report
}

// recordBriefFor records brief as the next revision of its review's brief
// document of member.
func recordBriefFor(t *testing.T, repo *trace.Repository, member config.WorkstreamID, brief Brief) {
	t.Helper()
	content, err := json.Marshal(brief)
	fixturekit.Must(t, err)
	revision, err := nextRevision(repo, member, BriefDocument(brief.Review))
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: BriefDocument(brief.Review), Revision: revision, Project: repo.Project(), Workstream: member, At: fixturekit.DemoStart, Actor: chiefActor, Cause: "fixture"}, Path: BriefPath(brief.Review), Content: string(content) + "\n"}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
}

func logAssumptionFor(t *testing.T, repo *trace.Repository, text string) {
	t.Helper()
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, fixturekit.DemoStart, fixturekit.ServiceActor)
	fixturekit.Must(t, err)
	scope := fixturekit.ClaimTurn(t, repo, t.TempDir(), stream, trace.ChiefOfStaff, trace.ChiefOfStaff, "assumption", fixturekit.DemoStart)
	_, err = repo.LogAssumption(ctx, trace.ChiefOfStaff, scope, text, fixturekit.DemoStart)
	fixturekit.Must(t, err)
}

func draftWith(t *testing.T, drafts *Controller, cfg *config.Config, repo *trace.Repository, member config.WorkstreamID, report FinalReport) (string, string) {
	t.Helper()
	title, body, err := drafts.DraftDelivery(cfg, repo, member, report)
	fixturekit.Must(t, err)
	return title, body
}

// The Before you ship block follows the dependency sentence, precedes the
// assumptions, and is filtered like the rest of the body.
func TestDraftDeliveryHoldsTheBriefsBeforeYouShipItems(t *testing.T) {
	t.Parallel()
	cfg, repo, _, report := draftFixture(t)
	defer repo.Close()
	drafts := New(Dependencies{PendingMergeOrderReferences: func(*trace.Repository, config.WorkstreamID) ([]string, error) { return []string{"acme/lib#4"}, nil }})
	report = recordFinalReportRevision(t, repo, stream, 1, report)
	logAssumptionFor(t, repo, "Used the existing retry backoff.")
	_, without := draftWith(t, drafts, cfg, repo, stream, report)
	const link = "https://dagger.cloud/private-org/traces/check123"
	recordBriefFor(t, repo, stream, Brief{Review: 1, ReviewRevision: 1, Commit: report.Commit, Text: "Uploads resume.", BeforeYouShip: []string{"Set UPLOAD_KEY before deploying " + link, "Restart the worker"}, Screenshots: []string{"img_shot"}})
	_, body := draftWith(t, drafts, cfg, repo, stream, report)
	if strings.Contains(body, "dagger.cloud") {
		t.Fatalf("private trace link in body:\n%s", body)
	}
	block := "\nBefore you ship:\n- Set UPLOAD_KEY before deploying " + "\n- Restart the worker\n"
	dependency, block0, assumptions := strings.Index(body, "This depends on"), strings.Index(body, "\nBefore you ship:\n"), strings.Index(body, "\nAssumptions:\n")
	if !strings.Contains(body, "- Set UPLOAD_KEY before deploying") || !strings.Contains(body, "\n- Restart the worker\n") || dependency < 0 || block0 < dependency || assumptions < block0 {
		t.Fatalf("block misplaced (%q):\n%s", block, body)
	}
	if strings.Contains(without, "Before you ship") || !strings.HasPrefix(body[assumptions:], without[strings.Index(without, "\nAssumptions:\n"):]) {
		t.Fatalf("the rest of the body changed:\n%s\n---\n%s", without, body)
	}
}

// Without a brief, with a brief only for an earlier review, or with a brief
// without items, the title and body are exactly those of a trace with no brief;
// the brief's other fields never enter them.
func TestDraftDeliveryIsUnchangedWithoutApplicableBriefItems(t *testing.T) {
	t.Parallel()
	cfg, repo, drafts, report := draftFixture(t)
	defer repo.Close()
	report = recordFinalReportRevision(t, repo, stream, 1, report)
	logAssumptionFor(t, repo, "Used the existing retry backoff.")
	wantTitle, wantBody := draftWith(t, drafts, cfg, repo, stream, report)
	const text, shot = "Uploads now resume from the last chunk.", "img_resume_shot"
	check := func(name string) {
		t.Helper()
		title, body := draftWith(t, drafts, cfg, repo, stream, report)
		if title != wantTitle || body != wantBody {
			t.Fatalf("%s changed the draft:\n%q\n%q\nwant\n%q\n%q", name, title, body, wantTitle, wantBody)
		}
	}
	check("no brief")
	recordBriefFor(t, repo, stream, Brief{Review: 1, ReviewRevision: 1, Commit: report.Commit, Text: text, Screenshots: []string{shot}})
	check("a brief without items")
	recordBriefFor(t, repo, stream, Brief{Review: 1, ReviewRevision: 1, Commit: report.Commit, Text: text, BeforeYouShip: []string{}, Screenshots: []string{shot}})
	check("a brief with an empty item list")
	// A newer final review makes the earlier brief stale, items and all.
	recordBriefFor(t, repo, stream, Brief{Review: 1, ReviewRevision: 1, Commit: report.Commit, Text: text, BeforeYouShip: []string{"Rotate the key"}})
	report = recordFinalReportRevision(t, repo, stream, 2, report)
	wantTitle, wantBody = draftWith(t, drafts, cfg, repo, stream, report)
	if strings.Contains(wantBody, "Rotate the key") || strings.Contains(wantBody, "Before you ship") {
		t.Fatalf("a stale brief reached the body:\n%s", wantBody)
	}
	// A brief naming the right review under another report revision or commit is not this review's.
	recordBriefFor(t, repo, stream, Brief{Review: 2, ReviewRevision: 1, Commit: report.Commit, Text: text, BeforeYouShip: []string{"Wrong revision"}})
	recordBriefFor(t, repo, stream, Brief{Review: 2, ReviewRevision: 2, Commit: strings.Repeat("f", 40), Text: text, BeforeYouShip: []string{"Wrong commit"}})
	check("a brief for another revision or commit")
	// The main text and screenshots stay out even with items.
	recordBriefFor(t, repo, stream, Brief{Review: 2, ReviewRevision: 2, Commit: report.Commit, Text: text, BeforeYouShip: []string{"Rotate the key"}, Screenshots: []string{shot}})
	title, body := draftWith(t, drafts, cfg, repo, stream, report)
	if !strings.Contains(body, "- Rotate the key\n") || title != wantTitle {
		t.Fatalf("title %q body:\n%s", title, body)
	}
	for _, leaked := range []string{text, shot} {
		if strings.Contains(title, leaked) || strings.Contains(body, leaked) {
			t.Fatalf("%q reached the draft:\n%s", leaked, body)
		}
	}
}

// Two members of a shared chief keep their own briefs: each body holds only
// its own items.
func TestDraftDeliveryOfSharedChiefMembersUsesEachMembersOwnBrief(t *testing.T) {
	t.Parallel()
	cfg, repo, drafts, report := draftFixture(t)
	defer repo.Close()
	other := fixturekit.Quiet
	src, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var docs []trace.Document
	for _, d := range src {
		if d.ID == plan.SpecDocument || d.ID == plan.PlanDocument {
			d.Workstream = other
			docs = append(docs, d)
		}
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), docs))
	first := recordFinalReportRevision(t, repo, stream, 1, report)
	second := recordFinalReportRevision(t, repo, other, 1, report)
	recordBriefFor(t, repo, stream, Brief{Review: 1, ReviewRevision: 1, Commit: first.Commit, Text: "One.", BeforeYouShip: []string{"Item of the first member"}})
	recordBriefFor(t, repo, other, Brief{Review: 1, ReviewRevision: 1, Commit: second.Commit, Text: "Two.", BeforeYouShip: []string{"Item of the second member"}})
	_, a := draftWith(t, drafts, cfg, repo, stream, first)
	_, b := draftWith(t, drafts, cfg, repo, other, second)
	if !strings.Contains(a, "- Item of the first member\n") || strings.Contains(a, "second member") || !strings.Contains(b, "- Item of the second member\n") || strings.Contains(b, "first member") {
		t.Fatalf("first:\n%s\nsecond:\n%s", a, b)
	}
}
