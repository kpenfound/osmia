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
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// handoffTitleGoal is the title the fake chief of staff gives the started work.
const handoffTitleGoal = "Resumable uploads across the server and the client"

// handoffEventWindows hold events for an hour, so nothing is delivered until
// a test shortens them. The routine-progress window stays at two hours.
const handoffEventWindows = "[events]\nwindow = \"1h\"\nprogress_window = \"2h\"\n"

// ownPrompt returns the part of a chief-of-staff prompt that belongs to the
// turn itself. A turn that is replayed rather than resumed carries the thread's
// earlier exchanges, encoded on one line, ahead of its own prompt.
func ownPrompt(prompt string) string {
	if !strings.HasPrefix(prompt, `{"source":"osmia-owned-log"`) {
		return prompt
	}
	if _, own, ok := strings.Cut(prompt, "\n\n"); ok {
		return own
	}
	return ""
}

// handoffTitleTurns returns the turns of thread that ask for a hand-off title.
func handoffTitleTurns(th trace.Thread) []trace.QueuedTurn {
	return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool {
		return !strings.Contains(q.Request.Prompt, intake.HandoffTitleOpening)
	})
}

// handoffTitleFixture is a service whose fake chief of staff answers a
// hand-off title turn with set_status.
type handoffTitleFixture struct {
	s       *Service
	c       *Client
	home    config.ProjectID
	other   config.ProjectID
	engine  *demoEngine
	origin  config.WorkstreamID
	window  string
	// interrupt makes the start fail at the step that records the hand-off
	// title request. It is read by the service's own goroutines.
	interrupt atomic.Bool
	mu        sync.Mutex
	calls     []string
	failure []string
}

func newHandoffTitleFixture(t *testing.T) *handoffTitleFixture {
	t.Helper()
	s, c, home, engine := newDesignEnforcementFixtureWith(t, "", handoffEventWindows)
	f := &handoffTitleFixture{s: s, c: c, home: home, engine: engine, window: "1h"}
	f.other = addDesignProject(t, c, "other-member")
	writeCharter(t, c, f.other, "1. Keep changes small.\n")
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		prompt := ownPrompt(req.Prompt)
		if !strings.Contains(prompt, intake.HandoffTitleOpening) {
			return questionResult(req, "session-chief", "Nothing to do."), nil
		}
		f.mu.Lock()
		f.calls = append(f.calls, prompt)
		f.mu.Unlock()
		if !strings.Contains(prompt, intake.DesignBriefPath) || !strings.Contains(prompt, "set_status") || !strings.Contains(prompt, "title") {
			f.problem("hand-off turn does not name the brief and ask for a title: %s", prompt)
		}
		input := map[string]any{"goal": handoffTitleGoal, "note": "The work is titled from the handed brief.", "agents": []string{}}
		out, err := callTool(ctx, tools, status.ToolName, input)
		if err == nil && strings.Contains(out, "attention is required") {
			// The status rules ask for attention while the owner holds a gate.
			input["attention"] = "Review what the owner holds."
			out, err = callTool(ctx, tools, status.ToolName, input)
		}
		if err != nil || !strings.Contains(out, `"stored":true`) {
			f.problem("set_status: %s %v", out, err)
		}
		return questionResult(req, "session-chief", "Titled."), nil
	}
	engine.Mu.Unlock()
	// Restart once so the service is armed with the interruption hook.
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

