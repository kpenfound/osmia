package recordspans

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	stream  config.WorkstreamID = "w_00000000000000000000000000000001"
)

var (
	created = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	owner   = trace.Actor{Kind: "owner", ID: "local"}
	service = trace.Actor{Kind: "service", ID: "foreman"}
	mason   = trace.Actor{Kind: "service", ID: "mason"}
	checks  = trace.Actor{Kind: "service", ID: "checks"}
	review  = trace.Actor{Kind: "service", ID: "reviewer"}
)

func minute(n int) time.Time { return created.Add(time.Duration(n) * time.Minute) }

type harness struct {
	t        *testing.T
	root     config.Root
	project  config.Project
	repo     *trace.Repository
	exporter *tracetest.InMemoryExporter
	sink     *telemetry.Sink
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "target")
	if err := os.Mkdir(clone, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	h := &harness{t: t, root: root, project: config.Project{ID: project, Clone: clone}, exporter: tracetest.NewInMemoryExporter()}
	h.sink, err = telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: h.exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sink.Close() })
	h.repo, err = trace.Create(context.Background(), root, h.project, created, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.repo.Close() })
	if err := h.repo.CreateWorkstream(context.Background(), stream, created, owner); err != nil {
		t.Fatal(err)
	}
	h.repo.ObserveSpans(Recorder{Sink: h.sink}.Observe)
	return h
}

func (h *harness) restart() {
	h.t.Helper()
	if err := h.repo.Close(); err != nil {
		h.t.Fatal(err)
	}
	repo, err := trace.Open(h.root, h.project)
	if err != nil {
		h.t.Fatal(err)
	}
	h.repo = repo
	h.t.Cleanup(func() { repo.Close() })
	repo.ObserveSpans(Recorder{Sink: h.sink}.Observe)
}

func header(schema, id string, at time.Time, actor trace.Actor, unit string) trace.Header {
	return trace.Header{Schema: "osmia.trace." + schema, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: "cause-" + id, Depth: 1}
}

func (h *harness) feature(id, to string, at time.Time, actor trace.Actor, reason string) {
	h.t.Helper()
	if _, err := h.repo.SetFeatureState(context.Background(), header("transition", id, at, actor, ""), to, reason); err != nil {
		h.t.Fatalf("feature %s: %v", id, err)
	}
}

func (h *harness) unit(id, unit, to string, at time.Time, actor trace.Actor, reason string) {
	h.t.Helper()
	subject := trace.UnitSubject(unit)
	state, err := h.repo.Workflow(stream, subject)
	if err != nil {
		h.t.Fatal(err)
	}
	_, err = h.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: header("transition", id, at, actor, unit), Subject: subject, From: state.Value, To: to, Reason: reason}})
	if err != nil {
		h.t.Fatalf("unit %s: %v", id, err)
	}
}

func (h *harness) spans() tracetest.SpanStubs {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.sink.Flush(ctx); err != nil {
		h.t.Fatal(err)
	}
	return h.exporter.GetSpans()
}

func named(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func value(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func str(s tracetest.SpanStub, key string) string {
	v, _ := value(s, key)
	return v.String()
}

func (h *harness) document(id, path string, at time.Time, actor trace.Actor, unit string, body any) {
	h.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	doc := trace.Document{Header: header("document", id, at, actor, unit), Path: path, Content: string(data) + "\n"}
	if err := h.repo.RecordDocuments(context.Background(), []trace.Document{doc}); err != nil {
		h.t.Fatalf("document %s: %v", path, err)
	}
}

func (h *harness) turn(id string, enqueued, claimed time.Time, mutate func(*trace.TurnRequest, *trace.TurnResponse)) trace.TurnResponse {
	h.t.Helper()
	ctx := context.Background()
	agent := trace.Agent{Header: header("agent", "mason", created, service, ""), Role: "mason", ThreadID: "thread", Session: coreadapter.BackendSession{Backend: "fake", ID: "original"}}
	if err := h.repo.CreateThread(ctx, agent); err != nil {
		h.t.Fatal(err)
	}
	req := trace.TurnRequest{Header: header("turn-request", "request_"+id, enqueued, service, "u1"), AgentID: "mason", ThreadID: "thread", TurnID: id,
		Profile: coreadapter.Profile{Name: "default", Backend: "Fake", Model: "test-model-1"}, Prompt: "prompt " + id}
	resp := trace.TurnResponse{Header: header("turn-response", "response_"+id, claimed.Add(5*time.Second), service, "u1"), AgentID: "mason", ThreadID: "thread", TurnID: id,
		RequestID: "request_" + id, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "Fake", ID: "continued"}, SessionDirectory: "/owned/" + id, StartedAt: claimed, Duration: 5 * time.Second, FinalResponse: "reply " + id}}
	resp.Cause = req.Cause
	if mutate != nil {
		mutate(&req, &resp)
	}
	if _, err := h.repo.EnqueueTurn(ctx, req); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.repo.ClaimTurn(ctx, stream, "mason", "token-"+id, "/owned/"+id, claimed); err != nil {
		h.t.Fatal(err)
	}
	if err := h.repo.CaptureTurn(ctx, "token-"+id, resp); err != nil {
		h.t.Fatal(err)
	}
	if err := h.repo.CompleteTurn(ctx, stream, "mason", id, "token-"+id, resp.At); err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func describe(s tracetest.SpanStub) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s..%s", s.Name, s.StartTime.Format(time.RFC3339Nano), s.EndTime.Format(time.RFC3339Nano))
	for _, kv := range s.Attributes {
		fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
	}
	return b.String()
}
