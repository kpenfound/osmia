package intakejourney

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addDesignProject(t *testing.T, c *Client, name string) config.ProjectID {
	t.Helper()
	clone := filepath.Join(t.TempDir(), name)
	demoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte(name+" source\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	demoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(context.Background(), ProjectAddRequest{Name: name, Upstream: "owner/" + name, Clone: clone})
	must(t, err)
	return added.Project.ID
}

func recordDesignProjects(t *testing.T, r *trace.Repository, stream config.WorkstreamID, content string, projects []config.ProjectID) int {
	t.Helper()
	_, revision, err := intake.LatestDesignBrief(r, stream)
	must(t, err)
	data, err := json.Marshal(intake.DesignBriefRecord{Content: content, Projects: projects})
	must(t, err)
	h := intake.OwnerHeader(intake.DesignDraftDocument, r.Project(), stream, intake.DraftBriefToolName, time.Now().UTC())
	h.Schema, h.Revision = "osmia.trace.document", revision+1
	must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(data)}}))
	return revision + 1
}

func TestDesignBriefProjectsValidationInheritanceAndContext(t *testing.T) {
	t.Parallel()
	s, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "client")
	empty := addDesignProject(t, c, "empty")
	inactive := addDesignProject(t, c, "inactive")
	_, err := c.RemoveProject(ctx, inactive)
	must(t, err)
	writeCharter(t, c, other, "1. Client charter rule.\n")
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: other}, Mode: "soft", Source: "owner"})
	for _, p := range []config.ProjectID{home, other} {
		r, err := s.repository(p)
		must(t, err)
		h := intake.OwnerHeader("subsystem-design", p, "", "knowledge-base", time.Now().UTC())
		h.Schema = "osmia.trace.document"
		must(t, r.RecordDocuments(ctx, []trace.Document{{Header: h, Path: "kb/design.md", Content: "KB text for " + string(p)}}))
	}
	opened, err := c.OpenDesign(ctx, home)
	must(t, err)
	stream := opened.Design.Workstream
	r, err := s.repository(home)
	must(t, err)
	// A record without a list reads as the session's project.
	must(t, recordDesignDraft(t, r, stream, productionBrief))
	view, err := c.Design(ctx, stream)
	must(t, err)
	if view.Design.BriefRevision != 1 || view.Design.Brief != productionBrief || !slices.Equal(view.Design.Projects, []config.ProjectID{home}) {
		t.Fatalf("legacy brief: %+v", view.Design)
	}
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, instruction := range []string{"owner request to add or remove a project is confirmation", "readiness question with a recommendation", "only after the owner agrees"} {
			if !strings.Contains(req.SystemPrompt, instruction) {
				return nil, fmt.Errorf("missing confirmation rule %q", instruction)
			}
		}
		if strings.Contains(req.Prompt, "check context") {
			if !strings.Contains(req.SystemPrompt, "Client charter rule") || !strings.Contains(req.SystemPrompt, "KB text for "+string(other)) || strings.Contains(req.SystemPrompt, "Keep changes small") || strings.Contains(req.SystemPrompt, "KB text for "+string(home)) {
				return nil, fmt.Errorf("context does not follow brief projects: %s", req.SystemPrompt)
			}
			if _, err := readTool(ctx, tools, "projects/"+string(home)+"/charter.md"); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "design-projects", ResultText: "context checked"}, nil
		}
		for _, projects := range []any{[]config.ProjectID{}, nil, []config.ProjectID{home, home}, []config.ProjectID{inactive}, []config.ProjectID{empty}, []string{"p_0123456789abcdef0123456789abcdef"}} {
			out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": productionBrief, "projects": projects})
			if projects == nil && err != nil {
				continue
			}
			if err != nil || !strings.Contains(out, `"recorded":false`) {
				return nil, fmt.Errorf("invalid list %v: %s %v", projects, out, err)
			}
		}
		_, revision, err := intake.LatestDesignBrief(r, stream)
		if err != nil || revision != 1 {
			return nil, fmt.Errorf("refusal recorded revision %d: %v", revision, err)
		}
		for _, input := range []map[string]any{
			{"content": productionBrief, "projects": []config.ProjectID{home, other}},
			{"content": productionBrief + "Keep the list."},
		} {
			out, err := callTool(ctx, tools, draftBriefToolName, input)
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				return nil, fmt.Errorf("valid brief: %s %v", out, err)
			}
		}
		brief, revision, err := intake.LatestDesignBrief(r, stream)
		if err != nil || revision != 3 || !slices.Equal(brief.Projects, []config.ProjectID{home, other}) {
			return nil, fmt.Errorf("omission lost list: %+v %d %v", brief, revision, err)
		}
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": productionBrief, "projects": []config.ProjectID{other}})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("remove home: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-projects", ResultText: "projects recorded"}, nil
	}
	engine.Mu.Unlock()
	for i, message := range []string{"Add client and then remove the home project.", "check context"} {
		_, err = c.DesignMessage(ctx, stream, message)
		must(t, err)
		conversation := awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
			if len(r.Entries) > 0 && r.Entries[len(r.Entries)-1].State == api.TurnFailed {
				return true
			}
			return len(r.Entries) == (i+1)*2 && r.Entries[len(r.Entries)-1].Kind == "response"
		})
		if conversation.Entries[len(conversation.Entries)-1].State == api.TurnFailed {
			th, err := r.ChiefOfStaffThread(stream)
			must(t, err)
			t.Fatalf("design turn failed: %+v; response: %+v", conversation, th.Turns[len(th.Turns)-1].Response)
		}
	}
	view, err = c.Design(ctx, stream)
	must(t, err)
	if view.Design.BriefRevision != 4 || view.Design.Brief != strings.TrimSpace(productionBrief) || !slices.Equal(view.Design.Projects, []config.ProjectID{other}) {
		t.Fatalf("latest brief: %+v", view.Design)
	}
}

func TestDesignStartRevisionConflictRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
	ctx := context.Background()
	r, err := f.s.repository(f.project)
	must(t, err)
	revision := recordDesignProjects(t, r, f.origin, productionBrief, []config.ProjectID{f.project, f.project2})
	stale := revision - 1
	err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &stale}, nil)
	if !failed(err, Conflict) {
		t.Fatalf("stale start: %v", err)
	}
	open, err := intake.IsDesignWorkstream(r, f.origin)
	must(t, err)
	if !open {
		t.Fatal("stale start closed the session")
	}
	docs, err := trace.Read[trace.Document](r, f.origin)
	must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument || d.ID == intake.DesignSelectionDocument || d.ID == intake.DesignStartIntentDocument {
			t.Fatalf("stale start recorded %s", d.ID)
		}
	}
	for _, p := range []config.ProjectID{f.project, f.project2} {
		repo, err := f.s.repository(p)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		if slices.Contains(streams, f.stream1) || slices.Contains(streams, f.stream2) {
			t.Fatal("stale start allocated")
		}
	}
	must(t, f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil))
	f.handed, err = trace.Get[trace.Document](r, f.origin, intake.HandedDocument, 1)
	must(t, err)
	f.awaitPacket(t)
	f.checkContinuation(t)
	selected, found, err := intake.DesignSelectionOf(r, f.origin)
	must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Repo != f.project || selected.Members[1].Repo != f.project2 {
		t.Fatalf("owner decision: %+v", selected)
	}
	doc, err := trace.Get[trace.Document](r, f.origin, intake.DesignSelectionDocument, 1)
	must(t, err)
	if doc.Actor != ownerActor {
		t.Fatalf("selection actor %s", doc.Actor)
	}
}

