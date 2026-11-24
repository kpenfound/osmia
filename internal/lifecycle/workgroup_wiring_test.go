package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type crossRepoWiringFixture struct {
	opts               Options
	cfg                *config.Config
	clock              *fixturekit.FixedClock
	engine             *fixturekit.DemoEngine
	ticks              chan time.Time
	project1, project2 config.ProjectID
	stream1, stream2   config.WorkstreamID
	mu                 sync.Mutex
	live               map[config.ProjectID]*trace.Repository
	problems           []string
}

func newCrossRepoWiringFixture(t *testing.T, agents map[string]string) *crossRepoWiringFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "wgw-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	opts.Workstreams = nil
	f := &crossRepoWiringFixture{opts: opts, project1: fixturekit.Project, project2: otherProject, stream1: fixturekit.Stream, stream2: otherStream, live: map[config.ProjectID]*trace.Repository{}}
	setup := intakefixture.NewWiringSetup(t, home, opts.Config, agents, func(r *trace.Repository) {
		f.mu.Lock()
		f.live[r.Project()] = r
		f.mu.Unlock()
	})
	f.cfg, f.clock, f.engine, f.ticks = setup.Config, setup.Clock, setup.Engine, setup.Ticks
	f.opts.Threads = setup.Threads
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = setup.Clock.Now, setup.Ticks
	return f
}

func (f *crossRepoWiringFixture) settle(t *testing.T, s *Service) {
	t.Helper()
	for range 8 {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(intakefixture.DemoTimeout):
			s.Close()
			t.Fatal("service did not finish its pass")
		}
	}
}
func (f *crossRepoWiringFixture) problem(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	f.problems = append(f.problems, msg)
}
func TestWorkgroupChiefDispatchVisitsLeaderAfterRetainedSourceLocatorAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCrossRepoWiringFixture(t, map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff})
	f.engine.Turns["*"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		return intakefixture.QuestionResult(req, "session-chief", "The agreed work continues."), nil
	}
	s, err := Start(ctx, f.opts)
	intakefixture.Must(t, err)
	defer func() { intakefixture.Must(t, s.Close()) }()
	id, apiFault := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	leader := lifecycle.LeastWorkstream([]config.WorkstreamID{f.stream1, f.stream2})
	project := f.project1
	if leader == f.stream2 {
		project = f.project2
	}
	repo, err := s.repository(project)
	intakefixture.Must(t, err)
	const source config.WorkstreamID = "w_00000000000000000000000000000000"
	if source >= leader {
		t.Fatal("retained source must sort before the dispatch leader")
	}
	intakefixture.Must(t, repo.CreateWorkstream(ctx, source, f.clock.Now(), intake.OwnerActor))
	intakefixture.Must(t, repo.RetractChiefOfStaff(ctx, source))
	intakefixture.Must(t, repo.RecordCrossRepoWorkgroup(ctx, source, id, f.clock.Now(), intake.OwnerActor, "design-start"))
	for lifetime := 0; lifetime < 2; lifetime++ {
		if lifetime > 0 {
			intakefixture.Must(t, s.Close())
			s, err = Start(ctx, f.opts)
			intakefixture.Must(t, err)
		}
		c := NewClient(s.Socket())
		sent, err := c.Send(ctx, leader, "Report the workgroup's status.")
		intakefixture.Must(t, err)
		wait, closeWait := serviceChanges(t, s)
		deadline := time.Now().Add(intakefixture.DemoTimeout)
		for {
			f.settle(t, s)
			conversation, err := c.Conversation(ctx, leader)
			intakefixture.Must(t, err)
			if slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone && e.Text == "The agreed work continues."
			}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lifetime %d: source %s consumed leader %s's dispatch: %+v", lifetime, source, leader, conversation)
			}
			wait()
		}
		closeWait()
		c.Close()
	}
}

func (f *crossRepoWiringFixture) repositories(t *testing.T, s *Service) (repo1, repo2 *trace.Repository) {
	t.Helper()
	defer func() {
		if t.Failed() {
			s.Close()
		}
	}()
	intakefixture.Eventually(t, "both projects' own repositories were never opened", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		repo1, repo2 = f.live[f.project1], f.live[f.project2]
		return repo1 != nil && repo2 != nil
	})
	return repo1, repo2
}

