package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
)

var verifiedPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func recordReviewerRun(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, unit, commit string, args []string) verifytool.Operation {
	t.Helper()
	runID := verifytool.NewRunID()
	result := verification.Result{
		Claims: []verification.Claim{
			{Name: "cart total", Status: verification.Pass, Detail: "shows 3", Evidence: []string{"shots/cart.png"}, Flow: "checkout"},
			{Name: "coupon", Status: verification.Fail, Detail: "no coupon field appears", Evidence: []string{"logs/console.txt"}, Flow: "checkout"},
		},
		Evidence: []string{"shots/cart.png", "logs/console.txt"},
	}
	outcome := verifytool.Outcome{Result: result, Files: []string{"shots/cart.png", "logs/console.txt"}, Images: []string{"shots/cart.png"}}
	meta, _ := json.Marshal(outcome)
	_, err := repo.RetainEvidence(stream, runID, []trace.EvidenceInput{{Path: "shots/cart.png", Data: verifiedPNG, Image: true}, {Path: "logs/console.txt", Data: []byte("console: ready\n")}}, meta)
	must(t, err)
	scope := coreadapter.Scope{Workstream: string(stream), Role: reviewerRole, Thread: reviewerAgent(unit), Unit: unit, Turn: "review-1"}
	op := verifytool.Operation{ID: "verify-" + runID, Scope: scope, Workstream: stream, Unit: unit, Commit: commit, Args: args, Timeout: "5m0s", RunID: runID, Outcome: &outcome, Finished: true}
	data, _ := json.Marshal(op)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: op.ID, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: ReviewerActor, Cause: "review-1"}, Path: "tools/" + op.ID + ".json", Content: string(data)}
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	return op
}

func TestSendBackCarriesTheReviewersVerifyResultsInFullAndPlacesTheirEvidenceForTheMason(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "verify-send-back")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	candidate := identity.Candidate.Revision
	run := recordReviewerRun(t, repo, stream, "resume", candidate, []string{"flow", "--name=checkout"})
	recordReviewerRun(t, repo, stream, "resume", strings.Repeat("a", 40), []string{"flow", "--name=other-commit"})

	result := UnitReviewResult{Identity: identity, Turn: ReviewTurnID("resume", 1), Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "coupon fails", Action: "add the field"}}}, Bounces: 1}
	must(t, r.enqueueFindings(context.Background(), stream, "resume", result))
	th, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	prompt := th.Turns[len(th.Turns)-1].Request.Prompt
	for _, want := range []string{run.RunID, candidate, `"flow","--name=checkout"`, `"status": "fail"`, "no coupon field appears", "shows 3", `"flow": "checkout"`, verifytool.Ref(run.RunID, "shots/cart.png"), verifytool.Ref(run.RunID, "logs/console.txt"), "evidence for you to weigh"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the send-back lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "other-commit") {
		t.Fatal("the send-back carries a run on another commit")
	}

	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	var review trace.Document
	for _, d := range docs {
		if d.ID == ReviewDocument("resume") {
			review = d
		}
	}
	review.Revision++
	review.At = time.Now().UTC()
	data, _ := json.Marshal(result)
	review.Content = string(data)
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{review}))
	view, err := (isolation.Views{Directory: t.TempDir()}).Create(context.Background(), coreadapter.Workspace{Directory: t.TempDir(), Access: coreadapter.ReadWrite}, nil)
	must(t, err)
	t.Cleanup(func() { view.Release(context.Background()) })
	must(t, PlaceReviewerVerification(repo, stream, "resume", view))
	for name, want := range map[string][]byte{verifytool.Ref(run.RunID, "shots/cart.png"): verifiedPNG, verifytool.Ref(run.RunID, "logs/console.txt"): []byte("console: ready\n")} {
		got, err := view.Read(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s in the mason's view: %q %v", name, got, err)
		}
	}
	if _, err := view.Read(verifytool.ImagePath(run.RunID, "shots/cart.png")); err == nil {
		t.Fatal("an image was placed at the mason's top-level images directory")
	}
}

func TestASendBackWithoutVerifyRunsIsUnchanged(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "verify-send-back-none")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	result := UnitReviewResult{Identity: identity, Turn: ReviewTurnID("resume", 1), Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "e", Action: "a"}}}, Bounces: 1}
	must(t, r.enqueueFindings(context.Background(), stream, "resume", result))
	th, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	if prompt := th.Turns[len(th.Turns)-1].Request.Prompt; strings.Contains(prompt, "verification") || strings.Contains(prompt, "Verify run") {
		t.Fatalf("prompt %s", prompt)
	}
}

func TestMirrorNeverCapturesTheServicesVerificationEvidence(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for name, data := range map[string]string{"main.go": "package main\n", verifytool.Ref("run-1", "shots/a.png"): "evidence", verifytool.EvidenceDir + "/results.json": "[]"} {
		path := filepath.Join(src, name)
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(data), 0o644))
	}
	must(t, Mirror(src, dst))
	if _, err := os.Stat(filepath.Join(dst, "main.go")); err != nil {
		t.Fatalf("a source file was not mirrored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, verifytool.EvidenceDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("evidence reached the mason's workspace: %v", err)
	}
}
