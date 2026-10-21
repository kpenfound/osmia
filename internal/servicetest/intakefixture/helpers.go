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
	questions "github.com/kpenfound/osmia/internal/questions"

	json "encoding/json"
	time "time"

	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
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

	jev "github.com/kpenfound/osmia/internal/jev"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"

	errors "errors"

	workspace "github.com/kpenfound/osmia/internal/workspace"

	maps "maps"

	strings "strings"

	filepath "path/filepath"

	shed "github.com/kpenfound/osmia/internal/shed"
	thread "github.com/kpenfound/osmia/internal/thread"
)

// step runs the amendment controller once on amendment 1 and checks the
// state it leaves it in.
func Step(t *testing.T, a AmendmentDebate, stream config.WorkstreamID, req trace.Amendment, want string) {
	t.Helper()
	Must(t, a.One(context.Background(), stream, req))
	if state, err := a.Repository.Workflow(stream, AmendmentSubject(req.ID)); err != nil || state.Value != want {
		t.Fatalf("amendment %s is %+v, want %s: %v", req.ID, state, want, err)
	}
}

// masonTurns returns the turns of the unit's mason thread with the given ID.
func MasonTurns(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit, turn string) []trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, MasonAgent(unit))
	Must(t, err)
	return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool { return q.Request.TurnID != turn })
}

// unitMoves returns the transitions of the unit's subject recorded for
// amendment 1.
func UnitMoves(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	Must(t, err)
	return slices.DeleteFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject != trace.UnitSubject(unit) || !strings.HasSuffix(tr.ID, "_amendment_1")
	})
}

var (
	CheckpointSpec = strings.Replace(ValidSpec, "the last acknowledged chunk", "a durable checkpoint", 1)
	RestartPlan    = strings.Replace(ValidPlan, "TestResume", "TestResumeAfterRestart", 1)
)

// parks has amendment 1's first attempt deliver the revised spec and ask
// question 1.
func Parks(p *Faults) FakeTurn {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(Delivers(p, plan.SpecPath, CheckpointSpec, DraftValid)(ctx, req, verified, tools), Asks(p, "1")(ctx, req, verified, tools))
	}
}

func AmendmentOp(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == action {
			return op.Operation
		}
	}
	t.Fatalf("missing operation %s", action)
	return coreadapter.Operation{}
}

// amendmentObjection is a committee record of round 1 of amendment 1 with one
// objection of the given kind.
func AmendmentObjection(kind shed.Kind) shed.Record {
	citation, part := "spec#1", "plan"
	if kind == shed.Charter {
		citation, part = "charter#1", "spec#1"
	}
	member := CommitteeAgent(1)
	return shed.Record{Version: shed.Version, Round: 1, Member: member, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: "amend-1-round-1-" + member + "-1",
		Objections: []shed.Objection{{ID: shed.ObjectionID(1, member, 1), Kind: kind, Part: part, Argument: "The proposal needs owner attention.", Citations: []string{citation}}}}
}

func ReadDocs(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	all, err := trace.Read[trace.Document](repo, stream)
	Must(t, err)
	var docs []trace.Document
	for _, doc := range all {
		if doc.ID == id {
			docs = append(docs, doc)
		}
	}
	return docs
}

type Code = api.Code

type APIError = api.APIError

type ErrorResponse = api.ErrorResponse

type Diagnostic = api.Diagnostic

type ConversationResponse = api.ConversationResponse

// delivers makes the architect's turn deliver one draft file, which the tool
// must report with verdict for everything the attempt delivered so far.
func Delivers(p *Faults, path, content, verdict string) FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := CallTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content})
		var status DraftStatus
		if err == nil {
			err = json.Unmarshal([]byte(got), &status)
		}
		if err != nil || status.Delivered != path || status.Verdict != verdict || status.Next == "" {
			p.Report("deliver %s: %q %v, want verdict %s", path, got, err, verdict)
		}
		return nil
	}
}

// asksThenWaits makes the architect's turn ask and then run until the service
// cancels it, signalling once it asked.
func AsksThenWaits(p *Faults, asked chan<- struct{}) FakeTurn {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := Asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	}
}

func Sketched(feature, _ string) bool { return feature == SketchedState }

func DraftAt(value string) func(string, string) bool {
	return func(_, draft string) bool { return draft == value }
}

