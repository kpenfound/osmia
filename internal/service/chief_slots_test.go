package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const chiefLimitTwo = "[capacity]\nchief = 2\n"

func TestChiefSlotsCapAFakeHearsayNoticeReachingFourteenProjects(t *testing.T) {
	t.Setenv("CHIEF_OWNER_TOKEN", "owner-secret")
	t.Setenv("CHIEF_AGENT_TOKEN", "agent-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args struct {
			After string `json:"after"`
		}
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Error(err)
			return
		}
		if args.After != "" {
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "cursor-1", "notifications": []trace.MemoryEvent{{Event: "event-1", Document: "document-1", Source: "discord", Kind: "message", Time: fixturekit.DemoStart}}})
	}))
	defer server.Close()
	agents := ""
	for _, class := range []string{"worker", "orchestrator", "observer"} {
		agents += fmt.Sprintf("[hearsay.agents.%s]\nid = %q\ntoken_env = \"CHIEF_AGENT_TOKEN\"\n", class, class+"-agent")
	}
	top := fmt.Sprintf("[capacity]\nchief = 3\n[hearsay]\nurl = %q\nprincipal = \"owner\"\ntoken_env = \"CHIEF_OWNER_TOKEN\"\n%s", server.URL, agents)
	f := newChiefFleet(t, 14, top, "hearsay_scope = \"work:osmia:project\"\n")
	s, _ := start(t, f.opts)

	intakefixture.Soon(t, "three chief turns running and eleven waiting", func() bool {
		st := s.ChiefSlots()
		return st.Used == 3 && len(st.Waiting) == 11
	})
	if running := f.agent.runningCount(); running != 3 {
		t.Fatalf("%d chief turns running with the pool full", running)
	}
	if st := s.ChiefSlots(); len(st.Unslotted) != 0 {
		t.Fatalf("a notice wave ran without a slot: %+v", st.Unslotted)
	}
	f.agent.releaseAll()
	intakefixture.Soon(t, "all fourteen notice turns to finish", func() bool {
		_, _, finished, _ := f.agent.snapshot()
		return len(finished) == 14
	})
	_, started, _, peak := f.agent.snapshot()
	if peak != 3 || len(started) != 14 {
		t.Fatalf("peak %d turns running at once, %d started", peak, len(started))
	}
	streams := map[string]bool{}
	for _, turn := range started {
		streams[turn[strings.Index(turn, "@")+1:]] = true
	}
	if len(streams) != 14 {
		t.Fatalf("notice turns reached %d workstreams: %v", len(streams), started)
	}
	intakefixture.Soon(t, "every slot to be released after the wave", func() bool {
		st := s.ChiefSlots()
		return st.Used == 0 && len(st.Waiting) == 0
	})
}

