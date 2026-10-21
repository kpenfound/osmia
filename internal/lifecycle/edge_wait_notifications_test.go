package lifecycle_test

import (
	context "context"
	config "github.com/kpenfound/osmia/internal/config"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/service"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
	time "time"
)

func TestEdgeWaitAndReleaseKeepInboxAndNotificationLedgerEmpty(t *testing.T) {
	t.Parallel()
	for _, edgeType := range []string{trace.EdgeHardBlock, trace.EdgeMergeOrder} {
		t.Run(edgeType, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newEdgeGraphFixture(t, edgeType)
			f.s.setSole(&activeProject{id: f.project, repository: f.repository})
			hook := newFakeWebhook(t)
			opts := Options{Options: service.Options{Config: config.Options{Root: f.s.cfg.Root.String()}}}
			hook.records(opts)
			f.s.cfg.Notify.Webhook = hook.hook()
			n := newNotifier(f.s, time.Second, nil)
			n.pass(ctx)

			assertStatusAndSilence := func(want string) {
				t.Helper()
				statuses, _, api := f.s.statuses()
				if api != nil {
					t.Fatal(api)
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
				inbox, api := f.s.inbox(ctx)
				if api != nil || len(inbox.Entries) != 0 {
					t.Fatalf("owner inbox = %+v, %v", inbox, api)
				}
				n.pass(ctx)
				if diagnostics := n.diagnostics(); len(diagnostics) != 0 {
					t.Fatalf("notifier failed instead of observing the empty inbox: %+v", diagnostics)
				}
				ledger := readLedger(t, opts)
				if !ledger.Enabled || len(ledger.Notifications) != 0 {
					t.Fatalf("wait or release changed the notification ledger: %+v", ledger)
				}
				if posts := hook.received(); len(posts) != 0 {
					t.Fatalf("wait or release sent webhook notifications: %q", posts)
				}
			}

			f.resolvePasses(t)
			assertStatusAndSilence(edgeWaitMessage(string(f.upstream)))
			recordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, DeliveryPublication{
				Status: publicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork",
				Branch: featureBranch(f.upstream), PullRequest: 123,
			})
			provider := &fakePulls{Prs: []pulls.PullRequest{{
				Number: 123, State: "open", HeadRepository: "owner/repo-a-fork", Head: featureBranch(f.upstream), Base: "main",
			}}}
			f.s.options.PullRequests = provider
			for range 3 {
				f.resolvePasses(t)
				assertStatusAndSilence("waiting on owner: owner/repo-a#123 merge")
				// Reload the durable ledger as a newly started notifier does.
				n = newNotifier(f.s, time.Second, nil)
				assertStatusAndSilence("waiting on owner: owner/repo-a#123 merge")
			}

			provider.Mu.Lock()
			provider.Prs[0].Merged = true
			provider.Prs[0].MergeCommit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
			provider.Mu.Unlock()
			f.resolvePasses(t)
			assertStatusAndSilence("")
			f.resolvePasses(t)
			n = newNotifier(f.s, time.Second, nil)
			assertStatusAndSilence("")
		})
	}
}
