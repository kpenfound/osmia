package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// A Beekeeper turn an earlier service session claimed without capturing a
// result, after its message tool call had already delivered one message to
// a chief of staff, is recovered exactly as an interrupted chief-of-staff
// turn is: the recovered continuation does not know whether its call
// delivered, so it tries the same key again, and the chief of staff's
// thread still holds exactly one message.
func TestBeekeeperMessageDeliveredBeforeARestartIsNotDeliveredAgainByRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := config.Load(opts.Config)
	must(t, err)

	shadow, err := beekeeper.Open(ctx, cfg.Root, cfg.Beekeeper, demoStart)
	must(t, err)
	if _, err := beekeeper.EnsureThread(ctx, shadow, demoStart); err != nil {
		t.Fatal(err)
	}
	beeReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_bee_crash", Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: demoStart, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message"},
		AgentID: beekeeper.AgentID, ThreadID: beekeeper.ThreadID, TurnID: "message_bee_crash", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "Check on the workstream.",
	}
	if _, err := shadow.EnqueueTurn(ctx, beeReq); err != nil {
		t.Fatal(err)
	}
	beeDir := filepath.Join(cfg.Root.String(), "threads", string(config.ShadowProjectID), string(config.BeekeeperWorkstreamID), beekeeper.AgentID, "message_bee_crash")
	if _, err := shadow.ClaimTurn(ctx, config.BeekeeperWorkstreamID, beekeeper.AgentID, "crashed_bee", beeDir, demoStart); err != nil {
		t.Fatal(err)
	}
	must(t, shadow.Close())

	// The message tool call delivered before the crash: the chief's thread
	// already holds the Beekeeper's message, under the same deterministic
	// turn identity a real call with this project, workstream and key
	// produces.
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, ownerActor)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, ownerActor))
	if _, err := repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor); err != nil {
		t.Fatal(err)
	}
	turnID := beekeeperMessageTurnID(cfg.Project.ID, stream, "status-1")
	chiefReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: cfg.Project.ID, Workstream: stream, At: demoStart, Actor: beekeeperActor, Cause: "beekeeper-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turnID, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the chief of staff.", Prompt: "Please report status.",
	}
	if _, err := repo.EnqueueTurn(ctx, chiefReq); err != nil {
		t.Fatal(err)
	}
	must(t, repo.Close())

	clock := &fixedClock{now: demoStart.Add(time.Hour)}
	opts.Reconciliation.Now = clock.Now
	controls := &runtimeControls{}
	opts.controls = controls
	opts.BeekeeperTurns = turnsFunc(func(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		// The recovered continuation does not know whether its message
		// delivered before the crash, so it retries the same key, through
		// the same production path message_chief_of_staff itself calls, so
		// that path's own de-duplication is what keeps the count at one.
		s := controls.service.Load()
		if s == nil {
			return coreadapter.SessionResult{}, errors.New("the service is not ready")
		}
		if _, err := s.messageChiefOfStaff(ctx, string(cfg.Project.ID), string(stream), chiefReq.Prompt, "status-1"); err != nil {
			return coreadapter.SessionResult{}, err
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "recovered-bee"}, FinalResponse: "Delivered again, just in case."}, nil
	})

	s, err := Start(ctx, opts)
	must(t, err)
	defer s.Close()

	beeThread, err := s.Beekeeper().Thread(config.BeekeeperWorkstreamID, beekeeper.AgentID)
	must(t, err)
	if len(beeThread.Turns) != 2 || beeThread.Turns[0].Status() != "interrupted" || beeThread.Turns[1].Status() != "idle" {
		t.Fatalf("beekeeper thread after recovery: %+v", beeThread.Turns)
	}

	chiefRepo, err := s.repository(cfg.Project.ID)
	must(t, err)
	th, err := chiefRepo.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != turnID {
		t.Fatalf("the message tool call delivered more than once across the restart: %+v", th.Turns)
	}
}
