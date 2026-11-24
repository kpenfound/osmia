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
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

type crossRepoStall struct {
	s     *Service
	repo  *trace.Repository
	other *trace.Repository
	chief *trace.Repository
	g     trace.Workgroup
	clock *fixturekit.FixedClock
	log   *exitLog
}

func newCrossRepoStall(t *testing.T) *crossRepoStall {
	t.Helper()
	s, chief, g := workgroupFeedFixture(t, true, 2)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	s.options.Reconciliation.Now = clock.Now
	key := gateKey(t, true)
	settings := config.Jev{URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: key, Timeout: "20s", CheckThreshold: config.DefaultJevCheckThreshold, DecisionThreshold: config.DefaultJevDecisionThreshold, OwnerThreshold: config.DefaultJevOwnerThreshold}
	s.jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: (&jevfixture.Transitions{}).Factory(), Getenv: os.Getenv}
	s.router = s.newRouter()
	return &crossRepoStall{s: s, repo: s.projects[0].repository, other: s.projects[1].repository, chief: chief, g: g, clock: clock, log: &exitLog{}}
}

func (f *crossRepoStall) watch() stallWatch {
	cfg := *f.s.current()
	cfg.Loop.StallAfter = "45m"
	return stallWatch{s: f.s, cfg: &cfg, repository: f.repo, submit: f.log.submit}
}

func (f *crossRepoStall) quietFor(d time.Duration) {
	f.clock.Mu.Lock()
	defer f.clock.Mu.Unlock()
	f.clock.At = fixturekit.DemoStart.Add(d)
}

func (f *crossRepoStall) pass(t *testing.T) []router.Exit {
	t.Helper()
	f.log.reset()
	intakefixture.Must(t, f.watch().Pass(context.Background()))
	var out []router.Exit
	for _, x := range f.log.occurrences() {
		out = append(out, x)
	}
	return out
}

func streamStalls(exits []router.Exit, ws config.WorkstreamID) []router.Exit {
	var out []router.Exit
	for _, x := range exits {
		if x.Workstream == ws && x.Trigger == intake.TriggerStalled && x.Unit == "" && !isChangeSubject(x.Subject) {
			out = append(out, x)
		}
	}
	return out
}

func seedFeature(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, state string, at time.Time) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "stall-seed-" + state, Revision: 1, Project: repo.Project(), Workstream: ws, At: at, Actor: stallSeeder, Cause: "seed"}
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: state, Reason: "seeded"}})
	intakefixture.Must(t, err)
}

func presentSkippedPacket(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, at time.Time) {
	t.Helper()
	content, err := shed.EncodePacket(shed.Present(1, shed.Pin{Spec: 1, Plan: 1}, true, "debate was skipped", nil))
	intakefixture.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.PacketDocumentID(1), Revision: 1, Project: repo.Project(), Workstream: ws, At: at, Actor: stallSeeder, Cause: "ratification-packet"}
	intakefixture.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: shed.PacketPath(1), Content: string(content)}}))
}

func TestAnUnapprovedWorkgroupExplainsAMemberOnlyWhileTheOwnerIsAsked(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	seedFeature(t, f.repo, intakefixture.Stream, SketchedState, fixturekit.DemoStart)
	seedFeature(t, f.other, fixturekit.Quiet, SketchedState, fixturekit.DemoStart)
	asked := func() bool {
		t.Helper()
		got, err := f.s.intake().GroupApprovalAsked(ctx, f.repo, intakefixture.Stream)
		intakefixture.Must(t, err)
		return got
	}
	if asked() {
		t.Fatal("the owner is asked for the approval of a workgroup whose members have no packet")
	}

	f.quietFor(44 * time.Minute)
	if got := streamStalls(f.pass(t), intakefixture.Stream); len(got) != 0 {
		t.Fatalf("a member sketched for 44 minutes raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	if got := streamStalls(f.pass(t), intakefixture.Stream); len(got) != 1 {
		t.Fatalf("a member sketched for 46 minutes with no card raised %d stalls, want 1", len(got))
	}

	presentSkippedPacket(t, f.other, fixturekit.Quiet, f.clock.Now())
	if asked() {
		t.Fatal("the owner is asked for the approval while one member has no ratification card")
	}
	if got := streamStalls(f.pass(t), intakefixture.Stream); len(got) != 1 {
		t.Fatalf("a member without a card raised %d stalls while another member's card is open, want 1", len(got))
	}

	presentSkippedPacket(t, f.repo, intakefixture.Stream, f.clock.Now())
	if !asked() {
		t.Fatal("the owner is not asked for the approval with every member's ratification card open")
	}
	f.quietFor(3 * time.Hour)
	if got := streamStalls(f.pass(t), intakefixture.Stream); len(got) != 0 {
		t.Fatalf("a member whose workgroup approval the owner is asked for raised %+v", got)
	}
}

func (f *crossRepoStall) seedChange(t *testing.T, c lifecycle.WorkgroupChange, revision int, at time.Time) {
	t.Helper()
	data, err := json.Marshal(c)
	intakefixture.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.WorkgroupChangeDocument(c.ID), Revision: revision, Project: f.chief.Project(), Workstream: f.g.Members[0], At: at, Actor: stallSeeder, Cause: "seed"}
	intakefixture.Must(t, f.chief.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: lifecycle.WorkgroupChangePath(c.ID), Content: string(data)}}))
}

