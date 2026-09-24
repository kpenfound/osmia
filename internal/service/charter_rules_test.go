package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	charterRuling = "Every upload must resume after a restart."
	charterRule   = "Uploads resume after a restart."
	charterBefore = "# Charter\n\n1. Keep state in files.\n"
)

// charterSourcePath is the proposal the planted ruling's charter rule comes
// from, and charterRulingPath the ruling.
var (
	charterSourcePath = "workstreams/" + string(stream) + "/questions/1/charter.json"
	charterRulingPath = "workstreams/" + string(stream) + "/questions/1/rulings.jsonl"
)

// charterFixture is a project trace with two workstreams, stream and quiet.
// The owner ruled on question 1 of stream, and its chief of staff proposed
// the ruling as a charter rule with propose_charter. No service runs until
// start.
type charterFixture struct {
	opts  Options
	cfg   *config.Config
	clock *fixedClock
	ticks chan time.Time
	s     *Service
	c     *Client
}

func newCharterFixture(t *testing.T) *charterFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cr-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &charterFixture{opts: fixtureAt(t, home), clock: &fixedClock{now: demoStart}, ticks: make(chan time.Time)}
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = f.clock.Now, f.ticks
	f.cfg, err = config.Load(f.opts.Config)
	must(t, err)
	demoGit(t, home, "init", "-q", f.cfg.Project.Clone)
	repo, err := trace.Create(ctx, f.cfg.Root, f.cfg.Project, f.clock.Now(), ownerActor)
	must(t, err)
	must(t, os.WriteFile(f.charterFile(t), []byte(charterBefore), 0600))
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		must(t, repo.CreateWorkstream(ctx, ws, f.clock.Now(), ownerActor))
	}
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: "agent_mason", Revision: 1, Project: project, Workstream: stream, At: f.clock.Now(), Actor: serviceActor, Cause: "fixture"}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: masonRole, ThreadID: "thread_mason"}))
	_, err = repo.EnsureChiefOfStaff(ctx, stream, f.clock.Now(), serviceActor)
	must(t, err)
	mason := f.claim(t, repo, "agent_mason", "thread_mason", masonRole, "build", serviceActor)
	_, err = repo.Ask(ctx, "agent_mason", mason, "May an upload restart from zero?", f.clock.Now())
	must(t, err)
	chief := f.claim(t, repo, trace.ChiefOfStaff, trace.ChiefOfStaff, trace.ChiefOfStaff, "events_1", serviceActor)
	_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, chief, trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "May an upload restart from zero?",
		Blocked: "The upload unit.", Options: []string{"Resume", "Restart"}, Recommendation: "Resume."}, f.clock.Now())
	must(t, err)
	_, err = repo.Rule(ctx, 1, charterRuling, ownerActor, f.clock.Now())
	must(t, err)
	tools, err := questions.Tools(repo, trace.ChiefOfStaff, chief, f.clock.Now)
	must(t, err)
	for _, tool := range tools {
		if tool.Name != questions.ProposeCharterTool {
			continue
		}
		out, err := tool.Handle(ctx, json.RawMessage(`{"question":"1","rule":"`+charterRule+`"}`))
		must(t, err)
		if string(out) != `{"recorded":true,"question":"1","number":2,"next":"The owner ratifies or declines the proposal; make it the attention of your status until they do."}` {
			t.Fatalf("propose_charter: %s", out)
		}
	}
	must(t, repo.Close())
	// A later session finds the planted turns abandoned.
	repo, err = trace.Open(f.cfg.Root, f.cfg.Project)
	must(t, err)
	must(t, repo.AbandonTurn(ctx, stream, "agent_mason", "build", f.clock.Now()))
	must(t, repo.AbandonTurn(ctx, stream, trace.ChiefOfStaff, "events_1", f.clock.Now()))
	must(t, repo.Close())
	return f
}

// claim queues a turn of the agent's thread and claims it, returning its
// scope.
func (f *charterFixture) claim(t *testing.T, repo *trace.Repository, agent, thread, role, turn string, actor trace.Actor) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: project, Workstream: stream, At: f.clock.Now(), Actor: actor, Cause: "fixture", Depth: 1}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + role + ".", Prompt: "Work: " + turn})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), f.clock.Now())
	must(t, err)
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: thread, Turn: turn, Role: role}
}

func (f *charterFixture) charterFile(t *testing.T) string {
	t.Helper()
	dir, err := f.cfg.Root.ProjectTrace(project)
	must(t, err)
	return filepath.Join(dir, "charter.md")
}

