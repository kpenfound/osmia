package intakejourney

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const handoffTitleGoal = "Resumable uploads across the server and the client"

const handoffEventWindows = "[events]\nwindow = \"1h\"\nprogress_window = \"2h\"\n"

func handoffTitleTurns(th trace.Thread) []trace.QueuedTurn {
	return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool {
		return !strings.Contains(q.Request.Prompt, intake.HandoffTitleOpening)
	})
}

type handoffTitleFixture struct {
	s         *journey.Service
	c         *journey.Client
	home      config.ProjectID
	other     config.ProjectID
	engine    *fixturekit.DemoEngine
	origin    config.WorkstreamID
	window    string
	interrupt atomic.Bool
	passed    atomic.Int64
	mu        sync.Mutex
	calls     []string
	failure   []string
}

func newHandoffTitleFixture(t *testing.T) *handoffTitleFixture {
	t.Helper()
	s, c, home, engine := newDesignEnforcementFixtureWith(t, "", handoffEventWindows)
	f := &handoffTitleFixture{s: s, c: c, home: home, engine: engine, window: "1h"}
	f.other = addDesignProject(t, c, "other-member")
	intakefixture.WriteCharter(t, c, f.other, "1. Keep changes small.\n")
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		prompt := intakefixture.OwnPrompt(req.Prompt)
		if !strings.Contains(prompt, intake.HandoffTitleOpening) {
			return intakefixture.QuestionResult(req, "session-chief", "Nothing to do."), nil
		}
		f.mu.Lock()
		f.calls = append(f.calls, prompt)
		f.mu.Unlock()
		if !strings.Contains(prompt, intake.DesignBriefPath) || !strings.Contains(prompt, "set_status") || !strings.Contains(prompt, "title") {
			f.problem("hand-off turn does not name the brief and ask for a title: %s", prompt)
		}
		input := map[string]any{"goal": handoffTitleGoal, "note": "The work is titled from the handed brief.", "agents": []string{}}
		out, err := fixturekit.CallTool(ctx, tools, status.ToolName, input)
		if err == nil && strings.Contains(out, "attention is required") {
			input["attention"] = "Review what the owner holds."
			out, err = fixturekit.CallTool(ctx, tools, status.ToolName, input)
		}
		if err != nil || !strings.Contains(out, `"stored":true`) {
			f.problem("set_status: %s %v", out, err)
		}
		return intakefixture.QuestionResult(req, "session-chief", "Titled."), nil
	}
	engine.Mu.Unlock()
	f.restart(t, f.window)
	return f
}

func (f *handoffTitleFixture) problem(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = append(f.failure, fmt.Sprintf(format, args...))
}

func (f *handoffTitleFixture) deliveredTurns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *handoffTitleFixture) restart(t *testing.T, window string) {
	t.Helper()
	options := f.s.Options
	path := filepath.Join(options.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	next := strings.Replace(string(data), "\nwindow = \""+f.window+"\"", "\nwindow = \""+window+"\"", 1)
	if next == string(data) && window != f.window {
		t.Fatalf("config has no events window %q to change", f.window)
	}
	f.window = window
	fixturekit.Must(t, os.WriteFile(path, []byte(next), 0600))
	f.interrupt.Store(false)
	options.SchedulePassed = func() { f.passed.Add(1) }
	options.Boundary = func(_ context.Context, name string) error {
		if name == "design-handoff-title-requested" && f.interrupt.Load() {
			return errors.New("interrupted after the hand-off title request")
		}
		return nil
	}
	f.c.Close()
	fixturekit.Must(t, f.s.Close())
	f.s, f.c = journey.StartT(t, options)
}

func (f *handoffTitleFixture) awaitStart(t *testing.T, r *trace.Repository, states ...string) {
	t.Helper()
	f.s.Until(t, fmt.Sprintf("the start to reach one of %v", states), func() bool {
		state, err := r.Workflow(f.origin, intake.DesignStartSubject)
		fixturekit.Must(t, err)
		return slices.Contains(states, state.Value)
	})
}

func (f *handoffTitleFixture) notices(t *testing.T) (recorded, acknowledged int) {
	t.Helper()
	r, err := f.s.Repository(f.home)
	fixturekit.Must(t, err)
	selected, found, err := intake.DesignSelectionOf(r, f.origin)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the start recorded no selection")
	}
	streams := map[config.WorkstreamID]config.ProjectID{f.origin: f.home}
	for _, member := range selected.Members {
		streams[member.Workstream] = member.Repo
	}
	for stream, project := range streams {
		repo, err := f.s.Repository(project)
		fixturekit.Must(t, err)
		outbox, err := repo.Outbox(stream)
		fixturekit.Must(t, err)
		for _, e := range outbox {
			if e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, intake.HandoffTitleOpening) {
				recorded++
				if e.Acknowledged {
					acknowledged++
				}
			}
		}
	}
	return recorded, acknowledged
}

func (f *handoffTitleFixture) chiefThread(t *testing.T, shared bool) trace.Thread {
	t.Helper()
	r, err := f.s.Repository(f.home)
	fixturekit.Must(t, err)
	if !shared {
		th, err := r.ManagerThread(f.origin)
		fixturekit.Must(t, err)
		return th
	}
	target, host, _, closeTarget, err := f.s.ChiefRepository(context.Background(), r, f.origin)
	fixturekit.Must(t, err)
	defer closeTarget()
	th, err := target.Thread(host, trace.Manager)
	fixturekit.Must(t, err)
	return th
}

