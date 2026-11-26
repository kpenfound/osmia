package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchivedWorkstreamKeepsLiveChangesAndStatusAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewTwoProjectFixture(t)
	opts, cfg := f.Opts, f.Cfg
	now := f.Clock.Now()
	opts.Reconciliation.Now = func() time.Time { return now }
	journey.RunChief(&opts, cfg, &fixturekit.ChiefTurns{})
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	previous := opts.Boundary
	opts.Boundary = func(ctx context.Context, step string) error {
		if previous != nil {
			if err := previous(ctx, step); err != nil {
				return err
			}
		}
		return cleanup.Checkpoint(ctx, step)
	}
	s, err := journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, s.Close()) })
	c := journey.NewClient(s.Socket())
	t.Cleanup(func() { c.Close() })

	_, err = c.Abandon(ctx, fixturekit.Stream, "Superseded.")
	fixturekit.Must(t, err)
	_, err = c.Archive(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)

	assertLive := func(deleted bool) {
		t.Helper()
		sent, err := c.Send(ctx, fixturekit.OtherStream, "Continue the unrelated work.")
		fixturekit.Must(t, err)
		wait, closeWait := journey.ServiceChanges(t, s)
		defer closeWait()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for {
			conversation, err := c.Conversation(ctx, fixturekit.OtherStream)
			fixturekit.Must(t, err)
			if slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone && e.Text == "Answer to Continue the unrelated work."
			}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("unrelated work stopped after archive: %+v", conversation)
			}
			wait()
		}
		health, err := c.Health(ctx)
		fixturekit.Must(t, err)
		if !health.Ready || !health.API || health.Backend != api.BackendReady {
			t.Fatalf("archive stopped the live controller: %+v", health)
		}
		archived, err := c.Status(ctx, fixturekit.Stream)
		fixturekit.Must(t, err)
		if !archived.Archived || archived.TraceDeleted != deleted || archived.State == nil || *archived.State != intake.AbandonedState {
			t.Fatalf("archived status: %+v", archived)
		}
		list, err := c.Statuses(ctx)
		fixturekit.Must(t, err)
		if len(list.Diagnostics) != 0 || !slices.ContainsFunc(list.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == fixturekit.OtherStream && !w.Archived }) ||
			!slices.ContainsFunc(list.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == fixturekit.Stream && w.Archived }) {
			t.Fatalf("global status after archive: %+v", list)
		}
	}
	assertLive(false)
	c.Close()
	fixturekit.Must(t, s.Close())
	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	c = journey.NewClient(s.Socket())
	assertLive(false)

	cleanup.Await(t)
	cleanup.Release()
	wait, closeWait := journey.ServiceChanges(t, s)
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		a, ok := intakefixture.ArchiveRecord(t, s.Store, fixturekit.Stream)
		if ok && !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner-authorized archive cleanup did not finish: %+v", a)
		}
		wait()
	}
	closeWait()
	archivedRepo, err := s.Repository(fixturekit.Project)
	fixturekit.Must(t, err)
	remaining, err := archivedRepo.Workstreams()
	fixturekit.Must(t, err)
	if slices.Contains(remaining, fixturekit.Stream) {
		t.Fatalf("cleanup changed unrelated work: %v", remaining)
	}
	unrelatedRepo, err := s.Repository(fixturekit.OtherProject)
	fixturekit.Must(t, err)
	unrelated, err := unrelatedRepo.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(unrelated, fixturekit.OtherStream) {
		t.Fatal("cleanup removed unrelated work")
	}
	assertLive(true)
	c.Close()
	fixturekit.Must(t, s.Close())
	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	c = journey.NewClient(s.Socket())
	assertLive(true)
}
func TestChangePassReportsLiveTraceErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	opts, cfg := journey.ConversationFixture(t, "change-errors-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, repo.Close()) })
	s := &journey.Service{Cfg: cfg, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Lifecycle().ChangePass(ctx, repo); err != context.Canceled {
		t.Fatalf("pass lost cancellation: %v", err)
	}
	fixturekit.Must(t, repo.Close())
	if err := s.Lifecycle().ChangePass(context.Background(), repo); err == nil {
		t.Fatal("pass suppressed a live trace error without an archive record")
	}
}

func TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cancel", "amend"} {
		for _, affectsRetired := range []bool{false, true} {
			for _, resumeRetained := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/retired-affected=%t/resume-retained=%t", kind, affectsRetired, resumeRetained), func(t *testing.T) {
					t.Parallel()
					c := newChangePass(t, kind, affectsRetired, retirementSnapshot)
					finished := c.runAfterTheLeaderIsArchived(t, resumeRetained)
					c.requireOneImpactPromptAndOwnerDecision(t)
					if c.retiresAffected {
						c.requireArchivedMemberNamed(t, finished)
					}
				})
			}
		}
	}
}