// start runs the service and lets it finish its passes.
func (f *charterFixture) start(t *testing.T) {
	t.Helper()
	s, err := Start(context.Background(), f.opts)
	must(t, err)
	f.s, f.c = s, NewClient(s.Socket())
	f.settle(t)
}

// settle sends ticks; a tick is received only between passes, so afterwards
// every pass an earlier one caused has finished.
func (f *charterFixture) settle(t *testing.T) {
	t.Helper()
	for range 3 {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("service did not finish its pass")
		}
	}
}

func (f *charterFixture) stop(t *testing.T) {
	t.Helper()
	f.c.Close()
	must(t, f.s.Close())
}

// open opens the stopped service's trace.
func (f *charterFixture) open(t *testing.T) *trace.Repository {
	t.Helper()
	repo, err := trace.Open(f.cfg.Root, f.cfg.Project)
	must(t, err)
	return repo
}

// charterRevisions returns the recorded revisions of charter.md.
func charterRevisions(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.Path == "charter.md" {
			out = append(out, d)
		}
	}
	return out
}

// appended returns the charter revisions that appended the planted rule.
func appended(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	var out []trace.Document
	for _, d := range charterRevisions(t, repo) {
		if d.Cause == charterSourcePath {
			out = append(out, d)
		}
	}
	return out
}

// assemble returns the bundle a turn of the workstream receives.
func assemble(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) bundle.Bundle {
	t.Helper()
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }, Now: func() time.Time { return demoStart }}
	b, err := files.Assemble(context.Background(), project, bundle.Scope{Workstream: ws})
	must(t, err)
	return b
}

// charterEvents returns the bodies of the workstream's outbox events raised
// by the charter proposal's decision or its recording in the charter.
func charterEvents(t *testing.T, repo *trace.Repository, ws config.WorkstreamID) []string {
	t.Helper()
	entries, err := repo.Outbox(ws)
	must(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.TransitionID, trace.CharterSubject("1")+"_") {
			out = append(out, e.Event.Body)
		}
	}
	return out
}

// withRule is the planted charter with the ratified rule appended as number.
func withRule(before string, number int) string {
	return before + "\n## Standing rulings\n\n" + string(rune('0'+number)) + ". " + charterRule + " <!-- ratified from " + charterRulingPath + " revision 1 -->\n"
}

// checkChartered checks that the planted rule is recorded once, as number in
// the charter content want, that the proposal says so, and that every
// workstream's bundle carries it as one notice.
func checkChartered(t *testing.T, repo *trace.Repository, want string, number int) {
	t.Helper()
	written := appended(t, repo)
	if len(written) != 1 || written[0].Content != want || written[0].Actor != charterActor {
		t.Fatalf("charter revisions appending the rule: %+v", written)
	}
	proposals, err := repo.CharterProposals()
	must(t, err)
	if len(proposals) != 1 || proposals[0].State.Value != trace.CharterChartered || proposals[0].Proposal.Number != number || proposals[0].Proposal.Charter != written[0].Revision || proposals[0].Proposal.Decision != trace.CharterRatify {
		t.Fatalf("chartered proposal %+v", proposals)
	}
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		b := assemble(t, repo, ws)
		wantNotice := []bundle.CharterNotice{{Source: charterSourcePath, Workstream: stream, Record: trace.CharterProposalID("1"), Revision: 3, At: proposals[0].Latest.At,
			Number: number, Rule: charterRule, Charter: written[0].Revision, Ruling: charterRulingPath, RulingRevision: 1, OwnerResponse: charterRuling}}
		if !reflect.DeepEqual(b.CharterNotices, wantNotice) {
			t.Fatalf("charter notices of %s: %+v", ws, b.CharterNotices)
		}
		text := b.Render()
		if strings.Count(text, "<<< osmia:charter_rule | chief of staff proposal, ratified by the owner | bytes=31 >>>\n| "+charterRule+"\n<<< /osmia:charter_rule >>>\n") != 1 ||
			!strings.Contains(text, "- charter#"+string(rune('0'+number))+" [Standing rulings]: "+charterRule+"\n") {
			t.Fatalf("bundle of %s:\n%s", ws, text)
		}
	}
	if events := charterEvents(t, repo, stream); len(events) != 1 || !strings.HasPrefix(events[0], "Charter rule "+string(rune('0'+number))+" is recorded from the owner's ruling on question 1") {
		t.Fatalf("chief of staff notices %q", events)
	}
}

