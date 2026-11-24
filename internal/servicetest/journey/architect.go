package journey

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ArchitectFixture is an active project with a ruled charter, a clone whose
// tracked files seed the entity map, and the fake engine the architect's
// turns run in.
type ArchitectFixture struct {
	Opts        Options
	S           *Service
	ServiceDone chan struct{}
	C           *Client
	Project     config.ProjectID
	Trace       string
	Clone       string
	Engine      *fixturekit.DemoEngine
	Sessions    *fixturekit.DemoSessions
	Clock       *fixturekit.DemoClock

	ScenarioOnce  sync.Once
	ScenarioState *intakefixture.ArchitectScenario
}

func NewArchitectOptions(t *testing.T) (Options, string, *fixturekit.DemoEngine, *fixturekit.DemoSessions, *fixturekit.DemoClock) {
	t.Helper()
	opts, clone := ProjectFixture(t)
	engine, sessions, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	return opts, clone, engine, sessions, clock
}

func NewArchitectFixture(t *testing.T, configure ...func(*Options)) *ArchitectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := NewArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f.Start(t)
	f.Project, f.Trace = intakefixture.AddArchitectProject(t, f.C, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { WriteCharter(t, f.C, p, text) })
	f.S.Cfg = f.S.Current()
	return f
}

func (f *ArchitectFixture) Start(t *testing.T) {
	t.Helper()
	f.S, f.C, f.ServiceDone = intakefixture.StartArchitect(t, func(ctx context.Context) (*Service, error) { return Start(ctx, f.Opts) }, NewClient, func(c *Client) {
		if f.Opts.WriteTimeout > c.DefaultTimeout {
			c.DefaultTimeout = f.Opts.WriteTimeout
			c.Client.SetTimeoutForTest(c.DefaultTimeout)
		}
	})
}

func (f *ArchitectFixture) Stop(t *testing.T) {
	t.Helper()
	intakefixture.StopArchitect(t, f.C.Close, f.S.Close, f.ServiceDone)
}

func (f *ArchitectFixture) HandIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	return f.Scenario().HandIn(t, f.Project, key, content)
}

func (f *ArchitectFixture) Repository() *trace.Repository { return f.S.Sole().Repository }

// Script installs the fake architect's behaviour for one turn: it delivers
// the given files and returns a successful result.
func (f *ArchitectFixture) Script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.Scenario().Script(turn, files, check)
}

// Delivered returns what each draft_write the scripted turn made returned.
func (f *ArchitectFixture) Delivered(turn string) []intake.DraftStatus {
	return f.Scenario().Delivered(turn)
}

func (f *ArchitectFixture) Runs() []string {
	return f.Scenario().Runs()
}

// Await polls the workstream's feature and draft states until want accepts
// them.
func (f *ArchitectFixture) Await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	f.Scenario().Await(t, stream, want)
}

func (f *ArchitectFixture) TransitionRecorded(t *testing.T, stream config.WorkstreamID, id string) func(string, string) bool {
	t.Helper()
	return f.Scenario().TransitionRecorded(t, stream, id)
}

func (f *ArchitectFixture) Documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	return f.Scenario().Documents(t, stream, id)
}

func (f *ArchitectFixture) Transitions(t *testing.T, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	return f.Scenario().Transitions(t, stream)
}

func (f *ArchitectFixture) DraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.Scenario().DraftOperations(t, stream)
}

// AcknowledgedDraftOperations returns the draft operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *ArchitectFixture) AcknowledgedDraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.Scenario().AcknowledgedDraftOperations(t, stream)
}

func (f *ArchitectFixture) ArchitectThread(t *testing.T, stream config.WorkstreamID) trace.Thread {
	t.Helper()
	return f.Scenario().ArchitectThread(t, stream)
}

func (f *ArchitectFixture) Scenario() *intakefixture.ArchitectScenario {
	f.ScenarioOnce.Do(func() {
		f.ScenarioState = &intakefixture.ArchitectScenario{Client: func() intakefixture.WorkflowClient { return f.C }, Engine: f.Engine, Repository: f.Repository, Changes: func(t *testing.T) (func(), func()) { return ServiceChanges(t, f.S) }}
	})
	return f.ScenarioState
}