func TestRetiringTheLeaderAfterAChangeKeepsTheDesignConversationsOriginalTurns(t *testing.T) {
	t.Parallel()
	c := newChangePass(t, "cancel", true, retirementSnapshot)
	c.runAfterTheLeaderIsArchived(t, true)
	c.requireDesignConversationUnchanged(t)
}

func TestArchiveSnapshotExcludesAnApprovedChangeEffectUntilItIsReleased(t *testing.T) {
	t.Parallel()
	c := newChangePass(t, "cancel", true, retirementSnapshot)
	c.requestAndPresent(t)
	c.approve(t)
	c.abandonLeader(t)
	c.holdArchiveAtTheRetirementSnapshot(t)
	c.awaitCleanupEligible(t)
	finished := c.completeTheChange(t, true, 1)
	c.confirmCompletedAcrossRestart(t, finished)
	c.requireOneImpactPromptAndOwnerDecision(t)
}

func TestArchiveIsRefusedWhileAnApprovedChangeEffectIsInFlightThenTheChangeReachesBothMembers(t *testing.T) {
	t.Parallel()
	c := newChangePass(t, "cancel", true, changeBeforeMember)
	c.requestAndPresent(t)
	c.approve(t)
	c.abandonLeader(t)
	c.refuseArchiveWhileTheMemberEffectIsInFlight(t)
	c.awaitLeaderArchived(t)
	c.awaitCleanupEligible(t)
	finished := c.completeTheChange(t, true, 2)
	c.confirmCompletedAcrossRestart(t, finished)
	c.requireOneImpactPromptAndOwnerDecision(t)
}

func TestApprovedChangeAfterMergedWorkAppliesOneSharedFollowupAcrossInterruptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		step        string
		interrupted bool
	}{
		{"uninterrupted", retirementSnapshot, false},
		{"interrupted-after-allocation", changeFollowupAlloc, true},
		{"interrupted-after-join", changeFollowupJoin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newChangePass(t, "amend", false, tc.step)
			c.recordSurvivorPullRequestMerged(t)
			c.requestAndPresent(t)
			c.approve(t)
			c.applySharedFollowup(t, tc.interrupted)
		})
	}
}

