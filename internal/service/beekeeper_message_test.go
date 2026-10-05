package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// messageFixture starts a service on a registered project with one
// workstream, for tests of the Beekeeper's message tool that never need it
// to run through a real Beekeeper turn.
func messageFixture(t *testing.T, prefix string) (*Service, config.ProjectID, config.WorkstreamID) {
	t.Helper()
	opts, cfg := conversationFixture(t, prefix)
	s, _ := start(t, opts)
	return s, cfg.Project.ID, stream
}

// Sending a message to an unknown project, an unknown workstream of a known
// project, or the shadow project is refused with an error, and changes
// nothing: no chief-of-staff thread gains a turn, and the shadow project's
// own thread is untouched.
func TestMessageChiefOfStaffRefusesUnknownProjectWorkstreamAndShadow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, proj, ws := messageFixture(t, "bk-refuse-")
	unknownProject := "p_99999999999999999999999999999999"
	unknownStream := "w_99999999999999999999999999999999"

	for name, call := range map[string]func() error{
		"unknown project": func() error {
			_, err := s.messageChiefOfStaff(ctx, unknownProject, string(ws), "Status?", "k1")
			return err
		},
		"unknown workstream": func() error {
			_, err := s.messageChiefOfStaff(ctx, string(proj), unknownStream, "Status?", "k2")
			return err
		},
		"shadow project": func() error {
			_, err := s.messageChiefOfStaff(ctx, string(config.ShadowProjectID), string(config.BeekeeperWorkstreamID), "Status?", "k3")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Fatalf("%s: message was accepted", name)
		}
	}

	repo, err := s.repository(proj)
	must(t, err)
	if th, err := repo.ChiefOfStaffThread(ws); err == nil && len(th.Turns) != 0 {
		t.Fatalf("a refused message changed the chief's thread: %+v", th.Turns)
	}
	beeThread, err := s.Beekeeper().Thread(config.BeekeeperWorkstreamID, beekeeper.AgentID)
	must(t, err)
	if len(beeThread.Turns) != 0 {
		t.Fatalf("a refused message changed the beekeeper's own thread: %+v", beeThread.Turns)
	}
}

// A message lands in the target chief of staff's thread attributed to the
// Beekeeper, never the owner: its request carries the beekeeper actor kind
// and the text exactly as given, with no owner actor anywhere in it.
func TestMessageChiefOfStaffIsAttributedToBeekeeperNeverOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, proj, ws := messageFixture(t, "bk-attr-")
	q, err := s.messageChiefOfStaff(ctx, string(proj), string(ws), "Please report status.", "k1")
	must(t, err)
	if q.Request.Actor.Kind != "beekeeper" || q.Request.Actor == (trace.Actor{Kind: "owner", ID: "local"}) {
		t.Fatalf("message actor: %+v", q.Request.Actor)
	}
	if q.Request.Prompt != "Please report status." {
		t.Fatalf("message text: %q", q.Request.Prompt)
	}
	repo, err := s.repository(proj)
	must(t, err)
	th, err := repo.ChiefOfStaffThread(ws)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.Actor.Kind != "beekeeper" {
		t.Fatalf("chief thread: %+v", th.Turns)
	}
}

// Delivering the same message tool call twice, with the same key for the
// same project and workstream, leaves exactly one message in the chief of
// staff's thread, and the second call still reports success.
func TestMessageChiefOfStaffDeliveredTwiceLeavesOneMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, proj, ws := messageFixture(t, "bk-dup-")
	first, err := s.messageChiefOfStaff(ctx, string(proj), string(ws), "Please report status.", "same-key")
	must(t, err)
	second, err := s.messageChiefOfStaff(ctx, string(proj), string(ws), "Please report status.", "same-key")
	must(t, err)
	if first.Request.TurnID != second.Request.TurnID {
		t.Fatalf("delivering the same key twice produced different turns: %+v %+v", first, second)
	}
	repo, err := s.repository(proj)
	must(t, err)
	th, err := repo.ChiefOfStaffThread(ws)
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("delivering the same key twice recorded %d messages: %+v", len(th.Turns), th.Turns)
	}

	// A different key for the same project and workstream is a distinct
	// message.
	third, err := s.messageChiefOfStaff(ctx, string(proj), string(ws), "A different message.", "other-key")
	must(t, err)
	if third.Request.TurnID == first.Request.TurnID {
		t.Fatal("a different key collided with the first message's turn")
	}
	th, err = repo.ChiefOfStaffThread(ws)
	must(t, err)
	if len(th.Turns) != 2 {
		t.Fatalf("a distinct key did not record a second message: %+v", th.Turns)
	}
}
