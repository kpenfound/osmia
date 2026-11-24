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

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addDesignProject(t *testing.T, c *journey.Client, name string) config.ProjectID {
	t.Helper()
	clone := filepath.Join(t.TempDir(), name)
	fixturekit.DemoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte(name+" source\n"), 0600))
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(context.Background(), api.ProjectAddRequest{Name: name, Upstream: "owner/" + name, Clone: clone})
	fixturekit.Must(t, err)
	return added.Project.ID
}

func recordDesignProjects(t *testing.T, r *trace.Repository, stream config.WorkstreamID, content string, projects []config.ProjectID) int {
	t.Helper()
	_, revision, err := intake.LatestDesignBrief(r, stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(intake.DesignBriefRecord{Content: content, Projects: projects})
	fixturekit.Must(t, err)
	h := intake.OwnerHeader(intake.DesignDraftDocument, r.Project(), stream, intake.DraftBriefToolName, time.Now().UTC())
	h.Schema, h.Revision = "osmia.trace.document", revision+1
	fixturekit.Must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(data)}}))
	return revision + 1
}

type designProjects struct {
	s        *journey.Service
	c        *journey.Client
	engine   *fixturekit.DemoEngine
	repo     *trace.Repository
	stream   config.WorkstreamID
	home     config.ProjectID
	other    config.ProjectID
	inactive config.ProjectID
}

func newDesignProjects(t *testing.T) *designProjects {
	t.Helper()
	s, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	d := &designProjects{s: s, c: c, engine: engine, home: home, other: addDesignProject(t, c, "client"), inactive: addDesignProject(t, c, "inactive")}
	_, err := c.RemoveProject(ctx, d.inactive)
	fixturekit.Must(t, err)
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	d.stream = opened.Design.Workstream
	d.repo, err = s.Repository(home)
	fixturekit.Must(t, err)
	fixturekit.Must(t, intakefixture.RecordDesignDraft(t, d.repo, d.stream, intakefixture.ProductionBrief))
	return d
}

func (d *designProjects) script(turn fixturekit.DemoTurn) {
	d.engine.Mu.Lock()
	defer d.engine.Mu.Unlock()
	d.engine.Turns["*"] = turn
}

func (d *designProjects) say(t *testing.T, message string, entries int) {
	t.Helper()
	_, err := d.c.DesignMessage(context.Background(), d.stream, message)
	fixturekit.Must(t, err)
	conversation := awaitDesignConversation(t, d.c, d.stream, func(r api.ConversationResponse) bool {
		if len(r.Entries) > 0 && r.Entries[len(r.Entries)-1].State == api.TurnFailed {
			return true
		}
		return len(r.Entries) == entries && r.Entries[len(r.Entries)-1].Kind == "response"
	})
	if conversation.Entries[len(conversation.Entries)-1].State == api.TurnFailed {
		th, err := d.repo.ChiefOfStaffThread(d.stream)
		fixturekit.Must(t, err)
		t.Fatalf("design turn failed: %+v; response: %+v", conversation, th.Turns[len(th.Turns)-1].Response)
	}
}

func TestDraftBriefRefusesInvalidProjectLists(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	d.script(func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, projects := range []any{[]config.ProjectID{}, nil, []config.ProjectID{d.home, d.home}, []config.ProjectID{d.inactive}, []string{"p_0123456789abcdef0123456789abcdef"}} {
			out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief, "projects": projects})
			if projects == nil && err != nil {
				continue
			}
			if err != nil || !strings.Contains(out, `"recorded":false`) {
				return nil, fmt.Errorf("invalid list %v: %s %v", projects, out, err)
			}
		}
		return &agent.Result{ClaudeID: "design-projects", ResultText: "refused"}, nil
	})
	d.say(t, "Record these projects.", 2)
	if _, revision, err := intake.LatestDesignBrief(d.repo, d.stream); err != nil || revision != 1 {
		t.Fatalf("a refusal recorded revision %d: %v", revision, err)
	}
}

