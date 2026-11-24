package intakejourney

import (
	context "context"
	json "encoding/json"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	reconcile "github.com/kpenfound/osmia/internal/reconcile"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func proposedAmendment(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.SeedAmendment(t, f.Project, f.Clock.Now, repo, stream)
	ctx := context.Background()
	at := f.Clock.Now()
	docs := []trace.Document{}
	for _, item := range []struct{ id, path, content string }{{"amendment_1_spec", "amendments/1/spec.md", strings.Replace(fixturekit.ValidSpec, "last acknowledged chunk", "durable checkpoint", 1)}, {"amendment_1_plan", "amendments/1/plan.json", fixturekit.ValidPlan}, {"amendment_1_affected", "amendments/1/affected.json", `{"criteria":["spec#1"],"units":["resume"],"proofs":["resume:spec#1"]}`}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ArchitectActor, Cause: "fixture"}, Path: item.path, Content: item.content})
	}
	tx := trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "amendment-1-proposed", Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ArchitectActor, Cause: "fixture"}, Subject: intake.AmendmentSubject("1"), From: "filed", To: "proposed", Reason: "proposal"}}
	_, err := repo.RecordDocumentsWith(ctx, docs, tx)
	fixturekit.Must(t, err)
}

func TestAmendmentShedPresentation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind shed.Kind
		want string
	}{{"consensus", "", "approve: no objection"}, {"charter-veto", shed.Charter, "do not approve"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSketchedDebateFixture(t, 2, 3)
			stopped := false
			defer func() {
				if !stopped {
					f.Stop(t)
				}
			}()
			f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
			for i := 1; i <= 2; i++ {
				member := intake.CommitteeAgent(i)
				turn := "amend-1-round-1-" + member + "-1"
				f.Script(turn, nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
					if i != 1 || tc.kind == "" {
						return nil
					}
					_, err := fixturekit.CallTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(tc.kind), "part": "plan#resume", "argument": "The proposal needs owner attention.", "citations": []string{"charter#1"}})
					return err
				})
			}
			f.Script("amend-1-reply-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				if tc.kind == "" {
					return nil
				}
				_, err := fixturekit.CallTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, intake.CommitteeAgent(1), 1), "answer": "The owner should consider this objection."})
				return err
			})
			stream := f.HandIn(t, "amend-debate-"+tc.name, fixturekit.HandedDesign)
			f.StopAtSketch(t, stream)
			stopped = true
			repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			f.S.SetSole(&journey.ActiveProject{Repository: repo})
			proposedAmendment(t, f, repo, stream)
			a := intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}}
			fixturekit.Must(t, a.Pass(context.Background()))
			round := intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentRoundAction)
			_, err = a.Apply(context.Background(), round)
			fixturekit.Must(t, err)
			records, err := intake.AmendmentRecords(repo, stream, "1")
			fixturekit.Must(t, err)
			if len(records) != 2 {
				t.Fatalf("records: %+v", records)
			}
			fixturekit.Must(t, a.Pass(context.Background()))
			reply := intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentReplyAction)
			_, err = a.Apply(context.Background(), reply)
			fixturekit.Must(t, err)
			fixturekit.Must(t, a.Pass(context.Background()))
			state, err := repo.Workflow(stream, intake.AmendmentSubject("1"))
			fixturekit.Must(t, err)
			if state.Value != "presented" {
				t.Fatalf("state %s", state.Value)
			}
			docs, err := trace.Read[trace.Document](repo, stream)
			fixturekit.Must(t, err)
			var packet string
			for _, doc := range docs {
				if doc.Path == intake.AmendmentPacketPath("1") {
					packet = doc.Content
				}
			}
			for _, needle := range []string{"durable checkpoint", "resume", "spec#1", tc.want} {
				if !strings.Contains(packet, needle) {
					t.Errorf("packet misses %q: %s", needle, packet)
				}
			}
			var body struct {
				Recommendation string       `json:"recommendation"`
				Dissent        []shed.Entry `json:"dissent"`
			}
			fixturekit.Must(t, json.Unmarshal([]byte(packet), &body))
			if tc.kind == shed.Charter && (len(body.Dissent) != 1 || !body.Dissent[0].Blocking) {
				t.Errorf("veto lost: %+v", body.Dissent)
			}
			if tc.kind == "" && len(body.Dissent) != 0 {
				t.Errorf("unexpected dissent: %+v", body.Dissent)
			}
			outbox, err := repo.Outbox(stream)
			fixturekit.Must(t, err)
			found := false
			for _, entry := range outbox {
				if entry.TransitionID == "amendment-1-presented" {
					found = true
					for _, part := range []string{"Proposed change: Revise the criterion", "Affected units:", "Recommendation:", "The round cap approves nothing"} {
						if !strings.Contains(entry.Event.Body, part) {
							t.Errorf("presentation misses %q: %s", part, entry.Event.Body)
						}
					}
					if tc.kind != "" && !strings.Contains(entry.Event.Body, "agent_committee_1-r1-1") {
						t.Errorf("presentation lost dissent: %s", entry.Event.Body)
					}
				}
			}
			if !found {
				t.Error("chief presentation notice missing")
			}
			feature, err := repo.Workflow(stream, trace.FeatureSubject)
			fixturekit.Must(t, err)
			if feature.Value != intake.BuildingState {
				t.Errorf("owner gate bypassed: %s", feature.Value)
			}
		})
	}
}

