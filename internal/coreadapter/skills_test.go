package coreadapter_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

var roleSkills = []string{"https://github.com/acme/skills#skills/tdd", "https://github.com/acme/review@v1"}

// skillTurn is a read-only claude turn of a role that configures skills, and
// an engine that prepares skills in its own directory.
func skillTurn(t *testing.T, mode string) (a.PreparedTurn, *adaptertest.Engine) {
	t.Helper()
	turn := toolTurn(t, mode)
	turn.Execution.Skills = slices.Clone(roleSkills)
	return turn, &adaptertest.Engine{Skills: []string{filepath.Join(t.TempDir(), "skills")}}
}

func TestClaudeTurnGetsRoleSkills(t *testing.T) {
	for _, mode := range []string{"none", "container", "sbx"} {
		t.Run(mode, func(t *testing.T) {
			turn, engine := skillTurn(t, mode)
			if _, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			req := engine.Requests[0]
			if !slices.Equal(req.Profile.Skills, roleSkills) {
				t.Fatalf("skills %v", req.Profile.Skills)
			}
			if !slices.Contains(req.Grants.Mounts, agent.Mount{Path: engine.Skills[0], Access: agent.ReadOnly}) {
				t.Fatalf("skill directory not granted read-only: %+v", req.Grants.Mounts)
			}
			if !slices.Equal(req.Grants.Tools[:4], []string{"Read", "Glob", "Grep", "Skill"}) || req.Profile.AllowedTools[len(req.Profile.AllowedTools)-1] != "Skill" {
				t.Fatalf("tools %v allowed %v", req.Grants.Tools, req.Profile.AllowedTools)
			}
			if req.Workspace.Directory() != filepath.Join(turn.SessionDirectory, "work") {
				t.Fatalf("read-only turn starts in %s", req.Workspace.Directory())
			}
		})
	}
}

func TestOtherBackendsGetNoSkills(t *testing.T) {
	for _, backend := range []string{agent.AgentCodex, agent.AgentOpenCode} {
		t.Run(backend, func(t *testing.T) {
			turn, engine := skillTurn(t, "none")
			turn.Profile.Backend = backend
			engine.Skills = nil
			if _, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			req := engine.Requests[0]
			if len(req.Profile.Skills) != 0 || len(req.Grants.Mounts) != 3 || slices.Contains(req.Grants.Tools, "Skill") || slices.Contains(req.Profile.AllowedTools, "Skill") {
				t.Fatalf("skills %v mounts %v tools %v allowed %v", req.Profile.Skills, req.Grants.Mounts, req.Grants.Tools, req.Profile.AllowedTools)
			}
		})
	}
}

func TestSkillsNeedAnEngineThatPreparesThem(t *testing.T) {
	turn, engine := skillTurn(t, "none")
	engine.Skills = nil
	result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
	if !errors.Is(err, a.ErrUnsupported) || !result.IsError || len(engine.Prepared) != 0 {
		t.Fatalf("err=%v result=%+v prepared=%d", err, result, len(engine.Prepared))
	}
}

func TestInvalidSkillReferenceIsRefused(t *testing.T) {
	for name, ref := range map[string]string{"empty": " ", "escapes": "https://github.com/acme/skills#../other", "option": "--upload-pack=x"} {
		t.Run(name, func(t *testing.T) {
			turn, engine := skillTurn(t, "none")
			turn.Execution.Skills = []string{ref}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if err := executor.Check(context.Background(), turn.Sandbox.Verified, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("check: %v", err)
			}
			if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); !errors.Is(err, a.ErrUnsupported) || len(engine.Prepared) != 0 {
				t.Fatalf("run: %v prepared=%d", err, len(engine.Prepared))
			}
		})
	}
}

func TestSkillPolicyMustMatchGrants(t *testing.T) {
	for name, mutate := range map[string]func(p *agent.Policy, skills string){
		"writable skills": func(p *agent.Policy, skills string) { setAccess(p, skills, agent.ReadWrite) },
		"no Skill tool": func(p *agent.Policy, _ string) {
			p.Tools = slices.DeleteFunc(slices.Clone(p.Tools), func(t string) bool { return t == "Skill" })
		},
	} {
		t.Run(name, func(t *testing.T) {
			turn, engine := skillTurn(t, "container")
			engine.Policy = func(p *agent.Policy) { mutate(p, engine.Skills[0]) }
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			var refusal *a.UnsupportedError
			if !errors.As(err, &refusal) || refusal.Capability != "session policy" || len(engine.Requests) != 0 || !engine.Released() {
				t.Fatalf("policy accepted: %v launches=%d", err, len(engine.Requests))
			}
		})
	}
}

func TestRawRequestCannotChangeRoleSkills(t *testing.T) {
	for name, mutate := range map[string]func(*agent.Request){
		"other skills": func(r *agent.Request) { r.Profile.Skills = []string{"https://github.com/acme/other"} },
		"no skills":    func(r *agent.Request) { r.Profile.Skills = nil },
		"no Skill tool": func(r *agent.Request) {
			r.Profile.AllowedTools = r.Profile.AllowedTools[:len(r.Profile.AllowedTools)-1]
		},
	} {
		t.Run(name, func(t *testing.T) {
			turn, engine := skillTurn(t, "container")
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			req := engine.Requests[0]
			req.Workspace = vcs.Directory(turn.Sandbox.Verified.Workspace.Directory)
			if _, err := executor.Run(context.Background(), req, turn.Execution); err != nil {
				t.Fatalf("unchanged request refused: %v", err)
			}
			mutate(&req)
			if _, err := executor.Run(context.Background(), req, turn.Execution); !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 2 {
				t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
			}
		})
	}
}
