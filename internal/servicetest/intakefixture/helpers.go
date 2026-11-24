package intakefixture

import (
	context "context"
	io "io"
	atomic "sync/atomic"

	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"

	reflect "reflect"
	testing "testing"

	intake "github.com/kpenfound/osmia/internal/intake"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	questions "github.com/kpenfound/osmia/internal/questions"

	json "encoding/json"
	time "time"

	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	router "github.com/kpenfound/osmia/internal/router"
	seal "github.com/kpenfound/osmia/internal/seal"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	status "github.com/kpenfound/osmia/internal/status"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	fmt "fmt"

	bundle "github.com/kpenfound/osmia/internal/bundle"

	exec "os/exec"

	fs "io/fs"
	http "net/http"
	os "os"
	slices "slices"
	sync "sync"

	agent "github.com/kpenfound/agentcore/agent"

	regexp "regexp"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"

	errors "errors"

	workspace "github.com/kpenfound/osmia/internal/workspace"

	maps "maps"

	strings "strings"

	filepath "path/filepath"

	shed "github.com/kpenfound/osmia/internal/shed"
	thread "github.com/kpenfound/osmia/internal/thread"
)

// Step runs the amendment controller once on amendment 1 and checks the
// state it leaves it in.
func Step(t *testing.T, a intake.AmendmentDebate, stream config.WorkstreamID, req trace.Amendment, want string) {
	t.Helper()
	fixturekit.Must(t, a.One(context.Background(), stream, req))
	if state, err := a.Repository.Workflow(stream, intake.AmendmentSubject(req.ID)); err != nil || state.Value != want {
		t.Fatalf("amendment %s is %+v, want %s: %v", req.ID, state, want, err)
	}
}

// MasonTurns returns the turns of the unit's mason thread with the given ID.
func MasonTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, turn string) []trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, intake.MasonAgent(unit))
	fixturekit.Must(t, err)
	return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool { return q.Request.TurnID != turn })
}

// UnitMoves returns the transitions of the unit's subject recorded for
// amendment 1.
func UnitMoves(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject != trace.UnitSubject(unit) || !strings.HasSuffix(tr.ID, "_amendment_1")
	})
}

var (
	CheckpointSpec = strings.Replace(fixturekit.ValidSpec, "the last acknowledged chunk", "a durable checkpoint", 1)
	RestartPlan    = strings.Replace(fixturekit.ValidPlan, "TestResume", "TestResumeAfterRestart", 1)
)

// Parks has amendment 1's first attempt deliver the revised spec and ask
// question 1.
func Parks(p *Faults) fixturekit.FakeTurn {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(Delivers(p, plan.SpecPath, CheckpointSpec, intake.DraftValid)(ctx, req, verified, tools), Asks(p, "1")(ctx, req, verified, tools))
	}
}

func AmendmentOp(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == action {
			return op.Operation
		}
	}
	t.Fatalf("missing operation %s", action)
	return coreadapter.Operation{}
}

// AmendmentObjection is a committee record of round 1 of amendment 1 with one
// objection of the given kind.
func AmendmentObjection(kind shed.Kind) shed.Record {
	citation, part := "spec#1", "plan"
	if kind == shed.Charter {
		citation, part = "charter#1", "spec#1"
	}
	member := intake.CommitteeAgent(1)
	return shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: "amend-1-round-1-" + member + "-1",
		Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: kind, Part: part, Argument: "The proposal needs owner attention.", Citations: []string{citation}}}}
}

func ReadDocs(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	all, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var docs []trace.Document
	for _, doc := range all {
		if doc.ID == id {
			docs = append(docs, doc)
		}
	}
	return docs
}

// Delivers makes the architect's turn deliver one draft file, which the tool
// must report with verdict for everything the attempt delivered so far.
func Delivers(p *Faults, path, content, verdict string) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": content})
		var status intake.DraftStatus
		if err == nil {
			err = json.Unmarshal([]byte(got), &status)
		}
		if err != nil || status.Delivered != path || status.Verdict != verdict || status.Next == "" {
			p.Report("deliver %s: %q %v, want verdict %s", path, got, err, verdict)
		}
		return nil
	}
}

// AsksThenWaits makes the architect's turn ask and then run until the service
// cancels it, signalling once it asked.
func AsksThenWaits(p *Faults, asked chan<- struct{}) fixturekit.FakeTurn {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := Asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	}
}

func Sketched(feature, _ string) bool { return feature == intake.SketchedState }

func DraftAt(value string) func(string, string) bool {
	return func(_, draft string) bool { return draft == value }
}

// ReadTool reads a file through the turn's file_read tool.
func ReadTool(ctx context.Context, tools *mcp.ClientSession, path string) (string, error) {
	got, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	var text string
	err = json.Unmarshal([]byte(got), &text)
	return text, err
}

// CheckReadOnlyGrants asserts the grants core verified for a read-only turn:
// the private view read-only, the session read-only with the scratch
// directory the turn starts in, native reading tools and the backend's web
// tools (with the guidance on how to use them) and the scoped MCP server, and
// no VCS or host environment.
func CheckReadOnlyGrants(req agent.Request, verified *agent.Turn, clone string, fail func(string, ...any)) {
	g := req.Grants
	if g == nil || len(g.Mounts) != 3 {
		fail("grants %+v are not a read-only view", g)
		return
	}
	view, scratch := g.Mounts[0].Path, filepath.Join(req.SessionDir, "work")
	if g.Mounts[0].Access != agent.ReadOnly || g.Mounts[1] != (agent.Mount{Path: req.SessionDir, Access: agent.ReadOnly}) || g.Mounts[2] != (agent.Mount{Path: scratch, Access: agent.ReadWrite}) ||
		req.Workspace == nil || req.Workspace.Directory() != scratch || view == clone || strings.HasPrefix(view, clone+string(filepath.Separator)) {
		fail("grants %+v expose more than the read-only private view", g)
	}
	if g.VCS || verified.VCS || !slices.Equal(verified.DeniedExecutables, agent.VCSExecutables) {
		fail("VCS is not denied: %+v", verified)
	}
	if !slices.Equal(g.Tools, []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "mcp__osmia_0"}) || !slices.Equal(verified.Tools, []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch"}) {
		fail("tools %v %v exceed the role grant", g.Tools, verified.Tools)
	}
	if !strings.Contains(req.SystemPrompt, isolation.WebGuidance) {
		fail("system prompt lacks the web guidance: %q", req.SystemPrompt)
	}
	for _, kv := range verified.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key != "HOME" && req.Env[key] != value {
			fail("verified environment exposes %s", key)
		}
	}
}

