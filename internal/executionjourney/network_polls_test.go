package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type heldPoll struct {
	pulls.Client
	once      sync.Once
	entered   chan struct{}
	cancelled chan struct{}
	exit      chan struct{}
	calls     atomic.Int64
}

func (p *heldPoll) Find(ctx context.Context, _, fork, branch string) ([]pulls.PullRequest, error) {
	if p.calls.Add(1) == 1 {
		p.once.Do(func() { close(p.entered) })
		<-ctx.Done()
		close(p.cancelled)
		<-p.exit
		return nil, ctx.Err()
	}
	return []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "merged-tip", State: "closed", Head: branch, HeadRepository: fork}}, nil
}

func TestNetworkPollingKeepsPassesAndConversationsRunning(t *testing.T) {
	t.Parallel()
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-pr", true: "archive"}[archived], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := conversationFixture(t, "network-poll-")
			clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
			opts.Reconciliation.Now = clock.Now
			turns := &fixturekit.ChiefTurns{}
			runChief(&opts, cfg, turns)
			host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
			var exit sync.Once
			opts.PullRequests = host
			var passes atomic.Int64
			opts.schedulePassed = func() { passes.Add(1) }
			s, client := start(t, opts)
			t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
			repo := s.sole().repository
			design, err := client.OpenDesign(ctx, cfg.Project.ID)
			fixturekit.Must(t, err)
			session := config.WorkstreamID("w_00000000000000000000000000000111")
			fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-test", "Session", s.now(), intake.OwnerActor))
			_, err = service.TestEnsureSessionThread(ctx, repo, cfg.Project.ID, session, s.now())
			fixturekit.Must(t, err)
			publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(fixturekit.Stream), PullRequest: 42}
			if archived {
				fixturekit.Must(t, s.store.SetArchived(runtime.Archive{Project: cfg.Project.ID, Workstream: fixturekit.Stream, State: intake.DeliveredState, ArchivedAt: fixturekit.DemoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: publication.Upstream, Fork: publication.Fork, Branch: publication.Branch, PullRequest: 42}}))
				InspectForTest(s).WakeConversations()
			} else {
				recordUpstreamOwnPullRequest(t, s, repo, fixturekit.Stream, publication)
			}
			fixturekit.AwaitPollSignal(t, host.entered)
			before := passes.Load()
			repo.WakeWorkflow()
			fixturekit.ConversationAwait(t, func() bool { return passes.Load() > before })
			chiefStream := fixturekit.Stream
			if archived {
				chiefStream = fixturekit.Quiet
			}
			chief, err := client.Send(ctx, chiefStream, "Chief message")
			fixturekit.Must(t, err)
			designed, err := client.DesignMessage(ctx, design.Design.Workstream, "Design message")
			fixturekit.Must(t, err)
			messaged, err := client.SessionMessage(ctx, session, "Session message")
			fixturekit.Must(t, err)
			for _, want := range []struct {
				stream      config.WorkstreamID
				agent, turn string
			}{{chiefStream, trace.ChiefOfStaff, chief.Turn}, {design.Design.Workstream, trace.ChiefOfStaff, designed.Turn}, {session, service.TestSessionAgentID, messaged.Turn}} {
				fixturekit.ConversationAwait(t, func() bool {
					th, err := repo.Thread(want.stream, want.agent)
					if err != nil {
						return false
					}
					for _, q := range th.Turns {
						if q.Request.TurnID == want.turn {
							return q.Response != nil && !q.CompletedAt.IsZero()
						}
					}
					return false
				})
			}

			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			fixturekit.AwaitPollSignal(t, host.cancelled)
			select {
			case err := <-closed:
				t.Fatalf("service closed before the poll exited: %v", err)
			default:
			}
			_, err = repo.Workstreams()
			fixturekit.Must(t, err)
			exit.Do(func() { close(host.exit) })
			select {
			case err := <-closed:
				fixturekit.Must(t, err)
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("shutdown did not join the poll")
			}

			clock.Advance(time.Second)
			s, _ = start(t, opts)
			repo = s.sole().repository
			fixturekit.ConversationAwait(t, func() bool {
				if archived {
					a, found := intakefixture.ArchiveRecord(t, s.store, fixturekit.Stream)
					return found && lifecycle.ArchiveResolved(a) && a.MergeCommit == "merged-tip"
				}
				out, revision, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
				fixturekit.Must(t, err)
				return revision == 1 && out.PullRequest == 42 && out.MergeCommit == "merged-tip"
			})
			fixturekit.Must(t, s.Close())
			if host.calls.Load() != 2 {
				t.Fatalf("lookups=%d, want interrupted lookup and restart lookup", host.calls.Load())
			}
		})
	}
}

