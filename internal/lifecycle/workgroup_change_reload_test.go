package lifecycle_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestApprovedChangeSurvivesProjectRemovalGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, client := start(t, f.opts)
	await(t, "admitted unit turn", entered)
	wait, closeWait := serviceChanges(t, s)
	defer closeWait()
	requested, api := s.requestChange(ctx, string(stream), ChangeRequest{Key: "boundary-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel after this whole unit boundary."})
	if api != nil {
		t.Fatal(api)
	}
	deadline := time.Now().Add(demoTimeout)
	for requested.State != "presented" {
		if time.Now().After(deadline) {
			t.Fatalf("controller did not present the impact summary: %+v", requested)
		}
		wait()
		requested = currentChange(t, s, stream, requested.ID)
	}
	approved, api := s.decideChange(ctx, string(stream), requested.ID, ChangeDecision{Revision: requested.Revision, Decision: "approve", PullRequests: "draft"})
	if api != nil {
		t.Fatal(api)
	}
	_, err := client.RemoveProject(ctx, project)
	assertCode(t, err, Validation)
	if approved.Workgroup == "" || !strings.Contains(err.Error(), string(approved.Workgroup)) || !s.current().Active(project) {
		t.Fatalf("removal was not refused naming the unresolved workgroup: %v", err)
	}
	repo := f.repository(project)
	changes, err := readChanges(repo, stream)
	must(t, err)
	if len(changes) != 1 || changes[0].State == "applied" || len(changes[0].Applied) != 0 || !equalJSONTest(changes[0].Decision, approved.Decision) || !equalJSONTest(changes[0].Members, approved.Members) || !equalJSONTest(changes[0].Before, approved.Before) || !equalJSONTest(changes[0].After, approved.After) {
		t.Fatalf("refused removal lost or applied the approved journal: %+v", changes)
	}
	unblock()
	deadline = time.Now().Add(demoTimeout)
	for {
		repo, err = s.repository(project)
		must(t, err)
		changes, err = readChanges(repo, stream)
		must(t, err)
		if len(changes) == 1 && changes[0].State == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed turn did not apply the original approval: %+v", changes)
		}
		wait()
	}
	finished := changes[0]
	if len(finished.Applied) != 1 || finished.Applied[0] != stream || !equalJSONTest(finished.Decision, approved.Decision) || !equalJSONTest(finished.Members, approved.Members) || !equalJSONTest(finished.Before, approved.Before) || !equalJSONTest(finished.After, approved.After) {
		t.Fatalf("cancellation changed original pins or duplicated effects: %+v", finished)
	}
	// The cancellation resolves the member, so its project can now be removed;
	// the retained trace holds the applied journal as it was.
	_, err = client.RemoveProject(ctx, project)
	must(t, err)
	if s.current().Active(project) {
		t.Fatal("resolved project stayed active after removal")
	}
	resolved, err := config.ResolveRoot(f.opts.Config.Root, "")
	must(t, err)
	repo, err = trace.OpenByID(resolved, project)
	must(t, err)
	defer repo.Close()
	changes, err = readChanges(repo, stream)
	must(t, err)
	if len(changes) != 1 || !equalJSONTest(changes[0], finished) {
		t.Fatalf("removal changed the applied journal: %+v", changes)
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	count := 0
	for _, tr := range transitions {
		if tr.ID == "cancel-"+requested.ID {
			count++
			// The member's abandonment is the owner's gate, applied as a pinned move.
			if tr.To != AbandonedState || tr.DecidedBy != trace.DecidedByOwner || tr.DefaultTo != AbandonedState || tr.Judgment != "" || tr.Departed() {
				t.Fatalf("the cancellation's abandonment %+v", tr)
			}
		}
	}
	if count != 1 {
		t.Fatalf("approved cancellation landed %d times", count)
	}
	if rec, ok := routeOf(t, repo, stream, "ws.abandon"); !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
}
