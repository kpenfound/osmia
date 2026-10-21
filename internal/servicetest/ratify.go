package servicetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// AwaitFeature waits on notifications before rereading the recorded feature state.
func (f *Project) AwaitFeature(t *testing.T, id config.WorkstreamID, want string) {
	t.Helper()
	wait, release := ServiceChanges(t, f.S)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		state, err := f.Repository(t).Workflow(id, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s stayed %q, want %q", id, state.Value, want)
		}
		wait()
	}
}

// ShedAs hands in a workstream with debate skipped and awaits its packet.
// The fixture supplies the drafting runner when it constructs the Project.
func (f *Project) ShedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	text := fixturekit.HandedDesign
	out, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.ID, Key: key, Stdin: &text, SkipDebate: true})
	fixturekit.Must(t, err)
	wait, release := ServiceChanges(t, f.S)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		packet, err := f.C.Packet(context.Background(), out.Workstream)
		var apiErr *api.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != api.NotFound) {
			t.Fatal(err)
		}
		if err == nil && packet.Packet.Recommendation == "ratify: no objection stands" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("packet did not settle: %+v %v", packet, err)
		}
		wait()
	}
	f.AwaitFeature(t, out.Workstream, service.InShedState)
	return out.Workstream
}

// RatifiedBuild ratifies the packet and awaits the acknowledged build operation.
func (f *Project) RatifiedBuild(t *testing.T, id config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	packet, err := f.C.Packet(context.Background(), id)
	fixturekit.Must(t, err)
	_, err = f.C.Ratify(context.Background(), id, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	fixturekit.Must(t, err)
	f.AwaitFeature(t, id, service.BuildingState)
	wait, release := ServiceChanges(t, f.S)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		ops, err := f.Repository(t).Operations(id)
		fixturekit.Must(t, err)
		var builds []trace.OperationRecord
		for _, op := range ops {
			if op.Operation.Action == service.BuildAction {
				builds = append(builds, op)
			}
		}
		if len(builds) == 1 && builds[0].Acknowledged && builds[0].Result != nil {
			if builds[0].Result.Outcome != "succeeded" {
				t.Fatalf("build failed: %+v", builds[0])
			}
			return builds[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("build operations did not settle: %+v", builds)
		}
		wait()
	}
}

// BuiltAs exercises hand-in and ratification through the running service.
func (f *Project) BuiltAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	id := f.ShedAs(t, key)
	return id, f.RatifiedBuild(t, id)
}

// BuiltPaused sets the owner pause after drafting and before ratification.
func (f *Project) BuiltPaused(t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	var ids []config.WorkstreamID
	for _, key := range keys {
		ids = append(ids, f.ShedAs(t, key))
	}
	Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	for _, id := range ids {
		f.RatifiedBuild(t, id)
	}
	return ids
}
