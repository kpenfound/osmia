package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/trace"
)

// A landing is posted to notify.webhook once, naming the landed commit and the
// base branch. Further passes and a restart post nothing for it, and a landing
// that was recorded before the webhook was set is not posted.
func TestNotifyLandingOnceNamingTheLandedCommitAndBaseBranch(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	ctx := context.Background()
	hook := fixturekit.NewWebhook(t)
	hook.Records(f.s.cfg.Root.String())
	f.s.mu.Lock()
	cfg := *f.s.cfg
	cfg.Notify.Webhook = hook.Hook()
	f.s.cfg = &cfg
	f.s.mu.Unlock()
	n := f.s.notifier
	n.inbox = f.s.inbox
	n.pass(ctx)
	if got := hook.Received(); len(got) != 0 {
		t.Fatalf("posts before a landing: %q", got)
	}

	const commit = "0123456789abcdef0123456789abcdef01234567"
	landing := delivery.DeliveryLanding{Push: 1, Review: 1, Branch: "osmia/" + string(stream), Commit: commit, Reviewed: "r", Remote: "upstream", Upstream: "acme/widgets", BaseBranch: "main", BaseBefore: "b", Confirmed: commit, At: time.Now().UTC(), DecidedBy: trace.DecidedByJev, Judgment: "judgment_1"}
	content, err := json.Marshal(landing)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.LandingDocument, Revision: 1, Project: repository.Project(), Workstream: stream, At: landing.At, Actor: ownerActor, Cause: "test"}, Path: delivery.LandingPath, Content: string(content)}}))
	n.pass(ctx)
	got := hook.Received()
	if len(got) != 1 {
		t.Fatalf("posts for the landing: %q", got)
	}
	for _, want := range []string{"landed", "Workstream: " + string(stream), "Landed commit: " + commit, "Base branch: main of acme/widgets", "Decided by: jev"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("the post %q does not say %q", got[0], want)
		}
	}
	n.pass(ctx)
	restarted := newNotifier(f.s, 10*time.Millisecond, f.s.inbox)
	restarted.pass(ctx)
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posts after more passes and a restart: %q", got)
	}
}
