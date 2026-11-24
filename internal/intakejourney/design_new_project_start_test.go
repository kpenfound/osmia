package intakejourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const gadgetCharter = "1. Keep the repository small.\n2. Every change has a test.\n"

type newProjectStart struct {
	opts     journey.Options
	s        *journey.Service
	c        *journey.Client
	home     config.ProjectID
	github   *servicetest.FakeGitHub
	dir      string
	cloneDir string
	remote   string
	readme   string
	input    atomic.Pointer[map[string]any]
	crash    atomic.Value
	crashed  atomic.Bool
}

func newNewProjectStartFixture(t *testing.T, creation bool) *newProjectStart {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	opts, clone, engine, _, _ := journey.NewArchitectOptions(t)
	opts.Reconciliation.Now = nil
	dir, err := os.MkdirTemp("", "os-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &newProjectStart{dir: dir, cloneDir: filepath.Join(dir, "clones")}
	f.github = servicetest.NewFakeGitHub(t, "Owner")
	f.remote, f.readme = servicetest.SeedRemote(t, dir, "gadget", "main")
	f.github.Remote = func(string) string { return f.remote }
	opts.Repositories = f.github.Client()
	text := "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"
	if creation {
		text += fmt.Sprintf("[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", f.cloneDir)
	}
	file, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = file.WriteString(text)
	fixturekit.Must(t, errors.Join(err, file.Close()))
	opts = journey.Enforce(opts, service.Enforcement{Engine: engine, Hosts: opts.Architect.Hosts})
	f.opts = opts
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.SystemPrompt, intake.DesignRolePrompt) {
			return &agent.Result{ClaudeID: "architect", ResultText: "drafting"}, nil
		}
		input := f.input.Load()
		if input == nil {
			return nil, errors.New("the test recorded no brief to draft")
		}
		call := map[string]any{"content": intakefixture.ProductionBrief}
		for k, v := range *input {
			call[k] = v
		}
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, call)
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("brief was not recorded: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design", ResultText: "brief recorded"}, nil
	}
	engine.Mu.Unlock()
	f.begin(t)
	added, err := f.c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f.home = added.Project.ID
	intakefixture.WriteCharter(t, f.c, f.home, "1. Keep changes small.\n")
	return f
}

func (f *newProjectStart) begin(t *testing.T) {
	t.Helper()
	f.opts.Boundary = func(_ context.Context, name string) error {
		if want, _ := f.crash.Load().(string); want != "" && (f.crashed.Load() || name == want) {
			f.crashed.Store(true)
			return fmt.Errorf("crash at %s", name)
		}
		return nil
	}
	f.s, f.c = journey.StartT(t, f.opts)
}

func (f *newProjectStart) restart(t *testing.T) {
	t.Helper()
	fixturekit.Must(t, f.s.Close())
	f.crash.Store("")
	f.crashed.Store(false)
	f.begin(t)
}

func (f *newProjectStart) gadget(extra map[string]any) map[string]any {
	np := map[string]any{"name": "Gadget", "repo": "gadget", "description": "A gadget.", "charter": gadgetCharter}
	for k, v := range extra {
		np[k] = v
	}
	return np
}

func (f *newProjectStart) draft(t *testing.T, fields map[string]any) (config.WorkstreamID, int) {
	t.Helper()
	ctx := context.Background()
	opened, err := f.c.OpenNewProjectDesign(ctx, "")
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	f.input.Store(&fields)
	_, err = f.c.DesignMessage(ctx, stream, "Design the gadget project.")
	fixturekit.Must(t, err)
	conversation := awaitDesignConversation(t, f.c, stream, func(r api.ConversationResponse) bool {
		return len(r.Entries) > 0 && (r.Entries[len(r.Entries)-1].State == api.TurnFailed || r.Entries[len(r.Entries)-1].Kind == "response")
	})
	if last := conversation.Entries[len(conversation.Entries)-1]; last.State == api.TurnFailed {
		t.Fatalf("the design turn failed: %+v", conversation)
	}
	view, err := f.c.Design(ctx, stream)
	fixturekit.Must(t, err)
	return stream, view.Design.BriefRevision
}

