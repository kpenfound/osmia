package lifecycle_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestApprovedChangeSurvivesProjectRemovalGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewTwoProjectFixture(t)
	entered, release := f.Turns.Block(intakefixture.Turn(fixturekit.Project, "first"))
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, client := journey.StartT(t, f.Opts)
	intakefixture.Await(t, "admitted unit turn", entered)
	wait, closeWait := journey.ServiceChanges(t, s)
	defer closeWait()
	requested, apiFault := s.Lifecycle().RequestChange(ctx, string(fixturekit.Stream), lifecycle.ChangeRequest{Key: "boundary-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel after this whole unit boundary."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for requested.State != "presented" {
		if time.Now().After(deadline) {
			t.Fatalf("controller did not present the impact summary: %+v", requested)
		}
		wait()
		requested = journey.CurrentChange(t, s, fixturekit.Stream, requested.ID)
	}
	approved, apiFault := s.Lifecycle().DecideChange(ctx, string(fixturekit.Stream), requested.ID, lifecycle.ChangeDecision{Revision: requested.Revision, Decision: "approve", PullRequests: "draft"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	_, err := client.RemoveProject(ctx, fixturekit.Project)
	fixturekit.AssertCode(t, err, api.Validation)
	if approved.Workgroup == "" || !strings.Contains(err.Error(), string(approved.Workgroup)) || !s.Current().Active(fixturekit.Project) {
		t.Fatalf("removal was not refused naming the unresolved workgroup: %v", err)
	}
	repo := f.Repository(fixturekit.Project)
	changes, err := lifecycle.ReadChanges(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(changes) != 1 || changes[0].State == "applied" || len(changes[0].Applied) != 0 || !intakefixture.EqualJSONTest(changes[0].Decision, approved.Decision) || !intakefixture.EqualJSONTest(changes[0].Members, approved.Members) || !intakefixture.EqualJSONTest(changes[0].Before, approved.Before) || !intakefixture.EqualJSONTest(changes[0].After, approved.After) {
		t.Fatalf("refused removal lost or applied the approved journal: %+v", changes)
	}
	unblock()
	deadline = time.Now().Add(fixturekit.DemoTimeout)
	for {
		repo, err = s.Repository(fixturekit.Project)
		fixturekit.Must(t, err)
		changes, err = lifecycle.ReadChanges(repo, fixturekit.Stream)
		fixturekit.Must(t, err)
		if len(changes) == 1 && changes[0].State == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed turn did not apply the original approval: %+v", changes)
		}
		wait()
	}
	finished := changes[0]
	if len(finished.Applied) != 1 || finished.Applied[0] != fixturekit.Stream || !intakefixture.EqualJSONTest(finished.Decision, approved.Decision) || !intakefixture.EqualJSONTest(finished.Members, approved.Members) || !intakefixture.EqualJSONTest(finished.Before, approved.Before) || !intakefixture.EqualJSONTest(finished.After, approved.After) {
		t.Fatalf("cancellation changed original pins or duplicated effects: %+v", finished)
	}
	_, err = client.RemoveProject(ctx, fixturekit.Project)
	fixturekit.Must(t, err)
	if s.Current().Active(fixturekit.Project) {
		t.Fatal("resolved project stayed active after removal")
	}
	resolved, err := config.ResolveRoot(f.Opts.Config.Root, "")
	fixturekit.Must(t, err)
	repo, err = trace.OpenByID(resolved, fixturekit.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	changes, err = lifecycle.ReadChanges(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(changes) != 1 || !intakefixture.EqualJSONTest(changes[0], finished) {
		t.Fatalf("removal changed the applied journal: %+v", changes)
	}
	transitions, err := trace.Read[trace.Transition](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	count := 0
	for _, tr := range transitions {
		if tr.ID == "cancel-"+requested.ID {
			count++
			if tr.To != intake.AbandonedState || tr.DecidedBy != trace.DecidedByOwner || tr.DefaultTo != intake.AbandonedState || tr.Judgment != "" || tr.Departed() {
				t.Fatalf("the cancellation's abandonment %+v", tr)
			}
		}
	}
	if count != 1 {
		t.Fatalf("approved cancellation landed %d times", count)
	}
	if rec, ok := journey.LookupRoute(t, repo, fixturekit.Stream, "ws.abandon"); !ok || rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted {
		t.Fatalf("route %+v (found %v)", rec, ok)
	}
}
