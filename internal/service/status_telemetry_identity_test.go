package service

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	telemetryTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	telemetrySpanIDPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// requireTelemetryIdentity fails unless w carries the trace ID and root span ID
// Osmia's telemetry derives from its workstream ID, in lowercase hex.
func requireTelemetryIdentity(t *testing.T, w WorkstreamStatus) {
	t.Helper()
	wantTrace, wantRoot := telemetry.WorkstreamTrace(w.Workstream), telemetry.WorkstreamRoot(w.Workstream)
	if !telemetryTraceIDPattern.MatchString(w.TelemetryTraceID) || w.TelemetryTraceID != wantTrace.String() || !wantTrace.IsValid() {
		t.Errorf("workstream %s telemetry_trace_id %q, want %q", w.Workstream, w.TelemetryTraceID, wantTrace)
	}
	if !telemetrySpanIDPattern.MatchString(w.TelemetryRootSpanID) || w.TelemetryRootSpanID != wantRoot.String() || !wantRoot.IsValid() {
		t.Errorf("workstream %s telemetry_root_span_id %q, want %q", w.Workstream, w.TelemetryRootSpanID, wantRoot)
	}
}

func identityHeader(schema, id string, at time.Time, actor trace.Actor, unit string) trace.Header {
	return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: "cause-" + id, Depth: 1}
}

// The status of a workstream carries the trace ID and the root span ID that the
// span recorder puts on the workstream's state, turn and check spans.
func TestStatusTelemetryIdentityMatchesTheSpansTheRecorderSends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	must(t, err)
	repository, err := trace.Create(ctx, cfg.Root, cfg.Project, at, serviceActor)
	must(t, err)
	repository.ObserveSpans(recordspans.Recorder{Sink: sink}.Observe)
	must(t, repository.CreateWorkstream(ctx, stream, at, serviceActor))
	featureTransition(t, repository, stream, "pending", at.Add(time.Minute))
	featureTransition(t, repository, stream, "delivered", at.Add(2*time.Minute))

	agent := trace.Agent{Header: identityHeader("agent", "mason", at, serviceActor, ""), Role: "mason", ThreadID: "thread", Session: coreadapter.BackendSession{Backend: "fake", ID: "original"}}
	must(t, repository.CreateThread(ctx, agent))
	req := trace.TurnRequest{Header: identityHeader("turn-request", "request_t1", at.Add(3*time.Minute), serviceActor, "u1"), AgentID: "mason", ThreadID: "thread", TurnID: "t1",
		Profile: coreadapter.Profile{Name: "default", Backend: "Fake", Model: "test-model-1"}, Prompt: "prompt"}
	claimed := at.Add(4 * time.Minute)
	resp := trace.TurnResponse{Header: identityHeader("turn-response", "response_t1", claimed.Add(5*time.Second), serviceActor, "u1"), AgentID: "mason", ThreadID: "thread", TurnID: "t1",
		RequestID: "request_t1", RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "Fake", ID: "continued"}, SessionDirectory: "/owned/t1", StartedAt: claimed, Duration: 5 * time.Second, FinalResponse: "reply"}}
	resp.Cause = req.Cause
	_, err = repository.EnqueueTurn(ctx, req)
	must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, "mason", "token-t1", "/owned/t1", claimed)
	must(t, err)
	must(t, repository.CaptureTurn(ctx, "token-t1", resp))
	must(t, repository.CompleteTurn(ctx, stream, "mason", "t1", "token-t1", resp.At))

	covered := 3
	run, err := json.Marshal(execution.UnitCheckRun{Unit: "u1", Run: 1, Status: "passed", Selection: &execution.CheckSelection{Mode: "selected", Covered: &covered}, StartedAt: at.Add(6 * time.Minute), FinishedAt: at.Add(7 * time.Minute)})
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: identityHeader("document", "unit-u1-checks-1", at.Add(7*time.Minute), trace.Actor{Kind: "service", ID: "checks"}, "u1"), Path: "units/u1/checks-1.json", Content: string(run) + "\n"}}))

	flush, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	must(t, sink.Flush(flush))
	spans := exporter.GetSpans()
	must(t, sink.Close())
	must(t, repository.Close())

	_, c := start(t, opts)
	status, err := c.Statuses(ctx)
	must(t, err)
	var row *WorkstreamStatus
	for i := range status.Workstreams {
		if status.Workstreams[i].Workstream == stream {
			row = &status.Workstreams[i]
		}
	}
	if row == nil {
		t.Fatalf("workstream %s is not in the status: %+v", stream, status.Workstreams)
	}
	requireTelemetryIdentity(t, *row)

	for name, group := range map[string]tracetest.SpanStubs{
		"state": named(spans, "osmia.workstream.state"),
		"turn":  named(spans, "invoke_agent mason"),
		"check": named(spans, "osmia.check.run"),
	} {
		if len(group) == 0 {
			t.Fatalf("the recorder sent no %s span: %v", name, spans)
		}
		for _, sp := range group {
			if got := sp.SpanContext.TraceID().String(); got != row.TelemetryTraceID {
				t.Errorf("%s span %s is in trace %s, status says %s", name, sp.Name, got, row.TelemetryTraceID)
			}
			if got := sp.Parent.SpanID().String(); got != row.TelemetryRootSpanID {
				t.Errorf("%s span %s has parent %s, status says root %s", name, sp.Name, got, row.TelemetryRootSpanID)
			}
		}
	}
}

