package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func proposedAmendment(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	seedAmendment(t, f.architectFixture, repo, stream)
	ctx := context.Background()
	at := f.clock.Now()
	docs := []trace.Document{}
	for _, item := range []struct{ id, path, content string }{{"amendment_1_spec", "amendments/1/spec.md", strings.Replace(validSpec, "last acknowledged chunk", "durable checkpoint", 1)}, {"amendment_1_plan", "amendments/1/plan.json", validPlan}, {"amendment_1_affected", "amendments/1/affected.json", `{"criteria":["spec#1"],"units":["resume"],"proofs":["resume:spec#1"]}`}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: architectActor, Cause: "fixture"}, Path: item.path, Content: item.content})
	}
	tx := trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "amendment-1-proposed", Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: architectActor, Cause: "fixture"}, Subject: amendmentSubject("1"), From: "filed", To: "proposed", Reason: "proposal"}}
	_, err := repo.RecordDocumentsWith(ctx, docs, tx)
	must(t, err)
}
func amendmentOp(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == action {
			return op.Operation
		}
	}
	t.Fatalf("missing operation %s", action)
	return coreadapter.Operation{}
}
func TestAmendmentShedPresentation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind shed.Kind
		want string
	}{{"consensus", "", "ratify: no objection"}, {"charter-veto", shed.Charter, "do not ratify"}, {"fit-advice", shed.Fit, "ratify: nothing blocks"}, {"size-split", shed.Size, "do not ratify"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDebateFixture(t, 2, 3)
			f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
			for i := 1; i <= 2; i++ {
				member := committeeAgent(i)
				turn := "amend-1-round-1-" + member + "-1"
				f.script(turn, nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
					if i != 1 || tc.kind == "" {
						return nil
					}
					part, citation := "plan#resume", "spec#1"
					if tc.kind == shed.Charter {
						citation = "charter#1"
					}
					if tc.kind == shed.Fit {
						part = "plan"
					}
					_, err := callTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(tc.kind), "part": part, "argument": "The proposal needs owner attention.", "citations": []string{citation}})
					return err
				})
			}
			f.script("amend-1-reply-1", nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				if tc.kind == "" {
					return nil
				}
				_, err := callTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, committeeAgent(1), 1), "answer": "The owner should consider this objection."})
				return err
			})
			stream := f.handIn(t, "amend-debate-"+tc.name, handedDesign)
			f.await(t, stream, sketched)
			f.stop(t)
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			f.s.active = &activeProject{repository: repo}
			proposedAmendment(t, f, repo, stream)
			a := amendmentDebate{&debate{s: f.s, repository: repo}}
			must(t, a.Pass(context.Background()))
			round := amendmentOp(t, repo, stream, AmendmentRoundAction)
			_, err = a.Apply(context.Background(), round)
			must(t, err)
			records, err := amendmentRecords(repo, stream, "1")
			must(t, err)
			if len(records) != 2 {
				t.Fatalf("records: %+v", records)
			}
			must(t, a.Pass(context.Background()))
			reply := amendmentOp(t, repo, stream, AmendmentReplyAction)
			_, err = a.Apply(context.Background(), reply)
			must(t, err)
			must(t, a.Pass(context.Background()))
			state, err := repo.Workflow(stream, amendmentSubject("1"))
			must(t, err)
			if state.Value != "presented" {
				t.Fatalf("state %s", state.Value)
			}
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			var packet string
			for _, doc := range docs {
				if doc.Path == amendmentPacketPath("1") {
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
			must(t, json.Unmarshal([]byte(packet), &body))
			if tc.kind == shed.Charter && (len(body.Dissent) != 1 || !body.Dissent[0].Blocking) {
				t.Errorf("veto lost: %+v", body.Dissent)
			}
			if tc.kind == shed.Fit && (len(body.Dissent) != 1 || body.Dissent[0].Blocking) {
				t.Errorf("advice lost: %+v", body.Dissent)
			}
			if tc.kind == "" && len(body.Dissent) != 0 {
				t.Errorf("unexpected dissent: %+v", body.Dissent)
			}
			outbox, err := repo.Outbox(stream)
			must(t, err)
			found := false
			for _, entry := range outbox {
				if entry.TransitionID == "amendment-1-presented" {
					found = true
					for _, part := range []string{"Proposed change: Revise the criterion", "Affected units and proofs:", "Recommendation:", "The round cap approves nothing"} {
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
			must(t, err)
			if feature.Value != BuildingState {
				t.Errorf("owner gate bypassed: %s", feature.Value)
			}
		})
	}
}

func TestAmendmentRoundResumesCompletedMember(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 3)
	for i := 1; i <= 2; i++ {
		turn := "amend-1-round-1-" + committeeAgent(i) + "-1"
		if i == 1 {
			f.script(turn, nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
				_, err := callTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": string(shed.Fit), "part": "plan", "argument": "Keep the checkpoint proof visible.", "citations": []string{"spec#1"}})
				return err
			})
		} else {
			f.script(turn, nil, nil)
		}
	}
	f.script("amend-1-reply-1", nil, nil)
	stream := f.handIn(t, "amend-restart", handedDesign)
	f.await(t, stream, sketched)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.active = &activeProject{repository: repo}
	proposedAmendment(t, f, repo, stream)
	a := amendmentDebate{&debate{s: f.s, repository: repo}}
	must(t, a.Pass(context.Background()))
	round := amendmentOp(t, repo, stream, AmendmentRoundAction)
	in := roundInput{Round: 1, Spec: 1, Plan: 1, Amendment: "1"}
	first := committeeAgent(1)
	must(t, a.enqueue(context.Background(), f.s.current(), stream, in, first, 1, round.ID, nil))
	_, err = a.dispatch(context.Background(), stream, in, first, in.memberPrefix(first)+"1")
	must(t, err)
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	f.s.active = &activeProject{repository: repo}
	a = amendmentDebate{&debate{s: f.s, repository: repo}}
	_, err = a.Apply(context.Background(), round)
	must(t, err)
	records, err := amendmentRecords(repo, stream, "1")
	must(t, err)
	if len(records) != 2 {
		t.Fatalf("lost round records: %+v", records)
	}
	for _, turn := range f.runs() {
		if turn == in.memberPrefix(first)+"2" {
			t.Fatalf("duplicated first member: %v", f.runs())
		}
	}
	must(t, a.Pass(context.Background()))
	reply := amendmentOp(t, repo, stream, AmendmentReplyAction)
	_, err = a.Apply(context.Background(), reply)
	must(t, err)
	must(t, repo.Close())
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	f.s.active = &activeProject{repository: repo}
	a = amendmentDebate{&debate{s: f.s, repository: repo}}
	_, err = a.Apply(context.Background(), reply)
	must(t, err)
	must(t, a.Pass(context.Background()))
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	replyCount, packetCount := 0, 0
	for _, doc := range docs {
		if doc.Path == amendmentReplyPath("1", 1) {
			replyCount++
		}
		if doc.Path == amendmentPacketPath("1") {
			packetCount++
		}
	}
	if replyCount != 1 || packetCount != 1 {
		t.Fatalf("restart duplicated artifacts: reply %d packet %d", replyCount, packetCount)
	}
	packet := readDocs(t, repo, stream, "amendment-1-presented-packet")
	if len(packet) != 1 || !strings.Contains(packet[0].Content, "Keep the checkpoint proof visible.") {
		t.Fatalf("restart lost the first member's objection: %+v", packet)
	}
}

