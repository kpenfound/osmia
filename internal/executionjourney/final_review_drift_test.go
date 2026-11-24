package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFinalReviewNamesAnApprovedDriftRebasesConflictedPathsAndApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "final-drift-notes")
	ctx := context.Background()
	rebase := service.DriftRebase{Drift: 1, Operation: "test-drift-1", Outcome: service.TestDriftReplayed, Branch: intake.FeatureBranch(stream),
		Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: strings.Repeat("a", 40)},
		Before:   strings.Repeat("b", 40), Commit: strings.Repeat("c", 40), Conflicts: []string{"CODEOWNERS"},
		Review: 1, Verdict: &service.UnitVerdict{Decision: "satisfactory", Summary: "Resolution kept both sides."}}
	content, err := json.MarshalIndent(rebase, "", "  ")
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.DriftDocument, Revision: 1, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: execution.ForemanActor, Cause: "test"}, Path: lifecycle.DriftPath, Content: string(content) + "\n"}
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{doc}))

	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	f.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Both criteria are shown.", "findings": []any{}})
		return err
	})
	result, err := a.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("final review result %+v", result)
	}

	th, err := repository.Thread(stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	turn := delivery.FinalTurnID(in.Review, intake.CommitteeAgent(1), 1)
	i := -1
	for j, q := range th.Turns {
		if q.Request.TurnID == turn {
			i = j
		}
	}
	if i < 0 {
		t.Fatalf("the final review turn %s is not recorded: %+v", turn, th.Turns)
	}
	prompt := th.Turns[i].Request.Prompt
	for _, want := range []string{"Drift rebase 1", "CODEOWNERS", "approved"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the final reader's prompt lacks %q:\n%s", want, prompt)
		}
	}
}
