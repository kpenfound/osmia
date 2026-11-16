package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// plantedLibrarian is an older trace's librarian records: the librarian's workstream with a pending extraction, a pending refresh
// of landings, a pending knowledge-gap refresh and an extraction that had
// already succeeded.
type plantedLibrarian struct {
	stream  config.WorkstreamID
	pending []coreadapter.Operation
	done    coreadapter.Operation
}

func plantLibrarianWork(t *testing.T, repo *trace.Repository) plantedLibrarian {
	t.Helper()
	at := demoStart
	p := plantedLibrarian{stream: intakefixture.PlantLibrarian(t, repo, at, registrationActor)}
	commit := strings.Repeat("a", 40)
	p.done = intakefixture.PlantLibrarianOperation(t, repo, at, registrationActor, "extract-1", "kb-extract", map[string]any{"extraction": 1})
	must(t, repo.WithOperation(context.Background(), p.stream, trace.EventID("extract-1", "run"), serviceActor, func() time.Time { return at }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		ctx := context.Background()
		observe := a.Action("observe", at)
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not started"}
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if err := a.Record(ctx, a.Action("effect", at)); err != nil {
			return err
		}
		result := a.Action("result", at)
		result.Result = &coreadapter.OperationResult{Outcome: "succeeded", Evidence: "recorded 1 subsystem files and the entity map"}
		if err := a.Record(ctx, result); err != nil {
			return err
		}
		return a.Record(ctx, a.Action("acknowledge", at))
	}))
	p.pending = []coreadapter.Operation{
		intakefixture.PlantLibrarianOperation(t, repo, at, registrationActor, "extract-2", "kb-extract", map[string]any{"extraction": 2}),
		intakefixture.PlantLibrarianOperation(t, repo, at, registrationActor, "refresh-landings", "kb-refresh", map[string]any{"workstream": stream, "unit": "resume", "landing": 1, "commit": commit, "report": 1}),
		intakefixture.PlantLibrarianOperation(t, repo, at, registrationActor, "refresh-gap", "kb-refresh", map[string]any{"workstream": stream, "question": "q1", "inspection": "i1", "commit": commit}),
	}
	return p
}

// knowledgeDocuments lists the path and revision of every knowledge-base
// record the retired librarian wrote.
func knowledgeDocuments(t *testing.T, repo *trace.Repository) []string {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	var out []string
	for _, d := range docs {
		if d.Actor.ID == intakefixture.LibrarianAgent {
			out = append(out, fmt.Sprintf("%s@%d", d.Path, d.Revision))
		}
	}
	slices.Sort(out)
	if len(out) == 0 {
		t.Fatal("the trace holds no librarian record")
	}
	return out
}

func librarianOperationRecords(t *testing.T, s *Service, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := s.sole().repository.Operations(stream)
	must(t, err)
	return ops
}

func resultsOf(op trace.OperationRecord) int {
	n := 0
	for _, a := range op.History {
		if a.Kind == "result" {
			n++
		}
	}
	return n
}

// TestPendingLibrarianOperationsAreRetiredOnceAcrossRestart shows that the
// first reconcile pass after an upgrade closes every pending librarian
// extraction, refresh and knowledge-gap refresh with the terminal result
// retired, leaves a finished one alone, and that a restart retries nothing and
// records no second result.
func TestPendingLibrarianOperationsAreRetiredOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "retire-librarian-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	planted := plantLibrarianWork(t, repo)
	before := knowledgeDocuments(t, repo)
	must(t, repo.Close())

	s, _ := start(t, opts)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return librarianOperationRecords(t, s, planted.stream) })
	if len(ops) != 4 {
		t.Fatalf("librarian operations: %+v", ops)
	}
	check := func(ops []trace.OperationRecord) {
		t.Helper()
		retired := 0
		for _, op := range ops {
			if resultsOf(op) != 1 || op.Result == nil || !op.Acknowledged {
				t.Fatalf("operation %s: %d results, acknowledged %t", op.Operation.ID, resultsOf(op), op.Acknowledged)
			}
			if op.Operation.ID == planted.done.ID {
				if op.Result.Outcome != "succeeded" {
					t.Fatalf("a finished extraction was rewritten: %+v", op.Result)
				}
				continue
			}
			retired++
			if op.Result.Outcome != "retired" || !slices.ContainsFunc(planted.pending, func(p coreadapter.Operation) bool { return p.ID == op.Operation.ID }) {
				t.Fatalf("operation %s ended %+v", op.Operation.ID, op.Result)
			}
		}
		if retired != 3 {
			t.Fatalf("%d operations retired, want the extraction, the refresh and the knowledge-gap refresh", retired)
		}
	}
	check(ops)
	history := map[string]int{}
	for _, op := range ops {
		history[op.Operation.ID] = len(op.History)
	}

	// The librarian ran nothing: no turn, no document, no thread change.
	librarianThread, err := s.sole().repository.Thread(planted.stream, intakefixture.LibrarianAgent)
	must(t, err)
	if len(librarianThread.Turns) != 0 {
		t.Fatalf("retiring queued librarian turns: %+v", librarianThread.Turns)
	}
	if after := knowledgeDocuments(t, s.sole().repository); !slices.Equal(after, before) {
		t.Fatalf("retiring changed the knowledge-base records: %v, were %v", after, before)
	}

	// A restart records nothing more for them.
	must(t, s.Close())
	s, c := start(t, opts)
	if _, err := c.Statuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return librarianOperationRecords(t, s, planted.stream) })
	check(again)
	for _, op := range again {
		if len(op.History) != history[op.Operation.ID] {
			t.Fatalf("operation %s recorded actions after the restart: %d, was %d", op.Operation.ID, len(op.History), history[op.Operation.ID])
		}
	}
}