func (f *newProjectStart) homeRepository(t *testing.T) *trace.Repository {
	t.Helper()
	r, err := f.s.Repository(f.home)
	fixturekit.Must(t, err)
	return r
}

func (f *newProjectStart) await(t *testing.T, what string, done func() bool) {
	t.Helper()
	f.s.Until(t, what, done)
}

func (f *newProjectStart) startState(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	state, err := f.homeRepository(t).Workflow(stream, intake.DesignStartSubject)
	fixturekit.Must(t, err)
	return state.Value
}

func (f *newProjectStart) awaitComplete(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	f.await(t, "the start to complete", func() bool { return f.startState(t, stream) == "complete" })
}

func (f *newProjectStart) intent(t *testing.T, stream config.WorkstreamID) intake.DesignStartIntent {
	t.Helper()
	intent, found, err := intake.DesignStartIntentOf(f.homeRepository(t), stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the start recorded no intent")
	}
	return intent
}

func (f *newProjectStart) journal() string {
	return filepath.Join(f.opts.Config.Root, "project-add.json")
}
func (f *newProjectStart) clone() string { return filepath.Join(f.cloneDir, "gadget") }

func (f *newProjectStart) assertNothingRecorded(t *testing.T, stream config.WorkstreamID, existing ...string) {
	t.Helper()
	r := f.homeRepository(t)
	docs, err := trace.Read[trace.Document](r, stream)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument || d.ID == intake.DesignSelectionDocument || d.ID == intake.DesignStartIntentDocument {
			t.Fatalf("a refused start recorded %s", d.ID)
		}
	}
	if open, err := intake.IsDesignWorkstream(r, stream); err != nil || !open {
		t.Fatalf("the refused start closed the session: %v %v", open, err)
	}
	if got := f.github.CreateCalls(); got != 0 {
		t.Fatalf("%d create calls reached GitHub", got)
	}
	if got := f.github.Writes(); len(got) != 0 {
		t.Fatalf("GitHub was written to: %+v", got)
	}
	if _, err := os.Lstat(f.journal()); !os.IsNotExist(err) {
		t.Fatalf("a journal was written: %v", err)
	}
	entries, err := os.ReadDir(f.cloneDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, existing) {
		t.Fatalf("clone directory holds %v, want %v", names, existing)
	}
	if ids := f.s.Current().ProjectIDs(); !slices.Equal(ids, []config.ProjectID{f.home}) {
		t.Fatalf("projects: %v", ids)
	}
}

func (f *newProjectStart) assertGadgetCreated(t *testing.T, stream config.WorkstreamID) config.ProjectID {
	t.Helper()
	id := f.intent(t, stream).Creation
	if got := f.github.CreateCalls(); got != 1 {
		t.Fatalf("%d create calls reached GitHub", got)
	}
	if f.github.Deleted() {
		t.Fatalf("a delete reached GitHub: %+v", f.github.Calls())
	}
	repo, ok := f.github.Repo("owner/gadget")
	if !ok || !repo.Private || !repo.AutoInit || repo.Description != "A gadget." {
		t.Fatalf("GitHub repository: %+v", repo)
	}
	for _, call := range f.github.Writes() {
		if call.Method == http.MethodPost && call.Path == "/user/repos" && (call.Body["private"] != true || call.Body["auto_init"] != true || call.Body["name"] != "gadget") {
			t.Fatalf("create call: %+v", call.Body)
		}
	}
	if got := fixturekit.Git(t, f.dir, "-C", f.clone(), "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("the clone has %q checked out", got)
	}
	if got := fixturekit.Git(t, f.dir, "-C", f.clone(), "rev-parse", "HEAD"); got != f.readme {
		t.Fatalf("the clone is at %s, not the README commit %s", got, f.readme)
	}
	cfg := f.s.Current()
	if !cfg.Active(id) {
		t.Fatalf("project %s is not active: %v", id, cfg.ProjectIDs())
	}
	p := cfg.For(id).Project
	if p.Name != "Gadget" || p.Upstream != "owner/gadget" || p.Fork != "" || p.BaseBranch != "main" || p.Clone != f.clone() {
		t.Fatalf("registered project: %+v", p)
	}
	r, err := f.s.Repository(id)
	fixturekit.Must(t, err)
	revisions := intakefixture.DocumentRevisions(t, r, "charter")
	if len(revisions) != 1 || revisions[0].Revision != 1 || revisions[0].Content != strings.TrimSpace(gadgetCharter) {
		t.Fatalf("charter revisions: %+v", revisions)
	}
	if _, err := os.Lstat(f.journal()); !os.IsNotExist(err) {
		t.Fatalf("the journal was kept: %v", err)
	}
	state, err := f.c.ProjectCreation(context.Background(), id)
	fixturekit.Must(t, err)
	if state.State != api.CreationComplete {
		t.Fatalf("creation state: %+v", state)
	}
	return id
}