func TestArchiveSucceedsDuringRoutineGroupApprovalRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &intakefixture.ChangeRaceGate{Step: "group-approval-retry-locked", Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
		if err := gate.Checkpoint(ctx, step); err != nil {
			return err
		}
		return cleanup.Checkpoint(ctx, step)
	}})
	defer func() { f.Stop(t) }()
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	_, err = f.C.Abandon(ctx, f.Stream1, "Owner retires this completed member.")
	fixturekit.Must(t, err)
	gate.Armed.Store(true)
	retry := make(chan error, 1)
	go func() {
		_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		retry <- err
	}()
	select {
	case <-gate.Reached:
	case err := <-retry:
		t.Fatalf("approval retry returned before checkpoint: %v", err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("approval retry checkpoint not reached")
	}
	archived := make(chan error, 1)
	go func() { _, err := f.C.Archive(ctx, f.Stream1); archived <- err }()
	lock := journey.InspectForTest(f.S).GroupApprovalLock(group)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("approval checkpoint did not retain group coordination")
	}
	select {
	case err := <-archived:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("archive waited for routine group coordination")
	}
	gate.Release()
	select {
	case err := <-retry:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("approval retry did not finish")
	}
	cleanup.Await(t)
}
func checkFollowupContinuation(t *testing.T, f *journey.ProductionDesignFixture, project config.ProjectID, addition config.WorkstreamID, brief string) {
	t.Helper()
	ctx := context.Background()
	for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2, addition} {
		p := f.Project
		if i == 1 {
			p = f.Project2
		}
		if i == 2 {
			p = project
		}
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		streams, err := repo.Workstreams()
		fixturekit.Must(t, err)
		features := slices.DeleteFunc(slices.Clone(streams), func(stream config.WorkstreamID) bool {
			return stream == f.Origin
		})
		expected := []config.WorkstreamID{f.Stream1}
		if p == f.Project2 {
			expected = []config.WorkstreamID{f.Stream2}
		}
		if p == project {
			expected = append(expected, addition)
		}
		slices.Sort(features)
		slices.Sort(expected)
		if !slices.Equal(features, expected) {
			t.Fatalf("repo has lost or duplicated its allocated member: %v", features)
		}
		if f.S.Context().Mode(p) != bundle.ModeFile {
			t.Fatal("production path requires Hearsay")
		}
		handed, err := trace.Get[trace.Document](repo, m, intake.HandedDocument, 1)
		fixturekit.Must(t, err)
		expectedBrief, source := f.Handed.Content, f.Handed.Source
		if m == addition {
			expectedBrief, source = brief, "stdin"
		}
		if handed.Content != expectedBrief || handed.Source != source {
			t.Fatal("member input differs from the immutable brief")
		}
		cfg := f.S.Current().For(p)
		for _, name := range []string{"spec.md", "plan.json", "handed", "tools/design-brief.json", "workgroups", ".osmia"} {
			if _, err := os.Stat(filepath.Join(cfg.Project.Clone, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("factory state leaked into target repo at %s: %v", name, err)
			}
		}
		if _, err := repo.Thread(m, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("member retains an independent chief: %v", err)
		}
		groups, _, err := trace.Workgroups(repo)
		fixturekit.Must(t, err)
		if _, found := trace.WorkgroupOf(groups, m); found {
			t.Fatal("member retains a one-workstream group")
		}
		target, host, _, close, err := f.S.Lifecycle().ChiefRepository(ctx, repo, m)
		fixturekit.Must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		close()
		fixturekit.Must(t, err)
		for _, design := range f.Design.Turns {
			if slices.ContainsFunc(chief.Turns, func(q trace.QueuedTurn) bool { return q.Request.ID == design.Request.ID }) {
				t.Fatalf("design turn %s is on the chief's thread", design.Request.ID)
			}
		}
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.Origin)
	fixturekit.Must(t, err)
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	fixturekit.Must(t, err)
	if group.Conversation == nil || group.Conversation.Workstream != f.Origin || group.Conversation.Project != f.Project ||
		len(group.Members) != 4 || group.Members[0] != f.Origin ||
		!slices.Contains(group.Members[1:], f.Stream1) || !slices.Contains(group.Members[1:], f.Stream2) || !slices.Contains(group.Members[1:], addition) {
		t.Fatalf("continued conversation or exact member coverage changed: %+v", group)
	}
	groups, err := os.ReadDir(filepath.Join(f.S.Current().Root.String(), "workgroups"))
	fixturekit.Must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(id) {
		t.Fatalf("start or recovery duplicated workgroup authority: %v", groups)
	}
	handed, err := trace.Get[trace.Document](repo, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(handed, f.Handed) {
		t.Fatal("immutable handed brief changed")
	}
	alias, err := f.C.Status(ctx, f.Origin)
	fixturekit.Must(t, err)
	if alias.Workstream != f.Stream1 || alias.Project != f.Project {
		t.Fatalf("design status did not resolve to its member: %+v", alias)
	}
	listed, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	if slices.ContainsFunc(listed.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == f.Origin }) {
		t.Fatal("status list exposes the adopted design host as independent work")
	}
	if _, err := repo.Thread(f.Origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retains an authoritative queue")
	}
	if f.Design.Identity.Role == trace.Designer {
		designed, err := repo.DesignerThread(f.Origin)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(f.Design.Identity, designed.Identity) || !intakefixture.EqualJSONTest(f.Design.Turns, designed.Turns) {
			t.Fatal("design identity, requests, responses or recovery state changed at adoption")
		}
	}
	historical, err := trace.Read[trace.TurnRequest](repo, f.Origin)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(historical, func(req trace.TurnRequest) bool { return req.ID == f.Design.Turns[0].Request.ID }) {
		t.Fatal("historical design request was lost")
	}
	chief, _, _, closeChief, err := f.S.Lifecycle().ChiefRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	defer closeChief()
	matching, err := readview.Consistent(func() (bool, error) {
		var entries []api.ConversationEntry
		var feed []api.FeedEntry
		for i, member := range []config.WorkstreamID{f.Origin, f.Stream1, f.Stream2, addition} {
			conversation, err := f.C.Conversation(ctx, member)
			if err != nil {
				return false, err
			}
			got, apiFault := f.S.FeedList(string(member))
			if apiFault != nil {
				return false, intake.ApiErrAsError{Api: apiFault}
			}
			shared := slices.DeleteFunc(slices.Clone(got.Entries), func(e api.FeedEntry) bool {
				return e.Kind != "message" && e.Kind != "response" && e.Kind != "action" && e.Kind != "status"
			})
			if i == 0 {
				entries, feed = conversation.Entries, shared
			} else if !intakefixture.EqualJSONTest(entries, conversation.Entries) || !intakefixture.EqualJSONTest(feed, shared) {
				return false, nil
			}
		}
		return true, nil
	}, chief.Generation)
	fixturekit.Must(t, err)
	if !matching {
		t.Fatal("continued address and member endpoints resolve to different conversations or feeds")
	}
}