// BuildTransitions returns, in the order they were recorded, the
// transitions of the build subject, of the unit subjects and the move to
// building.
func BuildTransitions(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, tr := range AllTransitions(t, root, project, stream) {
		if tr.Actor == intake.BuildingActor || tr.Subject == intake.BuildSubject || tr.To == intake.BuildingState {
			out = append(out, tr)
		}
	}
	return out
}

// AllTransitions returns every transition of the workstream, in the order
// they were recorded.
func AllTransitions(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, body := range fixturekit.WorkstreamRecords(t, root, project, stream, "events.jsonl") {
		var tr trace.Transition
		fixturekit.Must(t, json.Unmarshal(body, &tr))
		if tr.Schema == "osmia.trace.transition" && !router.IsJournal(tr.Subject) {
			out = append(out, tr)
		}
	}
	return out
}

// TransitionMove is what one recorded transition says.
type TransitionMove struct{ ID, Subject, From, To, Cause, Reason string }

// Moves returns what the transitions say, checking that the building
// controller recorded each.
func Moves(t *testing.T, transitions []trace.Transition) []TransitionMove {
	t.Helper()
	var out []TransitionMove
	for _, tr := range transitions {
		if tr.Actor != intake.BuildingActor {
			t.Fatalf("transition %s by %+v", tr.ID, tr.Actor)
		}
		out = append(out, TransitionMove{tr.ID, tr.Subject, tr.From, tr.To, tr.Cause, tr.Reason})
	}
	return out
}

const (
	CharterRuling = "Every upload must resume after a restart."
	CharterRule   = "Uploads resume after a restart."
	CharterBefore = "# Charter\n\n1. Keep state in files.\n"
)

var (
	CharterSourcePath = "workstreams/" + string(fixturekit.Stream) + "/questions/1/charter.json"
	CharterRulingPath = "workstreams/" + string(fixturekit.Stream) + "/questions/1/rulings.jsonl"
)

// CharterRevisions returns the recorded revisions of charter.md.
func CharterRevisions(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	fixturekit.Must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.Path == "charter.md" {
			out = append(out, d)
		}
	}
	return out
}

// Appended returns the charter revisions that appended the planted rule.
func Appended(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	var out []trace.Document
	for _, d := range CharterRevisions(t, repo) {
		if d.Cause == CharterSourcePath {
			out = append(out, d)
		}
	}
	return out
}

// Assemble returns the bundle a turn of the workstream receives.
func Assemble(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) bundle.Bundle {
	t.Helper()
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}
	b, err := files.Assemble(context.Background(), fixturekit.Project, bundle.Scope{Workstream: ws})
	fixturekit.Must(t, err)
	return b
}

// CharterEvents returns the bodies of the workstream's outbox events raised
// by the charter proposal's decision or its recording in the charter.
func CharterEvents(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) []string {
	t.Helper()
	entries, err := repo.Outbox(ws)
	fixturekit.Must(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.TransitionID, trace.CharterSubject("1")+"_") {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

// WithRule is the planted charter with the ratified rule appended as number.
func WithRule(before string, number int) string {
	return before + "\n## Standing rulings\n\n" + string(rune('0'+number)) + ". " + CharterRule + " <!-- ratified from " + CharterRulingPath + " revision 1 -->\n"
}

// CheckChartered checks that the planted rule is recorded once, as number in
// the charter content want, that the proposal says so, and that every
// workstream's bundle carries it as one notice.
func CheckChartered(t *testing.T, repo *trace.Repository, want string, number int) {
	t.Helper()
	written := Appended(t, repo)
	if len(written) != 1 || written[0].Content != want || written[0].Actor != intake.CharterActor {
		t.Fatalf("charter revisions appending the rule: %+v", written)
	}
	proposals, err := repo.CharterProposals()
	fixturekit.Must(t, err)
	if len(proposals) != 1 || proposals[0].State.Value != trace.CharterChartered || proposals[0].Proposal.Number != number || proposals[0].Proposal.Charter != written[0].Revision || proposals[0].Proposal.Decision != trace.CharterRatify {
		t.Fatalf("chartered proposal %+v", proposals)
	}
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, Quiet} {
		b := Assemble(t, repo, ws)
		wantNotice := []bundle.CharterNotice{{Source: CharterSourcePath, Workstream: fixturekit.Stream, Record: trace.CharterProposalID("1"), Revision: 3, At: proposals[0].Latest.At,
			Number: number, Rule: CharterRule, Charter: written[0].Revision, Ruling: CharterRulingPath, RulingRevision: 1, OwnerResponse: CharterRuling}}
		if !reflect.DeepEqual(b.CharterNotices, wantNotice) {
			t.Fatalf("charter notices of %s: %+v", ws, b.CharterNotices)
		}
		text := b.Render()
		if strings.Count(text, "<<< osmia:charter_rule | chief of staff proposal, ratified by the owner | bytes=31 >>>\n| "+CharterRule+"\n<<< /osmia:charter_rule >>>\n") != 1 ||
			!strings.Contains(text, "- charter#"+string(rune('0'+number))+" [Standing rulings]: "+CharterRule+"\n") {
			t.Fatalf("bundle of %s:\n%s", ws, text)
		}
	}
	if events := CharterEvents(t, repo, fixturekit.Stream); len(events) != 1 || !strings.HasPrefix(events[0], "Charter rule "+string(rune('0'+number))+" is recorded from the owner's ruling on question 1") {
		t.Fatalf("chief of staff notices %q", events)
	}
}

// Faults collects what the fakes of one test found wrong: an error a fake
// returns fails its turn instead of the test.
type Faults struct {
	Mu   sync.Mutex
	Errs []error
}

func (p *Faults) Report(format string, args ...any) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Errs = append(p.Errs, fmt.Errorf(format, args...))
}

