package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/isolation"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type stableWorkspaces struct{ dir string }

func (w stableWorkspaces) Acquire(_ context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{Directory: w.dir, Access: req.Access}, Lease: stableLease{}}, nil
}

type stableLease struct{}

func (stableLease) Release(context.Context) error { return nil }

var stableRoles = []string{trace.ChiefOfStaff, masonRole, reviewerRole, architectRole, committeeRole}

func stableStack(t *testing.T, cfg *config.Config, repo *trace.Repository, engine *adaptertest.Engine) thread.Runner {
	t.Helper()
	dir := t.TempDir()
	fixturekit.Must(t, os.MkdirAll(filepath.Join(dir, "src"), 0o700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(dir, "src", "file"), []byte("x"), 0o600))
	grants := map[string]coreadapter.Capabilities{}
	for _, role := range stableRoles {
		grants[role] = coreadapter.Capabilities{Tools: []string{"file_read"}}
	}
	turns := &isolation.Turns{Workspaces: stableWorkspaces{dir}, Views: isolation.Views{Directory: t.TempDir()}, Grants: grants, Engine: engine, Hosts: &adaptertest.RecordingHosts{},
		Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
			return isolation.Selection{Paths: []string{"src"}, Execution: coreadapter.ExecutionSettings{Mode: "none"}}, nil
		}}
	return thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: repo, Turns: memoryTurns(turns, cfg, repo), Now: func() time.Time { return time.Now().UTC() }}
}

func enqueueStable(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, role, backend, turn, system, turnContext string) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: repo.Project(), Workstream: stream, At: time.Now().UTC(), Actor: serviceActor, Cause: "owner-message"}
	_, err := repo.EnqueueTurn(context.Background(), trace.TurnRequest{Header: h, AgentID: "agent_" + role, ThreadID: "thread_" + role, TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: backend, Model: "test"}, SystemPrompt: system, Context: turnContext, Prompt: "Message " + turn})
	fixturekit.Must(t, err)
}

func runStable(t *testing.T, r thread.Runner, stream config.WorkstreamID, role, turn string) {
	t.Helper()
	_, err := r.RunNext(context.Background(), stream, "agent_"+role, coreadapter.PreparedTurn{SessionDirectory: filepath.Join(t.TempDir(), turn)})
	fixturekit.Must(t, err)
}

func lastRequest(t *testing.T, engine *adaptertest.Engine) agent.Request {
	t.Helper()
	if len(engine.Requests) == 0 {
		t.Fatal("the turn never reached the engine")
	}
	return engine.Requests[len(engine.Requests)-1]
}

func newStableEngine(name string, resume func() error) *adaptertest.Engine {
	e := &adaptertest.Engine{}
	e.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return resume() }
	e.SessionID = func(req agent.Request) string {
		if req.ResumeID != "" {
			return req.ResumeID
		}
		return name + "-session"
	}
	return e
}