func (f *newProjectStart) assertAllocatedOnce(t *testing.T, stream config.WorkstreamID, id config.ProjectID, members int) intake.DesignSelection {
	t.Helper()
	r := f.homeRepository(t)
	selected, found, err := intake.DesignSelectionOf(r, stream)
	fixturekit.Must(t, err)
	if !found || len(selected.Members) != members {
		t.Fatalf("selection: %+v", selected)
	}
	count := 0
	for _, m := range selected.Members {
		if m.Repo == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the new project is a member %d times: %+v", count, selected)
	}
	groups, err := os.ReadDir(filepath.Join(f.opts.Config.Root, "workgroups"))
	fixturekit.Must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(selected.Group) {
		t.Fatalf("workgroups: %v, want only %s", groups, selected.Group)
	}
	for _, m := range selected.Members {
		member, err := f.s.Repository(m.Repo)
		fixturekit.Must(t, err)
		handed, err := trace.Get[trace.Document](member, m.Workstream, intake.HandedDocument, 1)
		fixturekit.Must(t, err)
		if got := strings.HasPrefix(handed.Content, intake.NewProjectHandedLine+"\n\n"); got != (m.Repo == id) {
			t.Fatalf("member %s handed input has the new-project line: %v", m.Repo, got)
		}
		docs, err := trace.Read[trace.Document](member, m.Workstream)
		fixturekit.Must(t, err)
		handedRecords := 0
		for _, d := range docs {
			if d.ID == intake.HandedDocument {
				handedRecords++
			}
		}
		if handedRecords != 1 {
			t.Fatalf("member %s has %d handed records", m.Repo, handedRecords)
		}
	}
	return selected
}

func TestNewProjectStartCreatesTheProjectAndAllocatesIt(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
	view, err := f.c.Design(ctx, stream)
	fixturekit.Must(t, err)
	if view.Design.NewProject == nil || view.Design.NewProject.Visibility != api.VisibilityPrivate || view.Design.NewProject.Repo != "gadget" {
		t.Fatalf("the design view does not show the new project before Start: %+v", view.Design.NewProject)
	}
	if got := f.github.Calls(); len(got) != 0 {
		t.Fatalf("GitHub was called before Start: %+v", got)
	}
	_, err = f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)
	f.awaitComplete(t, stream)

	id := f.assertGadgetCreated(t, stream)
	intent := f.intent(t, stream)
	if intent.BriefRevision != revision || len(intent.Projects) != 0 || intent.NewProject == nil || intent.NewProject.Repo != "gadget" {
		t.Fatalf("start intent: %+v", intent)
	}
	selected := f.assertAllocatedOnce(t, stream, id, 1)
	member := selected.Members[0]
	if member.Workstream == stream {
		t.Fatal("the new project's member is the host's conversation address")
	}

	r := f.homeRepository(t)
	chief, host, _, release, err := f.s.ChiefRepository(ctx, r, stream)
	fixturekit.Must(t, err)
	defer release()
	if _, shared := trace.WorkgroupProjectID(chief.Project()); !shared {
		t.Fatalf("the conversation stayed in %s", chief.Project())
	}
	if _, err := chief.Thread(host, trace.ChiefOfStaff); err != nil {
		t.Fatalf("the workgroup holds no chief of staff thread: %v", err)
	}
	if _, err := r.Thread(stream, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host project keeps the conversation: %v", err)
	}
	streams, err := r.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Equal(streams, []config.WorkstreamID{stream}) {
		t.Fatalf("host project workstreams: %v", streams)
	}
	if _, err := f.c.DesignMessage(ctx, stream, "Revise the brief"); err == nil {
		t.Fatal("a started design accepted another brief turn")
	}

	memberRepo, err := f.s.Repository(id)
	fixturekit.Must(t, err)
	f.await(t, "the new project's drafting to begin", func() bool {
		th, err := memberRepo.Thread(member.Workstream, intake.ArchitectAgent)
		return err == nil && len(th.Turns) > 0
	})
}