// The owner reads the proposed charter rule as an attention item and ratifies
// it. The rule is appended to the charter as its next number with its source
// ruling, and the next bundle of the other workstream carries it as a notice,
// once, across a restart. Deciding it again the same way records nothing and
// the other way is refused.
func TestRatifiedCharterRuleReachesEveryWorkstreamOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)

	want := CharterProposalView{Workstream: stream, Question: "1", State: trace.CharterProposed, Rule: charterRule, Number: 2, Ruling: charterRulingPath, RulingRevision: 1, OwnerResponse: charterRuling, ProposedAt: demoStart}
	list, err := f.c.CharterProposals(ctx)
	must(t, err)
	if !reflect.DeepEqual(list, CharterProposalsResponse{Proposals: []CharterProposalView{want}}) {
		t.Fatalf("proposals %+v", list)
	}
	if st, err := f.c.Status(ctx, stream); err != nil || !reflect.DeepEqual(st.Gates, []trace.OwnerGate{{Kind: "charter", Reference: "1"}}) {
		t.Fatalf("status gates %+v: %v", st.Gates, err)
	}
	if b := assemble(t, f.s.active.repository, quiet); len(b.CharterNotices) != 0 {
		t.Fatalf("a proposed rule is a notice: %+v", b.CharterNotices)
	}
	_, err = f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: "accept"})
	apiError(t, err, Validation, "a charter decision is ratify or decline")
	_, err = f.c.DecideCharter(ctx, stream, "9", CharterDecisionRequest{Decision: trace.CharterRatify})
	apiError(t, err, NotFound, "workstream "+string(stream)+" has no charter proposal for question 9")
	_, err = f.c.CharterProposal(ctx, quiet, "1")
	apiError(t, err, NotFound, "workstream "+string(quiet)+" has no charter proposal for question 1")

	ratified, err := f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: trace.CharterRatify, Note: "It is how we work."})
	must(t, err)
	if ratified.State != trace.CharterRatified || ratified.Decision != trace.CharterRatify || ratified.Note != "It is how we work." {
		t.Fatalf("ratified %+v", ratified)
	}
	f.settle(t)
	chartered, err := f.c.CharterProposal(ctx, stream, "1")
	must(t, err)
	if chartered.State != trace.CharterChartered || chartered.Number != 2 || chartered.Charter == 0 {
		t.Fatalf("chartered %+v", chartered)
	}
	content, err := os.ReadFile(f.charterFile(t))
	must(t, err)
	if string(content) != withRule(charterBefore, 2) {
		t.Fatalf("charter.md:\n%s", content)
	}
	checkChartered(t, f.s.active.repository, withRule(charterBefore, 2), 2)
	if st, err := f.c.Status(ctx, stream); err != nil || len(st.Gates) != 0 {
		t.Fatalf("status gates after the decision %+v: %v", st.Gates, err)
	}
	if list, err := f.c.CharterProposals(ctx); err != nil || len(list.Proposals) != 0 {
		t.Fatalf("proposals after the decision %+v: %v", list, err)
	}
	again, err := f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: trace.CharterRatify})
	must(t, err)
	if again.State != trace.CharterChartered || again.Detail != "the owner's decision to ratify the charter proposal of question 1 is already recorded" {
		t.Fatalf("ratified again %+v", again)
	}
	_, err = f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: trace.CharterDecline})
	apiError(t, err, Conflict, "the charter proposal of question 1 is already decided: ratify")
	revisions := len(charterRevisions(t, f.s.active.repository))
	f.stop(t)

	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer repo.Close()
	if got := len(charterRevisions(t, repo)); got != revisions {
		t.Fatalf("%d charter revisions after a restart, was %d", got, revisions)
	}
	checkChartered(t, repo, withRule(charterBefore, 2), 2)
}

