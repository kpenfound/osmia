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

	"github.com/kpenfound/osmia/internal/servicetest/journey"

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
			var f *journey.ProductionDesignFixture
			var publications []api.DeliveryPublication
			configuration := journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait}
			if graph == "single" {
				configuration.Options = func(opts *journey.Options) { opts.PullRequests = host }
				f = journey.NewConfiguredProductionDesignFixture(t, configuration)
				f.Stream1 = f.Origin
				f.StartOwner(t)
				var packet api.PacketResponse
				journey.LifecycleAwait(t, f, "single-member packet", func() bool {
					got, err := f.C.Packet(ctx, f.Origin)
					packet = got
					return err == nil
				})
				_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
				fixturekit.Must(t, err)
				journey.LifecycleAwait(t, f, "single-member completed seal", func() bool {
					repo, err := f.S.Repository(f.Project)
					fixturekit.Must(t, err)
					_, _, found, err := seal.Latest(repo, f.Origin)
					fixturekit.Must(t, err)
					return found
				})
				repo, err := f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				cfg := f.S.Current().For(f.Project)
				p := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: "main", Branch: intake.FeatureBranch(f.Origin), Commit: "feature-tip", PullRequest: 101}
				journey.ResolutionDocument(t, repo, f.Origin, delivery.PublicationDocument, delivery.PublicationPath, p)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered-for-pane", Revision: 1, Project: f.Project, Workstream: f.Origin, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
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
			defer f.Stop(t)
			members := resolutionMembers(f)
			if graph == "single" {
				members = members[:1]
			}
			initial, err := f.C.Status(ctx, f.Stream1)
			fixturekit.Must(t, err)
			group := initial.Workgroup
			if group == nil || len(group.Members) != len(members) {
				t.Fatalf("group=%+v", group)
			}
			conversation, err := f.C.Conversation(ctx, f.Origin)
			fixturekit.Must(t, err)
			if len(conversation.Entries) == 0 {
				t.Fatal("original design conversation is empty")
			}
			await := func(phase string, ready func() bool) {
				t.Helper()
				journey.LifecycleAwait(t, f, phase, func() bool {
					if ready() {
						return true
					}
					f.Clock.Advance(10 * time.Minute)
					return false
				})
			}
			resolvedRevision := func() int {
				t.Helper()
				repo, err := f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				if graph != "single" {
					repo, err = trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
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
				f.Clock.Advance(10 * time.Minute)
			}
			archived := members
			shown := members
			if graph == trace.EdgeHardBlock {
				merge(0)
				down, err := f.S.Repository(f.Project2)
				fixturekit.Must(t, err)
				await("sink wait cleared", func() bool {
					wait, err := lifecycle.EdgeWaitDisplay(down, f.Stream2)
					fixturekit.Must(t, err)
					return wait == ""
				})
				await("chief of staff archives and cleans up the merged member", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, members[0].stream)
					return ok && a.Actor == runtime.ArchiveActorChief && !a.CleanedAt.IsZero()
				})
				archived = members[1:]
			}
			for _, m := range archived {
				_, err := f.C.Archive(ctx, m.stream)
				fixturekit.Must(t, err)
				await("authorized archive cleanup", func() bool {
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
					return ok && !a.CleanedAt.IsZero()
				})
			}
			merged := map[config.WorkstreamID]bool{}
			if graph == trace.EdgeHardBlock {
				merged[members[0].stream] = true
			}
			check := func(wantRevision int) {
				t.Helper()
				list, err := f.C.Statuses(ctx)
				fixturekit.Must(t, err)
				for _, m := range shown {
					wantState, wantWait := "delivered (merged)", ""
					if !merged[m.stream] {
						wantState = "blocked"
						wantWait = fmt.Sprintf("archived member %s has no pinned merge or cancel evidence; workgroup remains unresolved", m.stream)
					}
					row, err := f.C.Status(ctx, m.stream)
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
					gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, m.project, m.stream)
					fixturekit.Must(t, err)
					if !gone {
						t.Fatal("pane recreated cleaned member history")
					}
				}
				if graph != "single" {
					authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group.ID)
					fixturekit.Must(t, err)
					entries, err := service.ConversationEntries(authority, f.Origin)
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
					if graph == trace.EdgeHardBlock && m.stream == f.Stream1 {
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
					a, ok := intakefixture.ArchiveRecord(t, f.S.Store, m.stream)
					return ok && lifecycle.ArchiveResolved(a)
				})
				merged[m.stream] = true
				wantRevision := 0
				if i == len(members)-1 {
					await("last merge resolves group", func() bool { return resolvedRevision() == 1 })
					wantRevision = 1
				}
				check(wantRevision)
				f.Stop(t)
				f.Start(t)
				check(wantRevision)
			}
			f.Stop(t)
			f.Start(t)
			check(1)
		})
	}
}