func TestChiefSlotsFullPoolKeepsBackgroundTurnsQueuedAndOwnerTurnsRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newChiefFleet(t, 6, chiefLimitTwo, "")
	s, c := start(t, f.opts)
	f.background(t, s, 0, "H1", 1)
	f.background(t, s, 1, "H2", 2)
	awaitStarted(t, f.agent, f.key(0, "H1"), f.key(1, "H2"))
	f.background(t, s, 2, "W1", 3)
	f.background(t, s, 3, "W2", 4)
	f.background(t, s, 2, "L1", 5)
	st := awaitChiefSlots(t, s, 2, "W1", "W2")
	if st.Limit != 2 || !slices.Equal(chiefTurns(st.Active), []string{"H1", "H2"}) {
		t.Fatalf("pool status: %+v", st)
	}
	if reason := f.reason(t, s, 2, "W1"); reason != "waiting for a chief slot" {
		t.Fatalf("waiting turn reason %q", reason)
	}
	if reason := f.reason(t, s, 2, "L1"); !strings.HasPrefix(reason, "waiting for an event turn") {
		t.Fatalf("later turn in the thread gives %q, want it behind the waiting turn", reason)
	}
	if f.agent.hasStarted(f.key(2, "L1")) || f.agent.hasStarted(f.key(2, "W1")) {
		t.Fatal("a turn ran without a slot")
	}

	sent, err := c.Send(ctx, f.streams[4], "Owner message while the pool is full")
	fixturekit.Must(t, err)
	awaitStarted(t, f.agent, f.key(4, sent.Turn))
	repo := f.repo(t, s, 5)
	session := config.WorkstreamID("w_00000000000000000000000000000555")
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-test", "Session", s.now(), ownerActor))
	_, err = ensureSessionThread(ctx, repo, f.ids[5], session, s.now())
	fixturekit.Must(t, err)
	chat, err := c.SessionMessage(ctx, session, "Session turn while the pool is full")
	fixturekit.Must(t, err)
	awaitStarted(t, f.agent, chat.Turn+"@"+string(session))
	if running := f.agent.runningCount(); running != 4 {
		t.Fatalf("%d turns running: two slot holders, an owner turn and a session turn", running)
	}
	if st := s.ChiefSlots(); st.Used != 2 || len(st.Unslotted) != 0 || !slices.Equal(chiefTurns(st.Waiting), []string{"W1", "W2"}) {
		t.Fatalf("owner and session turns changed the pool: %+v", st)
	}
	f.agent.releaseAll()
	f.awaitSettled(t, s, 2, "W1", "L1")
	f.awaitSettled(t, s, 3, "W2")
}

func TestChiefSlotsOwnerMessageRunsTheWaitingBackgroundTurnAheadOfItWithoutASlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newChiefFleet(t, 4, chiefLimitTwo, "")
	s, c := start(t, f.opts)
	f.background(t, s, 0, "H1", 1)
	f.background(t, s, 1, "H2", 2)
	awaitStarted(t, f.agent, f.key(0, "H1"), f.key(1, "H2"))
	f.background(t, s, 2, "W", 3)
	f.background(t, s, 3, "X", 4)
	awaitChiefSlots(t, s, 2, "W", "X")

	sent, err := c.Send(ctx, f.streams[2], "Owner message behind a waiting background turn")
	fixturekit.Must(t, err)
	awaitStarted(t, f.agent, f.key(2, "W"))
	st := s.ChiefSlots()
	if st.Used != 2 || len(st.Unslotted) != 1 || st.Unslotted[0].Turn != "W" || !slices.Equal(chiefTurns(st.Waiting), []string{"X"}) {
		t.Fatalf("pool after the owner message: %+v", st)
	}
	if running := f.agent.runningCount(); running != 3 {
		t.Fatalf("%d background turns running, want the two slot holders and one more", running)
	}
	if f.agent.hasStarted(f.key(2, sent.Turn)) || f.agent.hasStarted(f.key(3, "X")) {
		t.Fatal("the owner's turn jumped its thread or another waiting turn ran without a slot")
	}
	f.agent.release(f.key(2, "W"))
	awaitStarted(t, f.agent, f.key(2, sent.Turn))
	st = s.ChiefSlots()
	if st.Used != 2 || len(st.Unslotted) != 0 || !slices.Equal(chiefTurns(st.Waiting), []string{"X"}) || f.agent.hasStarted(f.key(3, "X")) {
		t.Fatalf("pool after the owner's turn started: %+v", st)
	}
	f.agent.release(f.key(2, sent.Turn))
	f.awaitSettled(t, s, 2, "W", sent.Turn)
	f.agent.release(f.key(0, "H1"))
	awaitStarted(t, f.agent, f.key(3, "X"))
	f.agent.releaseAll()
	f.awaitSettled(t, s, 3, "X")
	if _, _, _, peak := f.agent.snapshot(); peak > 3 {
		t.Fatalf("%d turns ran at once", peak)
	}
}

