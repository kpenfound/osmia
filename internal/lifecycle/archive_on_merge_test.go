package lifecycle_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const autoArchivePullRequest = 7

type autoArchiveFixture struct {
	t     *testing.T
	cfg   *config.Config
	repo  *trace.Repository
	store *runtime.Store
	s     *journey.Service
	host  *fixturekit.FakePulls
}

func newAutoArchiveFixture(t *testing.T, key string) *autoArchiveFixture {
	t.Helper()
	opts, cfg := journey.ConversationFixture(t, key)
	host := &fixturekit.FakePulls{}
	opts.PullRequests = host
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, fixturekit.Quiet}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	return &autoArchiveFixture{t: t, cfg: cfg, repo: repo, store: store, s: s, host: host}
}

func (f *autoArchiveFixture) pass() error {
	return (&lifecycle.MergeArchiver{S: f.s.Lifecycle(), Repository: f.repo}).Pass(context.Background())
}

func (f *autoArchiveFixture) mustPass() {
	f.t.Helper()
	fixturekit.Must(f.t, f.pass())
}

func (f *autoArchiveFixture) deliver(pr int) api.DeliveryPublication {
	f.t.Helper()
	return fixturekit.RecordDeliveredPublication(f.t, f.repo, f.cfg, fixturekit.Stream, intake.FeatureBranch(fixturekit.Stream), pr, execution.ForemanActor)
}

func (f *autoArchiveFixture) outcome(pr int, outcome string) {
	f.t.Helper()
	journey.ResolutionDocument(f.t, f.repo, fixturekit.Stream, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath, delivery.OwnPullRequestOutcome{PullRequest: pr, Outcome: outcome, MergeCommit: "merge-commit"})
}

func (f *autoArchiveFixture) publish(p api.DeliveryPublication, pr int) {
	f.t.Helper()
	p.PullRequest = pr
	p.URL = p.URL[:len(p.URL)-1] + string(rune('0'+pr))
	journey.ResolutionDocument(f.t, f.repo, fixturekit.Stream, delivery.PublicationDocument, delivery.PublicationPath, p)
}

func (f *autoArchiveFixture) setState(id, state string) {
	f.t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: execution.ForemanActor, Cause: "fixture"}
	_, err := f.repo.SetFeatureState(context.Background(), h, state, "fixture moves the workstream")
	fixturekit.Must(f.t, err)
}

func (f *autoArchiveFixture) archive() (runtime.Archive, bool) {
	f.t.Helper()
	return intakefixture.ArchiveRecord(f.t, f.store, fixturekit.Stream)
}

func (f *autoArchiveFixture) archives() int {
	state, _ := f.store.Snapshot()
	return len(state.Archived)
}

func (f *autoArchiveFixture) during(step string, hook func()) {
	done := false
	f.s.Boundary = func(name string) error {
		if name == step && !done {
			done = true
			hook()
		}
		return nil
	}
}