func TestDraftBriefKeepsItsProjectListWhenAnEditOmitsOne(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	view, err := d.c.Design(context.Background(), d.stream)
	fixturekit.Must(t, err)
	if view.Design.BriefRevision != 1 || view.Design.Brief != intakefixture.ProductionBrief || !slices.Equal(view.Design.Projects, []config.ProjectID{d.home}) {
		t.Fatalf("design view: %+v", view.Design)
	}
	d.script(func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, input := range []map[string]any{
			{"content": intakefixture.ProductionBrief, "projects": []config.ProjectID{d.home, d.other}},
			{"content": intakefixture.ProductionBrief + "Keep the list."},
			{"content": intakefixture.ProductionBrief, "projects": []config.ProjectID{d.other}},
		} {
			out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, input)
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				return nil, fmt.Errorf("valid brief: %s %v", out, err)
			}
			if _, has := input["projects"]; !has {
				brief, revision, err := intake.LatestDesignBrief(d.repo, d.stream)
				if err != nil || revision != 3 || !slices.Equal(brief.Projects, []config.ProjectID{d.home, d.other}) {
					return nil, fmt.Errorf("omission lost the list: %+v %d %v", brief, revision, err)
				}
			}
		}
		return &agent.Result{ClaudeID: "design-projects", ResultText: "projects recorded"}, nil
	})
	d.say(t, "Add client and then remove the home project.", 2)
	view, err = d.c.Design(context.Background(), d.stream)
	fixturekit.Must(t, err)
	if view.Design.BriefRevision != 4 || view.Design.Brief != strings.TrimSpace(intakefixture.ProductionBrief) || !slices.Equal(view.Design.Projects, []config.ProjectID{d.other}) {
		t.Fatalf("latest brief: %+v", view.Design)
	}
}

func TestDesignTurnContextFollowsTheBriefsProjects(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	ctx := context.Background()
	intakefixture.WriteCharter(t, d.c, d.other, "1. Client charter rule.\n")
	intakefixture.Mutation(t, d.c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: d.other}, Mode: "soft", Source: "owner"})
	for _, p := range []config.ProjectID{d.home, d.other} {
		r, err := d.s.Repository(p)
		fixturekit.Must(t, err)
		m, err := kb.Load(r)
		fixturekit.Must(t, err)
		m.Entities = append(m.Entities, kb.Entity{ID: "design-notes", Name: "Entity text for " + string(p), Aliases: []string{}, Paths: []string{"design-notes/**"}, Owners: []string{}, PartOf: []string{}})
		fixturekit.Must(t, kb.Store(ctx, r, m, time.Now().UTC(), intake.OwnerActor, "fixture"))
	}
	recordDesignProjects(t, d.repo, d.stream, intakefixture.ProductionBrief, []config.ProjectID{d.other})
	d.script(func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.SystemPrompt, "Client charter rule") || !strings.Contains(req.SystemPrompt, "Entity text for "+string(d.other)) || strings.Contains(req.SystemPrompt, "Keep changes small") || strings.Contains(req.SystemPrompt, "Entity text for "+string(d.home)) {
			return nil, fmt.Errorf("context does not follow the brief's projects: %s", req.SystemPrompt)
		}
		if _, err := intakefixture.ReadTool(ctx, tools, "projects/"+string(d.home)+"/charter.md"); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "design-projects", ResultText: "context checked"}, nil
	})
	d.say(t, "check context", 2)
}

func TestDesignSystemPromptAsksForConfirmationBeforeChangingProjects(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	d.script(func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		for _, instruction := range []string{"owner request to add or remove a project is confirmation", "readiness question with a recommendation", "only after the owner agrees"} {
			if !strings.Contains(req.SystemPrompt, instruction) {
				return nil, fmt.Errorf("missing confirmation rule %q", instruction)
			}
		}
		return &agent.Result{ClaudeID: "design-projects", ResultText: "rules read"}, nil
	})
	d.say(t, "Which projects does the brief cover?", 2)
}