func (p *Faults) Check(t *testing.T) {
	t.Helper()
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if err := errors.Join(p.Errs...); err != nil {
		t.Fatal(err)
	}
}

// Objects makes a member's turn raise one objection.
func Objects(p *Faults, kind shed.Kind, part, citation string) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, reason, err := ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(kind), "part": part, "argument": "It does not hold.", "citations": []string{citation}}); err != nil || !recorded {
			p.Report("%s objection on %s: %q %v", kind, part, reason, err)
		}
		return nil
	}
}

// Concedes makes a member's turn concede its objections.
func Concedes(p *Faults, ids ...string) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, id := range ids {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "The reply settles it."}); err != nil || !recorded {
				p.Report("concede %s: %q %v", id, reason, err)
			}
		}
		return nil
	}
}

func Silent(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil }

// Answers makes the architect's turn answer objections.
func Answers(p *Faults, text string, ids ...string) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, id := range ids {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": text}); err != nil || !recorded {
				p.Report("reply to %s: %q %v", id, reason, err)
			}
		}
		return nil
	}
}

// CheckReplyBoundary makes the assertions from inside the architect's reply
// turn: it reads the round and the draft, answers and redrafts, and holds
// nothing that contributes as a member, writes or runs.
func CheckReplyBoundary(ctx context.Context, p *Faults, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone string) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		p.Report("list tools: %v", err)
		return
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{questions.AskTool, intake.DraftTool, "file_read", "project_context", shed.ReplyTool}; !slices.Equal(names, want) {
		p.Report("role tools %v, want %v", names, want)
	}
	for _, name := range []string{shed.ObjectTool, shed.ConcedeTool, "file_write", "shell", "fetch", "git_push", "notes_write", "set_status"} {
		if _, err := fixturekit.CallTool(ctx, tools, name, map[string]any{"kind": "fit", "part": "spec", "argument": "x", "citations": []string{"charter#1"}, "objection": "x", "reason": "y"}); err == nil {
			p.Report("the architect called %s", name)
		}
	}
	for path, want := range map[string]string{"spec.md": fixturekit.ValidSpec, "plan.json": fixturekit.ValidPlan, "handed/stdin": fixturekit.HandedDesign, "charter.md": ShedCharter} {
		if got, err := ReadTool(ctx, tools, path); err != nil || got != want {
			p.Report("read %s: %q %v", path, got, err)
		}
	}
	for _, path := range []string{"repo/CODEOWNERS", "redraft/plan.json", ".git/HEAD", filepath.Join(clone, "CODEOWNERS")} {
		if _, err := ReadTool(ctx, tools, path); err == nil {
			p.Report("the architect read %s", path)
		}
	}
	CheckReadOnlyGrants(req, verified, clone, p.Report)
	if req.Profile.VCSAccess || req.Profile.Name != intake.ArchitectRole {
		p.Report("request carries VCS access or another role: %+v", req.Profile)
	}
}

// RecordDesignDraft writes a draft document the same way draft_brief's tool
// handler does, without running a real agent turn.
func RecordDesignDraft(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, content string) error {
	t.Helper()
	return RecordDesignDraftRevision(t, repository, stream, 1, content)
}

// RecordDesignDraftRevision records the revision of the stream's drafted
// brief, as the draft_brief tool does.
func RecordDesignDraftRevision(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, revision int, content string) error {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: intake.DesignDraftDocument, Revision: revision,
		Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: intake.ChiefActor, Cause: intake.DraftBriefToolName}
	data, err := json.Marshal(intake.DesignBriefRecord{Content: content})
	if err != nil {
		return err
	}
	return repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(data)}})
}

// ApiError checks that err is the service's error with the code and message.
func ApiError(t *testing.T, err error, code api.Code, message string) {
	t.Helper()
	var api *api.APIError
	if !errors.As(err, &api) || api.Code != code || api.Message != message {
		t.Fatalf("want %s %q, got %v", code, message, err)
	}
}

func DecideWith(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	fixturekit.Must(t, err)
	return string(out)
}