func TestNewProjectStartAllocatesBesideAnExistingProject(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	stream, revision := f.draft(t, map[string]any{"projects": []config.ProjectID{f.home}, "new_project": f.gadget(nil)})
	_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)
	f.awaitComplete(t, stream)

	id := f.assertGadgetCreated(t, stream)
	selected := f.assertAllocatedOnce(t, stream, id, 2)
	if selected.Members[0].Repo != f.home || selected.Members[1].Repo != id {
		t.Fatalf("members are not the existing project, then the new one: %+v", selected.Members)
	}
	r := f.homeRepository(t)
	groupID, err := r.CrossRepoWorkgroupOf(stream)
	fixturekit.Must(t, err)
	if groupID != selected.Group {
		t.Fatalf("the conversation is in %s, not %s", groupID, selected.Group)
	}
	for _, m := range selected.Members {
		member, err := f.s.Repository(m.Repo)
		fixturekit.Must(t, err)
		joined, err := member.CrossRepoWorkgroupOf(m.Workstream)
		fixturekit.Must(t, err)
		if joined != selected.Group {
			t.Fatalf("member %s joined %s, not %s", m.Repo, joined, selected.Group)
		}
	}
}

func TestNewProjectStartRefusesBeforeRecordingAnything(t *testing.T) {
	for _, tc := range []struct {
		name     string
		creation bool
		prepare  func(t *testing.T, f *newProjectStart)
		existing []string
		message  string
	}{
		{name: "another account", creation: true, message: "projects.repo_owner",
			prepare: func(t *testing.T, f *newProjectStart) { f.github.Login = "someone-else" }},
		{name: "repo name taken", creation: true, message: "already exists on GitHub",
			prepare: func(t *testing.T, f *newProjectStart) { f.github.AddRepo("Owner/gadget", f.remote, "main") }},
		{name: "clone path exists", creation: true, existing: []string{"gadget"}, message: "already exists",
			prepare: func(t *testing.T, f *newProjectStart) { fixturekit.Must(t, os.MkdirAll(f.clone(), 0700)) }},
		{name: "creation off", creation: false, message: "projects.repo_owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNewProjectStartFixture(t, tc.creation)
			stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			_, err := f.c.DesignStartWithBriefRevision(context.Background(), stream, revision)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("start was not refused for %q: %v", tc.message, err)
			}
			f.assertNothingRecorded(t, stream, tc.existing...)
		})
	}
}

func (f *newProjectStart) stallOnClone(t *testing.T) {
	t.Helper()
	later := filepath.Join(f.dir, "later.git")
	f.github.Remote = func(string) string { return later }
}

func (f *newProjectStart) fixClone(t *testing.T) {
	t.Helper()
	f.remote, f.readme = servicetest.SeedRemote(t, f.dir, "later", "main")
}

func (f *newProjectStart) creationEntry(t *testing.T) api.InboxEntry {
	t.Helper()
	var entry api.InboxEntry
	f.await(t, "the stuck creation in the inbox", func() bool {
		inbox, err := f.c.Inbox(context.Background())
		fixturekit.Must(t, err)
		for _, e := range inbox.Entries {
			if e.Kind == api.InboxProjectCreation {
				entry = e
				return true
			}
		}
		return false
	})
	return entry
}

