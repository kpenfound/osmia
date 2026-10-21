package intakefixture

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// WorkflowClient is the local API surface used by intake fixture scenarios.
type WorkflowClient interface {
	HandIn(context.Context, api.HandInRequest) (api.HandInResponse, error)
	Ratify(context.Context, config.WorkstreamID, int, int, ...int) (api.RatifyResponse, error)
	Packet(context.Context, config.WorkstreamID) (api.PacketResponse, error)
	Inbox(context.Context) (api.InboxResponse, error)
}

func (f *ArchitectScenario) AwaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(DemoTimeout)
	for {
		state, err := f.Repository().Workflow(stream, trace.FeatureSubject)
		Must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s stayed %q, want %q", stream, state.Value, want)
		}
		wait()
	}
}

func (f *ArchitectScenario) BuildOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.Repository().Operations(stream)
	Must(t, err)
	var out []trace.OperationRecord
	for _, o := range ops {
		if o.Operation.Action == intake.BuildAction {
			out = append(out, o)
		}
	}
	return out
}

func (f *ArchitectScenario) RatifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	if _, err := f.Client().Ratify(context.Background(), stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	ops := AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.BuildOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("build operations %+v", ops)
	}
	if in, err := intake.DecodeBuild(ops[0].Operation); err != nil || in != (intake.BuildInput{Seal: 1}) {
		t.Fatalf("build operation %+v: %v", ops[0].Operation, err)
	}
	return ops[0]
}

func (f *ArchitectScenario) ShedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	stream := f.HandInSkipping(t, key)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	f.AwaitFeature(t, stream, intake.InShedState)
	return stream
}

func (f *ArchitectScenario) BuiltAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	stream := f.ShedAs(t, key)
	return stream, f.RatifiedBuild(t, stream)
}

func (f *ArchitectScenario) HandInSkipping(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	content := HandedDesign
	out, err := f.Client().HandIn(context.Background(), api.HandInRequest{Project: f.Repository().Project(), Key: key, Stdin: &content, SkipDebate: true})
	Must(t, err)
	if !out.SkipDebate || out.State != intake.HandedState {
		t.Fatalf("hand-in %+v", out)
	}
	return out.Workstream
}

func (f *ArchitectScenario) AwaitPacket(t *testing.T, stream config.WorkstreamID, recommendation string) shed.Packet {
	t.Helper()
	wait, close := f.Changes(t)
	defer close()
	deadline := time.Now().Add(DemoTimeout)
	for {
		out, err := f.Client().Packet(context.Background(), stream)
		if err != nil && !Failed(err, api.NotFound) {
			t.Fatal(err)
		}
		if err == nil && out.Packet.Recommendation == recommendation {
			if out.Workstream != stream || out.Project != f.Repository().Project() || out.Revision < 1 {
				t.Fatalf("packet response %+v", out)
			}
			return out.Packet
		}
		if time.Now().After(deadline) {
			t.Fatalf("the packet of workstream %s is %+v %v, want the recommendation %q", stream, out.Packet, err, recommendation)
		}
		wait()
	}
}

func (f *ArchitectScenario) InboxEntries(t *testing.T, stream config.WorkstreamID, kind string) []api.InboxEntry {
	t.Helper()
	inbox, err := f.Client().Inbox(context.Background())
	Must(t, err)
	var out []api.InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == kind && e.Workstream == stream {
			out = append(out, e)
		}
	}
	return out
}

func (f *ArchitectScenario) Ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	docs := f.Documents(t, stream, shed.RatificationDocumentID(round))
	if len(docs) != 1 || docs[0].Path != shed.RatificationPath(round) || docs[0].Actor != intake.OwnerActor || docs[0].Cause != "owner-shed" {
		t.Fatalf("ratification documents of round %d: %+v", round, docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	Must(t, err)
	return record
}

func (f *ArchitectScenario) DocumentRevisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	var out []int
	for _, d := range f.Documents(t, stream, id) {
		out = append(out, d.Revision)
	}
	return out
}

// Ratified debates one round to consensus and records the owner's ratification.
func (f *ArchitectScenario) Ratified(t *testing.T) (config.WorkstreamID, api.RatifyResponse) {
	t.Helper()
	p := &Faults{}
	f.Member(1, 1, 1, Concedes(p))
	stream := f.HandIn(t, f.Repository().Project(), "design", HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	out, err := f.Client().Ratify(context.Background(), stream, 1, 1)
	Must(t, err)
	return stream, out
}
