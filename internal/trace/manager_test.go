package trace

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const threadlessStream config.WorkstreamID = "w_00000000000000000000000000000002"

func threadlessWorkstream(t *testing.T, r *Repository, id config.WorkstreamID) {
	t.Helper()
	if err := createWorkstreamTrace(context.Background(), r.root, r.project, id, manifestRecord{}, "workstream-create", "Create workstream trace", at, owner); err != nil {
		t.Fatal(err)
	}
}

func managerAgents(t *testing.T, r *Repository, stream config.WorkstreamID) []Agent {
	t.Helper()
	agents, err := Read[Agent](r, stream)
	if err != nil {
		t.Fatal(err)
	}
	var out []Agent
	for _, a := range agents {
		if a.ID == Manager {
			out = append(out, a)
		}
	}
	return out
}

func TestCreateWorkstreamCreatesOneManagerThread(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	th, err := r.ManagerThread(streamID)
	if err != nil {
		t.Fatal(err)
	}
	a := th.Identity
	if a.ID != Manager || a.Role != Manager || a.ThreadID != Manager || a.Workstream != streamID || !a.At.Equal(at) || a.Actor != owner || th.Status != "idle" || len(th.Turns) != 0 {
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
	again, err := reopened.EnsureManager(ctx, streamID, at.Add(2*time.Hour), Actor{Kind: "service", ID: "osmia"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalJSON(again.Identity, a) {
		t.Fatalf("identity changed: %+v", again.Identity)
	}
	if got := managerAgents(t, reopened, streamID); len(got) != 1 {
		t.Fatalf("manager agents = %d", len(got))
	}
}

func TestEnsureManagerBackfillsOnce(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	threadlessWorkstream(t, r, threadlessStream)
	if _, err := r.ManagerThread(threadlessStream); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lookup before ensure = %v", err)
	}
	service := Actor{Kind: "service", ID: "osmia"}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.EnsureManager(ctx, threadlessStream, at.Add(time.Duration(i)*time.Second), service)
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
	if _, err := reopened.EnsureManager(ctx, threadlessStream, at.Add(time.Hour), service); err != nil {
		t.Fatal(err)
	}
	th, err := reopened.ManagerThread(threadlessStream)
	if err != nil {
		t.Fatal(err)
	}
	if th.Identity.Workstream != threadlessStream || th.Identity.Actor != service {
		t.Fatalf("thread = %+v", th.Identity)
	}
	if got := managerAgents(t, reopened, threadlessStream); len(got) != 1 {
		t.Fatalf("manager agents = %d", len(got))
	}
}

func TestEnsureManagerRefusesAnotherIdentity(t *testing.T) {
	for name, change := range map[string]func(*Agent){
		"role":   func(a *Agent) { a.ThreadID = Manager },
		"thread": func(a *Agent) { a.Role = Manager },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, _, _ := create(t)
			threadlessWorkstream(t, r, threadlessStream)
			a := threadAgent()
			a.Workstream, a.ID = threadlessStream, Manager
			change(&a)
			if err := r.CreateThread(ctx, a); err != nil {
				t.Fatal(err)
			}
			if _, err := r.EnsureManager(ctx, threadlessStream, at, owner); !errors.Is(err, ErrConflict) {
				t.Fatalf("ensure = %v", err)
			}
		})
	}
}