func (f *newProjectStart) retryRequest(t *testing.T, entry api.InboxEntry) error {
	t.Helper()
	if len(entry.Options) != 1 || entry.Options[0] != "retry" {
		t.Fatalf("the entry offers %v, want only retry", entry.Options)
	}
	var out api.ProjectResponse
	return f.c.Do(context.Background(), entry.Answer.Method, entry.Answer.Path, entry.Answer.Body, &out)
}

func (f *newProjectStart) retry(t *testing.T, entry api.InboxEntry) {
	t.Helper()
	fixturekit.Must(t, f.retryRequest(t, entry))
}

func TestNewProjectStartCloneFailureReachesTheInboxAndRetries(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	f.stallOnClone(t)
	stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
	_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)

	entry := f.creationEntry(t)
	if !strings.Contains(entry.Question, "git credential") {
		t.Fatalf("the entry does not name the host git credential: %s", entry.Question)
	}
	if entry.Workstream != stream || entry.Project != f.home || entry.Answer.Path != "/v1/projects/create" {
		t.Fatalf("the entry is not the start's: %+v", entry)
	}
	if got := f.github.CreateCalls(); got != 1 {
		t.Fatalf("%d create calls", got)
	}
	if _, err := os.Lstat(f.clone()); !os.IsNotExist(err) {
		t.Fatalf("a failed clone left %v", err)
	}
	if state := f.startState(t, stream); state == "complete" {
		t.Fatal("the start completed without its project")
	}

	f.fixClone(t)
	f.retry(t, entry)
	f.awaitComplete(t, stream)
	id := f.assertGadgetCreated(t, stream)
	f.assertAllocatedOnce(t, stream, id, 1)
	if inbox, err := f.c.Inbox(ctx); err != nil || slices.ContainsFunc(inbox.Entries, func(e api.InboxEntry) bool { return e.Kind == api.InboxProjectCreation }) {
		t.Fatalf("the entry stayed after the retry: %+v %v", inbox.Entries, err)
	}
}

func TestProjectCreationFromTheAPIReachesTheInboxAndRetries(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	f.stallOnClone(t)
	id, err := f.c.ReserveProjectCreation(ctx)
	fixturekit.Must(t, err)
	req := api.ProjectCreateRequest{ID: id, Name: "Gadget", Repo: "gadget", Description: "A gadget.", Charter: gadgetCharter}
	if _, err := f.c.CreateProject(ctx, req); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("a creation that cannot clone: %v", err)
	}
	entry := f.creationEntry(t)
	if entry.Workstream != "" || entry.Project != "" || !strings.Contains(entry.Question, "git credential") {
		t.Fatalf("entry: %+v", entry)
	}
	f.fixClone(t)
	f.retry(t, entry)
	state, err := f.c.ProjectCreation(ctx, id)
	fixturekit.Must(t, err)
	if state.State != api.CreationComplete || f.github.CreateCalls() != 1 {
		t.Fatalf("state %+v after %d create calls", state, f.github.CreateCalls())
	}
}

func TestNewProjectStartOwnershipNotEstablishedReachesTheInbox(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		g.AddRepo("Owner/gadget", f.remote, "main")
		return true, http.StatusUnprocessableEntity, servicetest.GitHubNameTaken
	}
	stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
	_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)

	entry := f.creationEntry(t)
	if !strings.Contains(entry.Question, "ownership could not be established") || !strings.Contains(entry.Question, "on GitHub") {
		t.Fatalf("the entry does not say the ownership could not be established: %s", entry.Question)
	}
	if len(entry.Options) != 1 || entry.Options[0] != "retry" || strings.Contains(strings.ToLower(strings.Join(entry.Options, " ")), "adopt") {
		t.Fatalf("options: %v", entry.Options)
	}
	if _, err := os.Lstat(f.clone()); !os.IsNotExist(err) {
		t.Fatalf("the repository was cloned: %v", err)
	}
	if ids := f.s.Current().ProjectIDs(); !slices.Equal(ids, []config.ProjectID{f.home}) {
		t.Fatalf("projects: %v", ids)
	}
	if got := f.github.CreateCalls(); got != 1 {
		t.Fatalf("%d create calls", got)
	}
	if err := f.retryRequest(t, entry); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("the retry did not stop again: %v", err)
	}
	if _, err := os.Lstat(f.clone()); !os.IsNotExist(err) {
		t.Fatalf("the retry adopted the repository: %v", err)
	}
	if got := f.github.CreateCalls(); got != 1 {
		t.Fatalf("the retry sent another create: %d calls", got)
	}
	if f.github.Deleted() {
		t.Fatal("a repository was deleted")
	}
}