func TestAutoArchiveRecordsTheManagerAndMatchesTheOwnersArchive(t *testing.T) {
	t.Parallel()
	auto := newAutoArchiveFixture(t, "auto-archive-auto-")
	owner := newAutoArchiveFixture(t, "auto-archive-owner-")
	for _, f := range []*autoArchiveFixture{auto, owner} {
		f.deliver(autoArchivePullRequest)
		f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
	}
	auto.mustPass()
	if resp, apiFault := owner.s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil || !resp.Archived || resp.ArchivedBy != "owner" {
		t.Fatalf("owner archive: %+v %v", resp, apiFault)
	}
	manager, ok := auto.archive()
	if !ok {
		t.Fatal("the merged workstream was not archived")
	}
	if manager.Actor != runtime.ArchiveActorManager || manager.State != intake.DeliveredState || manager.DeliveryLink != "https://example.invalid/pull/7" || manager.ResolutionEvidence != "merged" || !manager.Resolved ||
		manager.Publication.PullRequest != autoArchivePullRequest || manager.Publication.Branch != intake.FeatureBranch(fixturekit.Stream) || manager.MergeCommit != "merge-commit" || manager.MergeReference == "" {
		t.Fatalf("automatic archive=%+v", manager)
	}
	if lifecycle.ArchivedBy(manager) != "chief_of_staff" {
		t.Fatalf("archived by %q", lifecycle.ArchivedBy(manager))
	}
	byOwner, _ := owner.archive()
	if byOwner.Actor != "" || lifecycle.ArchivedBy(byOwner) != "owner" {
		t.Fatalf("owner archive=%+v", byOwner)
	}
	manager.Actor, manager.ArchivedAt, byOwner.ArchivedAt = "", time.Time{}, time.Time{}
	if !reflect.DeepEqual(manager, byOwner) {
		t.Fatalf("automatic archive differs from the owner's:\n%+v\n%+v", manager, byOwner)
	}
	if auto.host.Finds != 0 {
		t.Fatalf("the automatic archive looked up pull requests %d times", auto.host.Finds)
	}
	if resp, apiFault := auto.s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil || resp.ArchivedBy != "chief_of_staff" {
		t.Fatalf("repeated owner archive reports %+v %v", resp, apiFault)
	}
}

func TestAutoArchiveSkipsAWorkstreamWithoutAMergedCurrentPullRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(f *autoArchiveFixture)
	}{
		{"open pull request", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
		}},
		{"no recorded outcome", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.host.Prs = nil
		}},
		{"closed without merge", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, "closed")
		}},
		{"merged outcome of an earlier pull request", func(f *autoArchiveFixture) {
			p := f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
			f.publish(p, autoArchivePullRequest+1)
		}},
		{"owner-recorded merge without a pull request", func(f *autoArchiveFixture) {
			journey.RecordResolutionMerge(f.t, f.repo, fixturekit.Stream)
			f.setState("delivered", intake.DeliveredState)
		}},
		{"abandoned", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
			f.setState("abandoned", intake.AbandonedState)
		}},
		{"reopened out of delivered", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
			f.setState("reopened", intake.BuildingState)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutoArchiveFixture(t, "auto-archive-skip-")
			tc.setup(f)
			f.mustPass()
			f.mustPass()
			if f.archives() != 0 {
				state, _ := f.store.Snapshot()
				t.Fatalf("archived: %+v", state.Archived)
			}
			if _, apiFault := f.s.Lifecycle().ArchiveOnMerge(string(fixturekit.Stream)); apiFault == nil {
				t.Fatal("the archive itself accepted a workstream without a merged current pull request")
			}
			if f.archives() != 0 || f.host.Finds != 0 {
				t.Fatalf("archives=%d lookups=%d", f.archives(), f.host.Finds)
			}
		})
	}
}

func TestAutoArchiveSkipsASession(t *testing.T) {
	t.Parallel()
	f := newAutoArchiveFixture(t, "auto-archive-session-")
	session := config.WorkstreamID("w_00000000000000000000000000000b01")
	fixturekit.Must(t, f.repo.CreateSessionWorkstream(context.Background(), session, "default", "deadbeef", "osmia/session/"+string(session), "a session", fixturekit.DemoStart, execution.ForemanActor))
	f.mustPass()
	if _, apiFault := f.s.Lifecycle().ArchiveOnMerge(string(session)); apiFault == nil || f.archives() != 0 {
		t.Fatalf("session archive: %v archives=%d", apiFault, f.archives())
	}
}