// Every row of the status list carries the identity: ordinary, session, design
// and archived workstreams, the archived one whose trace was deleted included.
// The values need no telemetry export and survive a restart on the same root.
func TestStatusTelemetryIdentityIsOnEveryRowWithoutExportAndAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	must(t, err)
	created := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	archived := config.WorkstreamID("w_00000000000000000000000000000a01")
	session := config.WorkstreamID("w_00000000000000000000000000000a02")
	design := config.WorkstreamID("w_00000000000000000000000000000a03")
	cleaned := config.WorkstreamID("w_00000000000000000000000000000a04")
	repository, err := trace.Create(ctx, cfg.Root, cfg.Project, created, serviceActor)
	must(t, err)
	for _, id := range []config.WorkstreamID{stream, archived, design, cleaned} {
		must(t, repository.CreateWorkstream(ctx, id, created, serviceActor))
	}
	must(t, repository.CreateSessionWorkstream(ctx, session, "default", "base", "session-branch", "A session", created, serviceActor))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "open-design", Revision: 1, Project: project, Workstream: design, At: created, Actor: serviceActor, Cause: "fixture"}
	_, err = repository.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: intake.DesignSessionSubject, To: intake.DesignOpenValue, Reason: "open design"}})
	must(t, err)
	must(t, repository.Close())

	s, c := start(t, opts)
	if s.telemetry.Enabled() {
		t.Fatal("telemetry export is configured")
	}
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	featureTransition(t, active.repository, archived, "pending", created.Add(time.Minute))
	featureTransition(t, active.repository, archived, "delivered", created.Add(time.Hour))
	if _, err := c.Archive(ctx, archived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	must(t, s.store.SetArchived(runtime.Archive{Project: project, Workstream: cleaned, State: AbandonedState, Title: "cleaned", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: created, CleanupStartedAt: created, CleanedAt: created}))
	_, err = active.repository.MarkWorkstreamTraceForDeletion(cleaned)
	must(t, err)
	must(t, trace.FinishWorkstreamTraceDeletion(cfg.Root, project, cleaned))

	rows := func(c *Client) map[config.WorkstreamID]WorkstreamStatus {
		t.Helper()
		status, err := c.Statuses(ctx)
		must(t, err)
		out := map[config.WorkstreamID]WorkstreamStatus{}
		for _, w := range status.Workstreams {
			out[w.Workstream] = w
		}
		if len(out) != len(status.Workstreams) {
			t.Fatalf("a workstream is listed twice: %+v", status.Workstreams)
		}
		return out
	}
	before := rows(c)
	for id, want := range map[config.WorkstreamID]func(WorkstreamStatus) bool{
		stream:   func(w WorkstreamStatus) bool { return !w.Archived && w.Kind == "" },
		archived: func(w WorkstreamStatus) bool { return w.Archived },
		session:  func(w WorkstreamStatus) bool { return w.Kind == "session" },
		design:   func(w WorkstreamStatus) bool { return w.Kind == "design" },
		cleaned:  func(w WorkstreamStatus) bool { return w.Archived && w.TraceDeleted },
	} {
		w, ok := before[id]
		if !ok || !want(w) {
			t.Fatalf("workstream %s is not listed as expected: %+v", id, before)
		}
	}
	traces := map[string]config.WorkstreamID{}
	for id, w := range before {
		requireTelemetryIdentity(t, w)
		if other, ok := traces[w.TelemetryTraceID]; ok {
			t.Errorf("workstreams %s and %s share trace ID %s", id, other, w.TelemetryTraceID)
		}
		traces[w.TelemetryTraceID] = id
	}

	must(t, s.Close())
	_, c = start(t, opts)
	after := rows(c)
	for id, w := range before {
		got, ok := after[id]
		if !ok {
			t.Errorf("workstream %s is not listed after the restart", id)
			continue
		}
		if got.TelemetryTraceID != w.TelemetryTraceID || got.TelemetryRootSpanID != w.TelemetryRootSpanID {
			t.Errorf("workstream %s identity changed across the restart: %s/%s, then %s/%s", id, w.TelemetryTraceID, w.TelemetryRootSpanID, got.TelemetryTraceID, got.TelemetryRootSpanID)
		}
	}
}