// readTool reads a file through the turn's file_read tool.
func ReadTool(ctx context.Context, tools *mcp.ClientSession, path string) (string, error) {
	got, err := CallTool(ctx, tools, "file_read", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	var text string
	err = json.Unmarshal([]byte(got), &text)
	return text, err
}

// checkReadOnlyGrants asserts the grants core verified for a read-only turn:
// the private view read-only, the session read-only with the scratch
// directory the turn starts in, native reading tools and the scoped MCP server, and
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
	if !slices.Equal(g.Tools, []string{"Read", "Glob", "Grep", "mcp__osmia_0"}) || !slices.Equal(verified.Tools, []string{"Read", "Glob", "Grep"}) {
		fail("tools %v %v exceed the role grant", g.Tools, verified.Tools)
	}
	for _, kv := range verified.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key != "HOME" && req.Env[key] != value {
			fail("verified environment exposes %s", key)
		}
	}
}

// buildTransitions returns, in the order they were recorded, the
// transitions of the build subject, of the unit subjects and the move to
// building.
func BuildTransitions(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, tr := range AllTransitions(t, root, project, stream) {
		if tr.Actor == BuildingActor || tr.Subject == BuildSubject || tr.To == BuildingState {
			out = append(out, tr)
		}
	}
	return out
}

// allTransitions returns every transition of the workstream, in the order
// they were recorded.
func AllTransitions(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, body := range WorkstreamRecords(t, root, project, stream, "events.jsonl") {
		var tr trace.Transition
		Must(t, json.Unmarshal(body, &tr))
		if tr.Schema == "osmia.trace.transition" {
			out = append(out, tr)
		}
	}
	return out
}

// transitionMove is what one recorded transition says.
type TransitionMove struct{ ID, Subject, From, To, Cause, Reason string }

// moves returns what the transitions say, checking that the building
// controller recorded each.
func Moves(t *testing.T, transitions []trace.Transition) []TransitionMove {
	t.Helper()
	var out []TransitionMove
	for _, tr := range transitions {
		if tr.Actor != BuildingActor {
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

// charterSourcePath is the proposal the planted ruling's charter rule comes
// from, and charterRulingPath the ruling.
var (
	CharterSourcePath = "workstreams/" + string(Stream) + "/questions/1/charter.json"
	CharterRulingPath = "workstreams/" + string(Stream) + "/questions/1/rulings.jsonl"
)

// charterRevisions returns the recorded revisions of charter.md.
func CharterRevisions(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	Must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.Path == "charter.md" {
			out = append(out, d)
		}
	}
	return out
}

// appended returns the charter revisions that appended the planted rule.
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

// assemble returns the bundle a turn of the workstream receives.
func Assemble(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) bundle.Bundle {
	t.Helper()
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}
	b, err := files.Assemble(context.Background(), Project, bundle.Scope{Workstream: ws})
	Must(t, err)
	return b
}

// charterEvents returns the bodies of the workstream's outbox events raised
// by the charter proposal's decision or its recording in the charter.
func CharterEvents(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) []string {
	t.Helper()
	entries, err := repo.Outbox(ws)
	Must(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.TransitionID, trace.CharterSubject("1")+"_") {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

// withRule is the planted charter with the ratified rule appended as number.
func WithRule(before string, number int) string {
	return before + "\n## Standing rulings\n\n" + string(rune('0'+number)) + ". " + CharterRule + " <!-- ratified from " + CharterRulingPath + " revision 1 -->\n"
}

// checkChartered checks that the planted rule is recorded once, as number in
// the charter content want, that the proposal says so, and that every
// workstream's bundle carries it as one notice.
func CheckChartered(t *testing.T, repo *trace.Repository, want string, number int) {
	t.Helper()
	written := Appended(t, repo)
	if len(written) != 1 || written[0].Content != want || written[0].Actor != CharterActor {
		t.Fatalf("charter revisions appending the rule: %+v", written)
	}
	proposals, err := repo.CharterProposals()
	Must(t, err)
	if len(proposals) != 1 || proposals[0].State.Value != trace.CharterChartered || proposals[0].Proposal.Number != number || proposals[0].Proposal.Charter != written[0].Revision || proposals[0].Proposal.Decision != trace.CharterRatify {
		t.Fatalf("chartered proposal %+v", proposals)
	}
	for _, ws := range []config.WorkstreamID{Stream, Quiet} {
		b := Assemble(t, repo, ws)
		wantNotice := []bundle.CharterNotice{{Source: CharterSourcePath, Workstream: Stream, Record: trace.CharterProposalID("1"), Revision: 3, At: proposals[0].Latest.At,
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
	if events := CharterEvents(t, repo, Stream); len(events) != 1 || !strings.HasPrefix(events[0], "Charter rule "+string(rune('0'+number))+" is recorded from the owner's ruling on question 1") {
		t.Fatalf("chief of staff notices %q", events)
	}
}

// faults collects what the fakes of one test found wrong: an error a fake
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

// objects makes a member's turn raise one objection.
func Objects(p *Faults, kind shed.Kind, part, citation string) FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, reason, err := ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(kind), "part": part, "argument": "It does not hold.", "citations": []string{citation}}); err != nil || !recorded {
			p.Report("%s objection on %s: %q %v", kind, part, reason, err)
		}
		return nil
	}
}

// concedes makes a member's turn concede its objections.
func Concedes(p *Faults, ids ...string) FakeTurn {
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

// answers makes the architect's turn answer objections.
func Answers(p *Faults, text string, ids ...string) FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, id := range ids {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": text}); err != nil || !recorded {
				p.Report("reply to %s: %q %v", id, reason, err)
			}
		}
		return nil
	}
}