// DraftMoves lists the states the workstream's drafts went through, by
// transition.
func (f *ShedFixture) DraftMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == intake.DraftSubject {
			moves = append(moves, tr.ID+" "+tr.From+" -> "+tr.To)
		}
	}
	return moves
}

// IndependentPlan is intakefixture.ValidPlan with no dependency between its units.
const IndependentPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

// BuildOperations returns the workstream's build operations.
func (f *ArchitectFixture) BuildOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.Scenario().BuildOperations(t, stream)
}

// Built hands in a design that skips debate, ratifies it and returns the
// workstream once its build is acknowledged.
func (f *ShedFixture) Built(t *testing.T) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.BuiltAs(t, "design")
}

// BuiltAs is built with the design handed in under key.
func (f *ShedFixture) BuiltAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.Scenario().BuiltAs(t, key)
}

// BuildOperation returns the build operation of seal k of the workstream.
func (f *ArchitectFixture) BuildOperation(stream config.WorkstreamID, k int) coreadapter.Operation {
	_, event := intake.BuildIDs(k)
	input, _ := json.Marshal(intake.BuildInput{Seal: k})
	return coreadapter.Operation{ID: trace.OperationID(f.Project, stream, event), Boundary: coreadapter.RepositoryBoundary, Action: intake.BuildAction, Input: input}
}

// CheckUnits checks the unit states the status API lists for the workstream,
// alone and in the list.
func (f *ArchitectFixture) CheckUnits(t *testing.T, stream config.WorkstreamID, want []api.UnitStatus) {
	t.Helper()
	wait, close := ServiceChanges(t, f.S)
	t.Cleanup(close)
	intakefixture.CheckUnits(t, f.C, f.CheckStatusError, wait, stream, want)
}

func (f *ArchitectFixture) CheckStatusError(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	intakefixture.CheckStatusError(t, intakefixture.StatusService{Done: f.S.Done, Wait: f.S.Wait, Socket: f.S.Socket, Ready: f.S.Ready.Load}, err, elapsed)
}

// LandUpstream commits a new directory on the fixture's upstream through a
// scratch clone and returns the commit. The service's clone does not fetch it.
func (f *ArchitectFixture) LandUpstream(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Dir(f.Clone)
	scratch := t.TempDir()
	fixturekit.DemoGit(t, home, "clone", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(scratch, filepath.FromSlash(dir)), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(scratch, filepath.FromSlash(dir), "doc.go"), []byte("package x\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", scratch, "add", "--all")
	fixturekit.DemoGit(t, home, "-C", scratch, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "land "+dir)
	fixturekit.DemoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

// RemoveUpstream commits the removal of a directory on the fixture's upstream.
func (f *ArchitectFixture) RemoveUpstream(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Dir(f.Clone)
	scratch := t.TempDir()
	fixturekit.DemoGit(t, home, "clone", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	fixturekit.DemoGit(t, home, "-C", scratch, "rm", "-rq", dir)
	fixturekit.DemoGit(t, home, "-C", scratch, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "remove "+dir)
	fixturekit.DemoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

// UpstreamRef returns the commit the clone's remote-tracking ref of upstream's
// main branch is at.
func (f *ArchitectFixture) UpstreamRef(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "rev-parse", "refs/remotes/upstream/main"))
}

// Upstream gives the fixture's clone a local bare upstream, named by the
// project's upstream repository, as a second remote, and returns the commit
// its main branch is at.
func (f *ArchitectFixture) Upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.Clone)
}

// AwaitFeature waits until the workstream's feature state is the wanted one.
func (f *ArchitectFixture) AwaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	f.Scenario().AwaitFeature(t, stream, want)
}

// SealMoves lists the seal subject's values in order.
func (f *ArchitectFixture) SealMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == intake.SealSubject {
			if tr.Actor != intake.SealingActor {
				t.Fatalf("seal transition by %+v", tr)
			}
			moves = append(moves, tr.To)
		}
	}
	return moves
}

// SealOperations returns the workstream's sealing operations in sealing
// order.
func (f *ArchitectFixture) SealOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.Repository().Operations(stream)
	fixturekit.Must(t, err)
	var out []trace.OperationRecord
	number := map[string]int{}
	for _, o := range ops {
		if o.Operation.Action == intake.SealAction {
			in, err := intake.DecodeSeal(o.Operation)
			fixturekit.Must(t, err)
			number[o.EventID] = in.Seal
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b trace.OperationRecord) int { return number[a.EventID] - number[b.EventID] })
	return out
}

