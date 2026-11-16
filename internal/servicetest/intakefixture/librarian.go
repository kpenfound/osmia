package intakefixture

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

// Names the librarian's records carry in an older trace.
const (
	LibrarianRole   = "librarian"
	LibrarianAgent  = "agent_librarian"
	LibrarianThread = "thread_librarian"
)

// PlantLibrarian records in repo what an older trace holds of the librarian:
// its workstream with its thread, knowledge-base prose, the source ledger and
// its notes. It returns the
// librarian's workstream.
func PlantLibrarian(t *testing.T, repo *trace.Repository, at time.Time, actor trace.Actor) config.WorkstreamID {
	t.Helper()
	ctx := context.Background()
	stream := intake.LibrarianWorkstream(repo.Project())
	streams, err := repo.Workstreams()
	Must(t, err)
	if !slices.Contains(streams, stream) {
		Must(t, repo.CreateWorkstream(ctx, stream, at, actor))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: LibrarianAgent, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: "librarian-thread"}, Role: LibrarianRole, ThreadID: LibrarianThread}
		Must(t, repo.CreateThread(ctx, identity))
	}
	librarian := trace.Actor{Kind: "agent", ID: LibrarianAgent}
	doc := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), At: at, Actor: librarian, Cause: "extract"}, Path: path, Content: content}
	}
	Must(t, repo.RecordDocuments(ctx, []trace.Document{
		doc("subsystem-internal", "kb/internal.md", "# Internal\n\nOld prose.\n"),
		doc("kb-sources", "kb/sources.json", "[]\n"),
		doc("notes-librarian", "notes/librarian.md", "Old notes.\n"),
	}))
	return stream
}

// PlantLibrarianOperation queues a runner operation of the librarian in its
// workstream, as an older trace holds it, and returns it. The name
// identifies the request, so each operation of a trace needs its own.
func PlantLibrarianOperation(t *testing.T, repo *trace.Repository, at time.Time, actor trace.Actor, name, action string, input any) coreadapter.Operation {
	t.Helper()
	stream := intake.LibrarianWorkstream(repo.Project())
	data, err := json.Marshal(input)
	Must(t, err)
	event := trace.EventID(name, "run")
	op := coreadapter.Operation{ID: trace.OperationID(repo.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: action, Input: data}
	tx := trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: name, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: "fixture"},
		Subject: name, To: "requested", Reason: "Queued for the librarian"},
		Events: []trace.Event{{ID: event, Kind: action, Body: "Run " + action, Operation: &op}}}
	_, err = repo.Transact(context.Background(), tx)
	Must(t, err)
	return op
}
