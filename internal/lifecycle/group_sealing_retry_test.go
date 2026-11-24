package lifecycle_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func TestRatifyingAgainSealsAGroupMemberWhoseSealingFailed(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	ctx := context.Background()

	clone := f.repos[1].Clone
	home := filepath.Dir(clone)
	branch := intake.FeatureBranch(f.stream2)
	local := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local"))
	fixturekit.DemoGit(t, home, "-C", clone, "branch", branch, local)

	if _, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision); err != nil {
		t.Fatal(err)
	}
	repo2 := mustRepo(t, f.s, f.project2)
	intakefixture.Eventually(t, "the second member's sealing fails", func() bool {
		state, err := repo2.Workflow(f.stream2, intake.SealSubject)
		fixturekit.Must(t, err)
		return state.Value == "failed-1"
	})
	repo1 := mustRepo(t, f.s, f.project)
	if approved, err := repo1.GroupApproved(f.stream1); err != nil || approved {
		t.Fatalf("group approval completed with a failed member: %v %v", approved, err)
	}
	outbox, err := repo2.Outbox(f.stream2)
	fixturekit.Must(t, err)
	body := ""
	for _, e := range outbox {
		if e.TransitionID == "seal-1-failed" {
			body = e.Event.Body
		}
	}
	for _, want := range []string{"The sealing of member " + string(f.stream2), "after the owner's group approval", "the owner ratifies the workgroup again at the approved revisions", "cancel the workgroup and hand it in again", branch} {
		if !strings.Contains(body, want) {
			t.Fatalf("the failure notice lacks %q: %s", want, body)
		}
	}

	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-D", branch)
	out, err := f.c.Ratify(ctx, f.stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Detail, "member "+string(f.stream2)+" is approved") || !strings.Contains(out.Detail, "sealing 1 failed and the sealing is asked for again") {
		t.Fatalf("ratifying again: %+v", out)
	}
	intakefixture.Eventually(t, "the group approval completes", func() bool {
		approved, err := repo1.GroupApproved(f.stream1)
		fixturekit.Must(t, err)
		return approved
	})
	sealed, _, found, err := seal.Latest(repo2, f.stream2)
	fixturekit.Must(t, err)
	if !found || sealed.Seal != 2 || sealed.Branch != branch {
		t.Fatalf("the second member's seal: %+v", sealed)
	}
}