const (
	ExitDebated   = "# Resumable uploads\n\nUploads that drop resume where they stopped.\n"
	ExitCapped    = "# Upload retention\n\nUploads are kept for thirty days.\n"
	ExitSmall     = "# Upload error names the chunk\n\nThe error says which chunk failed.\n"
	ExitAbandoned = "# Upload quotas\n\nEach owner has an upload quota.\n"

	ExitSmallSpec = "# Upload error names the chunk\n\nA failed upload's error names the chunk it failed on.\n\n## Acceptance criteria\n\n1. The error of a failed upload names the chunk it failed on.\n"
	ExitSmallPlan = `{"version": 1, "units": [
  {"id": "message", "title": "Name the chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUploadErrorNamesTheChunk"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
	ExitCharter  = "1. Keep changes small.\n2. Every change has a test.\n"
	ExitQuestion = "Do deleted uploads count against the retention period?"
	ExitRuling   = "No. The period starts at upload and deletion ends it."
	ExitRelayed  = "Retention runs from upload until deletion; deleted uploads are not kept."
	ExitOverrule = "The reviewer's judgement is enough for retention."
)

// ExitDemo holds the fake agents' scripts, keyed by the handed design's first
// line and then by turn name, and what they saw.
type ExitDemo struct {
	Mu       sync.Mutex
	Trace    string
	Root     config.Root
	Project  config.ProjectID
	Plays    map[string]map[string]fixturekit.DemoTurn
	Ran      map[string][]string
	Prompts  map[string]string
	Streams  map[string]config.WorkstreamID
	Problems []string
}

// StreamOf returns the workstream a design was handed in as, once the
// hand-in has returned.
func (d *ExitDemo) StreamOf(design string) (config.WorkstreamID, bool) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	stream, ok := d.Streams[ExitTitle(design)]
	return stream, ok
}

// Design names the workstream a turn belongs to by the first line of the
// design handed to it. Every turn's session directory is
// <kind>/<project>/<workstream>/<turn or agent>/<session or turn>.
func (d *ExitDemo) Design(req agent.Request) (string, error) {
	stream := filepath.Base(filepath.Dir(filepath.Dir(req.SessionDir)))
	dir, err := d.Root.WorkstreamTrace(d.Project, config.WorkstreamID(stream))
	if err != nil {
		return "", err
	}
	data, err := fixturekit.LatestDocumentIn(dir, "workstreams/"+stream+"/documents.jsonl", "handed/stdin")
	if err != nil {
		return "", fmt.Errorf("turn %s of workstream %s: %w", req.Name, stream, err)
	}
	title, _, _ := strings.Cut(string(data), "\n")
	return title, nil
}

func (d *ExitDemo) Problem(format string, args ...any) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.Problems = append(d.Problems, fmt.Sprintf(format, args...))
}

func (d *ExitDemo) Route(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	title, err := d.Design(req)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(req.Name, "mason-") {

		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Nothing built", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	d.Mu.Lock()
	play := d.Plays[title][req.Name]
	d.Ran[title] = append(d.Ran[title], req.Name)
	d.Prompts[title+"/"+req.Name] = req.Prompt
	d.Mu.Unlock()
	if play == nil {
		d.Problem("unexpected turn %s of %q", req.Name, title)
		return nil, fmt.Errorf("unexpected turn %s", req.Name)
	}
	return play(ctx, req, verified, tools)
}

func (d *ExitDemo) Runs(title string) []string {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	return slices.Clone(d.Ran[title])
}

func ExitTitle(design string) string { title, _, _ := strings.Cut(design, "\n"); return title }

// Drafts makes an architect turn deliver the given files and answer the given
// objections.
func (d *ExitDemo) Drafts(files map[string]string, answers map[string]string) fixturekit.DemoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, id := range slices.Sorted(maps.Keys(answers)) {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": answers[id]}); err != nil || !recorded {
				d.Problem("reply to %s: %q %v", id, reason, err)
			}
		}
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			if content, ok := files[path]; ok {
				if _, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": content}); err != nil {
					return nil, fmt.Errorf("deliver %s: %w", path, err)
				}
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// Objection is one objection a member's turn raises.
type Objection struct {
	Kind                     shed.Kind
	Part, Argument, Citation string
}

// Debates makes a member's turn raise the given objections and concede the
// given ones.
func (d *ExitDemo) Debates(raise []Objection, concede ...string) fixturekit.DemoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, o := range raise {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(o.Kind), "part": o.Part, "argument": o.Argument, "citations": []string{o.Citation}}); err != nil || !recorded {
				d.Problem("%s objection on %s: %q %v", o.Kind, o.Part, reason, err)
			}
		}
		for _, id := range concede {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ConcedeTool, map[string]any{"objection": id, "reason": "The redraft settles it."}); err != nil || !recorded {
				d.Problem("concede %s: %q %v", id, reason, err)
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Round read", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// ExitAttention is what the fake chief of staff puts in front of the owner for
// a packet's recommendation, in words.
func ExitAttention(recommendation string) string {
	switch {
	case strings.HasPrefix(recommendation, "do not ratify yet"):
		return "A charter veto blocks the plan: overrule it, sustain it or ask for a redraft."
	case strings.HasPrefix(recommendation, "ratify: nothing blocks"):
		return "Ratify the spec and plan: nothing blocks, and advice stands on the record."
	}
	return "Ratify the spec and plan: no objection stands."
}

// Chief is the fake chief of staff. It escalates the committee's question,
// relays the owner's ruling, and makes every packet it is told of the
// attention item of its status.
func (d *ExitDemo) Chief(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	title, err := d.Design(req)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(req.Name, "mason-") {
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Nothing built", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	if !strings.HasPrefix(req.Name, "events_") {
		d.Problem("chief of staff turn %s of %q", req.Name, title)
	}
	d.Mu.Lock()
	d.Ran[title] = append(d.Ran[title], "events")
	d.Mu.Unlock()
	call := func(name string, args map[string]any) {
		if out, err := fixturekit.CallTool(ctx, tools, name, args); err != nil || !strings.Contains(out, `"recorded":true`) && !strings.Contains(out, `"stored":true`) {
			d.Problem("%s of %q: %s %v", name, title, out, err)
		}
	}
	if strings.Contains(req.Prompt, "Question 1 is open, asked by the committee: "+ExitQuestion) {
		call("escalate", map[string]any{"questions": []string{"1"}, "rephrasing": "Does deletion end an upload's retention period?",
			"blocked": "The committee's reading of the retention design.", "options": []string{"Deletion ends it", "Retention runs thirty days whatever happens"}, "recommendation": "Deletion ends it."})
	}
	if strings.Contains(req.Prompt, "escalation_1 (questions 1): "+ExitRuling) {
		call("relay_ruling", map[string]any{"question": "1", "text": ExitRelayed, "scope": "local"})
	}
	if _, rest, ok := strings.Cut(req.Prompt, "the recommendation to "); ok {
		recommendation, _, _ := strings.Cut(rest, ".\nPresent it")
		call(status.ToolName, map[string]any{"goal": "Ship " + strings.TrimPrefix(title, "# ") + ".", "attention": ExitAttention(recommendation),
			"note": "The ratification packet is ready.", "agents": []string{"The committee has finished its debate."}})
	}
	return &agent.Result{ClaudeID: "session-chief", ResultText: "Noted", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

const DoneTool = "done"

// ToolNames lists the tools a fake agent's session offers, sorted.
func ToolNames(ctx context.Context, tools *mcp.ClientSession) ([]string, error) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names, nil
}

func QuestionResult(req agent.Request, id, text string) *agent.Result {
	return &agent.Result{ClaudeID: id, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}
}

const (
	AskedQuestion = "Does the store keep acknowledged chunks across a restart?"
	AskedAnswer   = "Yes: the store is a file under the root, and files survive restarts."
	OwnerRuling   = "Yes, the store is durable; do not object on that account."
	RelayedRuling = "The store keeps acknowledged chunks across a restart: it is durable."
	ChiefRole     = "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"
	PinOne        = "spec.md revision 1 and plan.json revision 1"
)

var (
	OpenQuestion = regexp.MustCompile(`Question (\d+) is open, asked by the (?:committee|architect|mason|reviewer): `)
	OwnerRuled   = regexp.MustCompile(`The owner ruled on inbox entry \d+, \S+ \(questions ([^)]+)\): `)
)

// Chief is the fake chief of staff of the asking fixtures. A committee
// question it was told to release it answers from the record, citing the
// charter; one it was told to hold it answers the same way and then keeps the
// turn open until the hold closes or the turn is cancelled; any other it
// escalates to the owner, one entry per question, and relays the owner's
// ruling with local scope. Every other event it notes and leaves.
type Chief struct {
	P        *Faults
	Mu       sync.Mutex
	Released map[string]bool
	Held     map[string]chan struct{}
	Routes   map[string]map[string]any
}

// Route has the chief of staff route question id to an amendment with the
// given citations, change and reason.
func (c *Chief) Route(id string, citations []string, change, reason string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.Routes == nil {
		c.Routes = map[string]map[string]any{}
	}
	c.Routes[id] = map[string]any{"question": id, "citations": citations, "change": change, "reason": reason}
}

// Release has the chief of staff answer question id from the record.
func (c *Chief) Release(id string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Released[id] = true
}

// Hold has the chief of staff answer question id from the record and keep
// its turn open until the returned channel closes.
func (c *Chief) Hold(id string) chan struct{} {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Held[id] = make(chan struct{})
	return c.Held[id]
}

func (c *Chief) Turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {

	if strings.HasPrefix(req.Name, intake.ReviewerRole+"-") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.Mu.Lock()
	released, held, routes := maps.Clone(c.Released), maps.Clone(c.Held), maps.Clone(c.Routes)
	c.Mu.Unlock()
	for _, m := range OpenQuestion.FindAllStringSubmatch(req.Prompt, -1) {
		id := m[1]
		switch {
		case routes[id] != nil:
			c.Choose(ctx, tools, questions.RouteAmendmentTool, routes[id])
		case released[id] || held[id] != nil:
			c.Choose(ctx, tools, questions.AnswerTool, map[string]any{"question": id, "text": AskedAnswer, "citations": []string{"charter#1"}})
		default:
			c.Choose(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{id}, "rephrasing": "Is the store durable across restarts?", "blocked": "The committee's round.", "options": []string{"Yes", "No"}, "recommendation": "Yes."})
		}
		if hold := held[id]; hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	for _, m := range OwnerRuled.FindAllStringSubmatch(req.Prompt, -1) {
		c.Choose(ctx, tools, questions.RelayRulingTool, map[string]any{"question": strings.Split(m[1], ", ")[0], "text": RelayedRuling, "scope": "local"})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return QuestionResult(req, "session-chief", "Chosen"), nil
}

// Choose calls one question tool and reports a refusal, except one saying the
// question was chosen for already: a prompt replays earlier events.
func (c *Chief) Choose(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) {
	got, err := fixturekit.CallTool(ctx, tools, name, args)
	if err != nil {

		if ctx.Err() != nil {
			return
		}
		c.P.Report("%s: %v", name, err)
		return
	}
	var result struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		c.P.Report("%s returned %q", name, got)
		return
	}
	if !result.Recorded && !strings.Contains(result.Reason, "is already answered") && !strings.Contains(result.Reason, "is escalated to the owner") && !strings.Contains(result.Reason, "has the owner's ruling") && !strings.Contains(result.Reason, "was routed to an amendment") {
		c.P.Report("%s refused: %s", name, result.Reason)
	}
}

// Asks makes a member's turn ask the question that gets number id and check
// the tool's answer.
func Asks(p *Faults, id string) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": AskedQuestion})
		if err != nil || got != `{"recorded":true,"question":"`+id+`","next":"End your turn now. The answer arrives as your next turn on this thread."}` {
			p.Report("ask: %q %v", got, err)
		}
		if got, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "And another?"}); err != nil || !strings.Contains(got, "this turn already asked question "+id) {
			p.Report("a second question in the turn: %q %v", got, err)
		}
		return nil
	}
}

// Results renders acknowledged operations with their results, which %v
// prints as pointers.
func Results(ops []trace.OperationRecord) []string {
	var out []string
	for _, o := range ops {
		result := "no result"
		if o.Result != nil {
			result = fmt.Sprintf("%+v", *o.Result)
		}
		out = append(out, fmt.Sprintf("%s acknowledged %t: %s", o.Operation.ID, o.Acknowledged, result))
	}
	return out
}

// Held makes a member's turn report that it started and wait to be released,
// so the owner can act while the round runs.
func Held(started chan<- struct{}, release <-chan struct{}) fixturekit.FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func AwaitStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the member's turn did not start")
	}
}

// Failed reports whether err is an API error with that code.
func Failed(err error, code api.Code) bool {
	var api *api.APIError
	return errors.As(err, &api) && api.Code == code
}

const ShedCharter = "1. Keep changes small.\n2. Every change has a test.\n"

// tool calls object or concede and returns whether it was recorded, and the
// objection ID or the reason.
func ShedTool(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) (bool, string, error) {
	got, err := fixturekit.CallTool(ctx, tools, name, args)
	if err != nil {
		return false, "", err
	}
	var result struct {
		Recorded  bool   `json:"recorded"`
		Objection string `json:"objection"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		return false, "", err
	}
	if result.Recorded {
		return true, result.Objection, nil
	}
	return false, result.Reason, nil
}