func TestAmendmentRoundResumesCompletedMember(t *testing.T) {
	t.Parallel()
	f := newSketchedDebateFixture(t, 2, 3)
	for i := 1; i <= 2; i++ {
		turn := "amend-1-round-1-" + intake.CommitteeAgent(i) + "-1"
		if i == 1 {
			f.Script(turn, nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				_, err := fixturekit.CallTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(shed.Fit), "part": "plan", "argument": "Keep the checkpoint proof visible.", "citations": []string{"spec#1"}})
				return err
			})
		} else {
			f.Script(turn, nil, nil)
		}
	}
	f.Script("amend-1-reply-1", nil, nil)
	stream := f.HandIn(t, "amend-restart", fixturekit.HandedDesign)
	f.StopAtSketch(t, stream)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	proposedAmendment(t, f, repo, stream)
	a := intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}}
	fixturekit.Must(t, a.Pass(context.Background()))
	round := intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentRoundAction)
	in := intake.RoundInput{Round: 1, Spec: 1, Plan: 1, Amendment: "1"}
	first := intake.CommitteeAgent(1)
	fixturekit.Must(t, a.Enqueue(context.Background(), f.S.Current(), stream, in, first, 1, round.ID, nil))
	_, err = a.Dispatch(context.Background(), stream, in, first, in.MemberPrefix(first)+"1")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	a = intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}}
	_, err = a.Apply(context.Background(), round)
	fixturekit.Must(t, err)
	records, err := intake.AmendmentRecords(repo, stream, "1")
	fixturekit.Must(t, err)
	if len(records) != 2 {
		t.Fatalf("lost round records: %+v", records)
	}
	for _, turn := range f.Runs() {
		if turn == in.MemberPrefix(first)+"2" {
			t.Fatalf("duplicated first member: %v", f.Runs())
		}
	}
	fixturekit.Must(t, a.Pass(context.Background()))
	reply := intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentReplyAction)
	_, err = a.Apply(context.Background(), reply)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	a = intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}}
	_, err = a.Apply(context.Background(), reply)
	fixturekit.Must(t, err)
	fixturekit.Must(t, a.Pass(context.Background()))
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	replyCount, packetCount := 0, 0
	for _, doc := range docs {
		if doc.Path == intake.AmendmentReplyPath("1", 1) {
			replyCount++
		}
		if doc.Path == intake.AmendmentPacketPath("1") {
			packetCount++
		}
	}
	if replyCount != 1 || packetCount != 1 {
		t.Fatalf("restart duplicated artifacts: reply %d packet %d", replyCount, packetCount)
	}
	packet := intakefixture.ReadDocs(t, repo, stream, "amendment-1-presented-packet")
	if len(packet) != 1 || !strings.Contains(packet[0].Content, "Keep the checkpoint proof visible.") {
		t.Fatalf("restart lost the first member's objection: %+v", packet)
	}
}

func TestAmendmentRoundsOfTwoWorkstreamsRunAtTheSameTime(t *testing.T) {
	t.Parallel()
	var committee *intake.Committee

	f := journey.NewDebateFixtureWith(t, 2, 1, "", func(o *journey.Options) { committee, o.Committee = o.Committee, nil })
	var mu sync.Mutex
	var held []string
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(1)+"-1", nil, nil)
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(2)+"-1", nil, func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		mu.Lock()
		held = append(held, req.SessionDir)
		mu.Unlock()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	running := func(stream config.WorkstreamID) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(held, func(dir string) bool { return strings.Contains(dir, string(stream)) })
	}
	first := f.HandIn(t, "amend-first", fixturekit.HandedDesign)
	second := f.HandIn(t, "amend-second", fixturekit.HandedDesign)
	f.Await(t, first, intakefixture.Sketched)
	f.Await(t, second, intakefixture.Sketched)
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	f.S.Options.Committee = committee
	streams := []config.WorkstreamID{first, second}
	for _, stream := range streams {
		proposedAmendment(t, f, repo, stream)
	}
	a := intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}}
	fixturekit.Must(t, a.Pass(context.Background()))
	c, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: f.Clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a},
		Hold: func(_ config.WorkstreamID, op coreadapter.Operation) bool {
			return op.Action != intake.AmendmentRoundAction
		},
		Concurrent: service.ConcurrentOperation})
	fixturekit.Must(t, err)
	passed := make(chan error, 1)
	go func() { passed <- c.Pass(context.Background()) }()
	stop := func(format string, args ...any) {
		t.Helper()
		releaseOnce()
		<-passed
		c.Wait()
		t.Fatalf(format, args...)
	}
	ended := func(stream config.WorkstreamID) bool {
		th, err := repo.Thread(stream, intake.CommitteeAgent(1))
		return err == nil && len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero()
	}
	wake, releaseWake := fixturekit.Ticks(50 * time.Millisecond)
	defer releaseWake()
	overlap := time.NewTimer(overlapWait)
	defer overlap.Stop()
	for !running(first) || !running(second) || !ended(first) || !ended(second) {
		select {
		case <-overlap.C:
			stop("held members in flight: first %v, second %v; first members ended: %v, %v; want both rounds at once", running(first), running(second), ended(first), ended(second))
		default:
		}
		fixturekit.Must(t, wake())
	}
	releaseOnce()
	fixturekit.Must(t, <-passed)
	fixturekit.Must(t, c.Wait())
	for _, stream := range streams {
		state, err := repo.Workflow(stream, intake.AmendmentSubject("1"))
		fixturekit.Must(t, err)
		if state.Value != "heard" {
			t.Errorf("workstream %s amendment %q, want heard", stream, state.Value)
		}
		ops, err := repo.Operations(stream)
		fixturekit.Must(t, err)
		ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.AmendmentRoundAction })
		if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
			t.Errorf("workstream %s amendment rounds: %+v", stream, ops)
		}
		records, err := intake.AmendmentRecords(repo, stream, "1")
		fixturekit.Must(t, err)
		if len(records) != 2 || records[0].Failure != "" || records[1].Failure != "" {
			t.Errorf("workstream %s records: %+v", stream, records)
		}
	}
}
