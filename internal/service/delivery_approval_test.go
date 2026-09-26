package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/vcs"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func TestDeliveryRejectsFinalReportOnPreviousUpstreamBase(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	reviewer := &finalReviewer{s: f.s, repository: repository}
	must(t, reviewer.request(ctx, stream))
	if ops := finalOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("fresh report prompted another final read: %+v", ops)
	}
	latest, doc, found, err := seal.Latest(repository, stream)
	must(t, err)
	if !found {
		t.Fatal("no seal")
	}
	latest.Base.Commit = strings.Repeat("a", 40)
	content, err := seal.Encode(latest)
	must(t, err)
	doc.Revision++
	doc.Content = string(content)
	must(t, repository.RecordDocuments(ctx, []trace.Document{doc}))
	if _, reason, err := reviewer.finalGate(ctx, stream); err != nil || !strings.Contains(reason, "upstream base changed") {
		t.Fatalf("final gate accepted the old upstream base: %q %v", reason, err)
	}
	must(t, reviewer.request(ctx, stream))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("stale report did not prompt a fresh final read: %+v", ops)
	}
	if _, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: report.Review, Commit: report.Commit}); api == nil || api.Code != Conflict {
		t.Fatalf("delivery accepted the old upstream base: %v", api)
	}
}

func deliveryFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	return deliveryFixtureWith(t, "")
}

// deliveryFixtureWith is deliveryFixture with extra configuration appended.
func deliveryFixtureWith(t *testing.T, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport) {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, "delivery", extra)
	ctx := context.Background()
	mergeDirectly(t, f, repository, stream, "resume", map[string]string{"resume.go": "package demo\n"})
	mergeDirectly(t, f, repository, stream, "dedupe", map[string]string{"dedupe.go": "package demo\n"})
	must(t, a.Pass(ctx))
	in, sealed, err := a.governing(ctx, stream)
	must(t, err)
	report := FinalReport{Review: 1, Outcome: finalReviewed, Branch: featureBranch(stream), Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base, Summary: "Resumable uploads", Criteria: []FinalCriterion{
		{Criterion: "spec#1", Text: "Resume upload", Evidence: "resume.go and TestResume"},
		{Criterion: "spec#2", Text: "Skip duplicate chunks", Evidence: "dedupe.go and TestDedupe"},
	}}
	record := func(r FinalReport, rev int) {
		content, err := json.Marshal(r)
		must(t, err)
		must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: rev, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	}
	record(report, 1)
	f.s.mu.Lock()
	f.s.active = &activeProject{repository: repository}
	f.s.mu.Unlock()
	return f, stream, repository, report
}

func TestDeliveryPresentationOrdersGapsAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	report.Criteria[1].Evidence = ""
	report.Criteria[1].Gap = "No duplicate test"
	content, err := json.Marshal(report)
	must(t, err)
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: 2, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	p, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if len(p.Report.Criteria) != 2 || p.Report.Criteria[0].Criterion != "spec#2" || p.Report.Criteria[0].Gap != "No duplicate test" || p.Report.Criteria[1].Evidence != "resume.go and TestResume" {
		t.Fatalf("presentation %+v", p.Report.Criteria)
	}
	if !strings.Contains(p.Draft, "No duplicate test") || !strings.Contains(p.Draft, "resume.go and TestResume") {
		t.Fatalf("draft %q", p.Draft)
	}
	if _, api := f.s.approveDelivery(context.Background(), string(stream), DeliveryDecision{Review: 1, ReviewRevision: p.ReviewRevision, Commit: report.Commit, DraftHash: p.DraftHash}); api == nil || api.Code != Conflict {
		t.Fatalf("approved a gap: %v", api)
	}
}

func TestDeliveryApprovalAcceptEditRetryAndInvalidation(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	ctx := context.Background()
	p, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	decision := DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	accepted, api := f.s.approveDelivery(ctx, string(stream), decision)
	if api != nil || accepted.Description != p.Draft {
		t.Fatalf("accept %+v %v", accepted, api)
	}
	editedText := "Owner edited description\n"
	decision.Description = &editedText
	edited, api := f.s.approveDelivery(ctx, string(stream), decision)
	if api != nil || edited.Description != editedText || edited.DescriptionHash != descriptionHash(editedText) {
		t.Fatalf("edit %+v %v", edited, api)
	}
	again, api := f.s.approveDelivery(ctx, string(stream), decision)
	if api != nil || again != edited {
		t.Fatalf("retry %+v %v", again, api)
	}
	acceptRetry, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
	if api != nil || acceptRetry != edited {
		t.Fatalf("accept retry replaced the edit: %+v %v", acceptRetry, api)
	}
	if docs := streamDocuments(t, repository, stream, deliveryDocument); len(docs) != 2 {
		t.Fatalf("delivery revisions %+v", docs)
	}
	if _, reason, err := f.s.deliveryGate(ctx, repository, stream, editedText); err != nil || reason != "" {
		t.Fatalf("edited approval: %q %v", reason, err)
	}
	if _, reason, err := f.s.deliveryGate(ctx, repository, stream, p.Draft); err != nil || !strings.Contains(reason, "description differs") {
		t.Fatalf("replaced edit: %q %v", reason, err)
	}
	repository.Close()
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer reopened.Close()
	f.s.mu.Lock()
	f.s.active = &activeProject{repository: reopened}
	f.s.mu.Unlock()
	after, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil || after.Approval == nil || after.Approval.Description != editedText {
		t.Fatalf("restart %+v %v", after.Approval, api)
	}
	if _, reason, err := f.s.deliveryGate(ctx, reopened, stream, editedText); err != nil || reason != "" {
		t.Fatalf("restart gate: %q %v", reason, err)
	}
	moveFeature(t, f, stream, map[string]string{"late.go": "package demo\n"})
	if _, reason, err := f.s.deliveryGate(ctx, reopened, stream, editedText); err != nil || !strings.Contains(reason, "stale") {
		t.Fatalf("branch change: %q %v", reason, err)
	}
	// Restore the branch and revise a governing criterion.
	g := featureWorkspaces(f.s.cfg)
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
	must(t, err)
	current, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
	must(t, g.Move(ctx, acquired.(workspace.Worktree), current, report.Commit))
	spec := streamDocuments(t, reopened, stream, plan.SpecDocument)[0]
	spec.Revision++
	spec.At = time.Now().UTC()
	spec.Content = strings.Replace(spec.Content, "never sent again", "never sent twice", 1)
	must(t, reopened.RecordDocuments(ctx, []trace.Document{spec}))
	if _, reason, err := f.s.deliveryGate(ctx, reopened, stream, editedText); err != nil || !strings.Contains(reason, "spec revision") {
		t.Fatalf("criterion change: %q %v", reason, err)
	}
}