func TestNewProjectStartIsPinnedToTheBriefRevision(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	f.stallOnClone(t)
	stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
	_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)
	entry := f.creationEntry(t)

	r := f.homeRepository(t)
	changed := api.NewProject{Name: "Other", Repo: "other", Visibility: api.VisibilityPublic, Charter: gadgetCharter}
	h := intake.OwnerHeader(intake.DesignDraftDocument, r.Project(), stream, intake.DraftBriefToolName, time.Now().UTC())
	h.Schema, h.Revision = "osmia.trace.document", revision+1
	record, err := json.Marshal(intake.DesignBriefRecord{Content: intakefixture.ProductionBrief, Projects: []config.ProjectID{}, NewProject: &changed})
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.RecordDocuments(ctx, []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(record)}}))

	f.fixClone(t)
	f.retry(t, entry)
	f.awaitComplete(t, stream)
	f.assertGadgetCreated(t, stream)
	if _, ok := f.github.Repo("owner/other"); ok {
		t.Fatal("the later draft created a repository")
	}
	if intent := f.intent(t, stream); intent.NewProject == nil || intent.NewProject.Repo != "gadget" || intent.BriefRevision != revision {
		t.Fatalf("the pinned intent changed: %+v", intent)
	}
}

func TestNewProjectStartRecoversAtEachHandoff(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		journal    bool
		creates    int
	}{
		{"identity recorded before the journal exists", "design-start-intent-recorded", false, 0},
		{"journal written before any GitHub write", "journal-written", true, 0},
		{"registered before the journal is removed", "active-project-listed", true, 1},
		{"journal removed before Start records its selection", "design-project-created", false, 1},
		{"startup recovery completes the journal first", "repository-cloned", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNewProjectStartFixture(t, true)
			ctx := context.Background()
			stream, revision := f.draft(t, map[string]any{"projects": []config.ProjectID{f.home}, "new_project": f.gadget(nil)})
			f.crash.Store(tc.step)
			started := make(chan error, 1)
			go func() {
				_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
				started <- err
			}()
			f.await(t, "the interruption at "+tc.step, f.crashed.Load)
			<-started
			pinned := f.intent(t, stream)
			if pinned.Creation == "" || pinned.NewProject == nil {
				t.Fatalf("the intent holds no creation identity: %+v", pinned)
			}
			if _, err := os.Lstat(f.journal()); (err == nil) != tc.journal {
				t.Fatalf("journal present=%v at %s", err == nil, tc.step)
			}
			if got := f.github.CreateCalls(); got != tc.creates {
				t.Fatalf("%d create calls at %s, want %d", got, tc.step, tc.creates)
			}
			if _, found, err := intake.DesignSelectionOf(f.homeRepository(t), stream); err != nil || found {
				t.Fatalf("the selection was recorded before the project: %v %v", found, err)
			}

			f.restart(t)
			f.awaitComplete(t, stream)
			recovered := f.intent(t, stream)
			if !intakefixture.EqualJSONTest(recovered, pinned) {
				t.Fatalf("recovery rewrote the pinned intent: %+v", recovered)
			}
			id := f.assertGadgetCreated(t, stream)
			if id != pinned.Creation {
				t.Fatalf("created %s, want the recorded identity %s", id, pinned.Creation)
			}
			f.assertAllocatedOnce(t, stream, id, 2)
			if ids := f.s.Current().ProjectIDs(); len(ids) != 2 {
				t.Fatalf("projects: %v", ids)
			}
			f.restart(t)
			f.awaitComplete(t, stream)
			f.assertAllocatedOnce(t, stream, id, 2)
			if got := f.github.CreateCalls(); got != 1 {
				t.Fatalf("%d create calls after another restart", got)
			}
		})
	}
}
