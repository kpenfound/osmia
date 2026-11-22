package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// crossRepoStall is a stopped service over a cross-repo workgroup of two
// members, stream in the first project and fixturekit.Quiet in the second,
// with its shared chief of staff in the workgroup trace. stream leads the
// workgroup. Nothing runs on it but what a test calls.
type crossRepoStall struct {
	s     *Service
	repo  *trace.Repository
	other *trace.Repository
	chief *trace.Repository
	g     trace.Workgroup
	clock *fixedClock
	log   *exitLog
}

func newCrossRepoStall(t *testing.T) *crossRepoStall {
	t.Helper()
	s, chief, g := workgroupFeedFixture(t, true, 2)
	clock := &fixedClock{At: demoStart}
	s.options.Reconciliation.Now = clock.Now
	s.router = s.newRouter()
	return &crossRepoStall{s: s, repo: s.projects[0].repository, other: s.projects[1].repository, chief: chief, g: g, clock: clock, log: &exitLog{}}
}

// watch is the first project's watchdog with a window of 45 minutes.
func (f *crossRepoStall) watch() stallWatch {
	cfg := *f.s.current()
	cfg.Loop.StallAfter = "45m"
	return stallWatch{s: f.s, cfg: &cfg, repository: f.repo, submit: f.log.submit}
}

func (f *crossRepoStall) quietFor(d time.Duration) {
	f.clock.Mu.Lock()
	defer f.clock.Mu.Unlock()
	f.clock.At = demoStart.Add(d)
}

// pass runs one pass of the first project's watchdog and returns the exits it
// raised.
func (f *crossRepoStall) pass(t *testing.T) []router.Exit {
	t.Helper()
	f.log.reset()
	must(t, f.watch().Pass(context.Background()))
	var out []router.Exit
	for _, x := range f.log.occurrences() {
		out = append(out, x)
	}
	return out
}

// streamStalls returns the stalled exits of the workstream itself among exits.
func streamStalls(exits []router.Exit, ws config.WorkstreamID) []router.Exit {
	var out []router.Exit
	for _, x := range exits {
		if x.Workstream == ws && x.Trigger == intake.TriggerStalled && x.Unit == "" && !isChangeSubject(x.Subject) {
			out = append(out, x)
		}
	}
	return out
}

// seedFeature moves the workstream's feature state from nothing to state.
func seedFeature(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, state string, at time.Time) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "stall-seed-" + state, Revision: 1, Project: repo.Project(), Workstream: ws, At: at, Actor: stallSeeder, Cause: "seed"}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: state, Reason: "seeded"}})
	must(t, err)
}

// presentSkippedPacket records the ratification packet of a debate the owner
// skipped, which opens the workstream's ratification card.
func presentSkippedPacket(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, at time.Time) {
	t.Helper()
	content, err := shed.EncodePacket(shed.Present(1, shed.Pin{Spec: 1, Plan: 1}, true, "debate was skipped", nil))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(1), Revision: 1, Project: repo.Project(), Workstream: ws, At: at, Actor: stallSeeder, Cause: "ratification-packet"}
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: shed.PacketPath(1), Content: string(content)}}))
}

// A member of a cross-repo workgroup the owner has not approved is watched
// like any other workstream while the owner is not asked for the approval: a
// member sketched with nothing going on stalls. The unapproved workgroup
// explains the quiet once the inbox lists every member's ratification card.
func TestAnUnapprovedWorkgroupExplainsAMemberOnlyWhileTheOwnerIsAsked(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	seedFeature(t, f.repo, stream, SketchedState, demoStart)
	seedFeature(t, f.other, fixturekit.Quiet, SketchedState, demoStart)
	asked := func() bool {
		t.Helper()
		got, err := f.s.intake().GroupApprovalAsked(ctx, f.repo, stream)
		must(t, err)
		return got
	}
	if asked() {
		t.Fatal("the owner is asked for the approval of a workgroup whose members have no packet")
	}

	f.quietFor(44 * time.Minute)
	if got := streamStalls(f.pass(t), stream); len(got) != 0 {
		t.Fatalf("a member sketched for 44 minutes raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	if got := streamStalls(f.pass(t), stream); len(got) != 1 {
		t.Fatalf("a member sketched for 46 minutes with no card raised %d stalls, want 1", len(got))
	}

	// The other member's packet alone asks the owner nothing about the group.
	presentSkippedPacket(t, f.other, fixturekit.Quiet, f.clock.Now())
	if asked() {
		t.Fatal("the owner is asked for the approval while one member has no ratification card")
	}
	if got := streamStalls(f.pass(t), stream); len(got) != 1 {
		t.Fatalf("a member without a card raised %d stalls while another member's card is open, want 1", len(got))
	}

	// With every member's card in the inbox, the owner is asked.
	presentSkippedPacket(t, f.repo, stream, f.clock.Now())
	if !asked() {
		t.Fatal("the owner is not asked for the approval with every member's ratification card open")
	}
	f.quietFor(3 * time.Hour)
	if got := streamStalls(f.pass(t), stream); len(got) != 0 {
		t.Fatalf("a member whose workgroup approval the owner is asked for raised %+v", got)
	}
}