func TestCrossRepoWorkgroupWiringAcrossARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const question1, question2 = "Where does the server store chunks?", "Does the client ever see the offset?"
	const answer1, answer2 = "In a content-addressed store.", "No, only the server does."

	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff, "thread_mason": intakefixture.DemoAgent, "thread_mason2": "agent_mason2"}
	f := newCrossRepoWiringFixture(t, agents)
	opts, clock := f.opts, f.clock

	ask := func(question string) intakefixture.DemoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			out, err := intakefixture.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("%s ask: %s %v", req.Name, out, err)
			}
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	f.engine.Turns["build1"] = ask(question1)
	f.engine.Turns["build2"] = ask(question2)

	var flagMu sync.Mutex
	var answered1, answered2 bool
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		flagMu.Lock()
		needAnswer2 := !answered2 && strings.Contains(req.Prompt, "asked by the mason: "+question2)
		needAnswer1 := !answered1 && strings.Contains(req.Prompt, "asked by the mason: "+question1)
		flagMu.Unlock()
		acted := false
		if needAnswer2 {
			out, err := intakefixture.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream2) + ":1", "text": answer2, "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("answer2: %s %v", out, err)
			}
			flagMu.Lock()
			answered2 = true
			flagMu.Unlock()
			acted = true
		}
		if needAnswer1 {
			out, err := intakefixture.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream1) + ":1", "text": answer1, "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("answer1: %s %v", out, err)
			}
			flagMu.Lock()
			answered1 = true
			flagMu.Unlock()
			acted = true
		}
		if acted {
			return intakefixture.QuestionResult(req, "session-chief", "Handled"), nil
		}
		if strings.Contains(req.Prompt, "asked by the mason: "+question1) || strings.Contains(req.Prompt, "asked by the mason: "+question2) {
			return intakefixture.QuestionResult(req, "session-chief", "Nothing further"), nil
		}
		f.problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		return intakefixture.QuestionResult(req, "session-chief", "Nothing to do"), nil
	}
	f.engine.Turns[questions.TurnID("1")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return intakefixture.QuestionResult(req, "session-"+req.Name, "Continued"), nil
	}

	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	repo1, repo2 := f.repositories(t, s)

	id, apiFault := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}

	_, err = repo1.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_build1", Revision: 1, Project: f.project1, Workstream: f.stream1, At: clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "test"},
		AgentID: intakefixture.DemoAgent, ThreadID: "thread_mason", TurnID: "build1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: build1"})
	intakefixture.Must(t, err)
	_, err = repo2.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_build2", Revision: 1, Project: f.project2, Workstream: f.stream2, At: clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "test"},
		AgentID: "agent_mason2", ThreadID: "thread_mason2", TurnID: "build2", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: build2"})
	intakefixture.Must(t, err)

	f.settle(t, s)
	clock.Advance(time.Minute)

	bothAnswered := func() bool {
		q1, err := repo1.Questions(f.stream1)
		if err != nil || len(q1) != 1 || q1[0].State != trace.QuestionAnswered {
			return false
		}
		q2, err := repo2.Questions(f.stream2)
		if err != nil || len(q2) != 1 || q2[0].State != trace.QuestionAnswered {
			return false
		}
		return true
	}
	deadline := time.Now().Add(intakefixture.DemoTimeout)
	for !bothAnswered() {
		select {
		case f.ticks <- clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.mu.Lock()
			probs := slices.Clone(f.problems)
			f.mu.Unlock()
			s.Close()
			t.Fatalf("both questions were not answered in time; problems: %v", probs)
		}
	}

	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread: %v", err)
	}
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread: %v", err)
	}

	resumedWith := func(repo *trace.Repository, stream config.WorkstreamID, agentID, text string) bool {
		th, err := repo.Thread(stream, agentID)
		if err != nil {
			return false
		}
		i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") })
		return i >= 0 && strings.Contains(th.Turns[i].Request.Prompt, text)
	}
	answersReturned := func() bool {
		flagMu.Lock()
		defer flagMu.Unlock()
		return answered1 && answered2
	}
	deadline = time.Now().Add(intakefixture.DemoTimeout)
	for !(resumedWith(repo1, f.stream1, intakefixture.DemoAgent, answer1) && resumedWith(repo2, f.stream2, "agent_mason2", answer2) && answersReturned()) {
		select {
		case f.ticks <- clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.mu.Lock()
			probs := slices.Clone(f.problems)
			f.mu.Unlock()
			s.Close()
			t.Fatalf("one or both askers were not resumed with their own answer in time; problems: %v", probs)
		}
	}

	intakefixture.Must(t, s.Close())
	closed = true
	s2, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	closed2 := false
	defer func() {
		if !closed2 {
			s2.Close()
		}
	}()
	repo1b, repo2b := f.repositories(t, s2)

	th1Before, err := repo1b.Thread(f.stream1, intakefixture.DemoAgent)
	intakefixture.Must(t, err)
	th2Before, err := repo2b.Thread(f.stream2, "agent_mason2")
	intakefixture.Must(t, err)
	before1, before2 := slices.Clone(th1Before.Turns), slices.Clone(th2Before.Turns)

	clock.Advance(time.Minute)
	f.settle(t, s2)

	th1After, err := repo1b.Thread(f.stream1, intakefixture.DemoAgent)
	intakefixture.Must(t, err)
	th2After, err := repo2b.Thread(f.stream2, "agent_mason2")
	intakefixture.Must(t, err)
	if len(th1After.Turns) != len(before1) {
		t.Fatalf("member1's own thread duplicated a turn across the restart: before=%+v after=%+v", before1, th1After.Turns)
	}
	if len(th2After.Turns) != len(before2) {
		t.Fatalf("member2's own thread duplicated a turn across the restart: before=%+v after=%+v", before2, th2After.Turns)
	}
	q1After, err := repo1b.Questions(f.stream1)
	intakefixture.Must(t, err)
	if q1After[0].State != trace.QuestionAnswered || q1After[0].Ruling == nil || q1After[0].Ruling.ReturnedAnswer != answer1 {
		t.Fatalf("member1's question regressed across the restart: %+v", q1After[0])
	}
	q2After, err := repo2b.Questions(f.stream2)
	intakefixture.Must(t, err)
	if q2After[0].State != trace.QuestionAnswered || q2After[0].Ruling == nil || q2After[0].Ruling.ReturnedAnswer != answer2 {
		t.Fatalf("member2's question regressed across the restart: %+v", q2After[0])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.problems) != 0 {
		t.Fatal(strings.Join(f.problems, "\n"))
	}
	_ = id
}