func TestAutoArchiveRepeatedPassesAndOwnerArchiveFirst(t *testing.T) {
	t.Parallel()
	f := newAutoArchiveFixture(t, "auto-archive-repeat-")
	f.deliver(autoArchivePullRequest)
	f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
	f.mustPass()
	first, _ := f.archive()
	for range 3 {
		f.mustPass()
	}
	if again, _ := f.archive(); f.archives() != 1 || !reflect.DeepEqual(again, first) {
		t.Fatalf("repeated passes changed the archive: %d %+v %+v", f.archives(), first, again)
	}

	owner := newAutoArchiveFixture(t, "auto-archive-owner-first-")
	owner.deliver(autoArchivePullRequest)
	owner.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
	if _, apiFault := owner.s.Lifecycle().Archive(string(fixturekit.Stream)); apiFault != nil {
		t.Fatal(apiFault)
	}
	recorded, _ := owner.archive()
	owner.mustPass()
	if after, _ := owner.archive(); owner.archives() != 1 || after.Actor != "" || !reflect.DeepEqual(after, recorded) {
		t.Fatalf("the automatic archive replaced the owner's: %+v %+v", recorded, after)
	}
}

func TestAutoArchiveWritesNothingWhenEvidenceChangesBeforeCommit(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"auto-archive-selected", "auto-archive-prepared"} {
		for _, change := range []string{"new publication", "outcome closed", "reopened"} {
			t.Run(step+"/"+change, func(t *testing.T) {
				f := newAutoArchiveFixture(t, "auto-archive-race-")
				p := f.deliver(autoArchivePullRequest)
				f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
				f.during(step, func() {
					switch change {
					case "new publication":
						f.publish(p, autoArchivePullRequest+1)
					case "outcome closed":
						f.outcome(autoArchivePullRequest, "closed")
					case "reopened":
						f.setState("reopened", intake.BuildingState)
					}
				})
				f.mustPass()
				if f.archives() != 0 {
					t.Fatal("archived on evidence that no longer held at commit")
				}
				f.s.Boundary = nil
				f.mustPass()
				if f.archives() != 0 {
					t.Fatal("archived without a merged current pull request")
				}
				switch change {
				case "new publication":
					f.outcome(autoArchivePullRequest+1, delivery.OwnPullRequestOutcomeMerged)
				case "outcome closed":
					f.outcome(autoArchivePullRequest, delivery.OwnPullRequestOutcomeMerged)
				case "reopened":
					f.setState("redelivered", intake.DeliveredState)
				}
				f.mustPass()
				a, ok := f.archive()
				if !ok || f.archives() != 1 || a.Actor != runtime.ArchiveActorManager || a.State != intake.DeliveredState {
					t.Fatalf("later pass archive=%+v ok=%v", a, ok)
				}
				if change == "new publication" && a.Publication.PullRequest != autoArchivePullRequest+1 {
					t.Fatalf("archived the earlier publication: %+v", a.Publication)
				}
			})
		}
	}
}

func TestAutoArchiveEvidenceIsOnlyTheCurrentPublicationsMergedOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		e     lifecycle.MemberEvidence
		merge bool
	}{
		{"merged current", lifecycle.MemberEvidence{Published: true, Publication: api.DeliveryPublication{PullRequest: 3}, Outcome: delivery.OwnPullRequestOutcome{PullRequest: 3, Outcome: delivery.OwnPullRequestOutcomeMerged}}, true},
		{"merged earlier", lifecycle.MemberEvidence{Published: true, Publication: api.DeliveryPublication{PullRequest: 4}, Outcome: delivery.OwnPullRequestOutcome{PullRequest: 3, Outcome: delivery.OwnPullRequestOutcomeMerged}}, false},
		{"owner merge only", lifecycle.MemberEvidence{OwnerMerge: true, Merge: api.DeliveryMerge{Upstream: seal.Base{Commit: "c"}}}, false},
		{"unpublished outcome", lifecycle.MemberEvidence{Outcome: delivery.OwnPullRequestOutcome{PullRequest: 3, Outcome: delivery.OwnPullRequestOutcomeMerged}}, false},
	} {
		if got := tc.e.OwnPullRequestMerged(); got != tc.merge {
			t.Errorf("%s: OwnPullRequestMerged=%v", tc.name, got)
		}
	}
}