// restart restarts the service, with the event windows set to window.
func (f *handoffTitleFixture) restart(t *testing.T, window string) {
	t.Helper()
	options := f.s.options
	path := filepath.Join(options.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	next := strings.Replace(string(data), "\nwindow = \""+f.window+"\"", "\nwindow = \""+window+"\"", 1)
	if next == string(data) && window != f.window {
		t.Fatalf("config has no events window %q to change", f.window)
	}
	f.window = window
	must(t, os.WriteFile(path, []byte(next), 0600))
	f.interrupt.Store(false)
	options.boundary = func(_ context.Context, name string) error {
		if name == "design-handoff-title-requested" && f.interrupt.Load() {
			return errors.New("interrupted after the hand-off title request")
		}
		return nil
	}
	f.c.Close()
	must(t, f.s.Close())
	f.s, f.c = start(t, options)
}

// awaitStart waits for the start of origin to reach one of its settled states.
func (f *handoffTitleFixture) awaitStart(t *testing.T, r *trace.Repository, states ...string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := r.Workflow(f.origin, intake.DesignStartSubject)
		must(t, err)
		if slices.Contains(states, state.Value) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("start remained %q, want one of %v", state.Value, states)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// notices counts the hand-off title notices recorded for the start across the
// design's workstream and its members, and how many of them are acknowledged.
func (f *handoffTitleFixture) notices(t *testing.T) (recorded, acknowledged int) {
	t.Helper()
	r, err := f.s.repository(f.home)
	must(t, err)
	selected, found, err := intake.DesignSelectionOf(r, f.origin)
	must(t, err)
	if !found {
		t.Fatal("the start recorded no selection")
	}
	streams := map[config.WorkstreamID]config.ProjectID{f.origin: f.home}
	for _, member := range selected.Members {
		streams[member.Workstream] = member.Repo
	}
	for stream, project := range streams {
		repo, err := f.s.repository(project)
		must(t, err)
		outbox, err := repo.Outbox(stream)
		must(t, err)
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

// chiefThread returns the chief-of-staff thread that continues the design.
func (f *handoffTitleFixture) chiefThread(t *testing.T, shared bool) trace.Thread {
	t.Helper()
	r, err := f.s.repository(f.home)
	must(t, err)
	if !shared {
		th, err := r.ChiefOfStaffThread(f.origin)
		must(t, err)
		return th
	}
	target, host, _, closeTarget, err := f.s.chiefRepository(context.Background(), r, f.origin)
	must(t, err)
	defer closeTarget()
	th, err := target.Thread(host, trace.ChiefOfStaff)
	must(t, err)
	return th
}

// handoffTopology is a start the chief of staff titles, with whether the
// thread that continues the design is the workgroup's shared one.
type handoffTopology struct {
	name    string
	members func(home, other config.ProjectID) []config.ProjectID
	adopted bool
}

// handoffTitleTopologies are the session's own project as the sole member,
// several members, and one other project than the session's.
var handoffTitleTopologies = []handoffTopology{
	{"retained session", func(home, _ config.ProjectID) []config.ProjectID { return []config.ProjectID{home} }, false},
	{"several members", func(home, other config.ProjectID) []config.ProjectID { return []config.ProjectID{home, other} }, true},
	{"one other project", func(_, other config.ProjectID) []config.ProjectID { return []config.ProjectID{other} }, true},
}

// TestDesignStartDeliversOneHandoffTitleTurnToTheThreadThatContinues covers
// spec#7: whichever thread continues the design after Start receives exactly
// one chief-of-staff turn naming the handed brief and asking for the started
// work's title. A fake chief answers it with set_status, whose goal the
// started work reports. The turn is delivered before the routine-progress
// window, never reaches a retired local chief, and a start interrupted after
// the request is recorded, completed after a restart and retried delivers no
// second one, even across further restarts.
func TestDesignStartDeliversOneHandoffTitleTurnToTheThreadThatContinues(t *testing.T) {
	t.Parallel()
	for _, which := range handoffTitleTopologies {
		t.Run(which.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newHandoffTitleFixture(t)
			projects := which.members(f.home, f.other)
			opened, err := f.c.OpenDesign(ctx, f.home)
			must(t, err)
			f.origin = opened.Design.Workstream
			r, err := f.s.repository(f.home)
			must(t, err)
			revision := recordDesignProjects(t, r, f.origin, productionBrief, projects)
			before, err := r.ChiefOfStaffThread(f.origin)
			must(t, err)

			// The start fails once its hand-off title request is recorded.
			f.interrupt.Store(true)
			err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			if err == nil {
				// The start ran in a scheduled pass; its failure surfaces there.
				t.Log("start returned before the interruption")
			}
			f.restart(t, "1h")
			if got := f.deliveredTurns(); got != 0 {
				t.Fatalf("a held window delivered %d hand-off turns", got)
			}

			// A retried start and the recovery of the interrupted one complete it.
			r, err = f.s.repository(f.home)
			must(t, err)
			err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
			if err != nil && !failed(err, Conflict) && !failed(err, NotFound) {
				t.Fatalf("retried start: %v", err)
			}
			f.awaitStart(t, r, "single", "complete")

			// The event window closes while the routine-progress window stays open.
			f.restart(t, "100ms")
			deadline := time.Now().Add(demoTimeout)
			for f.deliveredTurns() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the hand-off title turn was not delivered before the routine-progress window")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Once the turn is complete and its notice acknowledged, later
			// restarts and retried starts deliver nothing more.
			for {
				recorded, acknowledged := f.notices(t)
				turns := handoffTitleTurns(f.chiefThread(t, which.adopted))
				if recorded == 1 && acknowledged == 1 && len(turns) == 1 && !turns[0].CompletedAt.IsZero() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the hand-off turn did not settle: %d notices, %d acknowledged, %d turns", recorded, acknowledged, len(turns))
				}
				time.Sleep(10 * time.Millisecond)
			}
			for i := range 2 {
				err = f.c.Do(ctx, "POST", "/v1/design/"+string(f.origin)+"/start", api.DesignStartRequest{BriefRevision: &revision}, nil)
				if err != nil && !failed(err, Conflict) && !failed(err, NotFound) {
					t.Fatalf("retried start %d: %v", i, err)
				}
				f.restart(t, "100ms")
			}
			time.Sleep(time.Second)
			r, err = f.s.repository(f.home)
			must(t, err)

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
			must(t, err)
			if !found || len(selected.Members) != len(projects) {
				t.Fatalf("selected members: %+v", selected)
			}
			if !which.adopted {
				st, err := f.c.Status(ctx, f.origin)
				must(t, err)
				if st.Status == nil || st.Status.Goal != handoffTitleGoal || st.Kind == "design" {
					t.Fatalf("started workstream status: %+v", st)
				}
				return
			}
			if _, err := r.Thread(f.origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the retired local chief remains: %v", err)
			}
			for _, turn := range before.Turns {
				if strings.Contains(turn.Request.Prompt, intake.HandoffTitleOpening) {
					t.Fatal("the hand-off turn reached the local chief before it retired")
				}
			}
			for _, member := range selected.Members {
				repo, err := f.s.repository(member.Repo)
				must(t, err)
				st, err := f.c.Status(ctx, member.Workstream)
				must(t, err)
				if st.Status == nil || st.Status.Goal != handoffTitleGoal {
					t.Fatalf("member %s status: %+v", member.Workstream, st)
				}
				if _, err := repo.Thread(member.Workstream, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("member %s keeps a local chief: %v", member.Workstream, err)
				}
			}
			alias, err := f.c.Status(ctx, f.origin)
			must(t, err)
			if alias.Status == nil || alias.Status.Goal != handoffTitleGoal {
				t.Fatalf("workgroup status through the design address: %+v", alias)
			}
		})
	}
}