func (f *crossRepoWiringFixture) driveTicksUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(intakefixture.DemoTimeout)
	for !condition() {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("condition was never reached")
		}
	}
}

func TestOwnerMessageAndConversationReadStayResponsiveWhileAWorkgroupChiefTurnRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff, "thread_mason": intakefixture.DemoAgent, "thread_mason2": "agent_mason2"}
	f := newCrossRepoWiringFixture(t, agents)
	opts := f.opts

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return intakefixture.QuestionResult(req, "session-chief", "Handled"), nil
	}

	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	t.Cleanup(func() { s.Close() })
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close() })
	f.repositories(t, s)

	if _, apiFault := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2}); apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}

	if _, err := c.Send(ctx, f.stream1, "First message"); err != nil {
		t.Fatalf("send first message: %v", err)
	}

	startedFired := func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}
	f.driveTicksUntil(t, startedFired)

	begin := time.Now()
	if _, err := c.Send(ctx, f.stream2, "Second message, to the other member"); err != nil {
		t.Fatalf("second owner message while the chief turn was held: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("owner message to the other member took %s while the chief turn was held", elapsed)
	}
	begin = time.Now()
	conv, err := c.Conversation(ctx, f.stream1)
	if err != nil {
		t.Fatalf("conversation read while the chief turn was held: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("conversation read took %s while the chief turn was held", elapsed)
	}
	if len(conv.Entries) == 0 || conv.Entries[0].Text != "First message" {
		t.Fatalf("conversation does not show the first message: %+v", conv.Entries)
	}
}

func TestCrossRepoWorkgroupFromRealSealExcludesStartupWrappingAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()

	main1 := f.upstream(t)
	home := filepath.Dir(f.clone)
	clone2 := filepath.Join(home, "clone2")
	bare2 := filepath.Join(home, "remotes", "owner", "other.git")
	intakefixture.Must(t, os.MkdirAll(filepath.Dir(bare2), 0700))
	fixturekit.DemoGit(t, home, "-C", clone2, "branch", "-M", "main")
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone2, bare2)
	fixturekit.DemoGit(t, home, "-C", clone2, "remote", "add", "upstream", bare2)
	main2 := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare2, "rev-parse", "refs/heads/main"))

	id, apiFault := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}
	packets := f.awaitDecisionPackets(t)
	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan, packets[0].ProjectSpecRevision); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	checkCompletedSeals := func(t *testing.T) trace.GroupApproval {
		t.Helper()
		wait, closeWait := serviceChanges(t, f.s)
		defer closeWait()
		deadline := time.Now().Add(intakefixture.DemoTimeout)
		var decision trace.GroupApproval
		for {
			repo, err := f.s.repository(f.project)
			intakefixture.Must(t, err)
			var found bool
			decision, found, err = repo.GroupApprovalOf(f.stream1)
			intakefixture.Must(t, err)
			if found && decision.Complete {
				break
			}
			if time.Now().After(deadline) {
				var diagnostic strings.Builder
				for _, packet := range packets {
					r, err := f.s.repository(packet.Project)
					intakefixture.Must(t, err)
					state, err := r.Workflow(packet.Workstream, trace.FeatureSubject)
					intakefixture.Must(t, err)
					ops, err := r.Operations(packet.Workstream)
					intakefixture.Must(t, err)
					fmt.Fprintf(&diagnostic, "member=%s state=%s operations=%+v\n", packet.Workstream, state.Value, ops)
				}
				t.Fatalf("group sealing did not complete: found=%v decision=%+v\n%s", found, decision, diagnostic.String())
			}
			wait()
		}
		if decision.Workgroup != id || len(decision.Members) != 2 {
			t.Fatalf("completed approval has wrong membership: %+v", decision)
		}
		for i, packet := range packets {
			repo, err := f.s.repository(packet.Project)
			intakefixture.Must(t, err)
			approval, found, err := repo.GroupApprovalOf(packet.Workstream)
			intakefixture.Must(t, err)
			if !found || !intakefixture.EqualJSONTest(approval, decision) || !slices.ContainsFunc(decision.Members, func(pin trace.GroupMemberApproval) bool {
				return pin.Project == packet.Project && pin.Workstream == packet.Workstream && pin.Spec == packet.Packet.Revision.Spec && pin.Plan == packet.Packet.Revision.Plan
			}) {
				t.Fatalf("member %s lost its exact approval pins: %+v", packet.Workstream, approval)
			}
			sealed, doc, found, err := seal.Latest(repo, packet.Workstream)
			intakefixture.Must(t, err)
			if !found || doc.Revision != 1 || sealed.Revision != packet.Packet.Revision || sealed.Base.Remote != "upstream" || sealed.Base.Branch != "main" || sealed.Base.Commit != [2]string{main1, main2}[i] || sealed.Branch != intake.FeatureBranch(packet.Workstream) {
				t.Fatalf("member %s has no completed pinned seal: %+v document=%+v found=%v", packet.Workstream, sealed, doc, found)
			}
		}
		return decision
	}
	completed := checkCompletedSeals(t)

	checkWorkgroupRecord := func(t *testing.T) {
		t.Helper()
		repo1, err := f.s.repository(f.project)
		intakefixture.Must(t, err)
		repo2, err := f.s.repository(f.project2)
		intakefixture.Must(t, err)
		gid1, err := repo1.CrossRepoWorkgroupOf(f.stream1)
		intakefixture.Must(t, err)
		gid2, err := repo2.CrossRepoWorkgroupOf(f.stream2)
		intakefixture.Must(t, err)
		if gid1 != id || gid2 != id {
			t.Fatalf("members do not both point to the sealed workgroup: member1=%s member2=%s want=%s", gid1, gid2, id)
		}
		_, g, found, err := repo1.CrossRepoProjectSpec(id)
		intakefixture.Must(t, err)
		if !found || len(g.Members) != 3 || g.Members[1] != f.stream1 || g.Members[2] != f.stream2 {
			t.Fatalf("workgroup record is not exactly the host and the two real members: %+v found=%v", g, found)
		}
	}
	checkWorkgroupRecord(t)

	repo1, err := f.s.repository(f.project)
	intakefixture.Must(t, err)
	repo2, err := f.s.repository(f.project2)
	intakefixture.Must(t, err)
	sent1, err := f.c.Send(ctx, f.stream1, "Where does the acknowledgment live?")
	intakefixture.Must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread of its own: %v", err)
	}
	sent2, err := f.c.Send(ctx, f.stream2, "Does the client ever see the offset?")
	intakefixture.Must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread of its own: %v", err)
	}

	for i, member := range []struct {
		repo   *trace.Repository
		stream config.WorkstreamID
	}{{repo1, f.stream1}, {repo2, f.stream2}} {
		unit := fmt.Sprintf("feed-member-%d", i)
		fixturekit.FeedDocument(t, member.repo, member.stream, execution.CheckSelectionDocument(unit, 1), execution.CheckSelectionPath(unit, 1), 1, fixturekit.DemoStart, execution.UnitCheckSelection{Unit: unit, Run: 1, StartedAt: fixturekit.DemoStart, Selection: checkrun.FullSelection("unconfigured", "OPENROUTER_API_KEY is not set")})
		fixturekit.FeedDocument(t, member.repo, member.stream, delivery.FinalReportDocument, delivery.FinalReportPath, 1, fixturekit.DemoStart, api.FinalReport{Review: i + 1, Outcome: delivery.FinalFailed, Failure: unit})
	}

	checkShared := func(t *testing.T) {
		t.Helper()
		for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			conv, err := f.c.Conversation(ctx, m)
			intakefixture.Must(t, err)
			if len(conv.Entries) != 2 || conv.Entries[0].Turn != sent1.Turn || conv.Entries[1].Turn != sent2.Turn {
				t.Fatalf("workstream %s's shared conversation: %+v", m, conv.Entries)
			}
			var feed api.FeedResponse
			intakefixture.Must(t, f.c.Do(ctx, "GET", api.Prefix+"/feed/"+string(m), nil, &feed))
			unit := "feed-member-0"
			review := 1
			if m == f.stream2 {
				unit = "feed-member-1"
				review = 2
			}
			var checks, outcomes int
			for _, entry := range feed.Entries {
				if entry.UnitChecks != nil {
					checks++
					if entry.UnitChecks.Unit != unit {
						t.Fatalf("member feed contains another member's checks: %+v", entry)
					}
				}
				if entry.FinalReview != nil {
					outcomes++
					if entry.FinalReview.Review != review || entry.FinalReview.Failure != unit {
						t.Fatalf("member feed contains another member's report: %+v", entry)
					}
				}
			}
			if checks != 1 || outcomes != 1 {
				t.Fatalf("member activity missing: checks=%d outcomes=%d", checks, outcomes)
			}

			for _, e := range feed.Entries {
				if e.Kind == "message" || e.Kind == "response" || e.Kind == "status" || e.Kind == "action" || e.Session != nil && e.Session.Role == trace.ChiefOfStaff {
					t.Fatalf("workstream %s's member feed contains a shared chief entry: %+v", m, e)
				}
			}
		}
		var shared api.WorkgroupFeedResponse
		intakefixture.Must(t, f.c.Do(ctx, "GET", api.Prefix+"/workgroups/"+string(id)+"/feed", nil, &shared))
		var sawFirst, sawSecond bool
		for _, e := range shared.Entries {
			sawFirst = sawFirst || e.Turn == sent1.Turn
			sawSecond = sawSecond || e.Turn == sent2.Turn
			if e.UnitChecks != nil || e.FinalReview != nil || e.Transition != nil || e.Session != nil {
				t.Fatalf("shared feed contains member activity: %+v", e)
			}
		}
		if !sawFirst || !sawSecond || len(shared.Members) != 2 {
			t.Fatalf("shared feed lost the conversation or member summaries: %+v", shared)
		}
	}
	checkShared(t)

	f.stop(t)
	f.start(t)
	if after := checkCompletedSeals(t); !intakefixture.EqualJSONTest(after, completed) {
		t.Fatalf("restart changed the completed approval: before=%+v after=%+v", completed, after)
	}

	repo1, err = f.s.repository(f.project)
	intakefixture.Must(t, err)
	repo2, err = f.s.repository(f.project2)
	intakefixture.Must(t, err)

	groups1, _, err := trace.Workgroups(repo1)
	intakefixture.Must(t, err)
	if len(groups1) != 0 {
		t.Fatalf("member1's own repository was wrapped in its own one-workstream workgroup: %+v", groups1)
	}
	groups2, _, err := trace.Workgroups(repo2)
	intakefixture.Must(t, err)
	if len(groups2) != 0 {
		t.Fatalf("member2's own repository was wrapped in its own one-workstream workgroup: %+v", groups2)
	}

	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread after restart: %v", err)
	}
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread after restart: %v", err)
	}

	checkWorkgroupRecord(t)
	checkShared(t)
}
