package lifecycle_test

import (
	context "context"
	errors "errors"
	fmt "fmt"
	reflect "reflect"
	slices "slices"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

type paneArchivePulls struct {
	*fixturekit.FakePulls
	mu       sync.Mutex
	answers  map[string]pulls.PullRequest
	fail     bool
	observed map[string]int
}

func (p *paneArchivePulls) Find(_ context.Context, _, _, branch string) ([]pulls.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observed[branch]++
	if p.fail {
		return nil, errors.New("provider unavailable")
	}
	if pr, ok := p.answers[branch]; ok {
		return []pulls.PullRequest{pr}, nil
	}
	return nil, nil
}

func TestProjectPaneArchivedPublicationObservationAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, graph := range []string{trace.EdgeHardBlock, trace.EdgeContract, "edgeless", "single"} {
		t.Run(graph, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			host := &paneArchivePulls{FakePulls: &fixturekit.FakePulls{}, answers: map[string]pulls.PullRequest{}, observed: map[string]int{}}
			var f *productionDesignFixture
			var publications []api.DeliveryPublication
			configuration := productionDesignConfiguration{wait: lifecycleAwait}
			if graph == "single" {
				configuration.options = func(opts *Options) { opts.PullRequests = host }
				f = newConfiguredProductionDesignFixture(t, configuration)
				f.stream1 = f.origin
				f.startOwner(t)
				var packet api.PacketResponse
				lifecycleAwait(t, f, "single-member packet", func() bool {
					got, err := f.c.Packet(ctx, f.origin)
					packet = got
					return err == nil
				})
				_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				fixturekit.Must(t, err)
				lifecycleAwait(t, f, "single-member completed seal", func() bool {
					repo, err := f.s.repository(f.project)
					fixturekit.Must(t, err)
					_, _, found, err := seal.Latest(repo, f.origin)
					fixturekit.Must(t, err)
					return found
				})
				repo, err := f.s.repository(f.project)
				fixturekit.Must(t, err)
				cfg := f.s.current().For(f.project)
				p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: "main", Branch: intake.FeatureBranch(f.origin), Commit: "feature-tip", PullRequest: 101}
				resolutionDocument(t, repo, f.origin, delivery.PublicationDocument, delivery.PublicationPath, p)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered-for-pane", Revision: 1, Project: f.project, Workstream: f.origin, At: f.clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
				_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "Owner-approved ready PR published")
				fixturekit.Must(t, err)
				publications = []api.DeliveryPublication{p}
			} else {
				edge := graph
				if graph == "edgeless" {
					edge = ""
				}
				f = resolutionApprovedFixture(t, configuration, edge, host)
				publications = deliverResolutionMembers(t, f)
			}
			defer f.stop(t)
			members := resolutionMembers(f)
			if graph == "single" {
				members = members[:1]
			}
			initial, err := f.c.Status(ctx, f.stream1)
			fixturekit.Must(t, err)
			group := initial.Workgroup
			if group == nil || len(group.Members) != len(members) {
				t.Fatalf("group=%+v", group)
			}
			conversation, err := f.c.Conversation(ctx, f.origin)
			fixturekit.Must(t, err)
			if len(conversation.Entries) == 0 {
				t.Fatal("original design conversation is empty")
			}
			await := func(phase string, ready func() bool) {
				t.Helper()
				lifecycleAwait(t, f, phase, func() bool {
					if ready() {
						return true
					}
					f.clock.Advance(10 * time.Minute)
					return false
				})
			}
			resolvedRevision := func() int {
				t.Helper()
				repo, err := f.s.repository(f.project)
				fixturekit.Must(t, err)
				if graph != "single" {
					repo, err = trace.OpenWorkgroupAuthority(f.s.current().Root, group.ID)
					fixturekit.Must(t, err)
					defer repo.Close()
				}
				ids, rev, err := trace.ResolvedWorkgroups(repo)
				fixturekit.Must(t, err)
				if rev > 0 && !slices.Equal(ids, []config.WorkgroupID{group.ID}) {
					t.Fatalf("resolved groups=%v", ids)
				}
				if graph != "single" {
					active, err := trace.ProjectSpecActive(repo, group.ID)
					fixturekit.Must(t, err)
					if active != (rev == 0) {
						t.Fatalf("project spec active=%v revision=%d", active, rev)
					}
				}
				return rev
			}
			merge := func(i int) {
				p := publications[i]
				host.mu.Lock()
				host.answers[p.Branch] = pulls.PullRequest{Number: p.PullRequest, State: "closed", Merged: true, MergeCommit: fmt.Sprintf("merged-%d", i), Head: p.Branch, HeadRepository: p.Fork}
				host.mu.Unlock()
				f.clock.Advance(10 * time.Minute)
			}
			archived := members
			shown := members
			if graph == trace.EdgeHardBlock {
				merge(0)
				down, err := f.s.repository(f.project2)
				fixturekit.Must(t, err)
				await("sink wait cleared", func() bool {
					wait, err := lifecycle.EdgeWaitDisplay(down, f.stream2)
					fixturekit.Must(t, err)
					return wait == ""
				})
				await("chief of staff archives and cleans up the merged member", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.s.store, members[0].stream)
					return ok && a.Actor == runtime.ArchiveActorChief && !a.CleanedAt.IsZero()
				})
				archived = members[1:]
			}
			for _, m := range archived {
				_, err := f.c.Archive(ctx, m.stream)
				fixturekit.Must(t, err)
				await("authorized archive cleanup", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.s.store, m.stream)
					return ok && !a.CleanedAt.IsZero()
				})
			}
			merged := map[config.WorkstreamID]bool{}
			if graph == trace.EdgeHardBlock {
				merged[members[0].stream] = true
			}
			check := func(wantRevision int) {
				t.Helper()
				list, err := f.c.Statuses(ctx)
				fixturekit.Must(t, err)
				for _, m := range shown {
					wantState, wantWait := "delivered (merged)", ""
					if !merged[m.stream] {
						wantState = "blocked"
						wantWait = fmt.Sprintf("archived member %s has no pinned merge or cancel evidence; workgroup remains unresolved", m.stream)
					}
					row, err := f.c.Status(ctx, m.stream)
					fixturekit.Must(t, err)
					rows := []api.WorkstreamStatus{row}
					for _, item := range list.Workstreams {
						if item.Workstream == m.stream {
							rows = append(rows, item)
						}
					}
					if len(rows) != 2 {
						t.Fatalf("pane lost or duplicated member %s", m.stream)
					}
					for _, row := range rows {
						if !row.Archived || !row.TraceDeleted || row.DisplayState != wantState || row.Wait != wantWait || !reflect.DeepEqual(row.Workgroup, group) {
							t.Fatalf("pane member=%+v want %q wait=%q group=%+v", row, wantState, wantWait, group)
						}
					}
					gone, err := trace.WorkstreamTraceGone(f.s.current().Root, m.project, m.stream)
					fixturekit.Must(t, err)
					if !gone {
						t.Fatal("pane recreated cleaned member history")
					}
				}
				if graph != "single" {
					authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group.ID)
					fixturekit.Must(t, err)
					entries, err := service.ConversationEntries(authority, f.origin)
					fixturekit.Must(t, err)
					fixturekit.Must(t, authority.Close())
					if len(entries) < len(conversation.Entries) || !slices.Equal(entries[:len(conversation.Entries)], conversation.Entries) {
						t.Fatal("workgroup lost the original shared conversation")
					}
				}
				if got := resolvedRevision(); got != wantRevision {
					t.Fatalf("resolution revision=%d want %d", got, wantRevision)
				}
			}
			check(0)
			for _, scenario := range []string{"open", "earlier merge", "closed", "failure"} {
				host.mu.Lock()
				host.fail = scenario == "failure"
				host.observed = map[string]int{}
				for i, m := range members {
					if graph == trace.EdgeHardBlock && m.stream == f.stream1 {
						continue
					}
					p := publications[i]
					pr := pulls.PullRequest{Number: p.PullRequest, State: "open", Head: p.Branch, HeadRepository: p.Fork}
					if scenario == "earlier merge" {
						pr.Number, pr.Merged, pr.MergeCommit = p.PullRequest-1, true, "earlier-tip"
					}
					if scenario == "closed" {
						pr.State = "closed"
					}
					host.answers[p.Branch] = pr
				}
				host.mu.Unlock()
				await(scenario+" observed by controller", func() bool {
					host.mu.Lock()
					defer host.mu.Unlock()
					for _, m := range archived {
						if host.observed[intake.FeatureBranch(m.stream)] < 2 {
							return false
						}
					}
					return true
				})
				check(0)
			}
			host.mu.Lock()
			host.fail = false
			host.mu.Unlock()
			for i, m := range members {
				if graph == trace.EdgeHardBlock && i == 0 {
					continue
				}
				merge(i)
				await("exact archived publication merged", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.s.store, m.stream)
					return ok && lifecycle.ArchiveResolved(a)
				})
				merged[m.stream] = true
				wantRevision := 0
				if i == len(members)-1 {
					await("last merge resolves group", func() bool { return resolvedRevision() == 1 })
					wantRevision = 1
				}
				check(wantRevision)
				f.stop(t)
				f.start(t)
				check(wantRevision)
			}
			f.stop(t)
			f.start(t)
			check(1)
		})
	}
}