// The owner declines the proposal through the chief of staff. The charter
// and every bundle stay as they were, and only the chief of staff hears of
// it.
func TestDeclinedCharterProposalChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer func() { _ = repo.Close() }()
	before := charterRevisions(t, repo)
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_message_1", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Decline the charter rule."})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-message", filepath.Join(t.TempDir(), "message"), demoStart)
	must(t, err)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "message_1"}
	out, err := controls.decideCharter(repo, scope).Handle(ctx, json.RawMessage(`{"question":"1","decision":"decline","note":"It is this feature's call."}`))
	must(t, err)
	if want := `{"recorded":true,"question":"1","state":"declined","detail":"the owner declined the charter proposal of question 1; the charter is unchanged: It is this feature's call."}`; string(out) != want {
		t.Fatalf("decide_charter: %s", out)
	}
	proposals, err := repo.CharterProposals()
	must(t, err)
	if len(proposals) != 1 || proposals[0].Latest.Actor != ownerActor || proposals[0].Latest.Cause != "request_message_1" || proposals[0].Proposal.Decision != trace.CharterDecline {
		t.Fatalf("declined proposal %+v", proposals)
	}
	must(t, repo.Close())

	f.start(t)
	_, err = f.c.DecideCharter(ctx, stream, "1", CharterDecisionRequest{Decision: trace.CharterRatify})
	apiError(t, err, Conflict, "the charter proposal of question 1 is already decided: decline")
	if st, err := f.c.Status(ctx, stream); err != nil || len(st.Gates) != 0 {
		t.Fatalf("status gates %+v: %v", st.Gates, err)
	}
	f.stop(t)
	repo = f.open(t)
	if got := charterRevisions(t, repo); !reflect.DeepEqual(got, before) {
		t.Fatalf("charter revisions %+v, were %+v", got, before)
	}
	content, err := os.ReadFile(f.charterFile(t))
	must(t, err)
	if string(content) != charterBefore {
		t.Fatalf("charter.md:\n%s", content)
	}
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		if b := assemble(t, repo, ws); len(b.CharterNotices) != 0 || !strings.HasSuffix(b.Render(), "## Notices\nNo project-wide notices.\n") {
			t.Fatalf("bundle of %s:\n%s", ws, b.Render())
		}
	}
	if events := charterEvents(t, repo, stream); !reflect.DeepEqual(events, []string{"the owner declined the charter proposal of question 1; the charter is unchanged: It is this feature's call."}) {
		t.Fatalf("chief of staff notices %q", events)
	}
}

// An owner edit of the charter made while the ratified rule is written is
// recorded as the owner's revision, not overwritten: the rule is appended
// after it with the next number.
func TestCharterWriteKeepsAConcurrentOwnerEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	defer repo.Close()
	if _, api := f.s.recordCharterDecision(ctx, project, stream, repo, "1", CharterDecisionRequest{Decision: trace.CharterRatify}, ownerActor, "owner-charter"); api != nil {
		t.Fatal(api)
	}
	edited := charterBefore + "2. Owner's own rule.\n"
	edits := 0
	f.s.boundary = func(name string) error {
		if name == "charter-write" && edits == 0 {
			edits++
			return os.WriteFile(f.charterFile(t), []byte(edited), 0600)
		}
		return nil
	}
	must(t, charterer{s: f.s, repository: repo}.Pass(ctx))
	if edits != 1 {
		t.Fatalf("%d edits", edits)
	}
	revisions := charterRevisions(t, repo)
	owner := revisions[len(revisions)-2]
	if owner.Content != edited || owner.Actor != ownerActor || owner.Cause != "owner-edit" {
		t.Fatalf("the owner's edit is not recorded before the rule: %+v", owner)
	}
	content, err := os.ReadFile(f.charterFile(t))
	must(t, err)
	if string(content) != withRule(edited, 3) {
		t.Fatalf("charter.md:\n%s", content)
	}
	checkChartered(t, repo, withRule(edited, 3), 3)
}

// A stop after the ratification and before the charter write, and another
// after the write and before the proposal records it, leave the rule to the
// next lifetime, which records it once and notifies once.
func TestCharterRuleIsWrittenOnceAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCharterFixture(t)
	f.start(t)
	f.stop(t)
	repo := f.open(t)
	ratified, api := f.s.recordCharterDecision(ctx, project, stream, repo, "1", CharterDecisionRequest{Decision: trace.CharterRatify}, ownerActor, "owner-charter")
	if api != nil || ratified.State != trace.CharterRatified {
		t.Fatalf("ratified %+v: %v", ratified, api)
	}
	rules := charterer{s: f.s, repository: repo}
	crash := errors.New("crash")
	for _, step := range []string{"charter-write", "charter-written"} {
		f.s.boundary = func(name string) error {
			if name == step {
				return crash
			}
			return nil
		}
		if err := rules.Pass(ctx); !errors.Is(err, crash) {
			t.Fatalf("pass stopping at %s: %v", step, err)
		}
		want := 0
		if step == "charter-written" {
			want = 1
		}
		if got := appended(t, repo); len(got) != want {
			t.Fatalf("stopped at %s: %d charter revisions append the rule", step, len(got))
		}
		if p, err := repo.CharterProposals(); err != nil || p[0].State.Value != trace.CharterRatified {
			t.Fatalf("stopped at %s: %+v %v", step, p, err)
		}
	}
	f.s.boundary = nil
	must(t, repo.Close())

	f.start(t)
	f.stop(t)
	repo = f.open(t)
	defer repo.Close()
	checkChartered(t, repo, withRule(charterBefore, 2), 2)
}