// checkReplyBoundary makes the assertions from inside the architect's reply
// turn: it reads the round and the draft, answers and redrafts, and holds
// nothing that contributes as a member, writes, runs or fetches.
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
	if want := []string{questions.AskTool, DraftTool, "file_read", "project_context", shed.ReplyTool}; !slices.Equal(names, want) {
		p.Report("role tools %v, want %v", names, want)
	}
	for _, name := range []string{shed.ObjectTool, shed.ConcedeTool, "file_write", "shell", "fetch", "git_push", "notes_write", "set_status"} {
		if _, err := CallTool(ctx, tools, name, map[string]any{"kind": "fit", "part": "spec", "argument": "x", "citations": []string{"charter#1"}, "objection": "x", "reason": "y"}); err == nil {
			p.Report("the architect called %s", name)
		}
	}
	for path, want := range map[string]string{"spec.md": ValidSpec, "plan.json": ValidPlan, "handed/stdin": HandedDesign, "charter.md": ShedCharter} {
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
	if req.Profile.VCSAccess || req.Profile.Name != ArchitectRole {
		p.Report("request carries VCS access or another role: %+v", req.Profile)
	}
}

// recordDesignDraft writes a draft document the same way draft_brief's tool
// handler does, without running a real agent turn, so a test of
// startDesignWorkgroup alone does not need a production enforcement
// fixture.
func RecordDesignDraft(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, content string) error {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DesignDraftDocument, Revision: 1,
		Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: ChiefActor, Cause: DraftBriefToolName}
	data, err := json.Marshal(DesignBriefRecord{Content: content})
	if err != nil {
		return err
	}
	return repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: DesignDraftPath, Content: string(data)}})
}

func RecordDesignDraft2(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, content string) error {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DesignDraftDocument, Revision: 2,
		Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: ChiefActor, Cause: DraftBriefToolName}
	data, err := json.Marshal(DesignBriefRecord{Content: content})
	if err != nil {
		return err
	}
	return repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: DesignDraftPath, Content: string(data)}})
}

// apiError checks that err is the service's error with the code and message.
func ApiError(t *testing.T, err error, code Code, message string) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code || api.Message != message {
		t.Fatalf("want %s %q, got %v", code, message, err)
	}
}

type AmendmentDebate = intake.AmendmentDebate

var AmendmentSubject = intake.AmendmentSubject

const ArchitectRole = intake.ArchitectRole

const SketchedState = intake.SketchedState

const DraftTool = intake.DraftTool

const DraftValid = intake.DraftValid

type DraftStatus = intake.DraftStatus

const BuildingState = intake.BuildingState

const BuildSubject = intake.BuildSubject

const UnitImplementing = intake.UnitImplementing

const UnitWaiting = intake.UnitWaiting

var BuildingActor = intake.BuildingActor

var CharterActor = intake.CharterActor

var ChiefActor = intake.ChiefActor

const DraftBriefToolName = intake.DraftBriefToolName

const DesignDraftDocument = intake.DesignDraftDocument

const DesignDraftPath = intake.DesignDraftPath

type DesignBriefRecord = intake.DesignBriefRecord

type StagedWorkspaces = intake.StagedWorkspaces

var MasonAgent = intake.MasonAgent

const ReviewerRole = intake.ReviewerRole

