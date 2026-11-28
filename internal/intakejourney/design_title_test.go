package intakejourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	intake "github.com/kpenfound/osmia/internal/intake"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const designTitleBrief = "Decisions: ship the export.\nIntent and tradeoffs: favor simplicity.\nRejected options: a separate service."

func TestDesignTitleToolRecordsReplacesAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project, engine := newDesignEnforcementFixture(t)
	opened, err := c.OpenDesign(ctx, project)
	fixturekit.Must(t, err)
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
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, intake.DesignTitleToolName) {
			if strings.Contains(req.Prompt, "Where do things stand?") {
				mu.Lock()
				postStart = names
				mu.Unlock()
			}
			return intakefixture.QuestionResult(req, "session-chief", "ok"), nil
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			fail("design tools %v, want %v", names, want)
		}
		if strings.Contains(req.Prompt, "second") {
			if out, err := fixturekit.CallTool(ctx, tools, intake.DesignTitleToolName, map[string]any{"title": "  " + second + "  "}); err != nil || !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"revision":2`) {
				fail("second title: %s %v", out, err)
			}
			return intakefixture.QuestionResult(req, "session-design", "Retitled."), nil
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
			out, err := fixturekit.CallTool(ctx, tools, intake.DesignTitleToolName, map[string]any{"title": title})
			if err != nil || !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, `"reason"`) {
				fail("%s title was not refused with a reason: %s %v", label, out, err)
			}
		}
		if out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": designTitleBrief}); err != nil || !strings.Contains(out, `"recorded":true`) {
			fail("draft brief: %s %v", out, err)
		}
		if out, err := fixturekit.CallTool(ctx, tools, intake.DesignTitleToolName, map[string]any{"title": first}); err != nil || !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"revision":1`) {
			fail("first title: %s %v", out, err)
		}
		return intakefixture.QuestionResult(req, "session-design", "Titled."), nil
	}
	await := func(client *journey.Client, entries int) {
		awaitDesignConversation(t, client, stream, func(r api.ConversationResponse) bool {
			return len(r.Entries) == entries && r.Entries[entries-1].Kind == "response"
		})
		mu.Lock()
		defer mu.Unlock()
		if len(problems) > 0 {
			t.Fatalf("title tool problems:\n%s", strings.Join(problems, "\n"))
		}
	}
	reported := func(client *journey.Client, title string) {
		t.Helper()
		read, err := client.Design(ctx, stream)
		fixturekit.Must(t, err)
		if read.Design.Title != title || read.Design.BriefRevision != 1 || !strings.HasPrefix(read.Design.Brief, "Decisions: ship the export.") {
			t.Fatalf("design read title %q revision %d brief %q, want title %q at revision 1", read.Design.Title, read.Design.BriefRevision, read.Design.Brief, title)
		}
		list, err := client.Designs(ctx, project)
		fixturekit.Must(t, err)
		if len(list.Designs) != 1 || list.Designs[0].Title != title {
			t.Fatalf("design list: %+v", list.Designs)
		}
		st, err := client.Status(ctx, stream)
		fixturekit.Must(t, err)
		if st.Kind != "design" || st.Title != title {
			t.Fatalf("status kind %q title %q, want design titled %q", st.Kind, st.Title, title)
		}
	}

	_, err = c.DesignMessage(ctx, stream, "first: what is this about?")
	fixturekit.Must(t, err)
	await(c, 2)
	reported(c, first)
	repository := s.Sole().Repository
	if title, revision, err := intake.LatestDesignTitle(repository, stream); err != nil || title != first || revision != 1 {
		t.Fatalf("recorded title %q revision %d: %v", title, revision, err)
	}

	_, err = c.DesignMessage(ctx, stream, "second: the subject changed")
	fixturekit.Must(t, err)
	await(c, 4)
	reported(c, second)

	options := s.Options
	fixturekit.Must(t, s.Close())
	restarted, client := journey.StartT(t, options)
	reported(client, second)
	if title, revision, err := intake.LatestDesignTitle(restarted.Sole().Repository, stream); err != nil || title != second || revision != 2 {
		t.Fatalf("recorded title after restart %q revision %d: %v", title, revision, err)
	}

	pinned, err := client.DesignStartWithBriefRevision(ctx, stream, 1)
	fixturekit.Must(t, err)
	if pinned.State != intake.HandedState {
		t.Fatalf("pinned start: %+v", pinned)
	}
	restarted.Until(t, "the started workstream to adopt the design conversation", func() bool {
		_, err := client.Send(ctx, stream, "Where do things stand?")
		if err == nil {
			return true
		}
		if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "the design workgroup is starting") {
			t.Fatal(err)
		}
		return false
	})
	var names []string
	restarted.Until(t, "the started workstream's manager to receive its tools", func() bool {
		mu.Lock()
		defer mu.Unlock()
		names = postStart
		return names != nil
	})
	if names == nil || slices.Contains(names, intake.DesignTitleToolName) || slices.Contains(names, intake.DraftBriefToolName) {
		t.Fatalf("started workstream's manager tools: %v", names)
	}
}

func TestUntitledDesignSessionReportsNoTitleAndStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, project := journey.NewDesignFixture(t)
	opened, err := c.OpenDesign(ctx, project)
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	repository := s.Sole().Repository
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, repository, stream, designTitleBrief))
	read, err := c.Design(ctx, stream)
	fixturekit.Must(t, err)
	list, err := c.Designs(ctx, project)
	fixturekit.Must(t, err)
	st, err := c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if read.Design.Title != "" || len(list.Designs) != 1 || list.Designs[0].Title != "" || st.Kind != "design" || st.Title != "" {
		t.Fatalf("untitled session reports title: read %q list %+v status %q", read.Design.Title, list.Designs, st.Title)
	}
	started, err := c.DesignStartWithBriefRevision(ctx, stream, read.Design.BriefRevision)
	fixturekit.Must(t, err)
	if started.State != intake.HandedState {
		t.Fatalf("start of an untitled session: %+v", started)
	}
}