// AwaitSealMove waits until the seal subject has reached the wanted value.
func (f *ArchitectFixture) AwaitSealMove(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	f.S.Until(t, fmt.Sprintf("workstream %s seal subject to reach %q", stream, want), func() bool {
		return slices.Contains(f.SealMoves(t, stream), want)
	})
}

// AwaitSealFailure waits until sealing k has recorded its failure, whether
// or not the subject moved for it, and returns the transition.
func (f *ArchitectFixture) AwaitSealFailure(t *testing.T, stream config.WorkstreamID, k int) trace.Transition {
	t.Helper()
	id := fmt.Sprintf("seal-%d-failed", k)
	var failure trace.Transition
	f.S.Until(t, fmt.Sprintf("sealing %d of workstream %s to record its failure", k, stream), func() bool {
		for _, tr := range f.Transitions(t, stream) {
			if tr.ID == id {
				failure = tr
				return true
			}
		}
		return false
	})
	return failure
}

// AwaitRetry waits until sealing k has recorded a retry whose reason contains
// the given text, and returns the reason.
func (f *ArchitectFixture) AwaitRetry(t *testing.T, stream config.WorkstreamID, k int, contains string) string {
	t.Helper()
	var reason string
	f.S.Until(t, fmt.Sprintf("sealing %d of workstream %s to record a retry saying %q", k, stream, contains), func() bool {
		for _, o := range f.SealOperations(t, stream) {
			for _, a := range o.History {
				if a.Kind == "retry" && strings.Contains(a.Failure, contains) {
					reason = a.Failure
					return true
				}
			}
		}
		return false
	})
	return reason
}

// Notice returns the body of the notice the given transition told the chief
// of staff with.
func (f *ArchitectFixture) Notice(t *testing.T, stream config.WorkstreamID, transition string) string {
	t.Helper()
	return intakefixture.Notice(t, f.Repository(), stream, transition)
}

// Ratified debates one round to consensus and ratifies it, returning the
// workstream and the ratification response.
func (f *ShedFixture) Ratified(t *testing.T) (config.WorkstreamID, api.RatifyResponse) {
	t.Helper()
	return f.Scenario().Ratified(t)
}

// RouteRecords returns the route journal of the workstream's exits, the latest
// revision of each.
func (f *ArchitectFixture) RouteRecords(t *testing.T, stream config.WorkstreamID) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.Repository(), stream)
	fixturekit.Must(t, err)
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if _, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []router.Record
	for _, id := range order {
		var rec router.Record
		fixturekit.Must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		out = append(out, rec)
	}
	return out
}

func (f *ArchitectFixture) RouteOf(t *testing.T, stream config.WorkstreamID, trigger string) (router.Record, bool) {
	t.Helper()
	for _, rec := range f.RouteRecords(t, stream) {
		if rec.Trigger == trigger {
			return rec, true
		}
	}
	return router.Record{}, false
}

// AwaitSubjectMove waits for a transition of the subject into the value and
// returns it.
func (f *ArchitectFixture) AwaitSubjectMove(t *testing.T, stream config.WorkstreamID, subject, to string) trace.Transition {
	t.Helper()
	var move trace.Transition
	f.S.Until(t, fmt.Sprintf("workstream %s to move %s to %s", stream, subject, to), func() bool {
		for _, tr := range f.Transitions(t, stream) {
			if tr.Subject == subject && tr.To == to {
				move = tr
				return true
			}
		}
		return false
	})
	return move
}

// NewDesignFixture starts a service with an active project whose charter
// has rules and whose clone has one commit, the same shape NewHandInFixture
// gives an ordinary hand-in.
func NewDesignFixture(t *testing.T) (*Service, *Client, config.ProjectID) {
	t.Helper()
	f := NewHandInFixture(t)
	return f.S, f.C, f.Project
}

type HandInFixture struct {
	S       *Service
	C       *Client
	Issues  *fixturekit.FakeIssues
	Project config.ProjectID
	Trace   string
	Home    string
}

