package trace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// chiefTurnFrom creates a chief-of-staff thread for agent with one claimed
// turn that actor asked for, and returns the scope of that turn.
func chiefTurnFrom(t *testing.T, r *Repository, agent, turn string, actor Actor) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	if err := r.CreateThread(ctx, Agent{Header: header("agent", agent), Role: ChiefOfStaff, ThreadID: agent + "_thread"}); err != nil {
		t.Fatal(err)
	}
	h := header("turn-request", "request_"+turn)
	h.Actor = actor
	req := TurnRequest{Header: h, AgentID: agent, ThreadID: agent + "_thread", TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Put the importer first."}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, streamID, agent, "token_"+turn, "/owned/"+turn, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: agent + "_thread", Turn: turn, Role: ChiefOfStaff}
}

func TestSetPriorityRecordsTheOwnersRequest(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	owner := Actor{Kind: "owner", ID: "local"}
	scope := chiefTurnFrom(t, r, "chief", "message1", owner)
	order := func(ids ...config.WorkstreamID) func() ([]config.WorkstreamID, error) {
		return func() ([]config.WorkstreamID, error) { return ids, nil }
	}

	first, err := r.SetPriority(ctx, "chief", scope, at.Add(time.Minute), order(streamID))
	if err != nil {
		t.Fatal(err)
	}
	want := PriorityChange{Header: Header{Schema: "osmia.trace.priority", Version: Version, ID: PriorityID, Revision: 1, Project: projectID, Workstream: streamID, At: at.Add(time.Minute), Actor: owner, Cause: "request_message1", Depth: 3},
		Agent: "chief", Turn: "message1", Order: []config.WorkstreamID{streamID}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first change %+v, want %+v", first, want)
	}
	second, err := r.SetPriority(ctx, "chief", scope, at.Add(2*time.Minute), order())
	if err != nil || second.Revision != 2 || second.Order == nil || len(second.Order) != 0 {
		t.Fatalf("second change %+v %v", second, err)
	}
	data, err := os.ReadFile(r.directory + "/workstreams/" + string(streamID) + "/priority.jsonl")
	if err != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("priority log: %q %v", data, err)
	}
	if out, err := r.git(ctx, nil, "status", "--porcelain", "--", "workstreams"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("priority not committed: %q %v", out, err)
	}

	// An apply error records nothing and is returned as it is.
	failed := errors.New("runtime unavailable")
	if _, err := r.SetPriority(ctx, "chief", scope, at, func() ([]config.WorkstreamID, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatalf("apply error: %v", err)
	}

	// A turn the owner did not ask for, and a turn of another role, change
	// nothing.
	called := false
	apply := func() ([]config.WorkstreamID, error) { called = true; return nil, nil }
	events := chiefTurnFrom(t, r, "chief_events", "events1", Actor{Kind: "service", ID: "events"})
	var refused *PriorityRefused
	if _, err := r.SetPriority(ctx, "chief_events", events, at, apply); !errors.As(err, &refused) || !strings.Contains(refused.Reason, "only the owner") {
		t.Fatalf("service turn: %v", err)
	}
	mason := scope
	mason.Role = "mason"
	if _, err := r.SetPriority(ctx, "chief", mason, at, apply); err == nil || errors.As(err, &refused) {
		t.Fatalf("mason scope: %v", err)
	}
	if called {
		t.Fatal("apply ran for a refused change")
	}
	if err := r.Append(ctx, PriorityChange{Header: first.Header, Agent: "chief", Turn: "message1", Order: []config.WorkstreamID{}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("direct append: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	all, err := Read[PriorityChange](reopened, streamID)
	if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0], first) || !reflect.DeepEqual(all[1], second) {
		t.Fatalf("recorded changes %+v %v", all, err)
	}
	if !slices.Equal(all[1].Order, []config.WorkstreamID{}) {
		t.Fatalf("empty order %v", all[1].Order)
	}
}
