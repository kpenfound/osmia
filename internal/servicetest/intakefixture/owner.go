package intakefixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type OwnerQuestionClient interface {
	Inbox(context.Context) (api.InboxResponse, error)
	Answer(context.Context, int, string, config.ProjectID) (api.AnswerResponse, error)
}
type UnitStatusClient interface {
	Status(context.Context, config.WorkstreamID) (api.WorkstreamStatus, error)
	Statuses(context.Context) (api.StatusResponse, error)
}
type StatusService struct {
	Done   <-chan struct{}
	Wait   func() error
	Socket func() string
	Ready  func() bool
}

func DocumentRevisions(t *testing.T, repository *trace.Repository, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, "")
	Must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

func PresentAmendment(t *testing.T, repository *trace.Repository, project config.ProjectID, at time.Time, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	ctx := context.Background()
	repo := repository

	sealed, sealDoc, found, err := seal.Latest(repo, stream)
	Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	requester, actor, stage := intake.MasonAgent("resume"), intake.MasonActor, intake.UnitImplementing
	if role == intake.ReviewerRole {
		requester, actor, stage = intake.ReviewerAgent("resume"), trace.Actor{Kind: "service", ID: intake.ReviewerRole}, intake.UnitReviewing
	}
	header := func(schema, id string, by trace.Actor, cause string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: "resume", At: at, Actor: by, Cause: cause}
	}
	Must(t, repo.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", requester, actor, "fixture"), Role: role, ThreadID: requester}))
	filing := header("osmia.trace.turn-request", "request_filing", actor, "fixture")
	filing.Depth = 1
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: filing, AgentID: requester, ThreadID: requester, TurnID: "filing", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "requester system", Prompt: "Work on the unit."})
	Must(t, err)
	subject := trace.UnitSubject("resume")
	unit, err := repo.Workflow(stream, subject)
	Must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: unit.Version, Transition: trace.Transition{Header: header("osmia.trace.transition", "fixture-"+stage, intake.OwnerActor, "fixture"), Subject: subject, From: unit.Value, To: stage, Reason: "fixture"}})
	Must(t, err)
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: unit.Version + 1, Transition: trace.Transition{Header: header("osmia.trace.transition", subject+"_waiting_amendment_1", trace.Actor{Kind: "service", ID: role}, "amendment_1_filed"), Subject: subject, From: stage, To: intake.UnitWaiting, Reason: "unit resume waits for amendment 1"}})
	Must(t, err)
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", trace.Actor{Kind: "agent", ID: requester}, "request_filing"), Requester: trace.Actor{Kind: "agent", ID: requester}, Role: role, Thread: requester, Turn: "filing",
		Citations: []string{"spec#1"}, Change: "Resume from a durable checkpoint", Reason: "Acknowledgements are not durable", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	Must(t, repo.Append(ctx, request))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: intake.ArchitectActor, Cause: "fixture"}, Path: path, Content: content}
	}
	sealedPlan, err := plan.Parse([]byte(ValidPlan))
	Must(t, err)
	proposedPlan, err := plan.Parse([]byte(graph))
	Must(t, err)
	affected, err := json.Marshal(intake.AffectedRevision(ValidSpec, spec, sealedPlan, proposedPlan))
	Must(t, err)
	docs := []trace.Document{document("amendment_1_spec", "amendments/1/spec.md", spec), document("amendment_1_plan", "amendments/1/plan.json", graph),
		document("amendment_1_affected", "amendments/1/affected.json", string(affected)),
		document("amendment-1-presented-packet", intake.AmendmentPacketPath("1"), `{"round":1}`+"\n")}
	for _, r := range records {
		data, err := shed.Encode(r)
		Must(t, err)
		docs = append(docs, document(fmt.Sprintf("amendment_1_round_%d_%s", r.Round, r.Member), intake.AmendmentRoundPath("1", r.Round, r.Member), string(data)))
	}
	presented := header("osmia.trace.transition", "amendment-1-presented", intake.ShedActor, "fixture")
	presented.Unit = ""
	_, err = repo.RecordDocumentsWith(ctx, docs, trace.Transaction{Transition: trace.Transition{Header: presented, Subject: intake.AmendmentSubject("1"), To: intake.AmendmentPresented, Reason: "presented"}})
	Must(t, err)
	return requester
}

func AwaitAmendment(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, want string) {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for {
		state, err := repository.Workflow(stream, intake.AmendmentSubject("1"))
		Must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("amendment 1 stayed %q, want %q", state.Value, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func AmendmentRuling(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	th, err := repository.Thread(stream, requester)
	Must(t, err)
	var out []trace.QueuedTurn
	for _, q := range th.Turns {
		if q.Request.TurnID == intake.AmendmentRulingTurn("1") {
			out = append(out, q)
		}
	}
	return out
}

func FileAmendment(t *testing.T, project config.ProjectID, now func() time.Time, repo *trace.Repository, stream config.WorkstreamID, id string) {
	t.Helper()
	ctx := context.Background()
	turn := "turn_resume"
	if id != "1" {
		turn += "_" + id
	}
	request := trace.Amendment{Header: trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: "resume", At: now(), Actor: trace.Actor{Kind: "agent", ID: "agent_mason_resume"}, Cause: "fixture"}, Requester: trace.Actor{Kind: "agent", ID: "agent_mason_resume"}, Role: "mason", Thread: "thread_mason_resume", Turn: turn, Citations: []string{"spec#1"}, Change: "Revise the criterion", Reason: "New constraint", Seal: 1, SealRevision: 1, SpecHash: "hash"}
	if err := repo.Append(ctx, request); err != nil {
		t.Fatalf("append request: %v", err)
	}
	_, err := repo.Transact(ctx, trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "amendment_" + id + "_filed", Revision: 1, Project: project, Workstream: stream, At: now(), Actor: request.Actor, Cause: request.Cause}, Subject: intake.AmendmentSubject(id), To: "filed", Reason: "filed"}})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
}

