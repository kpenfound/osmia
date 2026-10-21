package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fs "io/fs"
	http "net/http"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProjectCharterSharedByRepoAPIContextAndRestart(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	ctx := context.Background()
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	primary := added.Project.ID
	otherClone := filepath.Join(filepath.Dir(clone), "client")
	demoGit(t, filepath.Dir(clone), "init", "--quiet", otherClone)
	result, err := c.AddRepo(ctx, RepoAddRequest{Project: primary, Upstream: "owner/client", Clone: otherClone})
	must(t, err)
	var member RepoView
	for _, r := range result.Project.Repos {
		if r.ID != primary {
			member = r
		}
	}
	if member.ID == "" || result.Project.ID != added.Project.ID {
		t.Fatalf("shared project: %+v", result)
	}
	if _, err := projectDocument(t, s.current().Root, member.ID, "charter.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("member has independent charter: %v", err)
	}
	blockedBrief := "Build a client feature"
	_, err = c.HandIn(ctx, HandInRequest{Project: member.ID, Key: "empty-project-policy", Stdin: &blockedBrief})
	assertCode(t, err, CharterEmpty)
	if !strings.Contains(err.Error(), "osmia edit charter "+string(primary)) {
		t.Fatalf("empty charter named a member path: %v", err)
	}
	writeCharter(t, c, added.Project.ID, "1. Project policy for every repo.\n")
	primaryRepo, err := s.repository(primary)
	must(t, err)
	memberRepo, err := s.repository(member.ID)
	must(t, err)
	for _, repo := range []*trace.Repository{primaryRepo, memberRepo} {
		h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: "repo-knowledge", Revision: 1, Project: repo.Project(), At: s.now(), Actor: librarianActor, Cause: "fixture"}
		must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: "kb/repo.md", Content: string(repo.Project())}}))
	}
	endpoint := func(id config.ProjectID) string { return Prefix + "/projects/charter/" + string(id) }
	var policy trace.Document
	must(t, c.Do(ctx, http.MethodGet, endpoint(member.ID), nil, &policy))
	if policy.Project != primary || policy.Revision != 2 || !strings.Contains(policy.Content, "Project policy") {
		t.Fatalf("member API charter: %+v", policy)
	}
	var saved trace.Document
	must(t, c.Do(ctx, http.MethodPut, endpoint(member.ID), CharterEdit{Revision: policy.Revision, Content: "1. Shared policy edited through a member.\n"}, &saved))
	assertCode(t, c.Do(ctx, http.MethodPut, endpoint(primary), CharterEdit{Revision: policy.Revision, Content: "stale"}, nil), Conflict)

	text := "1. Owner policy applies to every repo.\n"
	writeCharter(t, c, added.Project.ID, text)
	assertCode(t, c.Do(ctx, http.MethodPut, endpoint(member.ID), CharterEdit{Revision: saved.Revision, Content: "overwrite"}, nil), Conflict)
	doc, _, err := s.loadCharter(ctx, member.ID)
	must(t, err)
	if doc.Revision != 4 || doc.Project != primary || doc.Content != text {
		t.Fatalf("owner edit: %+v", doc)
	}
	brief := "Build a client feature"
	handed, err := c.HandIn(ctx, HandInRequest{Project: member.ID, Key: "shared-charter", Stdin: &brief})
	must(t, err)
	check := func(s *Service, c *Client) {
		t.Helper()
		for _, id := range []config.ProjectID{primary, member.ID} {
			var got trace.Document
			must(t, c.Do(ctx, http.MethodGet, endpoint(id), nil, &got))
			if got != doc {
				t.Fatalf("repo %s charter: %+v", id, got)
			}
			for _, role := range []string{architectRole, committeeRole, masonRole, trace.ChiefOfStaff} {
				scope := bundle.Scope{Role: role}
				if id == member.ID {
					scope.Workstream = handed.Workstream
				}
				b, err := s.Context().Assemble(ctx, id, scope)
				must(t, err)
				if b.Charter.Revision != doc.Revision || len(b.Charter.Rules) != 1 || b.Charter.Rules[0].Text != strings.TrimSpace(strings.TrimPrefix(text, "1. ")) {
					t.Fatalf("%s repo %s charter: %+v", role, id, b.Charter)
				}
				if len(b.Knowledge) != 1 || b.Knowledge[0].Content != string(id) {
					t.Fatalf("repo knowledge mixed: %+v", b.Knowledge)
				}
				if len(b.CharterEdits) != 3 {
					t.Fatalf("shared edit notices: %+v", b.CharterEdits)
				}
			}
		}
	}
	check(s, c)

	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, ID: "request_context", Revision: 1, Project: member.ID, Workstream: handed.Workstream, At: s.now(), Actor: ownerActor, Cause: "owner"}
	_, err = memberRepo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "context", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read shared policy"})
	must(t, err)
	_, err = memberRepo.ClaimTurn(ctx, handed.Workstream, trace.ChiefOfStaff, "context-claim", "/owned/context", s.now())
	must(t, err)
	scope := coreadapter.Scope{Project: string(member.ID), Workstream: string(handed.Workstream), Thread: trace.ChiefOfStaff, Turn: "context", Role: trace.ChiefOfStaff}
	raw, err := projectContext(memberRepo, scope).Handle(ctx, json.RawMessage(`{"path":"charter.md","revision":3}`))
	must(t, err)
	var historical struct {
		Content  string
		Revision int
	}
	must(t, json.Unmarshal(raw, &historical))
	if historical.Content != saved.Content || historical.Revision != saved.Revision {
		t.Fatalf("project_context shared revision: %s", raw)
	}
	before, err := trace.Read[trace.Document](memberRepo, "")
	must(t, err)
	c.Close()
	must(t, s.Close())
	s, c = start(t, opts)
	check(s, c)
	memberRepo, err = s.repository(member.ID)
	must(t, err)
	after, err := trace.Read[trace.Document](memberRepo, "")
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("restart or charter reads rewrote member project history")
	}
}