// Barrier holds every member's turn until all of them are running, so a
// round that ran its members one after another never finishes.
type Barrier struct {
	Mu      sync.Mutex
	Waiting int
	Want    int
	Open    chan struct{}
}

func NewBarrier(n int) *Barrier { return &Barrier{Want: n, Open: make(chan struct{})} }

func (b *Barrier) Wait(ctx context.Context) error {
	b.Mu.Lock()
	b.Waiting++
	if b.Waiting == b.Want {
		close(b.Open)
	}
	b.Mu.Unlock()
	select {
	case <-b.Open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(fixturekit.DemoTimeout):
		return errors.New("the other members never started: the round does not run them in parallel")
	}
}

// CheckCommitteeBoundary makes the negative assertions from inside a member's
// turn: the role reads files, contributes and asks, and nothing carries
// notes, write, execute, network or VCS access.
func CheckCommitteeBoundary(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone string) error {
	var problems []error
	fail := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{questions.AskTool, shed.ConcedeTool, "file_read", shed.ObjectTool, "project_context"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	for _, name := range []string{"file_write", "shell", "exec", "fetch", "git_push", intake.DraftTool, "set_status", "notes_read", "notes_write", "answer", "escalate", shed.ReplyTool} {
		if _, err := fixturekit.CallTool(ctx, tools, name, map[string]any{"path": "x", "content": "y"}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for path, want := range map[string]string{"spec.md": fixturekit.ValidSpec, "plan.json": fixturekit.ValidPlan, "handed/stdin": fixturekit.HandedDesign, "charter.md": ShedCharter, "repo/CODEOWNERS": "/internal/ @core\n", "repo/internal/trace/git.go": "package trace\n"} {
		if got, err := ReadTool(ctx, tools, path); err != nil || got != want {
			fail("read %s: %q %v", path, got, err)
		}
	}
	if bundle, err := ReadTool(ctx, tools, "context.md"); err != nil || !strings.Contains(bundle, "- charter#2: Every change has a test.") || !strings.Contains(bundle, "project_context") {
		fail("context.md: %q %v", bundle, err)
	}
	for _, path := range []string{"repo/secret.env", "secret.env", "repo/.git/HEAD", ".git/HEAD", "../config.toml", filepath.Join(clone, "CODEOWNERS"), "kb/entities.json", "shed/round-1/" + intake.CommitteeAgent(2) + ".json"} {
		if _, err := ReadTool(ctx, tools, path); err == nil {
			fail("runtime read %s", path)
		}
	}
	CheckReadOnlyGrants(req, verified, clone, fail)
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || req.Profile.Name != intake.CommitteeRole {
		fail("request carries VCS access or another role: %+v", req.Profile)
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, fixturekit.DemoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, want := range []string{"Round 1 of the shed", "spec.md revision 1 and plan.json revision 1", "charter#<n>", "spec#<n>", "plan#<unit>", "kb/entities.json#<entity>", "veto", "advice", "split", "acceptance", shed.ObjectTool, "Call " + questions.AskTool + " when something you must know"} {
		if !strings.Contains(req.Prompt, want) {
			fail("prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, "still stand") {
		fail("round 1 lists earlier objections:\n%s", req.Prompt)
	}
	if !strings.Contains(req.SystemPrompt, "member of the committee") || !strings.Contains(req.SystemPrompt, "no tool that writes or runs") || !strings.Contains(req.SystemPrompt, "call "+questions.AskTool+": the round waits for the answer") {
		fail("system prompt: %q", req.SystemPrompt)
	}
	return errors.Join(problems...)
}

// Asking wraps the fake masons' turn: in the workstream stream, or in every
// workstream when stream is empty, the turn asks the question that gets
// number id before it writes its file.
func (m *FakeMasons) Asking(p *Faults, stream config.WorkstreamID, id string) func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if stream == "" || strings.Contains(filepath.ToSlash(req.SessionDir), "/"+string(stream)+"/") {
			if err := Asks(p, id)(ctx, req, verified, tools); err != nil {
				return nil, err
			}
		}
		return m.Turn(ctx, req, verified, tools)
	}
}

// AwaitPasses waits until passed, which a fixture advances once per completed
// reconciliation pass, has counted at least n more passes than when the call
// began, then returns the count. It fails at the deadline with the last count.
func AwaitPasses(t *testing.T, passed *atomic.Int64, n int64) int64 {
	t.Helper()
	target := passed.Load() + n
	wake, release := fixturekit.Ticks(50 * time.Millisecond)
	defer release()
	fixturekit.Until(t, fmt.Sprintf("%d reconciliation passes", target), wake, func() bool { return passed.Load() >= target })
	return passed.Load()
}

// AwaitTurn waits until the i-th turn of the agent's thread completed.
func AwaitTurn(t *testing.T, repository func() *trace.Repository, stream config.WorkstreamID, agent string, i int) trace.QueuedTurn {
	t.Helper()
	var q trace.QueuedTurn
	Eventually(t, fmt.Sprintf("turn %d of %s did not complete", i, agent), func() bool {
		th, err := repository().Thread(stream, agent)
		if err != nil || len(th.Turns) <= i || th.Turns[i].CompletedAt.IsZero() {
			return false
		}
		q = th.Turns[i]
		return true
	})
	return q
}

// ChangeRaceGate stops only the selected production boundary after approval.
type ChangeRaceGate struct {
	Step        string
	Armed       atomic.Bool
	Reached     chan struct{}
	Released    chan struct{}
	Once        sync.Once
	ReleaseOnce sync.Once
}

// ChiefEvent reports whether e is an event the chief of staff receives as
// information: a notice or routine progress.
func ChiefEvent(e trace.Event) bool {
	return e.Kind == trace.NoticeKind || e.Kind == trace.ProgressKind
}

// Deliver accepts an owner message and publishes the intent to run it.
func Deliver(t *testing.T, repo *trace.Repository, turn string, version uint64, at time.Time) trace.TurnRequest {
	t.Helper()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: owner, Cause: "message_" + turn, Depth: 1},
		AgentID: fixturekit.DemoAgent, ThreadID: fixturekit.DemoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the mason.", Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	fixturekit.Must(t, err)
	event := trace.EventID("deliver_"+turn, "turn")
	op, err := thread.TurnOperation(fixturekit.Project, event, thread.TurnInput{Workstream: fixturekit.Stream, Agent: fixturekit.DemoAgent, Turn: turn})
	fixturekit.Must(t, err)
	from := ""
	if version > 0 {
		from = "delivering"
	}
	tx := trace.Transaction{ExpectedVersion: version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "deliver_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: trace.Actor{Kind: "service", ID: "inbox"}, Cause: req.ID, Depth: 1},
		Subject: "mason_inbox", From: from, To: "delivering", Reason: "Owner message accepted for the mason thread"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver " + turn, Operation: &op}}}
	_, err = repo.Transact(context.Background(), tx)
	fixturekit.Must(t, err)
	return req
}

// DocumentCommit returns the commit that recorded the latest revision of the
// document at path in stream's trace.
func DocumentCommit(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, path string) string {
	t.Helper()
	rows := fixturekit.TraceQuery(t, fixturekit.WorkstreamTraceDirectory(t, root, project, stream), "SELECT CAST(commit_seq AS TEXT) FROM records WHERE path = ? AND schema = 'osmia.trace.document' AND json_extract(body, '$.path') = ? ORDER BY seq DESC LIMIT 1", "workstreams/"+string(stream)+"/documents.jsonl", path)
	if len(rows) != 1 {
		t.Fatalf("workstream %s records no %s", stream, path)
	}
	return string(rows[0])
}

// Done calls the done tool and returns whether the service accepted the
// report, and its reason when it did not.
func Done(ctx context.Context, tools *mcp.ClientSession, args map[string]any) (bool, string, error) {
	for key, value := range map[string]string{"headline": ExampleCard.Headline, "happened": ExampleCard.Happened, "needs_you": ExampleCard.NeedsYou} {
		if _, ok := args[key]; !ok {
			args[key] = value
		}
	}
	text, err := fixturekit.CallTool(ctx, tools, DoneTool, args)
	if err != nil {
		return false, err.Error(), nil
	}
	var out struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
		Next     string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return false, "", fmt.Errorf("done returned %q: %w", text, err)
	}
	return out.Recorded, out.Reason, nil
}

// ErrCrashTurn is what a fake mason's play returns to end its turn with an
// infrastructure failure, which the runner retries.
var ErrCrashTurn = errors.New("the turn crashes")

// ErrFailTurn is what a fake mason's play returns to end its turn with a
// behavioural failure: it reports an outcome no mason may report, so the turn
// is not retried.
var ErrFailTurn = errors.New("the turn fails")

// Eventually polls cond until it holds, failing with what after DemoTimeout.
func Eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	wake, release := fixturekit.Ticks(50 * time.Millisecond)
	defer release()
	fixturekit.Until(t, what, wake, cond)
}

// Exchange sends one raw request through client and returns its status and
// body.
func Exchange(t *testing.T, client *http.Client, method, url, contentType, body string, host string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	fixturekit.Must(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	fixturekit.Must(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	fixturekit.Must(t, err)
	return resp.StatusCode, data
}

// FakeMasons is a fake model session standing in for the real mason's model
// backend: it never calls out to a live model, so a mason's own reasoning,
// cost and latency are left unverified. Each turn checks that it holds its
// view's file tools, ask and done, and no amend, records what it saw,
// writes MasonWrote into its view and then plays what play holds for the
// turn, which ends the turn failed by returning ErrFailTurn or ErrCrashTurn.
type FakeMasons struct {
	Mu       sync.Mutex
	Runs     map[string][]agent.Request
	Problems []string
	Play     map[string]func(context.Context, agent.Request, *mcp.ClientSession) error
	Response map[string]string
	Chief    *Chief
}

// Handle runs a tool's handler with input and returns its result.
func Handle(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	fixturekit.Must(t, err)
	return string(out)
}

// NoticeOf returns the notice the transition raised for the chief of staff.
func NoticeOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	fixturekit.Must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && ChiefEvent(entry.Event) {
			return entry.Event.Body
		}
	}
	t.Fatalf("transition %s told the chief of staff nothing", transition)
	return ""
}

// ProviderOf returns the provider of the workstream's workspaces.
func ProviderOf(t *testing.T, w intake.StreamWorkspaces, stream config.WorkstreamID) workspace.Provider {
	t.Helper()
	g, err := w.Of(stream)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// RecordSpec records content as the owner's next revision of stream's spec
// directly in its trace, whatever the workstream's state.
func RecordSpec(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, content string) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var latest *trace.Document
	for i := range docs {
		if docs[i].Path == "spec.md" {
			latest = &docs[i]
		}
	}
	if latest == nil {
		t.Fatalf("workstream %s records no spec", stream)
	}
	next := *latest
	next.Revision++
	next.Content, next.At, next.Actor, next.Cause, next.Depth = content, latest.At.Add(time.Second), trace.Actor{Kind: "owner", ID: "local"}, "owner-edit", 0
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{next}))
}

