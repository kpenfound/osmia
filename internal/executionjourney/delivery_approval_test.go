package executionjourney

import (
	"context"
	"encoding/json"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDeliveryRejectsFinalReportOnPreviousUpstreamBase(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := journey.DeliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	reviewer := &journey.FinalReviewer{S: f.S, Repository: repository}
	fixturekit.Must(t, reviewer.Request(ctx, stream))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("fresh report prompted another final read: %+v", ops)
	}
	latest, doc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("no seal")
	}
	latest.Base.Commit = strings.Repeat("a", 40)
	content, err := seal.Encode(latest)
	fixturekit.Must(t, err)
	doc.Revision++
	doc.Content = string(content)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{doc}))
	if _, reason, err := reviewer.FinalGate(ctx, stream); err != nil || !strings.Contains(reason, "upstream base changed") {
		t.Fatalf("final gate accepted the old upstream base: %q %v", reason, err)
	}
	fixturekit.Must(t, reviewer.Request(ctx, stream))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("stale report did not prompt a fresh final read: %+v", ops)
	}
	if _, apiErr := f.S.ApproveDelivery(ctx, string(stream), api.DeliveryDecision{Review: report.Review, Commit: report.Commit}); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("delivery accepted the old upstream base: %v", apiErr)
	}
}

func TestDeliveryPresentationOrdersGapsAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := journey.DeliveryFixture(t)
	report.Criteria[1].Evidence = ""
	report.Criteria[1].Gap = "No duplicate test"
	content, err := json.Marshal(report)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: 2, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	p, apiErr := f.S.DeliveryPresentation(context.Background(), string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if len(p.Report.Criteria) != 2 || p.Report.Criteria[0].Criterion != "spec#2" || p.Report.Criteria[0].Gap != "No duplicate test" || p.Report.Criteria[1].Evidence != "resume.go and TestResume" {
		t.Fatalf("presentation %+v", p.Report.Criteria)
	}

	if strings.Contains(p.Draft, "No duplicate test") || strings.Contains(p.Draft, "resume.go and TestResume") {
		t.Fatalf("draft %q", p.Draft)
	}
	if _, apiErr := f.S.ApproveDelivery(context.Background(), string(stream), api.DeliveryDecision{Review: 1, ReviewRevision: p.ReviewRevision, Commit: report.Commit, DraftHash: p.DraftHash}); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("approved a gap: %v", apiErr)
	}
}

const editedDeliveryText = "Owner edited description\n"

func approveEditedDelivery(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, p api.DeliveryPresentation) api.DeliveryApproval {
	t.Helper()
	decision := api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	edited := editedDeliveryText
	decision.Description = &edited
	approval, apiErr := f.S.ApproveDelivery(context.Background(), string(stream), decision)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return approval
}

func TestDeliveryApprovalAcceptsTheDraftAndTheOwnersEdit(t *testing.T) {
	t.Parallel()
	f, stream, _, _ := journey.DeliveryFixture(t)
	ctx := context.Background()
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	decision := api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	accepted, apiErr := f.S.ApproveDelivery(ctx, string(stream), decision)
	if apiErr != nil || accepted.Description != p.Draft {
		t.Fatalf("accept %+v %v", accepted, apiErr)
	}
	edited := approveEditedDelivery(t, f, stream, p)
	if edited.Description != editedDeliveryText || edited.DescriptionHash != delivery.DescriptionHash(editedDeliveryText) {
		t.Fatalf("edit %+v", edited)
	}
}

func TestDeliveryApprovalRetriesLeaveTheEditAndRecordTwoRevisions(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	ctx := context.Background()
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	accept := api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	if _, apiErr := f.S.ApproveDelivery(ctx, string(stream), accept); apiErr != nil {
		t.Fatal(apiErr)
	}
	edited := approveEditedDelivery(t, f, stream, p)
	again := approveEditedDelivery(t, f, stream, p)
	if !reflect.DeepEqual(again, edited) {
		t.Fatalf("retry %+v, want %+v", again, edited)
	}
	acceptRetry, apiErr := f.S.ApproveDelivery(ctx, string(stream), accept)
	if apiErr != nil || !reflect.DeepEqual(acceptRetry, edited) {
		t.Fatalf("accept retry replaced the edit: %+v %v", acceptRetry, apiErr)
	}
	if docs := intakefixture.StreamDocuments(t, repository, stream, delivery.DeliveryDocument); len(docs) != 2 {
		t.Fatalf("delivery revisions %+v", docs)
	}
}

func TestDeliveryGateHoldsOnlyForTheApprovedText(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	p, apiErr := f.S.DeliveryPresentation(context.Background(), string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approveEditedDelivery(t, f, stream, p)
	ctx := context.Background()
	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, editedDeliveryText); err != nil || reason != "" {
		t.Fatalf("edited approval: %q %v", reason, err)
	}
	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, p.Draft); err != nil || !strings.Contains(reason, "description differs") {
		t.Fatalf("replaced edit: %q %v", reason, err)
	}
}

func TestDeliveryApprovalSurvivesARestart(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	ctx := context.Background()
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approveEditedDelivery(t, f, stream, p)
	repository.Close()
	reopened, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer reopened.Close()
	f.S.SetSole(&journey.ActiveProject{Repository: reopened})
	after, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil || after.Approval == nil || after.Approval.Description != editedDeliveryText {
		t.Fatalf("restart %+v %v", after.Approval, apiErr)
	}
	if _, reason, err := f.S.DeliveryGate(ctx, reopened, stream, editedDeliveryText); err != nil || reason != "" {
		t.Fatalf("restart gate: %q %v", reason, err)
	}
}

func TestDeliveryApprovalIsStaleWhenTheFeatureBranchMoves(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	ctx := context.Background()
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approveEditedDelivery(t, f, stream, p)
	journey.MoveFeature(t, f, stream, map[string]string{"late.go": "package demo\n"})
	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, editedDeliveryText); err != nil || !strings.Contains(reason, "stale") {
		t.Fatalf("branch change: %q %v", reason, err)
	}
}

func TestDeliveryApprovalIsInvalidatedWhenACriterionChanges(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	ctx := context.Background()
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	approveEditedDelivery(t, f, stream, p)
	spec := intakefixture.StreamDocuments(t, repository, stream, plan.SpecDocument)[0]
	spec.Revision++
	spec.At = time.Now().UTC()
	spec.Content = strings.Replace(spec.Content, "never sent again", "never sent twice", 1)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{spec}))
	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, editedDeliveryText); err != nil || !strings.Contains(reason, "spec revision") {
		t.Fatalf("criterion change: %q %v", reason, err)
	}
}

func TestDeliveryApprovalRejectsPrivateCheckTraceLinks(t *testing.T) {
	t.Parallel()
	f, w, repo, _ := journey.DeliveryFixture(t)
	defer repo.Close()
	p, apiErr := f.S.DeliveryPresentation(t.Context(), string(w))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	text := p.Draft + "\nhttps://dagger.cloud/private-org/traces/check123\n"
	_, apiErr = f.S.ApproveDelivery(t.Context(), string(w), api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash, Description: &text})
	if apiErr == nil || apiErr.Code != api.Validation || !strings.Contains(apiErr.Message, "private Dagger Cloud") {
		t.Fatalf("private description accepted: %v", apiErr)
	}
}
