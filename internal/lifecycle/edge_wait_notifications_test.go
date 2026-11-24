package lifecycle_test

import (
	context "context"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestEdgeWaitAndReleaseKeepInboxAndNotificationLedgerEmpty(t *testing.T) {
	t.Parallel()
	for _, edgeType := range []string{trace.EdgeHardBlock, trace.EdgeMergeOrder} {
		t.Run(edgeType, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newEdgeGraphFixture(t, edgeType)
			f.s.SetSole(&journey.ActiveProject{Id: f.project, Repository: f.repository})
			hook := fixturekit.NewWebhook(t)
			opts := journey.Options{Options: service.Options{Config: config.Options{Root: f.s.Cfg.Root.String()}}}
			hook.Records(opts.Config.Root)
			f.s.Cfg.Notify.Webhook = hook.Hook()
			n := journey.NewNotifier(f.s, time.Second)
			n.Pass(ctx)

			assertStatusAndSilence := func(want string) {
				t.Helper()
				statuses, _, apiFault := journey.InspectForTest(f.s).Statuses()
				if apiFault != nil {
					t.Fatal(apiFault)
				}
				found := false
				for _, status := range statuses {
					if status.Workstream != f.downstream {
						continue
					}
					found = true
					if status.Wait != want || want != "" && status.DisplayState != "blocked" {
						t.Fatalf("dependent status = %+v; want wait %q", status, want)
					}
				}
				if !found {
					t.Fatal("dependent missing from status")
				}
				if entries := f.inboxLen(t); entries != 0 {
					t.Fatalf("wait or release created %d durable inbox entries", entries)
				}
				inbox, apiFault := f.s.Inbox(ctx)
				if apiFault != nil || len(inbox.Entries) != 0 {
					t.Fatalf("owner inbox = %+v, %v", inbox, apiFault)
				}
				n.Pass(ctx)
				if diagnostics := n.Diagnostics(); len(diagnostics) != 0 {
					t.Fatalf("notifier failed instead of observing the empty inbox: %+v", diagnostics)
				}
				ledger := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root)
				if !ledger.Enabled || len(ledger.Notifications) != 0 {
					t.Fatalf("wait or release changed the notification ledger: %+v", ledger)
				}
				if posts := hook.Received(); len(posts) != 0 {
					t.Fatalf("wait or release sent webhook notifications: %q", posts)
				}
			}

			f.resolvePasses(t)
			assertStatusAndSilence(lifecycle.EdgeWaitMessage(string(f.upstream)))
			journey.RecordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, api.DeliveryPublication{
				Status: delivery.PublicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork",
				Branch: intake.FeatureBranch(f.upstream), PullRequest: 123,
			})
			provider := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{
				Number: 123, State: "open", HeadRepository: "owner/repo-a-fork", Head: intake.FeatureBranch(f.upstream), Base: "main",
			}}}
			f.s.Options.PullRequests = provider
			for range 3 {
				f.resolvePasses(t)
				assertStatusAndSilence("waiting on owner: owner/repo-a#123 merge")
				n = journey.NewNotifier(f.s, time.Second)
				assertStatusAndSilence("waiting on owner: owner/repo-a#123 merge")
			}

			provider.Mu.Lock()
			provider.Prs[0].Merged = true
			provider.Prs[0].MergeCommit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
			provider.Mu.Unlock()
			f.resolvePasses(t)
			assertStatusAndSilence("")
			f.resolvePasses(t)
			n = journey.NewNotifier(f.s, time.Second)
			assertStatusAndSilence("")
		})
	}
}
