package lifecycle_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const autoArchivePullRequest = 7

// autoArchiveFixture is one project's trace and runtime store with a service
// view over them, and the fake pull request host the service would look up.
type autoArchiveFixture struct {
	t     *testing.T
	cfg   *config.Config
	repo  *trace.Repository
	store *runtime.Store
	s     *Service
	host  *fakePulls
}

func newAutoArchiveFixture(t *testing.T, key string) *autoArchiveFixture {
	t.Helper()
	opts, cfg := conversationFixture(t, key)
	host := &fakePulls{}
	opts.PullRequests = host
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {stream, quiet}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	return &autoArchiveFixture{t: t, cfg: cfg, repo: repo, store: store, s: s, host: host}
}

// pass runs the archiver once, with whatever boundary hook is installed now.
func (f *autoArchiveFixture) pass() error {
	return (&lifecycle.MergeArchiver{S: f.s.lifecycle(), Repository: f.repo}).Pass(context.Background())
}

func (f *autoArchiveFixture) mustPass() {
	f.t.Helper()
	must(f.t, f.pass())
}

// deliver records the stream delivered with an open pull request numbered pr.
func (f *autoArchiveFixture) deliver(pr int) DeliveryPublication {
	f.t.Helper()
	return fixturekit.RecordDeliveredPublication(f.t, f.repo, f.cfg, stream, featureBranch(stream), pr, foremanActor)
}

func (f *autoArchiveFixture) outcome(pr int, outcome string) {
	f.t.Helper()
	resolutionDocument(f.t, f.repo, stream, ownPullRequestOutcomeDocument, ownPullRequestOutcomePath, ownPullRequestOutcome{PullRequest: pr, Outcome: outcome, MergeCommit: "merge-commit"})
}

// publish records another current publication of the stream, for pr.
func (f *autoArchiveFixture) publish(p DeliveryPublication, pr int) {
	f.t.Helper()
	p.PullRequest = pr
	p.URL = p.URL[:len(p.URL)-1] + string(rune('0'+pr))
	resolutionDocument(f.t, f.repo, stream, publicationDocument, publicationPath, p)
}

func (f *autoArchiveFixture) setState(id, state string) {
	f.t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "fixture"}
	_, err := f.repo.SetFeatureState(context.Background(), h, state, "fixture moves the workstream")
	must(f.t, err)
}

func (f *autoArchiveFixture) archive() (runtime.Archive, bool) {
	f.t.Helper()
	return archiveRecord(f.t, f.store, stream)
}

func (f *autoArchiveFixture) archives() int {
	state, _ := f.store.Snapshot()
	return len(state.Archived)
}

// during runs hook the first time the archiver reaches step.
func (f *autoArchiveFixture) during(step string, hook func()) {
	done := false
	f.s.boundary = func(name string) error {
		if name == step && !done {
			done = true
			hook()
		}
		return nil
	}
}

