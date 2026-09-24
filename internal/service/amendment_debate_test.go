package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
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
		f.script(turn, nil, nil)
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
}
