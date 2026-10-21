package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	reviewtools "github.com/kpenfound/osmia/internal/review"
)

func TestReviewDiffToolServesThePinnedCandidate(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "review-diff")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	scope := coreadapter.Scope{Project: string(f.cfg.Project.ID), Workstream: string(stream), Unit: "resume", Thread: reviewerAgent("resume"), Turn: "review-1", Role: reviewerRole}
	tool := reviewDiffTool(f.cfg, repo, scope, identity)
	type response struct {
		To    string                    `json:"to"`
		Files []reviewtools.ChangedFile `json:"files"`
		Diff  string                    `json:"diff"`
		Note  string                    `json:"note"`
	}
	call := func(input string) (response, error) {
		t.Helper()
		out, err := tool.Handle(context.Background(), json.RawMessage(input))
		var got response
		if err == nil {
			must(t, json.Unmarshal(out, &got))
		}
		return got, err
	}
	listed, err := call(`{"files_only":true}`)
	must(t, err)
	if listed.To != identity.Candidate.Revision || len(listed.Files) != 1 || listed.Files[0] != (reviewtools.ChangedFile{Path: masonWrote, Added: 1}) {
		t.Fatalf("files: %+v", listed)
	}
	whole, err := call(`{}`)
	must(t, err)
	if !strings.Contains(whole.Diff, "+++ b/"+masonWrote) || !strings.Contains(whole.Diff, "+package trace") {
		t.Fatalf("diff: %s", whole.Diff)
	}
	if none, err := call(`{"paths":["docs"]}`); err != nil || none.Note == "" || none.Diff != "" {
		t.Fatalf("unmatched path: %+v %v", none, err)
	}
	for _, input := range []string{`{"start":1,"end":2}`, `{"paths":["../x"]}`, `{"paths":["internal"],"start":3,"end":2}`, `{"command":"log"}`} {
		if _, err := call(input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	stale := identity
	stale.DiffSHA256 = strings.Repeat("0", 64)
	if _, err := reviewDiffTool(f.cfg, repo, scope, stale).Handle(context.Background(), json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("served a diff that differs from the pinned digest: %v", err)
	}
}