func OwnerMoves(t *testing.T, transitions []trace.Transition) []string {
	t.Helper()
	var moves []string
	for _, tr := range transitions {
		if tr.Subject == intake.OwnerSubject {
			if tr.Actor != intake.OwnerActor {
				t.Fatalf("owner transition by %+v", tr)
			}
			moves = append(moves, tr.To)
		}
	}
	return moves
}

func Upstream(t *testing.T, clone string) string {
	t.Helper()
	home := filepath.Dir(clone)
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	if _, err := os.Stat(bare); err != nil {
		Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
		fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")

		fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	}
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", bare, "rev-parse", "refs/heads/main"))
}

func Notice(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	Must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && ChiefEvent(entry.Event) {
			return entry.Event.Body
		}
	}
	t.Fatalf("transition %s of workstream %s told the chief of staff nothing", transition, stream)
	return ""
}

func Streams(t *testing.T, root config.Root, project config.ProjectID, librarian config.WorkstreamID) []config.WorkstreamID {
	t.Helper()
	base, err := root.WorkstreamTraces(project)
	Must(t, err)
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	Must(t, err)
	var out []config.WorkstreamID
	for _, entry := range entries {
		if id := config.WorkstreamID(entry.Name()); id != librarian {
			out = append(out, id)
		}
	}
	return out
}

func Rule(t *testing.T, client OwnerQuestionClient, id string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(DemoTimeout)
	for {
		inbox, err := client.Inbox(ctx)
		Must(t, err)
		for _, e := range inbox.Entries {
			if slices.ContainsFunc(e.Asked, func(q api.InboxQuestion) bool { return q.ID == id }) {
				if _, err := client.Answer(ctx, e.Number, OwnerRuling, ""); err != nil {
					t.Fatalf("rule on inbox entry %d: %v", e.Number, err)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("question %s was never escalated: %+v", id, inbox.Entries)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func Question(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) trace.QuestionState {
	t.Helper()
	asked, err := repository.Questions(stream)
	Must(t, err)
	for _, q := range asked {
		if q.Asked.ID == id {
			return q
		}
	}
	t.Fatalf("no question %s: %+v", id, asked)
	return trace.QuestionState{}
}

func AwaitQuestion(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id, state string) {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for Question(t, repository, stream, id).State != state {
		if time.Now().After(deadline) {
			t.Fatalf("question %s never became %s: %+v", id, state, Question(t, repository, stream, id))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func Thread(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	th, err := repository.Thread(stream, member)
	Must(t, err)
	return th
}

func AwaitNamedTurn(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, member, turn string) trace.QueuedTurn {
	t.Helper()
	deadline := time.Now().Add(DemoTimeout)
	for {
		th := Thread(t, repository, stream, member)
		if i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }); i >= 0 {
			return th.Turns[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread of %s never held turn %s: %+v", member, turn, th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func CheckUnits(t *testing.T, client UnitStatusClient, checkError func(*testing.T, error, time.Duration), stream config.WorkstreamID, want []api.UnitStatus) {
	t.Helper()
	ctx := context.Background()
	started := time.Now()
	one, err := client.Status(ctx, stream)
	checkError(t, err, time.Since(started))
	if one.State == nil || *one.State != intake.BuildingState || !reflect.DeepEqual(one.Units, want) {
		t.Fatalf("status %+v, want units %+v", one, want)
	}
	started = time.Now()
	list, err := client.Statuses(ctx)
	checkError(t, err, time.Since(started))
	i := slices.IndexFunc(list.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == stream })
	if i < 0 || !reflect.DeepEqual(list.Workstreams[i].Units, want) {
		t.Fatalf("status list %+v, want units %+v", list, want)
	}
}

func CheckStatusError(t *testing.T, state StatusService, err error, elapsed time.Duration) {
	t.Helper()
	if err == nil {
		return
	}
	select {
	case <-state.Done:
		t.Fatalf("status API after %s: %v; service stopped: %v", elapsed, err, state.Wait())
	case <-time.After(10 * time.Second):
		conn, dialErr := net.DialTimeout("unix", state.Socket(), time.Second)
		if conn != nil {
			conn.Close()
		}
		info, statErr := os.Lstat(state.Socket())
		t.Fatalf("status API after %s: %v; service ready: %t; socket: %v, stat: %v; dial: %v", elapsed, err, state.Ready(), info, statErr, dialErr)
	}
}

func Answer(engine *fixturekit.DemoEngine, id string, run FakeTurn) string {
	turn := questions.TurnID(id)
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, req, verified, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Answer read", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	return turn
}

// SeedAmendment moves the sketched feature to building and files amendment 1.
// The clock advances separately for the feature transition, request and filing.
func SeedAmendment(t *testing.T, project config.ProjectID, now func() time.Time, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	_, err := repo.MoveFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: project, Workstream: stream, At: now(), Actor: intake.OwnerActor, Cause: "fixture"}, intake.SketchedState, intake.BuildingState, "build")
	if err != nil {
		t.Fatalf("move feature to building: %v", err)
	}
	FileAmendment(t, project, now, repo, stream, "1")
}
