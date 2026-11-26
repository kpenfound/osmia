package intakejourney

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHandInRecordsItsKindInTheCreatingCommit(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	input := "# Design\n"
	defaulted := f.HandIn(t, api.HandInRequest{Key: "default", Stdin: &input})
	explicit := f.HandIn(t, api.HandInRequest{Key: "explicit", Kind: "feature", Stdin: &input})
	for _, out := range []api.HandInResponse{defaulted, explicit} {
		if recorded, err := f.Repository().RecordedKind(out.Workstream); err != nil || recorded != "feature" {
			t.Fatalf("recorded kind of %s: %q %v", out.Workstream, recorded, err)
		}
	}
	head := f.Head(t)
	_, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "mystery", Kind: "mystery", Stdin: &input})
	if !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "mystery") || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("an unregistered kind: %v", err)
	}
	if !f.Unchanged(t, head) {
		t.Fatal("a refused kind wrote to the trace")
	}
	_, err = f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "default", Kind: "mystery", Stdin: &input})
	if !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("retrying a key with another kind: %v", err)
	}
}

func TestDraftBriefRecordsAndKeepsTheKind(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	d.script(func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, want := range []string{"## Workstream kinds", "- feature: use when", "Limits: none.", "Current kind of the brief: feature"} {
			if !strings.Contains(req.Prompt, want) {
				return nil, fmt.Errorf("the design agent's turn context lacks %q", want)
			}
		}
		brief, revision, err := intake.LatestDesignBrief(d.repo, d.stream)
		if err != nil || revision != 1 || brief.Kind != "" {
			return nil, fmt.Errorf("a brief that never set a kind: %+v %d %v", brief, revision, err)
		}
		for i, input := range []map[string]any{
			{"content": intakefixture.ProductionBrief, "kind": "feature"},
			{"content": intakefixture.ProductionBrief + "Keep the kind."},
		} {
			out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, input)
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				return nil, fmt.Errorf("valid brief %d: %s %v", i, out, err)
			}
			brief, revision, err := intake.LatestDesignBrief(d.repo, d.stream)
			if err != nil || revision != i+2 || brief.Kind != "feature" {
				return nil, fmt.Errorf("brief %d: %+v %d %v", i, brief, revision, err)
			}
		}
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief, "kind": "mystery"})
		if err == nil && !strings.Contains(out, `"recorded":false`) {
			return nil, fmt.Errorf("an unregistered kind was recorded: %s", out)
		}
		if _, revision, err := intake.LatestDesignBrief(d.repo, d.stream); err != nil || revision != 3 {
			return nil, fmt.Errorf("a refusal recorded revision %d: %v", revision, err)
		}
		return &agent.Result{ClaudeID: "design-kinds", ResultText: "I chose the feature kind."}, nil
	})
	d.say(t, "Choose the kind.", 2)
	view, err := d.c.Design(context.Background(), d.stream)
	fixturekit.Must(t, err)
	if view.Design.Kind != "feature" || view.Design.BriefRevision != 3 {
		t.Fatalf("design view: %+v", view.Design)
	}
}

func TestDesignViewShowsFeatureForABriefThatNeverSetAKind(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	view, err := d.c.Design(context.Background(), d.stream)
	fixturekit.Must(t, err)
	if view.Design.Kind != "feature" {
		t.Fatalf("design view: %+v", view.Design)
	}
}

func TestDesignStartPinsTheKindOnEveryMember(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	ctx := context.Background()
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	revision := recordDesignProjects(t, r, f.Origin, intakefixture.ProductionBrief, []config.ProjectID{f.Project, f.Project2})
	fixturekit.Must(t, f.C.Do(ctx, "POST", "/v1/design/"+string(f.Origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil))
	f.AwaitPacket(t)
	intent, found, err := intake.DesignStartIntentOf(r, f.Origin)
	fixturekit.Must(t, err)
	if !found || intent.Kind != "feature" || intent.BriefRevision != revision {
		t.Fatalf("pinned start: %+v", intent)
	}
	for i, project := range []config.ProjectID{f.Project, f.Project2} {
		stream := []config.WorkstreamID{f.Stream1, f.Stream2}[i]
		repo, err := f.S.Repository(project)
		fixturekit.Must(t, err)
		if recorded, err := repo.RecordedKind(stream); err != nil || recorded != "feature" {
			t.Fatalf("recorded kind of %s: %q %v", stream, recorded, err)
		}
		docs, err := trace.Read[trace.Document](repo, stream)
		fixturekit.Must(t, err)
		if !slices.ContainsFunc(docs, func(d trace.Document) bool { return d.ID == intake.HandedDocument }) {
			t.Fatalf("%s has no handed brief", stream)
		}
	}
}