func TestChiefSlotsFreedSlotGoesToTheOldestWaiterWhicheverProjectScansFirst(t *testing.T) {
	t.Parallel()
	f := newChiefFleet(t, 6, "[capacity]\nchief = 1\n", "")
	s, _ := start(t, f.opts)
	f.background(t, s, 5, "H", 1)
	awaitStarted(t, f.agent, f.key(5, "H"))
	order := []int{3, 1, 4, 0}
	for n, i := range order {
		f.background(t, s, i, fmt.Sprintf("A%d", i), 2+n)
		awaitChiefSlots(t, s, 1, waitingNames(order[:n+1])...)
	}
	f.background(t, s, 2, "Z", 20)
	want := append(waitingNames(order), "Z")
	awaitChiefSlots(t, s, 1, want...)
	f.agent.release(f.key(5, "H"))
	for n, i := range order {
		name := fmt.Sprintf("A%d", i)
		awaitStarted(t, f.agent, f.key(i, name))
		_, started, _, _ := f.agent.snapshot()
		if len(started) != n+2 {
			t.Fatalf("started %v while %s was next", started, name)
		}
		f.agent.release(f.key(i, name))
	}
	awaitStarted(t, f.agent, f.key(2, "Z"))
	f.agent.releaseAll()
	f.awaitSettled(t, s, 2, "Z")
	if _, _, _, peak := f.agent.snapshot(); peak != 1 {
		t.Fatalf("%d turns ran at once", peak)
	}
}

func waitingNames(projects []int) []string {
	var names []string
	for _, i := range projects {
		names = append(names, fmt.Sprintf("A%d", i))
	}
	return names
}

func TestChiefSlotsTurnHeldByAnotherGateDoesNotBlockAYoungerEligibleTurn(t *testing.T) {
	t.Parallel()
	f := newChiefFleet(t, 3, chiefLimitTwo, "")
	s, _ := start(t, f.opts)
	f.background(t, s, 0, "H", 1)
	f.background(t, s, 1, "A1", 2)
	awaitStarted(t, f.agent, f.key(0, "H"), f.key(1, "A1"))
	f.background(t, s, 1, "A2", 3)
	f.background(t, s, 2, "B", 4)
	awaitChiefSlots(t, s, 2, "B")
	if f.agent.hasStarted(f.key(1, "A2")) {
		t.Fatal("a turn ran behind an unfinished turn of its thread")
	}
	f.agent.release(f.key(0, "H"))
	awaitStarted(t, f.agent, f.key(2, "B"))
	if f.agent.hasStarted(f.key(1, "A2")) {
		t.Fatal("the older turn held by its thread took the slot")
	}
	f.agent.release(f.key(1, "A1"))
	awaitStarted(t, f.agent, f.key(1, "A2"))
	f.agent.releaseAll()
	f.awaitSettled(t, s, 1, "A1", "A2")
	f.awaitSettled(t, s, 2, "B")
}

func TestChiefSlotsAbandonedWaiterDoesNotBlockAYoungerOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newChiefFleet(t, 3, "[capacity]\nchief = 1\n", "")
	s, c := start(t, f.opts)
	f.background(t, s, 0, "H", 1)
	awaitStarted(t, f.agent, f.key(0, "H"))
	f.background(t, s, 1, "A", 2)
	f.background(t, s, 2, "B", 3)
	awaitChiefSlots(t, s, 1, "A", "B")
	_, err := c.Abandon(ctx, f.streams[1], "Abandoned while waiting for a chief slot")
	fixturekit.Must(t, err)
	awaitChiefSlots(t, s, 1, "B")
	f.agent.release(f.key(0, "H"))
	awaitStarted(t, f.agent, f.key(2, "B"))
	if f.agent.hasStarted(f.key(1, "A")) {
		t.Fatal("an abandoned workstream's turn ran")
	}
	f.agent.releaseAll()
	f.awaitSettled(t, s, 2, "B")
}