func TestDesignPinnedStartRecoversEveryDurableBoundaryWithNewerDraft(t *testing.T) {
	for _, boundary := range []struct {
		step string
		nth  int32
	}{
		{"design-start-intent-recorded", 1}, {"design-brief-handed", 1}, {"design-selection-recorded", 1}, {"design-session-handed", 1},
		{"design-factory-policy-recorded", 1}, {"design-member-brief-recorded", 1}, {"design-member-brief-recorded", 2}, {"design-start-active", 1}, {"design-member-handed", 1}, {"design-member-handed", 2}, {"design-start-complete", 1},
		{"design-member-allocated", 1}, {"design-member-allocated", 2}, {"design-workgroup-prepared", 1},
		{"design-member-joined", 1}, {"design-member-joined", 2}, {"design-conversation-imported", 1}, {"design-conversation-adopted", 1},
	} {
		t.Run(fmt.Sprintf("%s-%d", boundary.step, boundary.nth), func(t *testing.T) {
			t.Parallel()
			f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
			ctx := context.Background()
			r, err := f.s.repository(f.project)
			must(t, err)
			revision := recordDesignProjects(t, r, f.origin, productionBrief, []config.ProjectID{f.project, f.project2})
			hook := &designInterruption{Step: boundary.step, Nth: boundary.nth, Reached: make(chan struct{})}
			f.hook.Store(hook)
			started := make(chan error, 1)
			go func() {
				started <- f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			}()
			select {
			case <-hook.Reached:
			case <-time.After(demoTimeout):
				f.timeout(t, "start interruption "+boundary.step)
			}
			pinned, found, err := intake.DesignStartIntentOf(r, f.origin)
			must(t, err)
			if !found || pinned.BriefRevision != revision || !slices.Equal(pinned.Projects, []config.ProjectID{f.project, f.project2}) || !strings.HasPrefix(pinned.Handed.Content, productionBrief) {
				t.Fatalf("pinned start: %+v", pinned)
			}
			f.handed = pinned.Handed
			// Simulate a newer trace draft arriving before recovery. It cannot supply
			// either half of the already accepted start.
			recordDesignProjects(t, r, f.origin, "Decisions: different. Intent and tradeoffs: new. Rejected: previous.", []config.ProjectID{f.project2})
			f.stop(t)
			<-started
			f.hook.Store(nil)
			f.start(t)
			f.awaitPacket(t)
			f.checkContinuation(t)
			r, err = f.s.repository(f.project)
			must(t, err)
			recovered, found, err := intake.DesignStartIntentOf(r, f.origin)
			must(t, err)
			if !found || !equalJSONTest(recovered, pinned) {
				t.Fatal("recovery rewrote pinned start")
			}
			selected, found, err := intake.DesignSelectionOf(r, f.origin)
			must(t, err)
			if !found || len(selected.Members) != 2 {
				t.Fatalf("owner selection: %+v", selected)
			}
			for i, member := range selected.Members {
				project := []config.ProjectID{f.project, f.project2}[i]
				stream := []config.WorkstreamID{f.stream1, f.stream2}[i]
				if member.Repo != project || member.Workstream != stream {
					t.Fatalf("member identity changed: %+v", member)
				}
				repo, err := f.s.repository(project)
				must(t, err)
				docs, err := trace.Read[trace.Document](repo, stream)
				must(t, err)
				count := 0
				for _, d := range docs {
					if d.ID == intake.HandedDocument {
						count++
						if d.Content != pinned.Handed.Content || d.Revision != 1 {
							t.Fatal("member handed input changed")
						}
					}
				}
				if count != 1 {
					t.Fatalf("member has %d handed records", count)
				}
				streams, err := repo.Workstreams()
				must(t, err)
				for _, other := range streams {
					if other != stream && other != f.origin && other != intake.LibrarianWorkstream(project) {
						t.Fatalf("extra allocated member %s", other)
					}
				}
			}
			if _, ok, err := f.s.intake().LegacyProjectsEntry(r, f.origin); err != nil || ok {
				t.Fatalf("new start held as legacy: %v %v", ok, err)
			}
			// A second restart reconciles the same member identities and shared chief.
			f.stop(t)
			f.start(t)
			f.awaitPacket(t)
			f.checkContinuation(t)
		})
	}
}