type handoffTopology struct {
	name    string
	members func(home, other config.ProjectID) []config.ProjectID
	adopted bool
}

var handoffTitleTopologies = []handoffTopology{
	{"retained session", func(home, _ config.ProjectID) []config.ProjectID { return []config.ProjectID{home} }, false},
	{"several members", func(home, other config.ProjectID) []config.ProjectID { return []config.ProjectID{home, other} }, true},
	{"one other project", func(_, other config.ProjectID) []config.ProjectID { return []config.ProjectID{other} }, true},
}

func TestDesignStartDeliversOneHandoffTitleTurnToTheThreadThatContinues(t *testing.T) {
	t.Parallel()
	for _, which := range handoffTitleTopologies {
		t.Run(which.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newHandoffTitleFixture(t)
			projects := which.members(f.home, f.other)
			opened, err := f.c.OpenDesign(ctx, f.home)
			fixturekit.Must(t, err)
			f.origin = opened.Design.Workstream
			r, err := f.s.Repository(f.home)
			fixturekit.Must(t, err)
			revision := recordDesignProjects(t, r, f.origin, intakefixture.ProductionBrief, projects)
			before, err := r.ManagerThread(f.origin)
			fixturekit.Must(t, err)

			f.interrupt.Store(true)
			err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			if err == nil {
				t.Log("start returned before the interruption")
			}
			f.restart(t, "1h")
			if got := f.deliveredTurns(); got != 0 {
				t.Fatalf("a held window delivered %d hand-off turns", got)
			}

			r, err = f.s.Repository(f.home)
			fixturekit.Must(t, err)
			err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			if err != nil && !intakefixture.Failed(err, api.Conflict) && !intakefixture.Failed(err, api.NotFound) {
				t.Fatalf("retried start: %v", err)
			}
			f.awaitStart(t, r, "single", "complete")

			f.restart(t, "100ms")
			f.s.Until(t, "the hand-off title turn to be delivered before the routine-progress window", func() bool { return f.deliveredTurns() > 0 })
			f.s.Until(t, "the hand-off turn to settle", func() bool {
				recorded, acknowledged := f.notices(t)
				turns := handoffTitleTurns(f.chiefThread(t, which.adopted))
				return recorded == 1 && acknowledged == 1 && len(turns) == 1 && !turns[0].CompletedAt.IsZero()
			})
			for i := range 2 {
				err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
				if err != nil && !intakefixture.Failed(err, api.Conflict) && !intakefixture.Failed(err, api.NotFound) {
					t.Fatalf("retried start %d: %v", i, err)
				}
				f.restart(t, "100ms")
			}
			intakefixture.AwaitFurtherPasses(t, &f.passed, 3)
			r, err = f.s.Repository(f.home)
			fixturekit.Must(t, err)

			f.mu.Lock()
			problems := strings.Join(f.failure, "\n")
			f.mu.Unlock()
			if problems != "" {
				t.Fatalf("hand-off turn problems:\n%s", problems)
			}
			if got := f.deliveredTurns(); got != 1 {
				t.Fatalf("%d hand-off turns reached the chief of staff, want 1", got)
			}
			thread := f.chiefThread(t, which.adopted)
			turns := handoffTitleTurns(thread)
			if len(turns) != 1 || turns[0].Response == nil || turns[0].CompletedAt.IsZero() {
				t.Fatalf("hand-off turns on the continuing thread: %+v", turns)
			}
			if thread.Identity.ThreadID != before.Identity.ThreadID || thread.Identity.ID != before.Identity.ID {
				t.Fatalf("the hand-off turn is not on the design's chief thread: before %+v, after %+v", before.Identity, thread.Identity)
			}

			selected, found, err := intake.DesignSelectionOf(r, f.origin)
			fixturekit.Must(t, err)
			if !found || len(selected.Members) != len(projects) {
				t.Fatalf("selected members: %+v", selected)
			}
			if !which.adopted {
				st, err := f.c.Status(ctx, f.origin)
				fixturekit.Must(t, err)
				if st.Status == nil || st.Status.Goal != handoffTitleGoal || st.Kind == "design" {
					t.Fatalf("started workstream status: %+v", st)
				}
				return
			}
			if _, err := r.Thread(f.origin, trace.Manager); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the retired local chief remains: %v", err)
			}
			for _, turn := range before.Turns {
				if strings.Contains(turn.Request.Prompt, intake.HandoffTitleOpening) {
					t.Fatal("the hand-off turn reached the local chief before it retired")
				}
			}
			for _, member := range selected.Members {
				repo, err := f.s.Repository(member.Repo)
				fixturekit.Must(t, err)
				st, err := f.c.Status(ctx, member.Workstream)
				fixturekit.Must(t, err)
				if st.Status == nil || st.Status.Goal != handoffTitleGoal {
					t.Fatalf("member %s status: %+v", member.Workstream, st)
				}
				if _, err := repo.Thread(member.Workstream, trace.Manager); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("member %s keeps a local chief: %v", member.Workstream, err)
				}
			}
			alias, err := f.c.Status(ctx, f.origin)
			fixturekit.Must(t, err)
			if alias.Status == nil || alias.Status.Goal != handoffTitleGoal {
				t.Fatalf("workgroup status through the design address: %+v", alias)
			}
		})
	}
}
