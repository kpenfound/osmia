package intakejourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	intake "github.com/kpenfound/osmia/internal/intake"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const designTitleToolName = intake.DesignTitleToolName

// designTitleBrief is a complete brief for draft_brief.
const designTitleBrief = "Decisions: ship the export.\nIntent and tradeoffs: favor simplicity.\nRejected options: a separate service."

// TestDesignTitleToolRecordsReplacesAndSurvivesRestart drives design turns,
// through production enforcement, that call the title tool: refused titles
// record nothing and leave the turn running, an accepted title is reported by
// the design and status responses, a later one replaces it, the brief and its
// revision never change, and the latest title survives a restart. A start
// pinned to the brief revision then succeeds, and the started workstream's
// chief-of-staff turn no longer reaches the tool.
func TestDesignTitleToolRecordsReplacesAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project, engine := newDesignEnforcementFixture(t)
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream
	if opened.Design.Title != "" {
		t.Fatalf("an untitled session reports title %q", opened.Design.Title)
	}

	const first, second = "Plan the billing export", "Plan the billing export and its retry rules"
	var mu sync.Mutex
	var problems []string
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	var postStart []string
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, designTitleToolName) {
			if strings.Contains(req.Prompt, "Where do things stand?") {
				mu.Lock()
				postStart = names
				mu.Unlock()
			}
			return questionResult(req, "session-chief", "ok"), nil
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			fail("design tools %v, want %v", names, want)
		}
		if strings.Contains(req.Prompt, "second") {
			if out, err := callTool(ctx, tools, designTitleToolName, map[string]any{"title": "  " + second + "  "}); err != nil || !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"revision":2`) {
				fail("second title: %s %v", out, err)
			}
			return questionResult(req, "session-design", "Retitled."), nil
		}
		for label, title := range map[string]string{
			"empty":      "   ",
			"multi-line": "Plan the export\nand its retries",
			"long":       strings.Repeat("a ", 101),
			"osmia id":   "Plan " + string(stream),
			"project id": "Plan " + string(project),
			"hash":       "Plan the change 3f2a9c1d7b",
			"model":      "Plan with claude-opus-4",
			"url":        "Plan https://example.com/export",
			"path":       "Plan /etc/export",
			"branch":     "Plan feature/export",
			"file":       "Plan export.go",
		} {
			out, err := callTool(ctx, tools, designTitleToolName, map[string]any{"title": title})
			if err != nil || !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, `"reason"`) {
				fail("%s title was not refused with a reason: %s %v", label, out, err)
			}
		}
		if out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": designTitleBrief}); err != nil || !strings.Contains(out, `"recorded":true`) {
			fail("draft brief: %s %v", out, err)
		}
		if out, err := callTool(ctx, tools, designTitleToolName, map[string]any{"title": first}); err != nil || !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"revision":1`) {
			fail("first title: %s %v", out, err)
		}
		return questionResult(req, "session-design", "Titled."), nil
	}
	await := func(client *Client, entries int) {
		awaitDesignConversation(t, client, stream, func(r ConversationResponse) bool {
			return len(r.Entries) == entries && r.Entries[entries-1].Kind == "response"
		})
		mu.Lock()
		defer mu.Unlock()
		if len(problems) > 0 {
			t.Fatalf("title tool problems:\n%s", strings.Join(problems, "\n"))
		}
	}
	reported := func(client *Client, title string) {
		t.Helper()
		read, err := client.Design(ctx, stream)
		must(t, err)
		if read.Design.Title != title || read.Design.BriefRevision != 1 || !strings.HasPrefix(read.Design.Brief, "Decisions: ship the export.") {
			t.Fatalf("design read title %q revision %d brief %q, want title %q at revision 1", read.Design.Title, read.Design.BriefRevision, read.Design.Brief, title)
		}
		list, err := client.Designs(ctx, project)
		must(t, err)
		if len(list.Designs) != 1 || list.Designs[0].Title != title {
			t.Fatalf("design list: %+v", list.Designs)
		}
		st, err := client.Status(ctx, stream)
		must(t, err)
		if st.Kind != "design" || st.Title != title {
			t.Fatalf("status kind %q title %q, want design titled %q", st.Kind, st.Title, title)
		}
	}

	_, err = c.DesignMessage(ctx, stream, "first: what is this about?")
	must(t, err)
	await(c, 2)
	reported(c, first)
	repository := s.sole().repository
	if title, revision, err := intake.LatestDesignTitle(repository, stream); err != nil || title != first || revision != 1 {
		t.Fatalf("recorded title %q revision %d: %v", title, revision, err)
	}

	_, err = c.DesignMessage(ctx, stream, "second: the subject changed")
	must(t, err)
	await(c, 4)
	reported(c, second)

	options := s.options
	must(t, s.Close())
	restarted, client := start(t, options)
	reported(client, second)
	if title, revision, err := intake.LatestDesignTitle(restarted.sole().repository, stream); err != nil || title != second || revision != 2 {
		t.Fatalf("recorded title after restart %q revision %d: %v", title, revision, err)
	}

	pinned, err := client.DesignStartWithBriefRevision(ctx, stream, 1)
	must(t, err)
	if pinned.State != HandedState {
		t.Fatalf("pinned start: %+v", pinned)
	}
	// Start returns before the started workstream has adopted the design
	// conversation; a send meanwhile is refused with a conflict that says to
	// retry. Retry only that refusal, until adoption completes.
	for end := time.Now().Add(demoTimeout); ; time.Sleep(10 * time.Millisecond) {
		_, err := client.Send(ctx, stream, "Where do things stand?")
		if err == nil {
			break
		}
		if !failed(err, Conflict) || !strings.Contains(err.Error(), "the design workgroup is starting") || !time.Now().Before(end) {
			t.Fatal(err)
		}
	}
	awaitSettled := func() []string {
		for end := time.Now().Add(demoTimeout); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			names := postStart
			mu.Unlock()
			if names != nil {
				return names
			}
		}
		return nil
	}
	if names := awaitSettled(); names == nil || slices.Contains(names, designTitleToolName) || slices.Contains(names, draftBriefToolName) {
		t.Fatalf("started workstream's chief-of-staff tools: %v", names)
	}
}