func TestDesignStartExcludesConversationHomeAndContinuesSharedChief(t *testing.T) {
	t.Parallel()
	s, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "only-member")
	writeCharter(t, c, other, "1. Keep changes small.\n")
	opened, err := c.OpenDesign(ctx, home)
	must(t, err)
	origin := opened.Design.Workstream
	r, err := s.repository(home)
	must(t, err)
	before, err := r.ChiefOfStaffThread(origin)
	must(t, err)
	revision := recordDesignProjects(t, r, origin, productionBrief, []config.ProjectID{other})
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "continued-chief", ResultText: "The shared chief continues."}, nil
	}
	engine.Mu.Unlock()
	must(t, c.Do(ctx, "POST", "/v1/design/"+string(origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil))
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := r.Workflow(origin, intake.DesignStartSubject)
		must(t, err)
		if state.Value == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("start remained %s", state.Value)
		}
		time.Sleep(10 * time.Millisecond)
	}
	selected, found, err := intake.DesignSelectionOf(r, origin)
	must(t, err)
	if !found || len(selected.Members) != 1 || selected.Members[0].Repo != other || selected.Members[0].Workstream == origin {
		t.Fatalf("selected members: %+v", selected)
	}
	groupID, err := r.CrossRepoWorkgroupOf(origin)
	must(t, err)
	_, group, _, err := r.CrossRepoProjectSpec(groupID)
	must(t, err)
	if group.Conversation == nil || group.Conversation.Project != home || group.Conversation.Workstream != origin {
		t.Fatalf("conversation moved: %+v", group)
	}
	chief, host, _, close, err := s.chiefRepository(ctx, r, origin)
	must(t, err)
	after, err := chief.Thread(host, trace.ChiefOfStaff)
	close()
	must(t, err)
	if !equalJSONTest(before.Identity, after.Identity) {
		t.Fatal("shared chief identity changed")
	}
	_, err = c.Send(ctx, origin, "Continue the conversation at home.")
	must(t, err)
	deadline = time.Now().Add(demoTimeout)
	for {
		conversation, err := c.Conversation(ctx, origin)
		must(t, err)
		if slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool { return e.Kind == "response" && e.Text == "The shared chief continues." }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared chief did not continue: %+v", conversation)
		}
		time.Sleep(10 * time.Millisecond)
	}
	memberRepo, err := s.repository(other)
	must(t, err)
	memberGroup, err := memberRepo.CrossRepoWorkgroupOf(selected.Members[0].Workstream)
	must(t, err)
	if memberGroup != groupID {
		t.Fatal("member joined another workgroup")
	}
}

func TestDesignStartKeepsUpgradeSelectionRecordedBeforeHandedInput(t *testing.T) {
	t.Parallel()
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{})
	ctx := context.Background()
	r, err := f.s.repository(f.project)
	must(t, err)
	recordDesignProjects(t, r, f.origin, productionBrief, []config.ProjectID{f.project})
	f.sharedDesign().RecordSelection(t, f.project, f.project2)
	_, err = f.c.DesignStart(ctx, f.origin)
	must(t, err)
	f.handed, err = trace.Get[trace.Document](r, f.origin, intake.HandedDocument, 1)
	must(t, err)
	f.awaitPacket(t)
	f.checkContinuation(t)
	pinned, found, err := intake.DesignStartIntentOf(r, f.origin)
	must(t, err)
	if !found || !slices.Equal(pinned.Projects, []config.ProjectID{f.project, f.project2}) {
		t.Fatalf("upgrade selection changed: %+v", pinned)
	}
}
