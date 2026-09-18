package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

// The M2 exit demonstration: on an onboarded project, the owner hands in four
// designs. One is debated to consensus after a redraft, one reaches the round
// cap with a charter veto the owner overrules, one is handed in with debate
// skipped and one, also handed in with debate skipped, is abandoned. Each
// ratified plan is sealed with a feature branch on the clone, across a restart
// in the middle of one sealing, and a question a committee member asks during
// the shed is answered through the inbox. See docs/m2-exit.md.

// exitDemoRoles runs every role in a container, the only sandbox the fake
// engine's boundary check accepts, with a committee of two and a cap of two
// rounds.
const exitDemoRoles = `[roles.librarian]
sandbox = "container"
image = "fixture-image"
[roles.committee]
sandbox = "container"
image = "fixture-image"
[roles.chief_of_staff]
sandbox = "container"
image = "fixture-image"
[capacity]
committee = 2
[shed]
max_rounds = 2
`

// The four designs the owner hands in. The fakes tell the workstreams apart
// by the first line of what was handed.
const (
	exitDebated   = "# Resumable uploads\n\nUploads that drop resume where they stopped.\n"
	exitCapped    = "# Upload retention\n\nUploads are kept for thirty days.\n"
	exitSmall     = "# Upload error names the chunk\n\nThe error says which chunk failed.\n"
	exitAbandoned = "# Upload quotas\n\nEach owner has an upload quota.\n"

	exitSmallSpec = "# Upload error names the chunk\n\nA failed upload's error names the chunk it failed on.\n\n## Acceptance criteria\n\n1. The error of a failed upload names the chunk it failed on.\n"
	exitSmallPlan = `{"version": 1, "units": [
  {"id": "message", "title": "Name the chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUploadErrorNamesTheChunk"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
	exitCharter  = "1. Keep changes small.\n2. Every change has a test.\n"
	exitQuestion = "Do deleted uploads count against the retention period?"
	exitRuling   = "No. The period starts at upload and deletion ends it."
	exitRelayed  = "Retention runs from upload until deletion; deleted uploads are not kept."
	exitOverrule = "The reviewer's judgement is enough for retention."
)

// exitDemo holds the fake agents' scripts, keyed by the handed design's first
// line and then by turn name, and what they saw.
type exitDemo struct {
	mu       sync.Mutex
	trace    string
	plays    map[string]map[string]demoTurn
	ran      map[string][]string
	prompts  map[string]string
	streams  map[string]config.WorkstreamID
	problems []string
}

// streamOf returns the workstream a design was handed in as, once the
// hand-in has returned.
func (d *exitDemo) streamOf(design string) (config.WorkstreamID, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stream, ok := d.streams[exitTitle(design)]
	return stream, ok
}

// design names the workstream a turn belongs to by the first line of the
// design handed to it. Every turn's session directory is
// <kind>/<project>/<workstream>/<turn or agent>/<session or turn>.
func (d *exitDemo) design(req agent.Request) (string, error) {
	stream := filepath.Base(filepath.Dir(filepath.Dir(req.SessionDir)))
	data, err := os.ReadFile(filepath.Join(d.trace, "workstreams", stream, "handed", "stdin"))
	if err != nil {
		return "", fmt.Errorf("turn %s of workstream %s: %w", req.Name, stream, err)
	}
	title, _, _ := strings.Cut(string(data), "\n")
	return title, nil
}

func (d *exitDemo) problem(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.problems = append(d.problems, fmt.Sprintf(format, args...))
}

func (d *exitDemo) route(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	title, err := d.design(req)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(req.Name, "mason-") {
		// A building workstream starts its first ready unit. The
		// demonstration ends at ratification, so the mason builds nothing.
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Nothing built", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	d.mu.Lock()
	play := d.plays[title][req.Name]
	d.ran[title] = append(d.ran[title], req.Name)
	d.prompts[title+"/"+req.Name] = req.Prompt
	d.mu.Unlock()
	if play == nil {
		d.problem("unexpected turn %s of %q", req.Name, title)
		return nil, fmt.Errorf("unexpected turn %s", req.Name)
	}
	return play(ctx, req, verified, tools)
}

func (d *exitDemo) runs(title string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ran[title])
}

func exitTitle(design string) string { title, _, _ := strings.Cut(design, "\n"); return title }

// drafts makes an architect turn deliver the given files and answer the given
// objections.
func (d *exitDemo) drafts(files map[string]string, answers map[string]string) demoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, id := range slices.Sorted(maps.Keys(answers)) {
			if recorded, reason, err := shedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": answers[id]}); err != nil || !recorded {
				d.problem("reply to %s: %q %v", id, reason, err)
			}
		}
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			if content, ok := files[path]; ok {
				if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil {
					return nil, fmt.Errorf("deliver %s: %w", path, err)
				}
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// objection is one objection a member's turn raises.
type objection struct {
	kind                     shed.Kind
	part, argument, citation string
}

// debates makes a member's turn raise the given objections and concede the
// given ones.
func (d *exitDemo) debates(raise []objection, concede ...string) demoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, o := range raise {
			if recorded, reason, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(o.kind), "part": o.part, "argument": o.argument, "citations": []string{o.citation}}); err != nil || !recorded {
				d.problem("%s objection on %s: %q %v", o.kind, o.part, reason, err)
			}
		}
		for _, id := range concede {
			if recorded, reason, err := shedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "The redraft settles it."}); err != nil || !recorded {
				d.problem("concede %s: %q %v", id, reason, err)
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Round read", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// exitAttention is what the fake chief of staff puts in front of the owner for
// a packet's recommendation, in words.
func exitAttention(recommendation string) string {
	switch {
	case strings.HasPrefix(recommendation, "do not ratify yet"):
		return "A charter veto blocks the plan: overrule it, sustain it or ask for a redraft."
	case strings.HasPrefix(recommendation, "ratify: nothing blocks"):
		return "Ratify the spec and plan: nothing blocks, and advice stands on the record."
	}
	return "Ratify the spec and plan: no objection stands."
}

// chief is the fake chief of staff. It escalates the committee's question,
// relays the owner's ruling, and makes every packet it is told of the
// attention item of its status.
func (d *exitDemo) chief(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	title, err := d.design(req)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(req.Name, "events_") {
		d.problem("chief of staff turn %s of %q", req.Name, title)
	}
	d.mu.Lock()
	d.ran[title] = append(d.ran[title], "events")
	d.mu.Unlock()
	call := func(name string, args map[string]any) {
		if out, err := callTool(ctx, tools, name, args); err != nil || !strings.Contains(out, `"recorded":true`) && !strings.Contains(out, `"stored":true`) {
			d.problem("%s of %q: %s %v", name, title, out, err)
		}
	}
	if strings.Contains(req.Prompt, "Question 1 is open, asked by the committee: "+exitQuestion) {
		call("escalate", map[string]any{"questions": []string{"1"}, "rephrasing": "Does deletion end an upload's retention period?",
			"blocked": "The committee's reading of the retention design.", "options": []string{"Deletion ends it", "Retention runs thirty days whatever happens"}, "recommendation": "Deletion ends it."})
	}
	if strings.Contains(req.Prompt, "escalation_1 (questions 1): "+exitRuling) {
		call("relay_ruling", map[string]any{"question": "1", "text": exitRelayed, "scope": "local"})
	}
	if _, rest, ok := strings.Cut(req.Prompt, "the recommendation to "); ok {
		recommendation, _, _ := strings.Cut(rest, ".\nPresent it")
		call(status.ToolName, map[string]any{"goal": "Ship " + strings.TrimPrefix(title, "# ") + ".", "attention": exitAttention(recommendation),
			"note": "The ratification packet is ready.", "agents": []string{"The committee has finished its debate."}})
	}
	return &agent.Result{ClaudeID: "session-chief", ResultText: "Noted", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// TestM2HandInToRatifiedPlan demonstrates the M2 exit: see docs/m2-exit.md.
func TestM2HandInToRatifiedPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	home := filepath.Dir(clone)
	root := opts.Config.Root
	configFile, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(exitDemoRoles)
	must(t, errors.Join(err, configFile.Close()))
	// Every backend session can be resumed, so a turn's prompt holds only
	// what is new to its thread.
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	// Production wiring: one engine runs the librarian, the architect, the
	// committee and the chief of staff.
	hosts := &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}
	opts = Enforce(opts, Enforcement{Engine: engine, Hosts: hosts})
	f := &shedFixture{architectFixture: &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}, members: 2}

	// The fake agents.
	d := &exitDemo{plays: map[string]map[string]demoTurn{}, ran: map[string][]string{}, prompts: map[string]string{}}
	veto, size := shed.ObjectionID(1, committeeAgent(1), 1), shed.ObjectionID(1, committeeAgent(1), 2)
	cappedVeto := veto
	chargeVeto := objection{shed.Charter, "spec#2", "Criterion 2 is shown by judgement, not by a test.", "charter#2"}
	quiet := d.debates(nil)
	d.plays[exitTitle(exitDebated)] = map[string]demoTurn{
		"draft-1-1":                          d.drafts(map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil),
		roundTurnID(1, committeeAgent(1), 1): d.debates([]objection{chargeVeto, {shed.Size, "plan#resume", "Resuming is two changes, not one.", "kb/entities.json#internal.trace"}}),
		roundTurnID(1, committeeAgent(2), 1): quiet,
		replyTurnID(1, 1):                    d.drafts(map[string]string{plan.PlanPath: splitPlan}, map[string]string{veto: "Criterion 2 now has TestNoChunkTwice.", size: "Resuming is split in two units."}),
		roundTurnID(2, committeeAgent(1), 1): d.debates(nil, veto, size),
		roundTurnID(2, committeeAgent(2), 1): quiet,
	}
	d.plays[exitTitle(exitCapped)] = map[string]demoTurn{
		"draft-1-1":                          d.drafts(map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil),
		roundTurnID(1, committeeAgent(1), 1): d.debates([]objection{chargeVeto}),
		// The second member asks while the workstream is in the shed, which
		// parks the round until the answer arrives.
		roundTurnID(1, committeeAgent(2), 1): func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if stream, ok := d.streamOf(exitCapped); !ok {
				d.problem("the member asked before its workstream was known")
			} else if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != InShedState {
				d.problem("the member asked with the workstream %q %v, not in the shed", state.Value, err)
			}
			if out, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": exitQuestion}); err != nil || !strings.Contains(out, `"recorded":true`) {
				d.problem("ask: %s %v", out, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 2}, nil
		},
		questions.TurnID("1"):                quiet,
		replyTurnID(1, 1):                    d.drafts(nil, map[string]string{cappedVeto: "The reviewer's judgement is the right proof here."}),
		roundTurnID(2, committeeAgent(1), 1): quiet,
		roundTurnID(2, committeeAgent(2), 1): quiet,
		replyTurnID(2, 1):                    d.drafts(nil, nil),
	}
	d.plays[exitTitle(exitSmall)] = map[string]demoTurn{"draft-1-1": d.drafts(map[string]string{plan.SpecPath: exitSmallSpec, plan.PlanPath: exitSmallPlan}, nil)}
	d.plays[exitTitle(exitAbandoned)] = map[string]demoTurn{"draft-1-1": d.drafts(map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)}
	for _, plays := range d.plays {
		for name := range plays {
			engine.turns[name] = d.route
		}
	}
	engine.turns["*"] = d.chief
	seed, err := kb.Seed(clone)
	must(t, err)
	var librarian []error
	f.script("extract-1-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for path, content := range map[string]string{"output/kb/trace.md": "# trace\n\nState lives in files under the root.\n", "output/kb/entities.json": entitiesWith(t, seed, "history")} {
			if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": path, "content": content}); err != nil {
				librarian = append(librarian, err)
			}
		}
		return errors.Join(librarian...)
	})

	awaitAttention := func(stream config.WorkstreamID, want string) StatusView {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			st, err := f.c.Status(ctx, stream)
			must(t, err)
			if st.Status != nil && st.Status.Attention == want {
				return *st.Status
			}
			if time.Now().After(deadline) {
				t.Fatalf("workstream %s status %+v, want the attention %q", stream, st.Status, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	sealOf := func(stream config.WorkstreamID) seal.Seal {
		t.Helper()
		record, doc, found, err := seal.Latest(f.repository(), stream)
		must(t, err)
		if !found || doc.Revision != 1 || len(f.documents(t, stream, seal.DocumentID)) != 1 {
			t.Fatalf("seal of workstream %s: %v %+v", stream, found, doc)
		}
		return record
	}
	branchAt := func(stream config.WorkstreamID) string {
		t.Helper()
		return strings.TrimSpace(demoGit(t, home, "-C", clone, "rev-parse", "refs/heads/osmia/"+string(stream)))
	}

	// 1. Onboarding. The project is added, the fake librarian writes the
	// knowledge base, and the owner writes the charter after a refused
	// hand-in.
	f.start(t)
	added, err := f.c.AddProject(ctx, request(clone))
	must(t, err)
	f.project, f.trace, d.trace = added.Project.ID, added.Project.Trace, added.Project.Trace
	if x := awaitExtraction(t, f.c); x.State != "succeeded" {
		t.Fatalf("extraction: %+v", x)
	}
	if _, err := f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "refused", Stdin: ptr(exitDebated)}); !failed(err, CharterEmpty) {
		t.Fatalf("a hand-in before the charter: %v", err)
	}
	must(t, os.WriteFile(added.Project.Charter, []byte(exitCharter), 0600))
	onboarded, err := f.c.Configuration(ctx)
	must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || !cs.Ready || cs.Rules != 2 {
		t.Fatalf("charter: %+v", cs)
	}
	entities, err := kb.Load(f.repository())
	must(t, err)
	footprint := entities.ResolveEntities([]string{"internal.trace"})
	if len(footprint.Entities) != 1 || len(footprint.Paths) == 0 || len(footprint.Unresolved) != 0 {
		t.Fatalf("the local entity map resolves internal.trace to %+v", footprint)
	}
	commit := f.upstream(t)

	// 2. Four hand-ins, each drafted by the architect to sketched. The small
	// and the quotas designs are handed in with debate skipped.
	handIn := func(key, design string, skip bool) config.WorkstreamID {
		t.Helper()
		out, err := f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: key, Stdin: ptr(design), SkipDebate: skip})
		must(t, err)
		if out.State != HandedState || out.SkipDebate != skip {
			t.Fatalf("hand-in %+v", out)
		}
		return out.Workstream
	}
	debated, capped := handIn("debated", exitDebated, false), handIn("capped", exitCapped, false)
	small, gone := handIn("small", exitSmall, true), handIn("abandoned", exitAbandoned, true)
	d.mu.Lock()
	d.streams = map[string]config.WorkstreamID{exitTitle(exitDebated): debated, exitTitle(exitCapped): capped, exitTitle(exitSmall): small, exitTitle(exitAbandoned): gone}
	d.mu.Unlock()
	// Once sketched, every workstream enters the shed: two with the
	// committee, two without one because their debate is skipped.
	for _, stream := range []config.WorkstreamID{debated, capped, small, gone} {
		f.awaitFeature(t, stream, InShedState)
		if moved := f.transition(t, stream, SketchedState); moved.From != HandedState || moved.Actor != draftingActor {
			t.Fatalf("the move to sketched %+v", moved)
		}
	}
	for stream, want := range map[config.WorkstreamID]string{
		debated: "spec.md revision 1 and plan.json revision 1 enter the shed with a committee of 2",
		small:   "spec.md revision 1 and plan.json revision 1 enter the shed without a committee: the owner skipped debate",
	} {
		if enter := f.transition(t, stream, InShedState); enter.From != SketchedState || enter.Reason != want {
			t.Fatalf("workstream %s entered the shed %+v", stream, enter)
		}
	}
	list, err := f.c.Statuses(ctx)
	must(t, err)
	if len(list.Workstreams) != 4 {
		t.Fatalf("statuses: %+v", list)
	}
	for _, st := range list.Workstreams {
		if st.State == nil || *st.State != InShedState {
			t.Fatalf("status of %s: %+v", st.Workstream, st)
		}
	}

	// 3. The fourth workstream is abandoned before it is ratified. Its trace
	// stays.
	abandoned, err := f.c.Abandon(ctx, gone, "Quotas wait for the billing work.")
	must(t, err)
	if abandoned.State != AbandonedState || abandoned.Reason != "Quotas wait for the billing work." {
		t.Fatalf("abandon %+v", abandoned)
	}

	// 4. The small workstream skipped debate at hand-in and is ratified
	// explicitly.
	packet := f.awaitPacket(t, small, "ratify: no objection stands")
	if !packet.Skipped || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 {
		t.Fatalf("the skipped debate's packet %+v", packet)
	}
	awaitAttention(small, exitAttention(packet.Recommendation))
	if out, err := f.c.Ratify(ctx, small, 1, 1); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the skipped debate: %+v %v", out, err)
	}
	f.awaitFeature(t, small, BuildingState)
	if s := sealOf(small); s.Base.Commit != commit || s.SpecHash != seal.SpecHash(exitSmallSpec) || len(s.Footprints) != 1 || s.Footprints[0].Unit != "message" || branchAt(small) != commit {
		t.Fatalf("the small workstream's seal %+v", s)
	}
	if ran := slices.DeleteFunc(d.runs(exitTitle(exitSmall)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the small workstream ran %v", ran)
	}

	// 5. A member of the retention workstream's committee asks in round 1.
	// The chief of staff escalates the question, and the owner answers it
	// through the inbox.
	var entry InboxEntry
	deadline := time.Now().Add(demoTimeout)
	for {
		inbox, err := f.c.Inbox(ctx)
		must(t, err)
		if len(inbox.Entries) == 1 {
			entry = inbox.Entries[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbox %+v", inbox)
		}
		time.Sleep(100 * time.Millisecond)
	}
	asker := committeeAgent(2)
	if entry.Number != 1 || entry.Workstream != capped || entry.Batch != "escalation_1" || entry.Question != "Does deletion end an upload's retention period?" ||
		len(entry.Asked) != 1 || entry.Asked[0] != (InboxQuestion{ID: "1", AskedBy: asker, Question: exitQuestion}) {
		t.Fatalf("inbox entry %+v", entry)
	}
	if state, err := f.repository().Workflow(capped, shedSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("the retention shed while the question is open: %+v %v", state, err)
	}
	if _, err := f.c.Answer(ctx, entry.Number, exitRuling); err != nil {
		t.Fatal(err)
	}
	// 6. The ruling reaches the member as its next turn, and round 1 resumes.
	f.awaitShed(t, capped, "heard-1", "concluded-2")
	th, err := f.repository().Thread(capped, asker)
	must(t, err)
	if len(th.Turns) < 2 || th.Turns[1].Request.TurnID != questions.TurnID("1") || th.Turns[1].Status() != "idle" {
		t.Fatalf("the member's thread %+v", th.Turns)
	}
	d.mu.Lock()
	answered := d.prompts[exitTitle(exitCapped)+"/"+questions.TurnID("1")]
	d.mu.Unlock()
	if !strings.Contains(answered, "You asked:\n"+exitQuestion+"\n\nAnswer:\n"+exitRelayed) {
		t.Fatalf("the member heard:\n%s", answered)
	}
	if inbox, err := f.c.Inbox(ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the answer %+v %v", inbox, err)
	}

	// 7. The first workstream: a charter veto and a size objection, a
	// redraft, and the committee concedes in round 2.
	f.awaitShed(t, debated, "concluded-2")
	if moves, want := f.shedMoves(t, debated), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed of the debated workstream went %v, want %v", moves, want)
	}
	if reply := f.reply(t, debated, 1); reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || len(reply.Answers) != 2 {
		t.Fatalf("the architect's reply %+v", reply)
	}
	if end := f.transition(t, debated, "shed-concluded-2"); end.Reason != "debate concluded by consensus after round 2: no objection stands" {
		t.Fatalf("the conclusion %+v", end)
	}
	packet = f.awaitPacket(t, debated, "ratify: no objection stands")
	if packet.Revision != (shed.Pin{Spec: 1, Plan: 2}) || len(packet.Dissent) != 0 {
		t.Fatalf("the debated packet %+v", packet)
	}
	awaitAttention(debated, exitAttention(packet.Recommendation))
	if out, err := f.c.Ratify(ctx, debated, 1, 2); err != nil || out.Sealing != "requested" {
		t.Fatalf("ratifying the debated workstream: %+v %v", out, err)
	}
	f.awaitFeature(t, debated, BuildingState)
	s := sealOf(debated)
	if s.Base != (seal.Base{Remote: "upstream", Branch: "main", Commit: commit}) || s.SpecHash != seal.SpecHash(validSpec) || s.Revision != (shed.Pin{Spec: 1, Plan: 2}) || s.Branch != "osmia/"+string(debated) {
		t.Fatalf("the debated seal %+v", s)
	}
	var units []string
	for _, fp := range s.Footprints {
		units = append(units, fp.Unit)
		if !slices.Equal(fp.Entities, footprint.Entities) || !slices.Equal(fp.Paths, footprint.Paths) {
			t.Fatalf("footprint %+v, want %+v", fp, footprint)
		}
	}
	if !slices.Equal(units, []string{"resume-read", "resume-write", "dedupe"}) || branchAt(debated) != commit {
		t.Fatalf("the debated seal's units %v and branch", units)
	}
	if on := strings.TrimSpace(demoGit(t, home, "-C", s.Workspace, "rev-parse", "--abbrev-ref", "HEAD")); on != s.Branch {
		t.Fatalf("the workspace is on %s", on)
	}

	// 8. The second workstream: the veto still stands at the cap, so
	// ratification is refused until the owner overrules it.
	f.awaitShed(t, capped, "concluded-2")
	if moves := f.shedMoves(t, capped); len(moves) < 4 || !slices.Equal(moves[:4], []string{"round-1", "waiting-1", "round-1", "heard-1"}) {
		t.Fatalf("shed of the retention workstream went %v", moves)
	}
	if end := f.transition(t, capped, "shed-concluded-2"); end.Reason != "debate stopped after round 2, at the shed.max_rounds cap of 2, with 1 objection standing, 1 of them blocking; the cap approves nothing" {
		t.Fatalf("the conclusion %+v", end)
	}
	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", cappedVeto)
	f.awaitPacket(t, capped, blocked)
	awaitAttention(capped, exitAttention(blocked))
	_, err = f.c.Ratify(ctx, capped, 1, 1)
	if !failed(err, Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on spec#2) blocks and has no disposition", cappedVeto, committeeAgent(1))) {
		t.Fatalf("ratifying over the veto: %v", err)
	}
	if _, err := f.c.ShedOverrule(ctx, capped, cappedVeto, exitOverrule); err != nil {
		t.Fatal(err)
	}
	packet = f.awaitPacket(t, capped, "ratify: nothing blocks, and 1 objection stands as advice on the record")
	if len(packet.Dissent) != 1 || packet.Dissent[0].Disposition != shed.Overruled || packet.Dissent[0].Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("the packet after the overrule %+v", packet)
	}
	// The overrule is recorded against the revision the veto was made on.
	rulings := f.documents(t, capped, shed.RulingsDocumentID(2))
	if len(rulings) != 1 || rulings[0].Actor != ownerActor {
		t.Fatalf("the owner's rulings %+v", rulings)
	}
	ruled, err := shed.ParseRulings([]byte(rulings[0].Content))
	must(t, err)
	if ruled.Revision != (shed.Pin{Spec: 1, Plan: 1}) || !slices.Equal(ruled.Rulings, []shed.Ruling{{Objection: cappedVeto, Disposition: shed.Overruled, Note: exitOverrule}}) {
		t.Fatalf("the overrule %+v", ruled)
	}

	// 9. The ratification is sealed, and the service stops once the feature
	// branch exists and before the seal is recorded.
	reached := make(chan struct{})
	lifetime := f.s
	var once sync.Once
	f.s.boundary = func(name string) error {
		if name != "seal-branch-created" {
			return nil
		}
		stop := false
		once.Do(func() { stop = true; close(reached) })
		if !stop {
			return nil
		}
		<-lifetime.lifetime.Done()
		return lifetime.lifetime.Err()
	}
	ratified, err := f.c.Ratify(ctx, capped, 1, 1)
	must(t, err)
	if ratified.Sealing != "requested" || ratified.Detail != "the owner ratified spec.md revision 1 and plan.json revision 1 after round 2, over 1 objection the owner disposed of; the sealing is asked for" {
		t.Fatalf("ratification %+v", ratified)
	}
	select {
	case <-reached:
	case <-time.After(demoTimeout):
		t.Fatal("the sealing never created the feature branch")
	}
	f.stop(t)
	if branchAt(capped) != commit {
		t.Fatal("the feature branch is not where the stopped sealing left it")
	}
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	if _, _, found, err := seal.Latest(repo, capped); err != nil || found {
		t.Fatalf("a seal before the restart: %v %v", found, err)
	}
	must(t, repo.Close())

	// 10. The next service finishes the sealing once, on the branch the
	// clone holds.
	f.start(t)
	defer f.stop(t)
	f.awaitFeature(t, capped, BuildingState)
	record := f.ratification(t, capped, 2)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(record.Dispositions) != 1 || record.Dispositions[0] != (shed.Ruling{Objection: cappedVeto, Disposition: shed.Overruled, Note: exitOverrule}) {
		t.Fatalf("the ratification %+v", record)
	}
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, capped) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	if !slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool {
		return a.Kind == "observe" && a.Observation.Evidence == fmt.Sprintf("the clone has feature branch osmia/%s; the sealing resumes from it", capped)
	}) {
		t.Fatalf("the sealing did not resume from the branch: %+v", ops[0].History)
	}
	if s := sealOf(capped); s.Base.Commit != commit || s.Revision != (shed.Pin{Spec: 1, Plan: 1}) || branchAt(capped) != commit {
		t.Fatalf("the capped seal %+v", s)
	}
	// Unit workspaces, which building opens as units start, are left out.
	if out := demoGit(t, home, "-C", clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ")-strings.Count(out, "worktree "+filepath.Join(f.opts.Config.Root, unitsDirectory)+string(filepath.Separator)) != 4 {
		t.Fatalf("worktrees:\n%s", out)
	}
	// Nothing is pushed: upstream has its main branch alone.
	if refs := demoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(refs) != "refs/heads/main" {
		t.Fatalf("upstream's refs:\n%s", refs)
	}

	// 11. The abandoned workstream ran nothing after its draft and kept its
	// trace; it has no committee and no branch.
	if state, err := f.repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != AbandonedState {
		t.Fatalf("the abandoned workstream %+v %v", state, err)
	}
	if ran := slices.DeleteFunc(d.runs(exitTitle(exitAbandoned)), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the abandoned workstream ran %v", ran)
	}
	if moves := f.shedMoves(t, gone); len(moves) != 0 {
		t.Fatalf("the abandoned workstream's shed went %v", moves)
	}
	if threads, err := f.repository().Threads(gone); err != nil || slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == committeeRole }) {
		t.Fatalf("the abandoned workstream has a committee: %+v %v", threads, err)
	}
	for _, path := range []string{"handed/stdin", plan.SpecPath, plan.PlanPath} {
		demoGit(t, home, "-C", f.trace, "cat-file", "blob", "HEAD:workstreams/"+string(gone)+"/"+path)
	}
	if refs := demoGit(t, home, "-C", clone, "for-each-ref", "--format=%(refname)", "refs/heads/osmia/"+string(gone)); strings.TrimSpace(refs) != "" {
		t.Fatalf("the abandoned workstream has a branch: %s", refs)
	}
	if _, err := os.Stat(filepath.Join(root, "branches", string(f.project), string(gone))); !os.IsNotExist(err) {
		t.Fatalf("the abandoned workstream has a workspace: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(d.problems, "\n"))
	}
}