// Requests returns the mason turns that ran in the workstream.
func (m *FakeMasons) Requests(stream config.WorkstreamID) []agent.Request {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return slices.Clone(m.Runs[string(stream)])
}

// RequireJJ skips the test when jj is not on PATH, and fails it there when
// OSMIA_REQUIRE_JJ says jj must be, as in the dagger test containers.
func RequireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed; Jujutsu workspaces need it")
	}
}

// Retries counts the retries recorded for the operation.
func Retries(op trace.OperationRecord) int {
	n := 0
	for _, a := range op.History {
		if a.Kind == "retry" {
			n++
		}
	}
	return n
}

// SealOf is a seal of one unit per footprint, resolved against the map.
func SealOf(t *testing.T, k int, m kb.Map, footprints ...[]string) seal.Seal {
	t.Helper()
	var p plan.Plan
	for i, names := range footprints {
		p.Units = append(p.Units, plan.Unit{ID: fmt.Sprintf("u%d", i+1), Footprint: names})
	}
	resolved, unresolved := seal.Take(p, m)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved %v", unresolved)
	}
	return seal.Seal{Seal: k, Footprints: resolved}
}

// Seals returns every recorded revision of seal.json.
func Seals(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []seal.Seal {
	t.Helper()
	var out []seal.Seal
	for _, d := range StreamDocuments(t, repository, stream, seal.DocumentID) {
		s, err := seal.Parse([]byte(d.Content))
		fixturekit.Must(t, err)
		out = append(out, s)
	}
	return out
}

// SessionStream returns the workstream of a thread turn, from its session
// directory threads/<project>/<workstream>/...
func SessionStream(req agent.Request) string {
	parts := strings.Split(filepath.ToSlash(req.SessionDir), "/")
	if i := slices.Index(parts, "threads"); i >= 0 && i+2 < len(parts) {
		return parts[i+2]
	}
	return ""
}

// Snapshot reads every file below dir, including VCS metadata.
func Snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	fixturekit.Must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

// Soon polls ok until it holds, failing with what after DemoTimeout.
func Soon(t *testing.T, what string, ok func() bool) {
	t.Helper()
	wake, release := fixturekit.Ticks(5 * time.Millisecond)
	defer release()
	fixturekit.Until(t, what, wake, ok)
}

// StreamDocuments returns the recorded revisions of one workstream document.
func StreamDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != id })
}

