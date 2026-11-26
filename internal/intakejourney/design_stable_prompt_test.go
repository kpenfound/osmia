package intakejourney

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type designTurn struct{ resume, system, prompt string }

func recordDesignBrief(t *testing.T, d *designProjects, record intake.DesignBriefRecord) {
	t.Helper()
	_, revision, err := intake.LatestDesignBrief(d.repo, d.stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(record)
	fixturekit.Must(t, err)
	h := intake.OwnerHeader(intake.DesignDraftDocument, d.repo.Project(), d.stream, intake.DraftBriefToolName, time.Now().UTC())
	h.Schema, h.Revision = "osmia.trace.document", revision+1
	fixturekit.Must(t, d.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(data)}}))
}

func TestDesignTurnsKeepTheSessionsSystemPromptWhileTheBriefChanges(t *testing.T) {
	for _, backend := range []string{"claude", "opencode"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			before, after := "", ""
			if backend == "opencode" {
				before, after = "[profiles.design]\nagent = \"opencode\"\nmodel = \"design-model\"\n", "profile = \"design\"\n"
			}
			s, c, home, engine := newDesignEnforcementFixtureWith(t, before, after)
			d := &designProjects{s: s, c: c, engine: engine, home: home, other: addDesignProject(t, c, "client")}
			opened, err := c.OpenDesign(context.Background(), home)
			fixturekit.Must(t, err)
			d.stream = opened.Design.Workstream
			d.repo, err = s.Repository(home)
			fixturekit.Must(t, err)
			recordDesignBrief(t, d, intake.DesignBriefRecord{Content: "Decisions, intent, tradeoffs and rejected options.", Projects: nil})

			var mu sync.Mutex
			var seen []designTurn
			engine.Mu.Lock()
			engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
			engine.Mu.Unlock()
			d.script(func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				mu.Lock()
				seen = append(seen, designTurn{req.ResumeID, req.SystemPrompt, req.Prompt})
				mu.Unlock()
				return &agent.Result{ClaudeID: "design-session", ResultText: "ok"}, nil
			})

			type change struct {
				record func() intake.DesignBriefRecord
				want   []string
				not    []string
			}
			content := "Decisions, intent, tradeoffs and rejected options."
			otherRow := fmt.Sprintf("%s | client | owner/client | true |", d.other)
			changes := []change{
				{record: func() intake.DesignBriefRecord { return intake.DesignBriefRecord{Content: content, Projects: nil} },
					want: []string{"Current kind of the brief: feature", fmt.Sprintf("%s | client | owner/client | false |", d.other)}, not: []string{"New project in the current brief"}},
				{record: func() intake.DesignBriefRecord {
					return intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home, d.other}}
				}, want: []string{otherRow, "Current kind of the brief: feature"}, not: []string{"New project in the current brief"}},
				{record: func() intake.DesignBriefRecord {
					return intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home}, Kind: "hill-climb"}
				}, want: []string{"Current kind of the brief: hill-climb", fmt.Sprintf("%s | client | owner/client | false |", d.other)}, not: []string{otherRow}},
				{record: func() intake.DesignBriefRecord {
					return intake.DesignBriefRecord{Content: content, Projects: []config.ProjectID{home, d.other}, Kind: "feature", NewProject: &api.NewProject{Name: "Gadget", Repo: "gadget", Visibility: "private", Description: "A gadget.", Charter: "1. Keep it small.\n"}}
				}, want: []string{"Current kind of the brief: feature", "New project in the current brief", "Name: Gadget", otherRow}},
			}
			entries := 0
			for i, step := range changes {
				if i == 2 {
					options := s.Options
					fixturekit.Must(t, s.Close())
					restarted, client := journey.StartT(t, options)
					d.s, d.c = restarted, client
					d.repo, err = restarted.Repository(home)
					fixturekit.Must(t, err)
				}
				if i > 0 {
					recordDesignBrief(t, d, step.record())
				}
				entries += 2
				d.say(t, fmt.Sprintf("message %d", i+1), entries)
				mu.Lock()
				ran, got, first := len(seen), seen[len(seen)-1], seen[0]
				mu.Unlock()
				if ran != i+1 {
					t.Fatalf("turn %d: the engine ran %d turns", i+1, ran)
				}
				if (i == 0) != (got.resume == "") || (i > 0 && got.resume != "design-session") {
					t.Fatalf("turn %d resumed %q", i+1, got.resume)
				}
				if got.system != intake.DesignRolePrompt || got.system != first.system {
					t.Fatalf("turn %d: the system prompt is not the one the session began with", i+1)
				}
				for _, want := range step.want {
					if !strings.Contains(got.prompt, want) {
						t.Fatalf("turn %d: the newest turn lacks %q:\n%s", i+1, want, got.prompt)
					}
				}
				for _, not := range step.not {
					if strings.Contains(got.prompt, not) {
						t.Fatalf("turn %d: the newest turn still carries %q:\n%s", i+1, not, got.prompt)
					}
				}
				if !strings.HasSuffix(got.prompt, fmt.Sprintf("message %d", i+1)) || !strings.Contains(got.prompt, coreadapter.TurnContextHeader) {
					t.Fatalf("turn %d: the message does not follow the turn context block:\n%s", i+1, got.prompt)
				}
			}
		})
	}
}