func TestChiefSlotsBackgroundTurnsIgnoreOwnerPausesAndKeepProviderLimits(t *testing.T) {
	t.Parallel()
	f := newChiefFleet(t, 2, "", "")
	s, _ := start(t, f.opts)
	fixturekit.Must(t, s.store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "Owner pause", Source: runtime.PauseOwner, SetAt: s.now()}))
	f.agent.releaseAll()
	f.background(t, s, 0, "Paused", 1)
	awaitStarted(t, f.agent, f.key(0, "Paused"))
	f.awaitSettled(t, s, 0, "Paused")
	for _, backend := range []string{"claude", "codex"} {
		fixturekit.Must(t, s.store.SetProviderLimit(runtime.ProviderLimit{Backend: backend, Status: "blocked", SetAt: s.now()}))
	}
	f.background(t, s, 1, "Limited", 2)
	s.conversationSignal.wake()
	time.Sleep(300 * time.Millisecond)
	if f.agent.hasStarted(f.key(1, "Limited")) {
		t.Fatal("a provider limit did not hold the background turn")
	}
	if st := s.ChiefSlots(); st.Used != 0 || len(st.Waiting) != 0 {
		t.Fatalf("a held turn is registered with the pool: %+v", st)
	}
	for _, backend := range []string{"claude", "codex"} {
		fixturekit.Must(t, s.store.ClearProviderLimit(backend))
	}
	s.conversationSignal.wake()
	awaitStarted(t, f.agent, f.key(1, "Limited"))
}

func TestChiefSlotsEveryExitReleasesTheSlot(t *testing.T) {
	t.Parallel()
	for _, exit := range []string{"completion", "failure", "interruption", "waiting", "abandonment", "shutdown"} {
		t.Run(exit, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newChiefFleet(t, 2, "[capacity]\nchief = 1\n", "")
			s, c := start(t, f.opts)
			prompt := map[string]string{"failure": "fail", "interruption": "cancel", "waiting": "wait"}[exit]
			if prompt == "" {
				prompt = "ok"
			}
			f.backgroundWith(t, s, 0, "A", 1, prompt, "default")
			awaitStarted(t, f.agent, f.key(0, "A"))
			f.background(t, s, 1, "B", 2)
			awaitChiefSlots(t, s, 1, "B")
			switch exit {
			case "abandonment":
				_, err := c.Abandon(ctx, f.streams[0], "Abandoned while running")
				fixturekit.Must(t, err)
			case "shutdown":
				fixturekit.Must(t, s.Close())
				if st := s.ChiefSlots(); st.Used != 0 {
					t.Fatalf("slots held after shutdown: %+v", st)
				}
				f.agent.releaseAll()
				s, _ = start(t, f.opts)
				if st := s.ChiefSlots(); st.Used > 1 {
					t.Fatalf("slots after restart: %+v", st)
				}
				f.awaitSettled(t, s, 1, "B")
				if _, _, _, peak := f.agent.snapshot(); peak != 1 {
					t.Fatalf("%d turns ran at once across the restart", peak)
				}
				return
			default:
				f.agent.release(f.key(0, "A"))
			}
			awaitStarted(t, f.agent, f.key(1, "B"))
			f.agent.releaseAll()
			f.awaitSettled(t, s, 1, "B")
			intakefixture.Soon(t, "every slot to be released after "+exit, func() bool { return s.ChiefSlots().Used == 0 })
		})
	}
}

func TestChiefSlotsRestartAdmitsFourteenInterruptedTurnsAgainstThePool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newChiefFleet(t, 14, "[capacity]\nchief = 3\n", "")
	for i, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		fixturekit.Must(t, err)
		_, err = repo.EnqueueTurn(ctx, f.request(i, "interrupted", chiefAt(i), serviceActor, "event", "ok", "default"))
		fixturekit.Must(t, err)
		dir := filepath.Join(f.cfg.Root.String(), "threads", string(p.ID), string(f.streams[i]), trace.ChiefOfStaff, "interrupted")
		fixturekit.Must(t, os.MkdirAll(dir, 0700))
		_, err = repo.ClaimTurn(ctx, f.streams[i], trace.ChiefOfStaff, "old-claim", dir, chiefAt(i))
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.Close())
	}
	s, _ := start(t, f.opts)
	if st := s.ChiefSlots(); st.Used > 3 {
		t.Fatalf("slots after restart: %+v", st)
	}
	intakefixture.Soon(t, "three recovery turns running and eleven waiting", func() bool {
		st := s.ChiefSlots()
		return st.Used == 3 && len(st.Waiting) == 11
	})
	if running := f.agent.runningCount(); running != 3 {
		t.Fatalf("%d recovery turns re-dispatched at once", running)
	}
	f.agent.releaseAll()
	intakefixture.Soon(t, "every recovery turn to finish", func() bool {
		_, _, finished, _ := f.agent.snapshot()
		return len(finished) == 14
	})
	if _, _, _, peak := f.agent.snapshot(); peak != 3 {
		t.Fatalf("%d recovery turns ran at once", peak)
	}
	for i := range f.ids {
		th := f.thread(t, s, i)
		if len(th.Turns) != 2 || th.Turns[1].CompletedAt.IsZero() || th.Turns[1].Response == nil || th.Turns[1].Response.Result.FinalResponse == "" {
			t.Fatalf("project %d continuation: %+v", i, th.Turns)
		}
	}
}