func TestDesignStartRevisionConflictRecordsNothing(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	ctx := context.Background()
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	revision := recordDesignProjects(t, r, f.Origin, intakefixture.ProductionBrief, []config.ProjectID{f.Project, f.Project2})
	stale := revision - 1
	err = f.C.Do(ctx, "POST", "/v1/design/"+string(f.Origin)+"/start", api.DesignStartRequest{BriefRevision: &stale}, nil)
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("stale start: %v", err)
	}
	open, err := intake.IsDesignWorkstream(r, f.Origin)
	fixturekit.Must(t, err)
	if !open {
		t.Fatal("stale start closed the session")
	}
	docs, err := trace.Read[trace.Document](r, f.Origin)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument || d.ID == intake.DesignSelectionDocument || d.ID == intake.DesignStartIntentDocument {
			t.Fatalf("stale start recorded %s", d.ID)
		}
	}
	for _, p := range []config.ProjectID{f.Project, f.Project2} {
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		streams, err := repo.Workstreams()
		fixturekit.Must(t, err)
		if slices.Contains(streams, f.Stream1) || slices.Contains(streams, f.Stream2) {
			t.Fatal("stale start allocated")
		}
	}
	fixturekit.Must(t, f.C.Do(ctx, "POST", "/v1/design/"+string(f.Origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil))
	f.Handed, err = trace.Get[trace.Document](r, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	f.AwaitPacket(t)
	f.CheckContinuation(t)
	selected, found, err := intake.DesignSelectionOf(r, f.Origin)
	fixturekit.Must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Repo != f.Project || selected.Members[1].Repo != f.Project2 {
		t.Fatalf("owner decision: %+v", selected)
	}
	doc, err := trace.Get[trace.Document](r, f.Origin, intake.DesignSelectionDocument, 1)
	fixturekit.Must(t, err)
	if doc.Actor != intake.OwnerActor {
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
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
			ctx := context.Background()
			r, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			revision := recordDesignProjects(t, r, f.Origin, intakefixture.ProductionBrief, []config.ProjectID{f.Project, f.Project2})
			hook := &intakefixture.DesignInterruption{Step: boundary.step, Nth: boundary.nth, Reached: make(chan struct{})}
			f.Hook.Store(hook)
			started := make(chan error, 1)
			go func() {
				started <- f.C.Do(ctx, "POST", "/v1/design/"+string(f.Origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			}()
			select {
			case <-hook.Reached:
			case <-time.After(fixturekit.DemoTimeout):
				f.Timeout(t, "start interruption "+boundary.step)
			}
			pinned, found, err := intake.DesignStartIntentOf(r, f.Origin)
			fixturekit.Must(t, err)
			if !found || pinned.BriefRevision != revision || !slices.Equal(pinned.Projects, []config.ProjectID{f.Project, f.Project2}) || !strings.HasPrefix(pinned.Handed.Content, intakefixture.ProductionBrief) {
				t.Fatalf("pinned start: %+v", pinned)
			}
			f.Handed = pinned.Handed
			recordDesignProjects(t, r, f.Origin, "Decisions: different. Intent and tradeoffs: new. Rejected: previous.", []config.ProjectID{f.Project2})
			f.Stop(t)
			<-started
			f.Hook.Store(nil)
			f.Start(t)
			f.AwaitPacket(t)
			f.CheckContinuation(t)
			r, err = f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			recovered, found, err := intake.DesignStartIntentOf(r, f.Origin)
			fixturekit.Must(t, err)
			if !found || !intakefixture.EqualJSONTest(recovered, pinned) {
				t.Fatal("recovery rewrote pinned start")
			}
			selected, found, err := intake.DesignSelectionOf(r, f.Origin)
			fixturekit.Must(t, err)
			if !found || len(selected.Members) != 2 {
				t.Fatalf("owner selection: %+v", selected)
			}
			for i, member := range selected.Members {
				project := []config.ProjectID{f.Project, f.Project2}[i]
				stream := []config.WorkstreamID{f.Stream1, f.Stream2}[i]
				if member.Repo != project || member.Workstream != stream {
					t.Fatalf("member identity changed: %+v", member)
				}
				repo, err := f.S.Repository(project)
				fixturekit.Must(t, err)
				docs, err := trace.Read[trace.Document](repo, stream)
				fixturekit.Must(t, err)
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
				fixturekit.Must(t, err)
				for _, other := range streams {
					if other != stream && other != f.Origin {
						t.Fatalf("extra allocated member %s", other)
					}
				}
			}
			f.Stop(t)
			f.Start(t)
			f.AwaitPacket(t)
			f.CheckContinuation(t)
		})
	}
}

func TestDesignStartExcludesConversationHomeAndContinuesSharedChief(t *testing.T) {
	t.Parallel()
	s, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "only-member")
	intakefixture.WriteCharter(t, c, other, "1. Keep changes small.\n")
	opened, err := c.OpenDesign(ctx, home)
	fixturekit.Must(t, err)
	origin := opened.Design.Workstream
	r, err := s.Repository(home)
	fixturekit.Must(t, err)
	before, err := r.ChiefOfStaffThread(origin)
	fixturekit.Must(t, err)
	revision := recordDesignProjects(t, r, origin, intakefixture.ProductionBrief, []config.ProjectID{other})
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "continued-chief", ResultText: "The shared chief continues."}, nil
	}
	engine.Mu.Unlock()
	fixturekit.Must(t, c.Do(ctx, "POST", "/v1/design/"+string(origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil))
	s.Until(t, "the start to complete", func() bool {
		state, err := r.Workflow(origin, intake.DesignStartSubject)
		fixturekit.Must(t, err)
		return state.Value == "complete"
	})
	selected, found, err := intake.DesignSelectionOf(r, origin)
	fixturekit.Must(t, err)
	if !found || len(selected.Members) != 1 || selected.Members[0].Repo != other || selected.Members[0].Workstream == origin {
		t.Fatalf("selected members: %+v", selected)
	}
	groupID, err := r.CrossRepoWorkgroupOf(origin)
	fixturekit.Must(t, err)
	_, group, _, err := r.CrossRepoProjectSpec(groupID)
	fixturekit.Must(t, err)
	if group.Conversation == nil || group.Conversation.Project != home || group.Conversation.Workstream != origin {
		t.Fatalf("conversation moved: %+v", group)
	}
	chief, host, _, close, err := s.ChiefRepository(ctx, r, origin)
	fixturekit.Must(t, err)
	after, err := chief.Thread(host, trace.ChiefOfStaff)
	close()
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(before.Identity, after.Identity) {
		t.Fatal("shared chief identity changed")
	}
	_, err = c.Send(ctx, origin, "Continue the conversation at home.")
	fixturekit.Must(t, err)
	s.Until(t, "the shared chief to continue the conversation", func() bool {
		conversation, err := c.Conversation(ctx, origin)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
			return e.Kind == "response" && e.Text == "The shared chief continues."
		})
	})
	memberRepo, err := s.Repository(other)
	fixturekit.Must(t, err)
	memberGroup, err := memberRepo.CrossRepoWorkgroupOf(selected.Members[0].Workstream)
	fixturekit.Must(t, err)
	if memberGroup != groupID {
		t.Fatal("member joined another workgroup")
	}
}

func TestDesignStartKeepsTheSelectionRecordedBeforeHandedInput(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	ctx := context.Background()
	r, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	recordDesignProjects(t, r, f.Origin, intakefixture.ProductionBrief, []config.ProjectID{f.Project})
	f.SharedDesign().RecordSelection(t, f.Project, f.Project2)
	_, err = f.C.DesignStart(ctx, f.Origin)
	fixturekit.Must(t, err)
	f.Handed, err = trace.Get[trace.Document](r, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	f.AwaitPacket(t)
	f.CheckContinuation(t)
	pinned, found, err := intake.DesignStartIntentOf(r, f.Origin)
	fixturekit.Must(t, err)
	if !found || !slices.Equal(pinned.Projects, []config.ProjectID{f.Project, f.Project2}) {
		t.Fatalf("recorded selection changed: %+v", pinned)
	}
}
