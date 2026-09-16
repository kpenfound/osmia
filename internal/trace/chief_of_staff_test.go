package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const legacyStream config.WorkstreamID = "w_00000000000000000000000000000002"

// legacyWorkstream lays out a workstream the way traces without a
// chief-of-staff thread hold it.
func legacyWorkstream(t *testing.T, r *Repository, id config.WorkstreamID) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := "workstreams/" + string(id)
	if err := r.mkdir(prefix); err != nil {
		t.Fatal(err)
	}
	h := Header{Schema: "osmia.trace.workstream", Version: Version, ID: string(id), Revision: 1, Project: r.project, Workstream: id, At: at, Actor: owner, Cause: "workstream-create"}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.writeFile(prefix+"/workstream.json", append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	paths := []string{prefix + "/workstream.json"}
	for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
		if err := r.writeFile(prefix+"/"+name, nil); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, prefix+"/"+name)
	}
	if err := r.commit(context.Background(), paths, "Create workstream trace"); err != nil {
		t.Fatal(err)
	}
}

func chiefAgents(t *testing.T, r *Repository, stream config.WorkstreamID) []Agent {
	t.Helper()
	agents, err := Read[Agent](r, stream)
	if err != nil {
		t.Fatal(err)
	}
	var out []Agent
	for _, a := range agents {
		if a.ID == ChiefOfStaff {
			out = append(out, a)
		}
	}
	return out
}

func TestCreateWorkstreamCreatesOneChiefOfStaffThread(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	th, err := r.ChiefOfStaffThread(streamID)
	if err != nil {
		t.Fatal(err)
	}
	a := th.Identity
	if a.ID != ChiefOfStaff || a.Role != ChiefOfStaff || a.ThreadID != ChiefOfStaff || a.Workstream != streamID || !a.At.Equal(at) || a.Actor != owner || th.Status != "idle" || len(th.Turns) != 0 {
		t.Fatalf("thread = %+v", th)
	}
	if err := r.CreateWorkstream(ctx, streamID, at.Add(time.Hour), owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create = %v", err)
	}
	r.Close()
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.EnsureChiefOfStaff(ctx, streamID, at.Add(2*time.Hour), Actor{Kind: "service", ID: "osmia"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalJSON(again.Identity, a) {
		t.Fatalf("identity changed: %+v", again.Identity)
	}
	if got := chiefAgents(t, reopened, streamID); len(got) != 1 {
		t.Fatalf("chief-of-staff agents = %d", len(got))
	}
}

func TestEnsureChiefOfStaffBackfillsOnce(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	legacyWorkstream(t, r, legacyStream)
	if _, err := r.ChiefOfStaffThread(legacyStream); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy lookup = %v", err)
	}
	service := Actor{Kind: "service", ID: "osmia"}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.EnsureChiefOfStaff(ctx, legacyStream, at.Add(time.Duration(i)*time.Second), service)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.EnsureChiefOfStaff(ctx, legacyStream, at.Add(time.Hour), service); err != nil {
		t.Fatal(err)
	}
	th, err := reopened.ChiefOfStaffThread(legacyStream)
	if err != nil {
		t.Fatal(err)
	}
	if th.Identity.Workstream != legacyStream || th.Identity.Actor != service {
		t.Fatalf("thread = %+v", th.Identity)
	}
	if got := chiefAgents(t, reopened, legacyStream); len(got) != 1 {
		t.Fatalf("chief-of-staff agents = %d", len(got))
	}
}

func TestEnsureChiefOfStaffRefusesAnotherRole(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	legacyWorkstream(t, r, legacyStream)
	a := threadAgent()
	a.Workstream, a.ID = legacyStream, ChiefOfStaff
	if err := r.CreateThread(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureChiefOfStaff(ctx, legacyStream, at, owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("ensure = %v", err)
	}
}