func TestChiefSlotsSharedWorkgroupChiefTakesFromTheSamePool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, members, group := crossProjectAdmissionFixture(t)
	agent := newChiefAgent()
	s.options.Threads = chiefThreads(s.cfg.Root.String(), s.now, agent)
	s.chief = newChiefSlotPool(1, nil, s.conversationSignal.wake)
	p := &conversationPath{s: s, repository: members[0].Project, drain: make(chan struct{})}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	host := group.Members[0]
	intakefixture.Soon(t, "the shared chief target", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.controllers) == 2
	})
	wt, err := trace.OpenWorkgroupTrace(s.current().Root, group.ID, members)
	fixturekit.Must(t, err)
	t.Cleanup(func() { wt.Close() })
	s.chief.resize(0)
	_, err = wt.EnsureChiefOfStaff(ctx, host, chiefBase, serviceActor)
	fixturekit.Must(t, err)
	profile, err := s.cfg.NamedProfile("default")
	fixturekit.Must(t, err)
	_, err = wt.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_shared", Project: wt.Project(), Workstream: host, At: chiefAt(1), Actor: serviceActor, Cause: "event"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "shared", Profile: profile, Prompt: "ok"})
	fixturekit.Must(t, err)
	s.conversationSignal.wake()
	intakefixture.Soon(t, "the shared chief turn to wait for a slot", func() bool {
		return slices.Equal(chiefTurns(s.ChiefSlots().Waiting), []string{"shared"})
	})
	th, err := wt.Thread(host, trace.ChiefOfStaff)
	fixturekit.Must(t, err)
	if reason := s.queuedReason(th, th.Turns[0]); reason != "waiting for a chief slot" || agent.hasStarted("shared@"+string(host)) {
		t.Fatalf("shared chief turn: reason %q", reason)
	}
	s.chief.resize(1)
	awaitStarted(t, agent, "shared@"+string(host))
	st := s.ChiefSlots()
	if st.Used != 1 || len(st.Active) != 1 || st.Active[0].Project != wt.Project() || st.Active[0].Turn != "shared" {
		t.Fatalf("shared chief turn did not hold a slot of the factory pool: %+v", st)
	}
	agent.releaseAll()
	intakefixture.Soon(t, "the slot to be released", func() bool { return s.ChiefSlots().Used == 0 })
}