func changeStalls(exits []router.Exit, subject string) []router.Exit {
	var out []router.Exit
	for _, x := range exits {
		if x.Subject == subject {
			out = append(out, x)
		}
	}
	return out
}

func TestACrossRepoWorkgroupChangeStallsOnItsLeader(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	change := lifecycle.WorkgroupChange{ID: "c1", Workgroup: f.g.ID, Host: f.g.Members[0], Request: lifecycle.ChangeRequest{Key: "c1", Kind: "amend", Scope: "workgroup", Change: "Rename the field"}, State: "drafting", Failure: "member history is unavailable"}
	f.seedChange(t, change, 1, fixturekit.DemoStart)
	subject := lifecycle.WorkgroupChangeDocument("c1")

	f.quietFor(44 * time.Minute)
	if got := changeStalls(f.pass(t), subject); len(got) != 0 {
		t.Fatalf("a change drafted for 44 minutes raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	got := changeStalls(f.pass(t), subject)
	if len(got) != 1 || got[0].Workstream != intakefixture.Stream || got[0].Repo != f.repo || got[0].Trigger != intake.TriggerStalled || len(got[0].Targets) != 1 {
		t.Fatalf("a change drafted for 46 minutes raised %+v", got)
	}
	x := got[0]
	if x.Evidence.Facts["state"] != "drafting" || x.Evidence.Facts["held"] != change.Failure || x.Evidence.Facts["minutes_quiet"] != 46 {
		t.Fatalf("the change's evidence %+v", x.Evidence.Facts)
	}
	if res, err := f.s.RouteExit(ctx, x); err != nil || !res.Committed || res.Target != intake.TargetWait {
		t.Fatalf("the change's stalled exit = %+v, %v", res, err)
	}
	outbox, err := f.repo.Outbox(intakefixture.Stream)
	intakefixture.Must(t, err)
	told := 0
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, "Workgroup change c1 has sat drafting for 46 minutes") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the chief of staff was told of the change %d times through the leader, want 1", told)
	}
	other := f.watch()
	other.repository = f.other
	f.log.reset()
	intakefixture.Must(t, other.Pass(ctx))
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
	for _, e := range recoveryEntriesOf(t, f.s, f.repo, intakefixture.Stream) {
		if e.Recovery != nil && e.Recovery.Subject == subject {
			card = &e
		}
	}
	if card == nil || card.Recovery.From != "drafting" || card.Recovery.Version != 1 || card.Recovery.Target != "failed" || card.Recovery.Origin != api.RecoveryStall {
		t.Fatalf("the change's card is %+v", card)
	}
	if _, apiErr := f.s.approveRecovery(ctx, string(intakefixture.Stream), recoveryRequestOf(t, *card)); apiErr != nil {
		t.Fatalf("approving the change's card: %s", apiErr.Message)
	}
	changes, err := lifecycle.ReadChanges(f.chief, f.g.Members[0])
	intakefixture.Must(t, err)
	if len(changes) != 1 || changes[0].State != "failed" {
		t.Fatalf("the approved card left the workgroup's change %+v", changes)
	}
}

// The first project amendment request of a cross-repo workgroup that was not
// drafted into a change stalls on the leader too.
func TestACrossRepoAmendmentRequestStallsOnItsLeader(t *testing.T) {
	ctx := context.Background()
	f := newCrossRepoStall(t)
	_, err := trace.FileProjectAmendment(ctx, f.chief, trace.ProjectAmendment{Workgroup: f.g.ID, Origin: fixturekit.Quiet, Role: "mason", Requester: stallSeeder, Citations: []string{"criterion 1"}, Change: "Widen the contract", Reason: "the slice cannot meet it"}, fixturekit.DemoStart)
	intakefixture.Must(t, err)
	subject := lifecycle.WorkgroupChangeDocument("contract-1")
	f.quietFor(44 * time.Minute)
	if got := changeStalls(f.pass(t), subject); len(got) != 0 {
		t.Fatalf("a request filed 44 minutes ago raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	got := changeStalls(f.pass(t), subject)
	if len(got) != 1 || got[0].Workstream != intakefixture.Stream || got[0].Evidence.Facts["state"] != "requested" {
		t.Fatalf("a request filed 46 minutes ago raised %+v", got)
	}
}
