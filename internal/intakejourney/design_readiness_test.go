package intakejourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignReadinessOwnerOverridesAssumptionBeforeBriefHandoff(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream

	const request = "Add resumable uploads while preserving the public API."
	const readiness = "Readiness: Keep the upload endpoint compatible?\nRecommended answer: Yes, preserve existing clients.\n" +
		"<details><summary>Assumptions</summary>Use the inferred upload name. Reply to override.</details>"
	const answer = "Yes, preserve compatibility. Override the assumption: call it resumable-upload."
	const brief = "Decisions: preserve the endpoint and use the owner-selected name resumable-upload.\n" +
		"Intent: add resumable uploads without breaking existing clients.\n" +
		"Tradeoffs: accept a narrower interface in exchange for compatibility.\n" +
		"Rejected options: replacing the endpoint and using the inferred upload name."
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, instruction := range []string{
			"expensive to undo", "contracts, data", "models, migrations, public APIs",
			"genuine forks with no clear signal", "tension with the owner's stated intent",
			"recommended answer", "Infer everything else", "collapsed, overridable assumption",
			"Once the readiness pass is answered",
		} {
			if !strings.Contains(req.SystemPrompt, instruction) {
				return nil, fmt.Errorf("design prompt lacks readiness instruction %q", instruction)
			}
		}
		if !strings.Contains(req.Prompt, answer) {
			return &agent.Result{ClaudeID: "design-readiness", ResultText: readiness}, nil
		}
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": brief})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("record answered readiness brief: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-readiness", ResultText: brief}, nil
	}

	_, err = c.DesignMessage(ctx, stream, request)
	must(t, err)
	awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
		return slices.ContainsFunc(r.Entries, func(e ConversationEntry) bool { return e.Kind == "response" && e.Text == readiness })
	})
	if _, err := c.DesignStart(ctx, stream); err == nil {
		t.Fatal("readiness questions without a brief permitted handoff")
	}
	docs, err := trace.Read[trace.Document](s.sole().repository, stream)
	must(t, err)
	if slices.ContainsFunc(docs, func(d trace.Document) bool { return d.ID == designDraftDocument }) {
		t.Fatal("a brief was recorded before the owner answered readiness")
	}

	_, err = c.DesignMessage(ctx, stream, answer)
	must(t, err)
	conversation := awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
		return slices.ContainsFunc(r.Entries, func(e ConversationEntry) bool { return e.Kind == "response" && e.Text == brief })
	})
	var texts []string
	for _, e := range conversation.Entries {
		texts = append(texts, e.Text)
	}
	if want := []string{request, readiness, answer, brief}; !slices.Equal(texts, want) {
		t.Fatalf("readiness, override and brief order: got %q, want %q", texts, want)
	}
	docs, err = trace.Read[trace.Document](s.sole().repository, stream)
	must(t, err)
	var drafts []trace.Document
	for _, d := range docs {
		if d.ID == designDraftDocument {
			drafts = append(drafts, d)
		}
	}
	if len(drafts) != 1 {
		t.Fatalf("want one brief after readiness, got %+v", drafts)
	}
	var recorded designBriefRecord
	must(t, json.Unmarshal([]byte(drafts[0].Content), &recorded))
	if recorded.Content != brief {
		t.Fatalf("recorded brief lost the owner's decisions: %q", recorded.Content)
	}
	_, err = c.DesignStart(ctx, stream)
	must(t, err)
	handed, err := trace.Get[trace.Document](s.sole().repository, stream, handedDocument, 1)
	must(t, err)
	if !strings.HasPrefix(handed.Content, brief+"\n"+designTranscriptHeading) {
		t.Fatalf("handoff did not put the answered brief ahead of its reference transcript: %q", handed.Content)
	}
}