// TransitionByID returns the workstream's transition with the given ID.
func TransitionByID(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatalf("workstream %s has no transition %s", stream, id)
	}
	return transitions[i]
}

// Turn is the ID queue gives a project's turn.
func Turn(id config.ProjectID, name string) string { return string(id[len(id)-4:]) + "-" + name }

const (
	ChiefAgent  = "agent_chief"
	ChiefThread = "thread_chief"
	Quiet       = config.WorkstreamID("w_fedcba9876543210fedcba9876543210")
)

func (g *ChangeRaceGate) Await(t *testing.T) {
	t.Helper()
	select {
	case <-g.Reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("production checkpoint %s not reached", g.Step)
	}
}

func (g *ChangeRaceGate) Checkpoint(ctx context.Context, step string) error {
	if !g.Armed.Load() || step != g.Step {
		return nil
	}
	g.Once.Do(func() { close(g.Reached) })
	select {
	case <-g.Released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *ChangeRaceGate) Release() { g.ReleaseOnce.Do(func() { close(g.Released) }) }

func (m *FakeMasons) Check(t *testing.T) {
	t.Helper()
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if len(m.Problems) != 0 {
		t.Fatal(strings.Join(m.Problems, "\n"))
	}
}

func (m *FakeMasons) Turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if slices.Sort(names); !slices.Equal(names, []string{"ask", DoneTool, "factory_context", "file_read", "file_write", "project_context", "record_discovery"}) {
		m.Problems = append(m.Problems, fmt.Sprintf("mason tools %v", names))
	}
	view := req.Workspace.Directory()
	if err := os.WriteFile(filepath.Join(view, fixturekit.MasonWrote), []byte("package trace\n"), 0644); err != nil {
		m.Problems = append(m.Problems, err.Error())
	}
	stream := SessionStream(req)
	m.Runs[stream] = append(m.Runs[stream], req)
	if play := m.Play[req.Name]; play != nil {
		if err := play(ctx, req, tools); errors.Is(err, ErrFailTurn) {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reported", SessionDir: req.SessionDir, NumTurns: 1, HasOutcome: true, Outcome: agent.Outcome{Status: "shipped"}}, nil
		} else if errors.Is(err, ErrCrashTurn) {
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Crashed", SessionDir: req.SessionDir, NumTurns: 1, IsError: true, ErrorSubtype: "execution"}, nil
		} else if err != nil {
			m.Problems = append(m.Problems, fmt.Sprintf("%s: %v", req.Name, err))
		}
	}
	response := "Built"
	if text, ok := m.Response[req.Name]; ok {
		response = text
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: response, SessionDir: req.SessionDir, NumTurns: 2}, nil
}