func TestChiefSlotsOnlyBackgroundTurnsNeedASlot(t *testing.T) {
	t.Parallel()
	turn := func(id string, actor trace.Actor, cause string) trace.QueuedTurn {
		return trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Actor: actor, Cause: cause}, TurnID: id}}
	}
	interrupted := func(of trace.QueuedTurn) trace.QueuedTurn {
		of.Response = &trace.TurnResponse{Header: trace.Header{ID: "response_" + of.Request.TurnID}}
		return of
	}
	ownerTurn := turn("owner", ownerActor, "owner-message")
	eventTurn := turn("event", events.Actor, "transition")
	recoveredOwner := turn("owner-recover", recoveryActor, "response_owner")
	recoveredEvent := turn("event-recover", recoveryActor, "response_event")
	th := trace.Thread{Turns: []trace.QueuedTurn{interrupted(ownerTurn), interrupted(eventTurn), recoveredOwner, recoveredEvent}}
	for _, test := range []struct {
		name  string
		turn  trace.QueuedTurn
		owner bool
	}{
		{"owner message", ownerTurn, true},
		{"recovered owner message", recoveredOwner, true},
		{"event", eventTurn, false},
		{"recovered event", recoveredEvent, false},
		{"follow-up and check-back", turn("follow-up", followUpActor, "follow-up"), false},
		{"contest", turn("contest", serviceActor, "contest"), false},
		{"amendment", turn("amendment", intake.AmendmentActor, "amendment"), false},
		{"answer delivery", turn("answer", questions.Actor, "question_answered"), false},
		{"refused project creation", turn("refusal", serviceActor, "refusal"), false},
	} {
		if got := chiefOwnerStarted(th, test.turn); got != test.owner {
			t.Errorf("%s: owner-started is %v, want %v", test.name, got, test.owner)
		}
	}
}

func TestChiefSlotsFullPoolIsReportedByTheCapacityToolAndStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newChiefFleet(t, 6, chiefLimitTwo, "")
	s, c := start(t, f.opts)
	f.background(t, s, 0, "H1", 1)
	f.background(t, s, 1, "H2", 2)
	awaitStarted(t, f.agent, f.key(0, "H1"), f.key(1, "H2"))
	f.background(t, s, 2, "W1", 3)
	f.background(t, s, 3, "W2", 4)
	awaitChiefSlots(t, s, 2, "W1", "W2")

	sent, err := c.Send(ctx, f.streams[4], "Owner message while the pool is full")
	fixturekit.Must(t, err)
	awaitStarted(t, f.agent, f.key(4, sent.Turn))
	repo := f.repo(t, s, 4)
	scope := coreadapter.Scope{Project: string(f.ids[4]), Workstream: string(f.streams[4]), Thread: trace.ChiefOfStaff, Turn: sent.Turn, Role: trace.ChiefOfStaff}
	controls := &runtimeControls{}
	controls.service.Store(s)
	out, err := controls.capacity(repo, scope).Handle(ctx, json.RawMessage(`{}`))
	fixturekit.Must(t, err)
	var status json.RawMessage
	fixturekit.Must(t, c.Do(ctx, "GET", Prefix+"/status", nil, &status))

	for name, raw := range map[string][]byte{"tool": out, "status": status} {
		var doc struct {
			Capacity *CapacityStatus `json:"capacity"`
		}
		fixturekit.Must(t, json.Unmarshal(raw, &doc))
		if doc.Capacity == nil {
			t.Fatalf("%s reports no capacity: %s", name, raw)
		}
		var roles []string
		for _, r := range doc.Capacity.Roles {
			roles = append(roles, r.Role)
		}
		if !slices.Equal(roles, []string{masonRole, reviewerRole, committeeRole, architectRole, checksRole, chiefRole}) {
			t.Fatalf("%s pools %v", name, roles)
		}
		chief := fixturekit.RoleCapacityEntry(t, doc.Capacity, chiefRole)
		if fixturekit.LimitOf(t, chief) != 2 || chief.Used != 2 || len(chief.Waiting) != 2 {
			t.Fatalf("%s chief entry %+v", name, chief)
		}
		for i, want := range []struct {
			stream config.WorkstreamID
			turn   string
		}{{f.streams[2], "W1"}, {f.streams[3], "W2"}} {
			w := chief.Waiting[i]
			if w.Project != f.ids[i+2] || w.Workstream != want.stream || w.Agent != trace.ChiefOfStaff || w.Turn != want.turn || w.Reason != DeferCapacity {
				t.Fatalf("%s chief waiting[%d] %+v", name, i, w)
			}
		}
	}
	f.agent.releaseAll()
	f.awaitSettled(t, s, 2, "W1")
	f.awaitSettled(t, s, 3, "W2")
}