// TestUntitledDesignSessionReportsNoTitleAndStarts covers the ID fallback:
// a session that was never titled reports no title in the design and status
// responses, and its start succeeds.
func TestUntitledDesignSessionReportsNoTitleAndStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project := newDesignFixture(t)
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	stream := opened.Design.Workstream
	repository := s.sole().repository
	must(t, recordDesignDraft(t, repository, stream, designTitleBrief))
	read, err := c.Design(ctx, stream)
	must(t, err)
	list, err := c.Designs(ctx, project)
	must(t, err)
	st, err := c.Status(ctx, stream)
	must(t, err)
	if read.Design.Title != "" || len(list.Designs) != 1 || list.Designs[0].Title != "" || st.Kind != "design" || st.Title != "" {
		t.Fatalf("untitled session reports title: read %q list %+v status %q", read.Design.Title, list.Designs, st.Title)
	}
	started, err := c.DesignStartWithBriefRevision(ctx, stream, read.Design.BriefRevision)
	must(t, err)
	if started.State != HandedState {
		t.Fatalf("start of an untitled session: %+v", started)
	}
}

// TestDesignRolePromptInstructsTitlingTheSession covers the instruction the
// design agent holds: title the session once its context is clear, replace
// the title when the subject changes, and know a title starts no work.
func TestDesignRolePromptInstructsTitlingTheSession(t *testing.T) {
	t.Parallel()
	for _, want := range []string{designTitleToolName + " once its context is clear", "Call it again to replace the title when the subject of the session changes materially", "starts no work and does not change the brief"} {
		if !strings.Contains(designRolePrompt, want) {
			t.Errorf("design role prompt lacks %q", want)
		}
	}
}
