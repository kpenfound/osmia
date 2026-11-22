package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
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
	key := gateKey(t, true)
	settings := config.Jev{URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: key, Timeout: "20s", CheckThreshold: config.DefaultJevCheckThreshold, DecisionThreshold: config.DefaultJevDecisionThreshold, OwnerThreshold: config.DefaultJevOwnerThreshold}
	s.jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: (&jevfixture.Transitions{}).Factory(), Getenv: os.Getenv}
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

// seedChange records the workgroup's change journal at the revision, in the
// workgroup trace under its host.
func (f *crossRepoStall) seedChange(t *testing.T, c lifecycle.WorkgroupChange, revision int, at time.Time) {
	t.Helper()
	data, err := json.Marshal(c)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.WorkgroupChangeDocument(c.ID), Revision: revision, Project: f.chief.Project(), Workstream: f.g.Members[0], At: at, Actor: stallSeeder, Cause: "seed"}
	must(t, f.chief.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: lifecycle.WorkgroupChangePath(c.ID), Content: string(data)}}))
}

// changeStalls returns the exits on the subject among exits.
func changeStalls(exits []router.Exit, subject string) []router.Exit {
	var out []router.Exit
	for _, x := range exits {
		if x.Subject == subject {
			out = append(out, x)
		}
	}
	return out
}

// A change of a cross-repo workgroup, whose journal is in the workgroup trace,
// stalls on the member that leads the workgroup: the leader's project raises it,
// the chief of staff is told through the leader's outbox, and a stall nobody
// clears ends on the leader's recovery card, whose approval ends the change in
// the workgroup's journal.
func TestACrossRepoWorkgroupChangeStallsOnItsLeader(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	change := lifecycle.WorkgroupChange{ID: "c1", Workgroup: f.g.ID, Host: f.g.Members[0], Request: lifecycle.ChangeRequest{Key: "c1", Kind: "amend", Scope: "workgroup", Change: "Rename the field"}, State: "drafting", Failure: "member history is unavailable"}
	f.seedChange(t, change, 1, demoStart)
	subject := lifecycle.WorkgroupChangeDocument("c1")

	f.quietFor(44 * time.Minute)
	if got := changeStalls(f.pass(t), subject); len(got) != 0 {
		t.Fatalf("a change drafted for 44 minutes raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	got := changeStalls(f.pass(t), subject)
	if len(got) != 1 || got[0].Workstream != stream || got[0].Repo != f.repo || got[0].Trigger != intake.TriggerStalled || len(got[0].Targets) != 1 {
		t.Fatalf("a change drafted for 46 minutes raised %+v", got)
	}
	x := got[0]
	if x.Evidence.Facts["state"] != "drafting" || x.Evidence.Facts["held"] != change.Failure || x.Evidence.Facts["minutes_quiet"] != 46 {
		t.Fatalf("the change's evidence %+v", x.Evidence.Facts)
	}
	if res, err := f.s.RouteExit(ctx, x); err != nil || !res.Committed || res.Target != intake.TargetWait {
		t.Fatalf("the change's stalled exit = %+v, %v", res, err)
	}
	outbox, err := f.repo.Outbox(stream)
	must(t, err)
	told := 0
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, "Workgroup change c1 has sat drafting for 46 minutes") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the chief of staff was told of the change %d times through the leader, want 1", told)
	}
	// The other member's project does not lead the workgroup.
	other := f.watch()
	other.repository = f.other
	f.log.reset()
	must(t, other.Pass(ctx))
	for _, x := range f.log.occurrences() {
		if x.Subject == subject {
			t.Fatalf("the member that does not lead the workgroup raised %+v", x)
		}
	}

	f.quietFor(46*time.Minute + 50*time.Minute)
	if again := changeStalls(f.pass(t), subject); len(again) != 0 {
		t.Fatalf("a decided stall of the change was raised again: %+v", again)
	}
	var card *InboxEntry
	for _, e := range recoveryEntriesOf(t, f.s, f.repo, stream) {
		if e.Recovery != nil && e.Recovery.Subject == subject {
			card = &e
		}
	}
	if card == nil || card.Recovery.From != "drafting" || card.Recovery.Version != 1 || card.Recovery.Target != "failed" || card.Recovery.Origin != api.RecoveryStall {
		t.Fatalf("the change's card is %+v", card)
	}
	if _, apiErr := f.s.approveRecovery(ctx, string(stream), recoveryRequestOf(t, *card)); apiErr != nil {
		t.Fatalf("approving the change's card: %s", apiErr.Message)
	}
	changes, err := lifecycle.ReadChanges(f.chief, f.g.Members[0])
	must(t, err)
	if len(changes) != 1 || changes[0].State != "failed" {
		t.Fatalf("the approved card left the workgroup's change %+v", changes)
	}
}

// The first project amendment request of a cross-repo workgroup that was not
// drafted into a change stalls on the leader too.
func TestACrossRepoAmendmentRequestStallsOnItsLeader(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	_, err := trace.FileProjectAmendment(ctx, f.chief, trace.ProjectAmendment{Workgroup: f.g.ID, Origin: fixturekit.Quiet, Role: "mason", Requester: stallSeeder, Citations: []string{"criterion 1"}, Change: "Widen the contract", Reason: "the slice cannot meet it"}, demoStart)
	must(t, err)
	subject := lifecycle.WorkgroupChangeDocument("contract-1")
	f.quietFor(44 * time.Minute)
	if got := changeStalls(f.pass(t), subject); len(got) != 0 {
		t.Fatalf("a request filed 44 minutes ago raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	got := changeStalls(f.pass(t), subject)
	if len(got) != 1 || got[0].Workstream != stream || got[0].Evidence.Facts["state"] != "requested" {
		t.Fatalf("a request filed 46 minutes ago raised %+v", got)
	}
}