// The amendment rounds of two workstreams run at the same time under the
// service's reconciliation options: each round's held member is in flight
// while the other's is, and a member that ended leaves the operation lock
// free while its round's other member runs. Released, each amendment is heard
// once with a record of every member.
func TestAmendmentRoundsOfTwoWorkstreamsRunAtTheSameTime(t *testing.T) {
	t.Parallel()
	var committee *Committee
	// Without a committee runner the workstreams stay sketched, with no shed
	// round, until the amendments are seeded.
	f := newDebateFixtureWith(t, 2, 1, "", func(o *Options) { committee, o.Committee = o.Committee, nil })
	var mu sync.Mutex
	// Each member's session directory lies under its workstream's.
	var held []string
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, nil)
	f.script("amend-1-round-1-"+committeeAgent(2)+"-1", nil, func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
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
	first := f.handIn(t, "amend-first", handedDesign)
	second := f.handIn(t, "amend-second", handedDesign)
	f.await(t, first, sketched)
	f.await(t, second, sketched)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	f.s.active = &activeProject{repository: repo}
	f.s.options.Committee = committee
	streams := []config.WorkstreamID{first, second}
	for _, stream := range streams {
		proposedAmendment(t, f, repo, stream)
	}
	a := amendmentDebate{&debate{s: f.s, repository: repo}}
	must(t, a.Pass(context.Background()))
	c, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: f.clock.Now, RetryDelay: time.Minute,
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: runnerAdapter{amendRounds: a}},
		Hold:       func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != AmendmentRoundAction },
		Concurrent: concurrentOperation})
	must(t, err)
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
		th, err := repo.Thread(stream, committeeAgent(1))
		return err == nil && len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero()
	}
	deadline := time.Now().Add(overlapWait)
	for !running(first) || !running(second) || !ended(first) || !ended(second) {
		if time.Now().After(deadline) {
			stop("held members in flight: first %v, second %v; first members ended: %v, %v; want both rounds at once", running(first), running(second), ended(first), ended(second))
		}
		time.Sleep(50 * time.Millisecond)
	}
	free := make(chan struct{})
	go func() { repo.Serialize(func() error { close(free); return nil }) }()
	select {
	case <-free:
	case <-time.After(10 * time.Second):
		stop("a round's ended member took the operation lock back while its other member runs")
	}
	releaseOnce()
	must(t, <-passed)
	must(t, c.Wait())
	for _, stream := range streams {
		state, err := repo.Workflow(stream, amendmentSubject("1"))
		must(t, err)
		if state.Value != "heard" {
			t.Errorf("workstream %s amendment %q, want heard", stream, state.Value)
		}
		ops, err := repo.Operations(stream)
		must(t, err)
		ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != AmendmentRoundAction })
		if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
			t.Errorf("workstream %s amendment rounds: %+v", stream, ops)
		}
		records, err := amendmentRecords(repo, stream, "1")
		must(t, err)
		if len(records) != 2 || records[0].Failure != "" || records[1].Failure != "" {
			t.Errorf("workstream %s records: %+v", stream, records)
		}
	}
}
