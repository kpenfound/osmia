package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

type capturedReview struct{ request coreadapter.ReviewRequest }

func (f *capturedReview) Review(_ context.Context, req coreadapter.ReviewRequest) (coreadapter.ReviewResult, error) {
	f.request = req
	sum := sha256.Sum256([]byte(req.Diff))
	return coreadapter.ReviewResult{Subject: req.Subject, Candidate: req.Candidate, DiffSHA256: hex.EncodeToString(sum[:])}, nil
}

func TestReviewBundlePinsCandidateAndPersistsIdentity(t *testing.T) {
	t.Parallel()
	f, agents := newMasonFixture(t, 1, independentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.stop(t)
		}
	}()
	agents.play[masonTurnID("resume")] = reportDone("Built")
	stream, _ := f.builtAs(t, "design")
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	svc := f.s
	f.stop(t)
	stopped = true
	agents.check(t)
	repo, err := trace.Open(svc.cfg.Root, svc.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	controller := newMasonController(svc, repo)
	first, identity, err := controller.prepareUnitReview(context.Background(), stream, "resume")
	if err != nil {
		t.Fatal(err)
	}
	if first.Candidate != identity.Candidate || first.Diff == "" || !strings.Contains(first.Diff, "+package trace") {
		t.Fatalf("candidate or diff: %+v", first)
	}
	sum := sha256.Sum256([]byte(first.Diff))
	if identity.DiffSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("diff digest %s", identity.DiffSHA256)
	}
	for _, source := range []string{"spec.md", "plan.json", "units/resume/report.json", "seal.json footprint", "local context"} {
		found := false
		for _, c := range first.Context {
			found = found || c.Source == source && c.Content != ""
		}
		if !found {
			t.Fatalf("missing %s: %+v", source, first.Context)
		}
	}
	for _, c := range first.Context {
		if c.Source == "local context" && (!strings.Contains(c.Content, "scope: entities internal.trace") || !strings.Contains(c.Content, "charter#1")) {
			t.Fatalf("context escaped the footprint or lost the charter: %s", c.Content)
		}
	}
	var review capturedReview
	returned, err := review.Review(context.Background(), first)
	if err != nil || !identity.Matches(returned) {
		t.Fatalf("review boundary: %+v %v", returned, err)
	}
	returned.Candidate.Revision = "different"
	if identity.Matches(returned) {
		t.Fatal("identity accepted another candidate")
	}
	cmd := exec.Command("git", "-C", f.clone, "update-ref", "refs/heads/"+unitBranch(stream, "resume"), identity.Candidate.BaseRevision)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("move unit branch: %s: %v", out, err)
	}
	second, again, err := controller.prepareUnitReview(context.Background(), stream, "resume")
	if err != nil || second.Diff != first.Diff || again != identity {
		t.Fatalf("moving branch changed review: %+v %+v %v", second, again, err)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			n++
			var stored UnitReviewIdentity
			if err := json.Unmarshal([]byte(d.Content), &stored); err != nil || stored != identity {
				t.Fatalf("stored identity %+v: %v", stored, err)
			}
		}
	}
	if n != 1 {
		t.Fatalf("review identity revisions: %d", n)
	}
}

func TestReviewBundleMissingCandidateFailsClosed(t *testing.T) {
	t.Parallel()
	f, agents := newMasonFixture(t, 1, independentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.stop(t)
		}
	}()
	agents.play[masonTurnID("resume")] = reportDone("Built")
	stream, _ := f.builtAs(t, "design")
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	svc := f.s
	f.stop(t)
	stopped = true
	repo, err := trace.Open(svc.cfg.Root, svc.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.ID != reportDocument("resume") {
			continue
		}
		var report UnitReport
		if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
			t.Fatal(err)
		}
		report.Candidate = ""
		content, _ := json.Marshal(report)
		d.Header.Revision++
		d.Header.Cause = reviewingTransitionID("resume", 1)
		d.Content = string(content)
		if err := repo.RecordDocuments(context.Background(), []trace.Document{d}); err != nil {
			t.Fatal(err)
		}
		break
	}
	_, _, err = newMasonController(svc, repo).prepareUnitReview(context.Background(), stream, "resume")
	if err == nil || !strings.Contains(err.Error(), "lacks the unit, candidate") {
		t.Fatalf("missing candidate: %v", err)
	}
	state, err := repo.Workflow(stream, reviewBlockedSubject("resume"))
	if err != nil || state.Value == "" {
		t.Fatalf("missing recorded error: %+v %v", state, err)
	}
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			t.Fatal("missing evidence recorded a review identity")
		}
	}
}