// TestOldLibrarianRecordsLoadAndStayHidden shows that a trace holding the
// librarian's workstream, thread, prose, source ledger and notes opens and
// validates, that status lists nothing of it, and that no scheduling pass
// admits a turn of its thread.
func TestOldLibrarianRecordsLoadAndStayHidden(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "hidden-librarian-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	librarian := intakefixture.PlantLibrarian(t, repo, demoStart, registrationActor)
	// A turn an earlier release queued and never ran.
	_, err = repo.EnqueueTurn(context.Background(), trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_extract-9-1", Revision: 1, Project: cfg.Project.ID, Workstream: librarian, At: demoStart, Actor: registrationActor, Cause: "fixture", Depth: 1},
		AgentID: intakefixture.LibrarianAgent, ThreadID: intakefixture.LibrarianThread, TurnID: "extract-9-1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Extract"})
	must(t, err)
	must(t, repo.Close())

	s, c := start(t, opts)
	status, err := c.Statuses(context.Background())
	must(t, err)
	for _, w := range status.Workstreams {
		if w.Workstream == librarian {
			t.Fatalf("status lists the librarian's workstream: %+v", w)
		}
	}
	docs, err := trace.Read[trace.Document](s.sole().repository, "")
	must(t, err)
	for _, path := range []string{"kb/internal.md", "kb/sources.json", "notes/librarian.md"} {
		if !slices.ContainsFunc(docs, func(d trace.Document) bool { return d.Path == path }) {
			t.Fatalf("the old record %s is gone", path)
		}
	}
	streams, err := s.sole().repository.Workstreams()
	must(t, err)
	if !slices.Contains(streams, librarian) {
		t.Fatalf("the librarian's workstream is gone: %v", streams)
	}
	holds := s.dispatchHolds(s.current(), s.sole().repository)
	thread, err := s.sole().repository.Thread(librarian, intakefixture.LibrarianAgent)
	must(t, err)
	held, err := holds(scheduler.Candidate{Project: cfg.Project.ID, Workstream: librarian, Thread: thread, Turn: thread.Turns[0]})
	if err != nil || !held {
		t.Fatalf("the librarian's queued turn is not held: %t %v", held, err)
	}
	if got := thread.Turns[0]; got.Claim != nil || got.Response != nil {
		t.Fatalf("the librarian's turn ran: %+v", got)
	}
}

// TestNoRoleIsOfferedRetiredKnowledgeTools shows that the tools of the
// retired librarian are in no role's grant, and that a file-context turn's
// memory tools do not include them either.
func TestNoRoleIsOfferedRetiredKnowledgeTools(t *testing.T) {
	t.Parallel()
	retired := []string{"kb_check", "notes_read", "notes_write"}
	for name, grant := range map[string]coreadapter.Capabilities{"chief": chiefGrant, "mason": masonGrant, "unit mason": unitMasonGrant, "reviewer": reviewerGrant} {
		for _, tool := range retired {
			if slices.Contains(grant.Tools, tool) {
				t.Errorf("the %s grant offers %s", name, tool)
			}
		}
	}
	cfg, repo := memoryTurnConfig(t, config.Hearsay{}, "")
	granted, scoped := memoryTurnTools(t, cfg, repo)
	for _, tools := range []map[string][]string{granted, scoped} {
		for role, names := range tools {
			for _, tool := range retired {
				if slices.Contains(names, tool) {
					t.Errorf("role %s is offered %s", role, tool)
				}
			}
		}
	}
}
