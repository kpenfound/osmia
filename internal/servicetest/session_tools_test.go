package servicetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestSessionReplySucceedsWithShowImageAvailable(t *testing.T) {
	t.Parallel()
	s, c, project, engine, _ := SessionWebFixture(t)
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		engine.Sessions.Mu.Lock()
		tools := engine.Sessions.ByKey[req.Env[coreadapter.TokenEnvironment]]
		engine.Sessions.Mu.Unlock()
		if tools == nil {
			return nil, fmt.Errorf("session has no scoped tool host")
		}
		list, err := tools.ListTools(ctx, nil)
		if err != nil {
			return nil, err
		}
		if len(list.Tools) != 1 || list.Tools[0].Name != "show_image" {
			return nil, fmt.Errorf("unexpected session tools: %+v", list.Tools)
		}
		if req.Grants == nil || req.Grants.VCS || req.VCSEnv != nil || len(req.VCSContainerEnv) != 0 {
			return nil, fmt.Errorf("session received version control access")
		}
		return &agent.Result{ClaudeID: "ordinary-session", ResultText: "Hello from the session."}, nil
	}
	opened, err := c.OpenSession(context.Background(), service.SessionOpenRequest{Project: project, Profile: "default"})
	fixturekit.Must(t, err)
	stream := opened.Session.Workstream
	_, err = c.SessionMessage(context.Background(), stream, "Hello")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v service.SessionView) bool { return v.State == trace.SessionIdle })
	if len(engine.Calls()) != 1 {
		t.Fatalf("session ran %d turns", len(engine.Calls()))
	}
	thread, err := service.InspectForTest(s).SoleRepository().Thread(stream, service.TestSessionAgentID)
	fixturekit.Must(t, err)
	if len(thread.Turns) != 1 || thread.Turns[0].Response == nil {
		t.Fatalf("session did not record a reply: %+v", thread.Turns)
	}
	if got := thread.Turns[0].Response.Result.FinalResponse; got != "Hello from the session." {
		t.Fatalf("session reply: %q", got)
	}
}