func TestResumedSessionsKeepTheirSystemPromptAfterPolicyAndPromptTextChange(t *testing.T) {
	for _, backend := range []string{"claude", "opencode"} {
		for _, role := range stableRoles {
			t.Run(backend+"/"+role, func(t *testing.T) {
				f, stream, repo, _ := newFinalFixture(t, "stable-"+backend+"-"+role)
				defer func() { repo.Close() }()
				ctx := context.Background()
				owner := trace.Actor{Kind: "owner", ID: "local"}
				fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "agent_" + role, Project: repo.Project(), Workstream: stream, At: time.Now().UTC(), Actor: owner, Cause: "test"}, Role: role, ThreadID: "thread_" + role}))

				policy := "policy v1"
				previous := factoryInstructions
				factoryInstructions = func(scope coreadapter.Scope, _ kindreg.Kind) string {
					return "\n\nCurrent factory operating policy for " + scope.Role + ": " + policy + "."
				}
				t.Cleanup(func() { factoryInstructions = previous })

				first := newStableEngine("first", func() error { return nil })
				enqueueStable(t, repo, stream, role, backend, "t1", "base text v1 of "+role, "design context v1")
				runStable(t, stableStack(t, f.s.cfg, repo, first), stream, role, "t1")
				began := lastRequest(t, first)
				if began.ResumeID != "" || began.SystemPrompt != "base text v1 of "+role || strings.Contains(began.SystemPrompt, "policy v1") {
					t.Fatalf("a new session's system prompt %q resume %q", began.SystemPrompt, began.ResumeID)
				}
				if !strings.Contains(began.Prompt, "policy v1") || !strings.Contains(began.Prompt, "design context v1") {
					t.Fatalf("a new session's first turn lacks the current policy and context: %q", began.Prompt)
				}

				policy = "policy v2"
				fixturekit.Must(t, repo.Close())
				var err error
				repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
				fixturekit.Must(t, err)
				second := newStableEngine("second", func() error { return nil })
				enqueueStable(t, repo, stream, role, backend, "t2", "base text v2 REWRITTEN by a deploy of "+role, "design context v2")
				runStable(t, stableStack(t, f.s.cfg, repo, second), stream, role, "t2")
				resumed := lastRequest(t, second)
				if resumed.ResumeID != "first-session" {
					t.Fatalf("the turn did not resume the saved session: %q", resumed.ResumeID)
				}
				if resumed.SystemPrompt != began.SystemPrompt {
					t.Fatalf("resumed system prompt %q, the session began with %q", resumed.SystemPrompt, began.SystemPrompt)
				}
				for _, want := range []string{"policy v2", "design context v2", "Message t2"} {
					if !strings.Contains(resumed.Prompt, want) {
						t.Fatalf("the newest turn lacks %q: %q", want, resumed.Prompt)
					}
				}
				for _, stale := range []string{"policy v1", "design context v1"} {
					if strings.Contains(resumed.Prompt, stale) {
						t.Fatalf("the newest turn carries the stale %q: %q", stale, resumed.Prompt)
					}
				}

				policy = "policy v3"
				third := newStableEngine("third", func() error { return coreadapter.ErrResumeUnavailable })
				enqueueStable(t, repo, stream, role, backend, "t3", "base text v3 of "+role, "design context v3")
				runStable(t, stableStack(t, f.s.cfg, repo, third), stream, role, "t3")
				replayed := lastRequest(t, third)
				if replayed.ResumeID != "" || replayed.SystemPrompt != "base text v3 of "+role || !strings.Contains(replayed.Prompt, "osmia-owned-log") || !strings.Contains(replayed.Prompt, "policy v3") || !strings.Contains(replayed.Prompt, "design context v3") {
					t.Fatalf("replay: resume %q system %q prompt %q", replayed.ResumeID, replayed.SystemPrompt, replayed.Prompt)
				}

				policy = "policy v4"
				fixturekit.Must(t, repo.Close())
				repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
				fixturekit.Must(t, err)
				fourth := newStableEngine("fourth", func() error { return nil })
				enqueueStable(t, repo, stream, role, backend, "t4", "base text v4 of "+role, "design context v4")
				runStable(t, stableStack(t, f.s.cfg, repo, fourth), stream, role, "t4")
				held := lastRequest(t, fourth)
				if held.ResumeID != "third-session" {
					t.Fatalf("the replacement session was not resumed: %q", held.ResumeID)
				}
				if held.SystemPrompt != "base text v3 of "+role || !strings.Contains(held.Prompt, "policy v4") || strings.Contains(held.Prompt, "policy v3") {
					t.Fatalf("the replacement session's resume: system %q prompt %q", held.SystemPrompt, held.Prompt)
				}
			})
		}
	}
}

func TestLegacyBaselineIsEstablishedOnlyWhereTheServiceAddedNothingToTheRecordedPrompt(t *testing.T) {
	opening := trace.QueuedTurn{Request: trace.TurnRequest{SystemPrompt: "recorded", TurnID: "opened"}}
	production := map[string]coreadapter.Capabilities{trace.ChiefOfStaff: chiefGrant, masonRole: masonGrant, reviewerRole: reviewerGrant, sessionRole: {WriteFiles: true, Execute: true, Web: true}}
	for role := range production {
		if prompt, ok := legacyBaseline(production)(role, opening); ok {
			t.Fatalf("%s: a prompt %q was recovered although the service appended text to it that no record shows", role, prompt)
		}
	}
	quiet := map[string]coreadapter.Capabilities{sessionRole: {WriteFiles: true}}
	if prompt, ok := legacyBaseline(quiet)(sessionRole, opening); !ok || prompt != "recorded" {
		t.Fatalf("a role nothing was appended to: %q %v", prompt, ok)
	}
	if _, ok := legacyBaseline(quiet)(sessionRole, trace.QueuedTurn{}); ok {
		t.Fatal("an opening request that records no system prompt established one")
	}
	if _, ok := legacyBaseline(quiet)("unknown", opening); ok {
		t.Fatal("a role with no grant established a baseline")
	}
}