const CommitteeRole = intake.CommitteeRole

var CommitteeAgent = intake.CommitteeAgent

const MasonRole = intake.MasonRole

type StreamWorkspaces = intake.StreamWorkspaces

func DecideWith(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	Must(t, err)
	return string(out)
}

// The four designs the owner hands in. The fakes tell the workstreams apart
// by the first line of what was handed.
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

// exitDemo holds the fake agents' scripts, keyed by the handed design's first
// line and then by turn name, and what they saw.
type ExitDemo struct {
	Mu       sync.Mutex
	Trace    string
	Root     config.Root
	Project  config.ProjectID
	Plays    map[string]map[string]DemoTurn
	Ran      map[string][]string
	Prompts  map[string]string
	Streams  map[string]config.WorkstreamID
	Problems []string
}

// streamOf returns the workstream a design was handed in as, once the
// hand-in has returned.
func (d *ExitDemo) StreamOf(design string) (config.WorkstreamID, bool) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	stream, ok := d.Streams[ExitTitle(design)]
	return stream, ok
}

// design names the workstream a turn belongs to by the first line of the
// design handed to it. Every turn's session directory is
// <kind>/<project>/<workstream>/<turn or agent>/<session or turn>.
func (d *ExitDemo) Design(req agent.Request) (string, error) {
	stream := filepath.Base(filepath.Dir(filepath.Dir(req.SessionDir)))
	dir, err := d.Root.WorkstreamTrace(d.Project, config.WorkstreamID(stream))
	if err != nil {
		return "", err
	}
	data, err := LatestDocumentIn(dir, "workstreams/"+stream+"/documents.jsonl", "handed/stdin")
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

// drafts makes an architect turn deliver the given files and answer the given
// objections.
func (d *ExitDemo) Drafts(files map[string]string, answers map[string]string) DemoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, id := range slices.Sorted(maps.Keys(answers)) {
			if recorded, reason, err := ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": id, "answer": answers[id]}); err != nil || !recorded {
				d.Problem("reply to %s: %q %v", id, reason, err)
			}
		}
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			if content, ok := files[path]; ok {
				if _, err := CallTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil {
					return nil, fmt.Errorf("deliver %s: %w", path, err)
				}
			}
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

// objection is one objection a member's turn raises.
type Objection struct {
	Kind                     shed.Kind
	Part, Argument, Citation string
}

// debates makes a member's turn raise the given objections and concede the
// given ones.
func (d *ExitDemo) Debates(raise []Objection, concede ...string) DemoTurn {
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

// exitAttention is what the fake chief of staff puts in front of the owner for
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

// chief is the fake chief of staff. It escalates the committee's question,
// relays the owner's ruling, and makes every packet it is told of the
// attention item of its status.
func (d *ExitDemo) Chief(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	title, err := d.Design(req)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(req.Name, "events_") {
		d.Problem("chief of staff turn %s of %q", req.Name, title)
	}
	d.Mu.Lock()
	d.Ran[title] = append(d.Ran[title], "events")
	d.Mu.Unlock()
	call := func(name string, args map[string]any) {
		if out, err := CallTool(ctx, tools, name, args); err != nil || !strings.Contains(out, `"recorded":true`) && !strings.Contains(out, `"stored":true`) {
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

// judgments returns the latest revision of every judgment record of the
// workstream, by ID.
func Judgments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]jev.Record {
	t.Helper()
	documents, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	out := map[string]jev.Record{}
	for _, d := range documents {
		if !strings.HasPrefix(d.Path, "judgments/") {
			continue
		}
		var r jev.Record
		Must(t, json.Unmarshal([]byte(d.Content), &r))
		out[d.ID] = r
	}
	return out
}

// toolNames lists the tools a fake agent's session offers, sorted.
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
	// chiefRole runs the chief of staff in a container, like every other role
	// of the shed fixtures.
	ChiefRole = "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"
	// pinOne names the revision the fixtures' round 1 is pinned to.
	PinOne = "spec.md revision 1 and plan.json revision 1"
)

var (
	OpenQuestion = regexp.MustCompile(`Question (\d+) is open, asked by the (?:committee|architect|mason|reviewer): `)
	OwnerRuled   = regexp.MustCompile(`The owner ruled on inbox entry \d+, \S+ \(questions ([^)]+)\): `)
)

// chief is the fake chief of staff of the asking fixtures. A committee
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
	// routes holds, by question ID, the citations, change and reason of
	// the amendment the chief of staff routes that question to.
	Routes map[string]map[string]any
}

// route has the chief of staff route question id to an amendment with the
// given citations, change and reason.
func (c *Chief) Route(id string, citations []string, change, reason string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.Routes == nil {
		c.Routes = map[string]map[string]any{}
	}
	c.Routes[id] = map[string]any{"question": id, "citations": citations, "change": change, "reason": reason}
}

// release has the chief of staff answer question id from the record.
func (c *Chief) Release(id string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Released[id] = true
}

// hold has the chief of staff answer question id from the record and keep
// its turn open until the returned channel closes.
func (c *Chief) Hold(id string) chan struct{} {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Held[id] = make(chan struct{})
	return c.Held[id]
}

func (c *Chief) Turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {

	if strings.HasPrefix(req.Name, ReviewerRole+"-") {
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

// choose calls one question tool and reports a refusal, except one saying the
// question was chosen for already: a prompt replays earlier events.
func (c *Chief) Choose(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) {
	got, err := CallTool(ctx, tools, name, args)
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

// asks makes a member's turn ask the question that gets number id and check
// the tool's answer.
func Asks(p *Faults, id string) FakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := CallTool(ctx, tools, questions.AskTool, map[string]any{"question": AskedQuestion})
		if err != nil || got != `{"recorded":true,"question":"`+id+`","next":"End your turn now. The answer arrives as your next turn on this thread."}` {
			p.Report("ask: %q %v", got, err)
		}
		if got, err := CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "And another?"}); err != nil || !strings.Contains(got, "this turn already asked question "+id) {
			p.Report("a second question in the turn: %q %v", got, err)
		}
		return nil
	}
}