func (w ProductionWorkerWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if ProductionWorker(req.Scope) {
		return intake.StagedWorkspaces{}.Acquire(ctx, req)
	}
	return w.Production.Acquire(ctx, req)
}

func AcceptancePlan(t *testing.T, graph string, items ...string) string {
	t.Helper()
	p, err := plan.Parse([]byte(graph))
	fixturekit.Must(t, err)
	p.Units[0].Acceptance = items
	data, err := plan.Encode(p)
	fixturekit.Must(t, err)
	return string(data)
}

func Await(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

func EqualJSONTest(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func ErrorCode(t *testing.T, body []byte) api.Code {
	t.Helper()
	var out api.ErrorResponse
	fixturekit.Must(t, json.Unmarshal(body, &out))
	return out.Error.Code
}

func FailMasonTurn(context.Context, agent.Request, *mcp.ClientSession) error {
	return ErrFailTurn
}

func HasDiagnostic(ds []api.Diagnostic, code api.Code) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func Parked(unit, q string) TransitionMove {
	return TransitionMove{trace.UnitSubject(unit) + "-waiting-" + q, trace.UnitSubject(unit), intake.UnitImplementing, intake.UnitWaiting, trace.QuestionSubject(q) + "_" + trace.QuestionOpen,
		"unit " + unit + " is waiting: its mason asked question " + q + "; the unit's workspace is kept and it takes no mason slot until the answer arrives"}
}

func ProductionWorker(scope coreadapter.Scope) bool {
	return scope.Role == intake.MasonRole && slices.Contains([]string{"worker_first", "worker_second", "worker_unaffected"}, scope.Thread)
}

func Ptr(s string) *string { return &s }

func Resumed(unit, q string) TransitionMove {
	return TransitionMove{trace.UnitSubject(unit) + "-implementing-" + q, trace.UnitSubject(unit), intake.UnitWaiting, intake.UnitImplementing, trace.QuestionSubject(q) + "_" + trace.QuestionAnswered,
		"unit " + unit + " resumes implementing: the answer to question " + q + " is its mason's next turn"}
}

func States(list api.ConversationResponse) []string {
	var out []string
	for _, e := range list.Entries {
		out = append(out, e.Kind+":"+string(e.State))
	}
	return out
}

func UnitState(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	return state.Value
}

func WebHTTP() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

type ProductionWorkerWorkspaces struct{ Production coreadapter.Workspaces }

var ExampleCard = coreadapter.Card{Headline: "Uploads resume", Happened: "The storage client resumes uploads from the last chunk.", NeedsYou: "Review the candidate."}