func TestAutoArchiveRecordsTheChiefOfStaffAndMatchesTheOwnersArchive(t *testing.T) {
	t.Parallel()
	auto := newAutoArchiveFixture(t, "auto-archive-auto-")
	owner := newAutoArchiveFixture(t, "auto-archive-owner-")
	for _, f := range []*autoArchiveFixture{auto, owner} {
		f.deliver(autoArchivePullRequest)
		f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
	}
	auto.mustPass()
	if resp, api := owner.s.archive(string(stream)); api != nil || !resp.Archived || resp.ArchivedBy != "owner" {
		t.Fatalf("owner archive: %+v %v", resp, api)
	}
	chief, ok := auto.archive()
	if !ok {
		t.Fatal("the merged workstream was not archived")
	}
	if chief.Actor != runtime.ArchiveActorChief || chief.State != DeliveredState || chief.DeliveryLink != "https://example.invalid/pull/7" || chief.ResolutionEvidence != "merged" || !chief.Resolved ||
		chief.Publication.PullRequest != autoArchivePullRequest || chief.Publication.Branch != featureBranch(stream) || chief.MergeCommit != "merge-commit" || chief.MergeReference == "" {
		t.Fatalf("automatic archive=%+v", chief)
	}
	if lifecycle.ArchivedBy(chief) != "chief_of_staff" {
		t.Fatalf("archived by %q", lifecycle.ArchivedBy(chief))
	}
	byOwner, _ := owner.archive()
	if byOwner.Actor != "" || lifecycle.ArchivedBy(byOwner) != "owner" {
		t.Fatalf("owner archive=%+v", byOwner)
	}
	chief.Actor, chief.ArchivedAt, byOwner.ArchivedAt = "", time.Time{}, time.Time{}
	if !reflect.DeepEqual(chief, byOwner) {
		t.Fatalf("automatic archive differs from the owner's:\n%+v\n%+v", chief, byOwner)
	}
	if auto.host.Finds != 0 {
		t.Fatalf("the automatic archive looked up pull requests %d times", auto.host.Finds)
	}
	if resp, api := auto.s.archive(string(stream)); api != nil || resp.ArchivedBy != "chief_of_staff" {
		t.Fatalf("repeated owner archive reports %+v %v", resp, api)
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
			f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
			f.publish(p, autoArchivePullRequest+1)
		}},
		{"owner-recorded merge without a pull request", func(f *autoArchiveFixture) {
			recordResolutionMerge(f.t, f.repo, stream)
			f.setState("delivered", DeliveredState)
		}},
		{"abandoned", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
			f.setState("abandoned", AbandonedState)
		}},
		{"reopened out of delivered", func(f *autoArchiveFixture) {
			f.deliver(autoArchivePullRequest)
			f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
			f.setState("reopened", BuildingState)
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
			if _, api := f.s.lifecycle().ArchiveOnMerge(string(stream)); api == nil {
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
	must(t, f.repo.CreateSessionWorkstream(context.Background(), session, "default", "deadbeef", "osmia/session/"+string(session), "a session", demoStart, foremanActor))
	f.mustPass()
	if _, api := f.s.lifecycle().ArchiveOnMerge(string(session)); api == nil || f.archives() != 0 {
		t.Fatalf("session archive: %v archives=%d", api, f.archives())
	}
}

func TestAutoArchiveRepeatedPassesAndOwnerArchiveFirst(t *testing.T) {
	t.Parallel()
	f := newAutoArchiveFixture(t, "auto-archive-repeat-")
	f.deliver(autoArchivePullRequest)
	f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
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
	owner.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
	if _, api := owner.s.archive(string(stream)); api != nil {
		t.Fatal(api)
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
				f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
				f.during(step, func() {
					switch change {
					case "new publication":
						f.publish(p, autoArchivePullRequest+1)
					case "outcome closed":
						f.outcome(autoArchivePullRequest, "closed")
					case "reopened":
						f.setState("reopened", BuildingState)
					}
				})
				f.mustPass()
				if f.archives() != 0 {
					t.Fatal("archived on evidence that no longer held at commit")
				}
				// A later pass archives once the current pull request qualifies.
				f.s.boundary = nil
				f.mustPass()
				if f.archives() != 0 {
					t.Fatal("archived without a merged current pull request")
				}
				switch change {
				case "new publication":
					f.outcome(autoArchivePullRequest+1, ownPullRequestOutcomeMerged)
				case "outcome closed":
					f.outcome(autoArchivePullRequest, ownPullRequestOutcomeMerged)
				case "reopened":
					f.setState("redelivered", DeliveredState)
				}
				f.mustPass()
				a, ok := f.archive()
				if !ok || f.archives() != 1 || a.Actor != runtime.ArchiveActorChief || a.State != DeliveredState {
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
		{"merged current", lifecycle.MemberEvidence{Published: true, Publication: DeliveryPublication{PullRequest: 3}, Outcome: ownPullRequestOutcome{PullRequest: 3, Outcome: ownPullRequestOutcomeMerged}}, true},
		{"merged earlier", lifecycle.MemberEvidence{Published: true, Publication: DeliveryPublication{PullRequest: 4}, Outcome: ownPullRequestOutcome{PullRequest: 3, Outcome: ownPullRequestOutcomeMerged}}, false},
		{"owner merge only", lifecycle.MemberEvidence{OwnerMerge: true, Merge: DeliveryMerge{Upstream: seal.Base{Commit: "c"}}}, false},
		{"unpublished outcome", lifecycle.MemberEvidence{Outcome: ownPullRequestOutcome{PullRequest: 3, Outcome: ownPullRequestOutcomeMerged}}, false},
	} {
		if got := tc.e.OwnPullRequestMerged(); got != tc.merge {
			t.Errorf("%s: OwnPullRequestMerged=%v", tc.name, got)
		}
	}
}