func TestMemberCharterProposalWritesProjectAuthorityOnceAfterInterruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	primary := f.cfg.Project
	primary.ID = "p_00000000000000000000000000000099"
	authority, err := trace.Create(ctx, f.cfg.Root, primary, f.clock.Now(), ownerActor)
	must(t, err)
	t.Cleanup(func() { authority.Close() })
	policy := "1. Project rule one.\n2. Project rule two.\n3. Project rule three.\n"
	_, err = authority.EditCharter(ctx, 1, policy, f.clock.Now())
	must(t, err)
	member := f.cfg.Project
	member.GroupID = primary.ID
	repo, err := trace.Open(f.cfg.Root, member)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	bind := func() {
		repo.SetCharterResolver(func(config.ProjectID) (*trace.Repository, error) { return authority, nil })
	}
	bind()
	legacy := charterRevisions(t, repo)
	_, api := f.s.recordCharterDecision(ctx, member.ID, stream, repo, "1", CharterDecisionRequest{Decision: trace.CharterRatify}, ownerActor, "owner-charter")
	if api != nil {
		t.Fatal(api)
	}
	crash := errors.New("interrupted after shared charter write")
	f.s.boundary = func(step string) error {
		if step == "charter-written" {
			return crash
		}
		return nil
	}
	if err := (charterer{S: f.s.intake().ChartererDependencies(), Repository: repo}).Pass(ctx); !errors.Is(err, crash) {
		t.Fatalf("interruption: %v", err)
	}
	if got := appended(t, authority); len(got) != 1 || got[0].Project != primary.ID {
		t.Fatalf("shared append: %+v", got)
	}
	if !reflect.DeepEqual(legacy, charterRevisions(t, repo)) {
		t.Fatal("rule was written in member charter")
	}
	must(t, repo.Close())
	must(t, authority.Close())
	authority, err = trace.Open(f.cfg.Root, primary)
	must(t, err)
	repo, err = trace.Open(f.cfg.Root, member)
	must(t, err)
	bind()
	f.s.boundary = nil
	for range 2 {
		must(t, (charterer{S: f.s.intake().ChartererDependencies(), Repository: repo}).Pass(ctx))
	}
	if got := appended(t, authority); len(got) != 1 {
		t.Fatalf("retry duplicated shared rule: %+v", got)
	}
	proposals, err := repo.CharterProposals()
	must(t, err)
	if len(proposals) != 1 || proposals[0].State.Value != trace.CharterChartered || proposals[0].Proposal.Number != 4 {
		t.Fatalf("recovered proposal: %+v", proposals)
	}
	for _, r := range []*trace.Repository{authority, repo} {
		doc, err := r.Charter(ctx)
		must(t, err)
		if doc.Content != withRule(policy, 4) || doc.Project != primary.ID {
			t.Fatalf("shared standing rule: %+v", doc)
		}
	}
	if !reflect.DeepEqual(legacy, charterRevisions(t, repo)) {
		t.Fatal("recovery rewrote member charter history")
	}
}

