package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestUnitReviewReadsThePinnedCandidate(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "checks")
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	_, identity, err := r.unitReviewEvidence(ctx, stream, "resume")
	must(t, err)

	must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Unit: "resume", Thread: th.Identity.ThreadID, Turn: th.Turns[0].Request.TurnID, Role: reviewerRole}
	pinned, err := unitReviewerIdentity(repo, scope)
	must(t, err)
	if pinned != identity {
		t.Fatalf("review turn identity %+v, want %+v", pinned, identity)
	}

	w, _, err := newUnitWorkspaces(f.cfg, repo).open(ctx, stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("unreviewed\n"), 0600))
	demoGit(t, f.cfg.Project.Clone, "-C", f.cfg.Project.Clone, "update-ref", "refs/heads/"+unitBranch(stream, "resume"), identity.Candidate.BaseRevision)

	selection, err := unitReviewerSelection(ctx, f.cfg, repo, scope, coreadapter.ExecutionSettings{})
	must(t, err)
	selected, err := os.ReadFile(filepath.Join(selection.Workspace.SourceDirectory, masonWrote))
	must(t, err)
	if string(selected) != "package trace\n" || selection.Workspace.BaseRevision != identity.Candidate.Revision {
		t.Fatalf("review input is not pinned: %s", selected)
	}
}
