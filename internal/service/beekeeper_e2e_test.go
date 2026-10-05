package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
)

// beekeeperDemoConfig gives the chief of staff and the Beekeeper the
// container sandbox the fake engine's boundary check accepts.
const beekeeperDemoConfig = "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n" +
	"[beekeeper]\nname = \"Hive\"\nprofile = \"default\"\nsandbox = \"container\"\nimage = \"fixture-image\"\n"

// awaitChiefTurn polls the workstream's chief-of-staff thread until it
// holds exactly one completed turn, and returns its final response.
func awaitChiefTurn(t *testing.T, s *Service) string {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := s.sole().repository.ChiefOfStaffThread(stream)
		must(t, err)
		if len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero() {
			return th.Turns[0].Response.Result.FinalResponse
		}
		if time.Now().After(deadline) {
			t.Fatalf("the chief of staff's turn did not complete: %+v", th.Turns)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitRelayedReply polls the Beekeeper chat until it holds a message
// attributed to workstream's chief of staff with the given text.
func awaitRelayedReply(t *testing.T, s *Service, workstream config.WorkstreamID, text string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		msgs, _, err := beekeeper.Messages(s.Beekeeper(), 50)
		must(t, err)
		for _, m := range msgs {
			if m.Author.Kind == beekeeper.AuthorChiefOfStaff && m.Author.Workstream == workstream && m.Text == text {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relayed reply did not appear in the beekeeper chat: %+v", msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A Beekeeper message, sent on a fake Beekeeper model session, lands in the
// target chief of staff's thread attributed to the Beekeeper and runs a
// chief-of-staff turn on a fake model session of its own; the reply is
// recorded once in the shadow project's trace as a relayed reply attributed
// to that workstream's chief of staff, and no Beekeeper turn is started by
// it. The fixture configures no Hearsay section, so this also shows the
// same end-to-end path working with Hearsay not configured.
func TestBeekeeperMessageRunsChiefTurnAndRelaysItsReplyWithNoBeekeeperTurnStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "bk-e2e-")
	if cfg.Hearsay.URL != "" {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(beekeeperDemoConfig)
	must(t, errors.Join(err, configFile.Close()))

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	opts = Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}})

	const sentKey = "status-check-1"
	const sentText = "Please report status."
	const chiefReply = "Two masons are building the feature; nothing needs you yet."
	var toolErr error

	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.SystemPrompt, beekeeper.RolePrompt):
			_, toolErr = callTool(ctx, session, messageChiefOfStaffTool, map[string]any{
				"project": string(project), "workstream": string(stream), "text": sentText, "key": sentKey,
			})
			return questionResult(req, "session-bk", "Sent."), nil
		case strings.Contains(req.SystemPrompt, "message from the Beekeeper"):
			return questionResult(req, "session-chief", chiefReply), nil
		default:
			return nil, fmt.Errorf("unexpected turn %q", req.Name)
		}
	}

	s, _ := start(t, opts)

	q, err := s.PostBeekeeper(ctx, "Ask the workstream for status.")
	must(t, err)
	if q.Response == nil || q.Response.Result.FinalResponse != "Sent." {
		t.Fatalf("beekeeper turn did not complete: %+v", q)
	}
	must(t, toolErr)

	// The message landed in the chief's thread, attributed to the
	// Beekeeper, never the owner.
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.Actor.Kind != "beekeeper" || th.Turns[0].Request.Prompt != sentText {
		t.Fatalf("chief thread after the message: %+v", th.Turns)
	}

	if got := awaitChiefTurn(t, s); got != chiefReply {
		t.Fatalf("chief of staff's reply: %q, want %q", got, chiefReply)
	}
	awaitRelayedReply(t, s, stream, chiefReply)

	// The reply is recorded once, and starts no Beekeeper turn.
	replies, err := beekeeper.RelayedReplies(s.Beekeeper())
	must(t, err)
	if len(replies) != 1 {
		t.Fatalf("relayed reply recorded more than once: %+v", replies)
	}
	beeThread, err := s.Beekeeper().Thread(config.BeekeeperWorkstreamID, beekeeper.AgentID)
	must(t, err)
	if len(beeThread.Turns) != 1 {
		t.Fatalf("a relayed reply started a beekeeper turn: %+v", beeThread.Turns)
	}
}