func TestDesignProductionWorkerBundlesShareProjectCharter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	read := make(chan json.RawMessage, 1)
	f.engine.Mu.Lock()
	fallback := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Read the shared project policy") {
			raw, err := callTool(ctx, tools, projectContextTool, map[string]any{"path": "charter.md"})
			if err != nil {
				return nil, err
			}
			select {
			case read <- json.RawMessage(raw):
			default:
			}
			return questionResult(req, "session-chief", "Read the shared project policy."), nil
		}
		return fallback(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	check := func() {
		t.Helper()
		primary, err := f.s.repository(f.project)
		must(t, err)
		doc, err := primary.Charter(ctx)
		must(t, err)
		for i, stream := range []config.WorkstreamID{f.stream1, f.stream2} {
			id := f.project
			if i == 1 {
				id = f.project2
			}
			repo, err := f.s.repository(id)
			must(t, err)
			files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}
			unit := "accept"
			if i == 1 {
				unit = "resume"
			}
			worker, err := files.Mason(ctx, id, stream, unit)
			must(t, err)
			if worker.Context.Charter.Revision != doc.Revision || len(worker.Context.Charter.Rules) == 0 {
				t.Fatalf("member %s worker policy: %+v", id, worker.Context.Charter)
			}
			for _, rule := range worker.Context.Charter.Rules {
				if !strings.Contains(doc.Content, rule.Text) {
					t.Fatalf("independent member rule: %+v", rule)
				}
			}
		}
		_, err = f.c.Send(ctx, f.stream2, "Read the shared project policy")
		must(t, err)
		var raw json.RawMessage
		f.wait(t, "shared chief project context", func() bool {
			select {
			case raw = <-read:
				return true
			default:
				return false
			}
		})
		var retrieved struct {
			Content  string
			Revision int
		}
		must(t, json.Unmarshal(raw, &retrieved))
		if retrieved.Content != doc.Content || retrieved.Revision != doc.Revision {
			t.Fatalf("shared chief policy: %s", raw)
		}
	}
	check()
	f.stop(t)
	f.start(t)
	check()
}

func TestProjectCharterRemainsReadableWhileMemberTurnsDrain(t *testing.T) {
	for _, mode := range []string{"reload", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newTwoProjectFixture(t)
			member := f.cfg.For(otherProject).Project
			repo, err := trace.Open(f.cfg.Root, member)
			must(t, err)
			_, err = trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{otherStream}, f.clock.Now(), serviceActor, "fixture")
			must(t, err)
			must(t, repo.Close())
			member.GroupID = project
			path, err := f.cfg.Root.ProjectConfig(member.ID)
			must(t, err)
			must(t, config.WriteProjectConfig(path, member))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			read := make(chan error, 1)
			f.turns.Mu.Lock()
			f.turns.Hooks[turn(otherProject, "first")] = func(ctx context.Context) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				doc, err := f.repository(otherProject).Charter(context.WithoutCancel(ctx))
				if err == nil && doc.Project != project {
					err = errors.New("draining turn read member policy")
				}
				read <- err
			}
			f.turns.Mu.Unlock()
			s, c := start(t, f.opts)
			await(t, "the member's running turn", entered)
			finished := make(chan error, 1)
			if mode == "reload" {
				path := filepath.Join(f.opts.Config.Root, "config.toml")
				top, err := os.ReadFile(path)
				must(t, err)
				line := "active_projects = [\"" + string(project) + "\", \"" + string(otherProject) + "\"]"
				updated := strings.Replace(string(top), line, "active_projects = []", 1)
				if updated == string(top) {
					t.Fatal("fixture did not remove both repo registrations")
				}
				must(t, os.WriteFile(path, []byte(updated), 0600))
				go func() { _, err := c.Reload(ctx); finished <- err }()
				soon(t, "both repos draining", func() bool { return !s.current().Active(project) && !s.current().Active(otherProject) })
				unblock()
			} else {
				go func() { finished <- s.Close() }()
			}
			select {
			case err := <-read:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("draining turn could not read the project charter")
			}
			select {
			case err := <-finished:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("repos did not finish draining")
			}
		})
	}
}