// NewHandInFixture starts a service with an active project whose charter has
// rules.
func NewHandInFixture(t *testing.T) *HandInFixture {
	t.Helper()
	opts, clone := ProjectFixture(t)
	f := &HandInFixture{}
	data := intakefixture.NewHandInSetup(t, clone, func(fake *fixturekit.FakeIssues) intakefixture.ArchitectProjectClient {
		opts.Issues = fake
		f.S, f.C = StartT(t, opts)
		return f.C
	}, func(t *testing.T, project config.ProjectID, text string) { WriteCharter(t, f.C, project, text) })
	f.Issues, f.Project, f.Trace, f.Home = data.Issues, data.Project, data.Trace, data.Home
	return f
}

func (f *HandInFixture) HandIn(t *testing.T, req api.HandInRequest) api.HandInResponse {
	t.Helper()
	return intakefixture.HandIn(t, f.C, f.Project, req)
}

func (f *HandInFixture) Repository() *trace.Repository { return f.S.Sole().Repository }

// Head returns how many commits the project's trace and each of its
// workstreams' traces hold.
func (f *HandInFixture) Head(t *testing.T) map[string]int {
	t.Helper()
	return intakefixture.HandInHead(t, f.Trace, func() []config.WorkstreamID { return f.Streams(t) }, func(stream config.WorkstreamID) string {
		return fixturekit.WorkstreamTraceDirectory(t, f.S.Current().Root, f.Project, stream)
	})
}

// Unchanged reports whether nothing was committed since head and no
// workstream was created.
func (f *HandInFixture) Unchanged(t *testing.T, head map[string]int) bool {
	t.Helper()
	return intakefixture.HandInUnchanged(t, f.Head(t), head)
}

// Streams lists the project's workstreams.
func (f *HandInFixture) Streams(t *testing.T) []config.WorkstreamID {
	t.Helper()
	return intakefixture.Streams(t, f.S.Current().Root, f.Project)
}

// CheckHanded verifies everything a successful hand-in records.
func (f *HandInFixture) CheckHanded(t *testing.T, out api.HandInResponse, key, name, source, content string) {
	t.Helper()
	ctx := context.Background()
	stream := intake.HandInWorkstream(f.Project, key)
	if out.Project != f.Project || out.Workstream != stream || out.State != intake.HandedState || out.Source != source {
		t.Fatalf("response %+v", out)
	}
	data, err := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, stream, "handed/"+name)
	fixturekit.Must(t, err)
	if string(data) != content {
		t.Fatalf("handed bytes %q, want %q", data, content)
	}
	r := f.Repository()
	docs, err := trace.Read[trace.Document](r, stream)
	fixturekit.Must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	if len(docs) != 1 || docs[0].Path != "handed/"+name || docs[0].Content != content || docs[0].Source != source || docs[0].Actor != owner {
		t.Fatalf("handed document %+v", docs)
	}
	transitions, err := trace.Read[trace.Transition](r, stream)
	fixturekit.Must(t, err)
	transitions = slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != trace.FeatureSubject })
	if len(transitions) != 1 {
		t.Fatalf("transitions %+v", transitions)
	}
	tr := transitions[0]
	if tr.Subject != trace.FeatureSubject || tr.From != "" || tr.To != "handed" || tr.Actor != owner || !strings.Contains(tr.Reason, "owner handed in handed/"+name+" from "+source) {
		t.Fatalf("transition %+v", tr)
	}
	if events := intakefixture.AllTransitions(t, f.S.Current().Root, f.Project, stream); !slices.ContainsFunc(events, func(tr trace.Transition) bool { return tr.To == "handed" }) {
		t.Fatalf("transitions: %+v", events)
	}
	state, err := r.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if state != (trace.WorkflowState{Version: 1, Value: "handed"}) {
		t.Fatalf("state %+v", state)
	}
	outbox, err := r.Outbox(stream)
	fixturekit.Must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if intakefixture.ChiefEvent(e.Event) {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || notices[0].TransitionID != tr.ID || !strings.Contains(notices[0].Event.Body, "changed to handed") {
		t.Fatalf("outbox %+v", outbox)
	}
	th, err := r.ChiefOfStaffThread(stream)
	fixturekit.Must(t, err)
	if th.Identity.Role != trace.ChiefOfStaff {
		t.Fatalf("chief of staff %+v", th)
	}
	if _, err := r.EnsureChiefOfStaff(ctx, stream, time.Now(), owner); err != nil {
		t.Fatal(err)
	}
}