// results renders acknowledged operations with their results, which %v
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

// held makes a member's turn report that it started and wait to be released,
// so the owner can act while the round runs.
func Held(started chan<- struct{}, release <-chan struct{}) FakeTurn {
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
	case <-time.After(DemoTimeout):
		t.Fatal("the member's turn did not start")
	}
}

// failed reports whether err is an API error with that code.
func Failed(err error, code Code) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == code
}

const ShedCharter = "1. Keep changes small.\n2. Every change has a test.\n"

// tool calls object or concede and returns whether it was recorded, and the
// objection ID or the reason.
func ShedTool(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) (bool, string, error) {
	got, err := CallTool(ctx, tools, name, args)
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

// barrier holds every member's turn until all of them are running, so a
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
	case <-time.After(DemoTimeout):
		return errors.New("the other members never started: the round does not run them in parallel")
	}
}

// checkCommitteeBoundary makes the negative assertions from inside a member's
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
	for _, name := range []string{"file_write", "shell", "exec", "fetch", "git_push", DraftTool, "set_status", "notes_read", "notes_write", "answer", "escalate", shed.ReplyTool} {
		if _, err := CallTool(ctx, tools, name, map[string]any{"path": "x", "content": "y"}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for path, want := range map[string]string{"spec.md": ValidSpec, "plan.json": ValidPlan, "handed/stdin": HandedDesign, "charter.md": ShedCharter, "repo/CODEOWNERS": "/internal/ @core\n", "repo/internal/trace/git.go": "package trace\n"} {
		if got, err := ReadTool(ctx, tools, path); err != nil || got != want {
			fail("read %s: %q %v", path, got, err)
		}
	}
	if bundle, err := ReadTool(ctx, tools, "context.md"); err != nil || !strings.Contains(bundle, "- charter#2: Every change has a test.") || !strings.Contains(bundle, "project_context") {
		fail("context.md: %q %v", bundle, err)
	}
	for _, path := range []string{"repo/secret.env", "secret.env", "repo/.git/HEAD", ".git/HEAD", "../config.toml", filepath.Join(clone, "CODEOWNERS"), "kb/entities.json", "shed/round-1/" + CommitteeAgent(2) + ".json"} {
		if _, err := ReadTool(ctx, tools, path); err == nil {
			fail("runtime read %s", path)
		}
	}
	CheckReadOnlyGrants(req, verified, clone, fail)
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || req.Profile.Name != CommitteeRole {
		fail("request carries VCS access or another role: %+v", req.Profile)
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, DemoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, want := range []string{"Round 1 of the shed", "spec.md revision 1 and plan.json revision 1", "charter#<n>", "spec#<n>", "plan#<unit>", "kb/<subsystem>.md", "kb/entities.json#<entity>", "veto", "advice", "split", "acceptance", shed.ObjectTool, "Call " + questions.AskTool + " when something you must know"} {
		if !strings.Contains(req.Prompt, want) {
			fail("prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, "still stand") {
		fail("round 1 lists earlier objections:\n%s", req.Prompt)
	}
	if !strings.Contains(req.SystemPrompt, "member of the committee") || !strings.Contains(req.SystemPrompt, "no tool that writes, runs or fetches") || !strings.Contains(req.SystemPrompt, "call "+questions.AskTool+": the round waits for the answer") {
		fail("system prompt: %q", req.SystemPrompt)
	}
	return errors.Join(problems...)
}

// asking wraps the fake masons' turn: in the workstream stream, or in every
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

// awaitAcknowledged polls fetch until it returns operations and the reconcile
// controller has acknowledged every one of them. An operation's result and its
// acknowledgement are committed after the state its Apply commits, so a test
// that reached an operation through an awaited state must wait here before it
// asserts on the operation's result, acknowledgement or history.
var AwaitAcknowledged = fixturekit.AwaitAcknowledged

// awaitPasses waits until passed, advanced once per completed reconciliation
// pass by a schedulePassed hook a fixture's prepare func installed, has
// counted at least n more passes than when this call began, then returns
// what the counter observed. It fails at the deadline with the last count,
// so a check that nothing more happened still runs against passes that
// demonstrably ran, rather than a fixed sleep that might not have covered
// any.
func AwaitPasses(t *testing.T, passed *atomic.Int64, n int64) int64 {
	t.Helper()
	target := passed.Load() + n
	deadline := time.Now().Add(DemoTimeout)
	for {
		if got := passed.Load(); got >= target {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("passes %d, want at least %d", passed.Load(), target)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitTurn waits until the i-th turn of the agent's thread completed.
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

// changeRaceGate stops only the selected production boundary after approval.
type ChangeRaceGate struct {
	Step        string
	Armed       atomic.Bool
	Reached     chan struct{}
	Released    chan struct{}
	Once        sync.Once
	ReleaseOnce sync.Once
}

// chiefEvent reports whether e is an event the chief of staff receives as
// information: a notice or routine progress.
func ChiefEvent(e trace.Event) bool {
	return e.Kind == trace.NoticeKind || e.Kind == trace.ProgressKind
}

// deliver accepts an owner message and publishes the intent to run it.
func Deliver(t *testing.T, repo *trace.Repository, turn string, version uint64, at time.Time) trace.TurnRequest {
	t.Helper()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: Project, Workstream: Stream, At: at, Actor: owner, Cause: "message_" + turn, Depth: 1},
		AgentID: DemoAgent, ThreadID: DemoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the mason.", Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	Must(t, err)
	event := trace.EventID("deliver_"+turn, "turn")
	op, err := thread.TurnOperation(Project, event, thread.TurnInput{Workstream: Stream, Agent: DemoAgent, Turn: turn})
	Must(t, err)
	from := ""
	if version > 0 {
		from = "delivering"
	}
	tx := trace.Transaction{ExpectedVersion: version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "deliver_" + turn, Project: Project, Workstream: Stream, At: at, Actor: trace.Actor{Kind: "service", ID: "inbox"}, Cause: req.ID, Depth: 1},
		Subject: "mason_inbox", From: from, To: "delivering", Reason: "Owner message accepted for the mason thread"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver " + turn, Operation: &op}}}
	_, err = repo.Transact(context.Background(), tx)
	Must(t, err)
	return req
}

// documentCommit returns the commit that recorded the latest revision of the
// document at path in stream's trace.
func DocumentCommit(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, path string) string {
	t.Helper()
	rows := TraceQuery(t, WorkstreamTraceDirectory(t, root, project, stream), "SELECT CAST(commit_seq AS TEXT) FROM records WHERE path = ? AND schema = 'osmia.trace.document' AND json_extract(body, '$.path') = ? ORDER BY seq DESC LIMIT 1", "workstreams/"+string(stream)+"/documents.jsonl", path)
	if len(rows) != 1 {
		t.Fatalf("workstream %s records no %s", stream, path)
	}
	return string(rows[0])
}

// done calls the done tool and returns whether the service accepted the
// report, and its reason when it did not.
func Done(ctx context.Context, tools *mcp.ClientSession, args map[string]any) (bool, string, error) {
	for key, value := range map[string]string{"headline": ExampleCard.Headline, "happened": ExampleCard.Happened, "needs_you": ExampleCard.NeedsYou} {
		if _, ok := args[key]; !ok {
			args[key] = value
		}
	}
	text, err := CallTool(ctx, tools, DoneTool, args)
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

// errCrashTurn is what a fake mason's play returns to end its turn with an
// infrastructure failure, which the runner retries.
var ErrCrashTurn = errors.New("the turn crashes")

// errFailTurn is what a fake mason's play returns to end its turn with a
// behavioural failure: it reports an outcome no mason may report, so the turn
// is not retried.
var ErrFailTurn = errors.New("the turn fails")

// eventually polls cond until it holds, failing with what after demoTimeout.
func Eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// exchange sends one raw request through client and returns its status and
// body.
func Exchange(t *testing.T, client *http.Client, method, url, contentType, body string, host string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	Must(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	Must(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	Must(t, err)
	return resp.StatusCode, data
}

// fakeMasons is a fake model session standing in for the real mason's model
// backend: it never calls out to a live model, so a mason's own reasoning,
// cost and latency are left unverified. Each turn checks that it holds its
// view's file tools, ask and done, and no amend, records what it saw,
// writes masonWrote into its view and then plays what play holds for the
// turn, which ends the turn failed by returning errFailTurn or errCrashTurn.
type FakeMasons struct {
	Mu       sync.Mutex
	Runs     map[string][]agent.Request
	Problems []string
	// play holds, by turn ID, what the turn does after writing masonWrote.
	Play     map[string]func(context.Context, agent.Request, *mcp.ClientSession) error
	Response map[string]string
	// chief plays the fixture's chief-of-staff turns.
	Chief *Chief
}

// handle runs a tool's handler with input and returns its result.
func Handle(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	Must(t, err)
	return string(out)
}

// noticeOf returns the notice the transition raised for the chief of staff.
func NoticeOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	Must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && ChiefEvent(entry.Event) {
			return entry.Event.Body
		}
	}
	t.Fatalf("transition %s told the chief of staff nothing", transition)
	return ""
}

// providerOf returns the provider of the workstream's workspaces.
func ProviderOf(t *testing.T, w StreamWorkspaces, stream config.WorkstreamID) workspace.Provider {
	t.Helper()
	g, err := w.Of(stream)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// recordSpec records content as the owner's next revision of stream's spec
// directly in its trace, whatever the workstream's state.
func RecordSpec(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, content string) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	Must(t, err)
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
	Must(t, repo.RecordDocuments(context.Background(), []trace.Document{next}))
}

// requests returns the mason turns that ran in the workstream.
func (m *FakeMasons) Requests(stream config.WorkstreamID) []agent.Request {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return slices.Clone(m.Runs[string(stream)])
}

// requireJJ skips the test when jj is not on PATH, and fails it there when
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

// retries counts the retries recorded for the operation.
func Retries(op trace.OperationRecord) int {
	n := 0
	for _, a := range op.History {
		if a.Kind == "retry" {
			n++
		}
	}
	return n
}

// sealOf is a seal of one unit per footprint, resolved against the map.
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

// seals returns every recorded revision of seal.json.
func Seals(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []seal.Seal {
	t.Helper()
	var out []seal.Seal
	for _, d := range StreamDocuments(t, repository, stream, seal.DocumentID) {
		s, err := seal.Parse([]byte(d.Content))
		Must(t, err)
		out = append(out, s)
	}
	return out
}

// sessionStream returns the workstream of a thread turn, from its session
// directory threads/<project>/<workstream>/...
func SessionStream(req agent.Request) string {
	parts := strings.Split(filepath.ToSlash(req.SessionDir), "/")
	if i := slices.Index(parts, "threads"); i >= 0 && i+2 < len(parts) {
		return parts[i+2]
	}
	return ""
}

// settle lets the service's loop run several more passes.
func Settle() { time.Sleep(1500 * time.Millisecond) }

// snapshot reads every file below dir, including VCS metadata.
func Snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	Must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

// soon polls ok until it holds, failing with what after 20 seconds.
func Soon(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamDocuments returns the recorded revisions of one workstream document.
func StreamDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	Must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != id })
}

// transitionByID returns the workstream's transition with the given ID.
func TransitionByID(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	Must(t, err)
	i := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatalf("workstream %s has no transition %s", stream, id)
	}
	return transitions[i]
}

// turn is the ID queue gives a project's turn.
func Turn(id config.ProjectID, name string) string { return string(id[len(id)-4:]) + "-" + name }

const (
	ChiefAgent  = "agent_chief"
	ChiefThread = "thread_chief"
	Quiet       = config.WorkstreamID("w_fedcba9876543210fedcba9876543210")
)

const DemoAgent = fixturekit.DemoAgent

const DemoSecret = fixturekit.DemoSecret

const DemoThread = fixturekit.DemoThread

const DemoTimeout = fixturekit.DemoTimeout

const HandedDesign = fixturekit.HandedDesign

const MasonWrote = fixturekit.MasonWrote

const Project = fixturekit.Project

const Stream = fixturekit.Stream

const ValidPlan = fixturekit.ValidPlan

const ValidSpec = fixturekit.ValidSpec

func (g *ChangeRaceGate) Await(t *testing.T) {
	t.Helper()
	select {
	case <-g.Reached:
	case <-time.After(DemoTimeout):
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
	if err := os.WriteFile(filepath.Join(view, MasonWrote), []byte("package trace\n"), 0644); err != nil {
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
		return StagedWorkspaces{}.Acquire(ctx, req)
	}
	return w.Production.Acquire(ctx, req)
}

func AcceptancePlan(t *testing.T, graph string, items ...string) string {
	t.Helper()
	p, err := plan.Parse([]byte(graph))
	Must(t, err)
	p.Units[0].Acceptance = items
	data, err := plan.Encode(p)
	Must(t, err)
	return string(data)
}

var AssertCode = fixturekit.AssertCode

func Await(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(DemoTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

func EntitiesWith(t *testing.T, seed kb.Map, alias string) string {
	t.Helper()
	m := kb.Map{Version: kb.Version}
	for _, e := range seed.Entities {
		if e.ID == "internal.trace" {
			e.Aliases = append(e.Aliases, alias)
		}
		m.Entities = append(m.Entities, e)
	}
	data, err := kb.Encode(m)
	Must(t, err)
	return string(data)
}

func EqualJSONTest(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func ErrorCode(t *testing.T, body []byte) Code {
	t.Helper()
	var out ErrorResponse
	Must(t, json.Unmarshal(body, &out))
	return out.Error.Code
}

func FailMasonTurn(context.Context, agent.Request, *mcp.ClientSession) error {
	return ErrFailTurn
}

func HasDiagnostic(ds []Diagnostic, code Code) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func Parked(unit, q string) TransitionMove {
	return TransitionMove{trace.UnitSubject(unit) + "-waiting-" + q, trace.UnitSubject(unit), UnitImplementing, UnitWaiting, trace.QuestionSubject(q) + "_" + trace.QuestionOpen,
		"unit " + unit + " is waiting: its mason asked question " + q + "; the unit's workspace is kept and it takes no mason slot until the answer arrives"}
}

func ProductionWorker(scope coreadapter.Scope) bool {
	return scope.Role == MasonRole && slices.Contains([]string{"worker_first", "worker_second", "worker_unaffected"}, scope.Thread)
}

func Ptr(s string) *string { return &s }

func Resumed(unit, q string) TransitionMove {
	return TransitionMove{trace.UnitSubject(unit) + "-implementing-" + q, trace.UnitSubject(unit), UnitWaiting, UnitImplementing, trace.QuestionSubject(q) + "_" + trace.QuestionAnswered,
		"unit " + unit + " resumes implementing: the answer to question " + q + " is its mason's next turn"}
}

func States(list ConversationResponse) []string {
	var out []string
	for _, e := range list.Entries {
		out = append(out, e.Kind+":"+string(e.State))
	}
	return out
}

func UnitState(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	Must(t, err)
	return state.Value
}

func WebHTTP() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

type DemoTurn = fixturekit.DemoTurn

type FakeTurn = fixturekit.FakeTurn

type ProductionWorkerWorkspaces struct{ Production coreadapter.Workspaces }

var CallTool = fixturekit.CallTool

var ExampleCard = coreadapter.Card{Headline: "Uploads resume", Happened: "The storage client resumes uploads from the last chunk.", NeedsYou: "Review the candidate."}

var LatestDocumentIn = fixturekit.LatestDocumentIn

var Must = fixturekit.Must

var TraceQuery = fixturekit.TraceQuery

var WorkstreamRecords = fixturekit.WorkstreamRecords

var WorkstreamTraceDirectory = fixturekit.WorkstreamTraceDirectory