func TestBaseRefreshBypassesOwnWorkflow(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh := ownPRFixture(t)
	ctx := context.Background()

	event := "held-network-workflow"
	op := coreadapter.Operation{ID: trace.OperationID(cfg.Project.ID, fixturekit.Stream, event), Boundary: coreadapter.RepositoryBoundary, Action: event, Input: []byte(`{}`)}
	_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: event, Revision: 1, Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}, Subject: event, To: "held", Reason: "Hold workflow"}, Events: []trace.Event{{ID: event, Kind: event, Body: "Hold workflow", Operation: &op}}})
	fixturekit.Must(t, err)
	attempt, _, err := repo.BeginOperation(ctx, fixturekit.Stream, event, trace.WorkflowOperation, intake.ServiceActor, refresh.s.now)
	fixturekit.Must(t, err)
	if attempt == nil {
		t.Fatal("workflow was not reserved")
	}
	defer attempt.Close()
	sealStream(t, repo)
	p := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, Merged: true, MergeCommit: "merged-tip", Head: p.Branch, HeadRepository: p.Fork}}
	fixturekit.Must(t, refresh.Pass(ctx))
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "poll-test", Now: refresh.s.now, Concurrent: service.TestConcurrentOperation, Class: service.TestOperationClass, Limit: InspectForTest(refresh.s).GitOperationLimit(repo, nil), Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: refresh}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, controller.Pass(ctx))
	fixturekit.Must(t, controller.Wait())
	out, rev, err := delivery.OwnPullRequestOutcomeAt(repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if rev != 1 || out.MergeCommit != "merged-tip" {
		t.Fatalf("poll waited for its workflow: %+v rev=%d", out, rev)
	}
}

func TestWorkgroupChangePollingKeepsSchedulingRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "change-network-")
	opts.Reconciliation.Now = (&fixturekit.FixedClock{At: fixturekit.DemoStart}).Now
	host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	var exit sync.Once
	opts.PullRequests = host
	var passes atomic.Int64
	opts.schedulePassed = func() { passes.Add(1) }
	s, _ := start(t, opts)
	t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
	repo := s.sole().repository

	resolutionDocument(t, repo, fixturekit.Stream, delivery.PublicationDocument, delivery.PublicationPath, api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(fixturekit.Stream), PullRequest: 42})
	c, apiErr := s.requestChange(ctx, string(fixturekit.Stream), lifecycle.ChangeRequest{Key: "blocked-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel the workgroup"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.ConversationAwait(t, func() bool { return currentChange(t, s, fixturekit.Stream, c.ID).State == "presented" })
	c = currentChange(t, s, fixturekit.Stream, c.ID)
	_, apiErr = s.decideChange(ctx, string(fixturekit.Stream), c.ID, service.ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "close"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.AwaitPollSignal(t, host.entered)
	before := passes.Load()
	repo.WakeWorkflow()
	fixturekit.ConversationAwait(t, func() bool { return passes.Load() > before })
	if held, err := lifecycle.ChangeHeld(repo, fixturekit.Stream); err != nil || !held {
		t.Fatalf("change boundary lost during lookup: held=%v err=%v", held, err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	fixturekit.AwaitPollSignal(t, host.cancelled)
	exit.Do(func() { close(host.exit) })
	select {
	case err := <-closed:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("change observer did not drain")
	}
}

func TestNetworkPollingStopsOnProjectRemoval(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "remove-network-")
	opts.Reconciliation.Now = (&fixturekit.FixedClock{At: fixturekit.DemoStart}).Now
	host := &heldPoll{entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	opts.PullRequests = host
	s, client := start(t, opts)
	var exit sync.Once
	t.Cleanup(func() { exit.Do(func() { close(host.exit) }) })
	repo := s.sole().repository
	fixturekit.Must(t, s.store.SetArchived(runtime.Archive{Project: cfg.Project.ID, Workstream: fixturekit.Stream, State: intake.DeliveredState, ArchivedAt: fixturekit.DemoStart, ResolutionEvidence: "unresolved", Publication: runtime.ArchivePublication{Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(fixturekit.Stream), PullRequest: 42}}))
	InspectForTest(s).WakeConversations()
	fixturekit.AwaitPollSignal(t, host.entered)
	_, err := client.RemoveProject(context.Background(), cfg.Project.ID)
	fixturekit.AssertCode(t, err, api.Validation)
	_, err = client.Abandon(context.Background(), fixturekit.Quiet, "Owner cancelled the other member before removing its project")
	fixturekit.Must(t, err)
	state, err := repo.Workflow(fixturekit.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resolve-before-project-removal", Revision: 1, Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "owner"},
		Subject: trace.FeatureSubject, From: state.Value, To: intake.AbandonedState, Reason: "Owner cancelled the archived member before removing its project",
	}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.store.SetArchived(runtime.Archive{Project: cfg.Project.ID, Workstream: fixturekit.Stream, State: intake.AbandonedState, ArchivedAt: fixturekit.DemoStart, Resolved: true, ResolutionEvidence: "cancelled"}))
	removed := make(chan error, 1)
	go func() { _, err := client.RemoveProject(context.Background(), cfg.Project.ID); removed <- err }()
	select {
	case <-host.cancelled:
	case err := <-removed:
		t.Fatalf("project removal refused before cancelling its poll: %v", err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("project removal did not cancel its poll")
	}
	select {
	case err := <-removed:
		t.Fatalf("project removal did not join poll: %v", err)
	default:
	}
	_, err = repo.Workstreams()
	fixturekit.Must(t, err)
	exit.Do(func() { close(host.exit) })
	select {
	case err := <-removed:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("project removal did not finish")
	}
}
