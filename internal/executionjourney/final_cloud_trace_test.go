package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"strings"
	"sync"
	"testing"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type traceReads struct {
	mu    sync.Mutex
	asked []string
}

func (r *traceReads) FetchTrace(_ context.Context, traceID, check string) (checkrun.TraceOutput, *checkrun.TraceFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, traceID+"|"+check)
	out := "trace " + traceID + " check " + check + "\n"
	return checkrun.TraceOutput{Output: out, Size: len(out)}, nil
}

func (r *traceReads) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

func TestFinalReaderFetchesATraceOutput(t *testing.T) {
	t.Parallel()
	d := newDraftFixture(t, "squash")
	reads := &traceReads{}
	d.f.s.options.CloudTrace = reads
	var listedTools []string
	var results []string
	d.f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		listed, err := tools.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tool := range listed.Tools {
			listedTools = append(listedTools, tool.Name)
		}
		for _, input := range []map[string]any{{"trace_id": "trace1"}, {"trace_id": "trace1", "check": "test:a"}, {"trace_id": "trace1", "check": "test:a"}} {
			out, err := fixturekit.CallTool(ctx, tools, checkrun.TraceToolName, input)
			if err != nil {
				return err
			}
			results = append(results, out)
		}
		_, err = fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": []any{}})
		return err
	})
	result, err := d.a.Apply(context.Background(), d.first)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("final review %+v: %v", result, err)
	}
	if !slices.Contains(listedTools, checkrun.TraceToolName) {
		t.Fatalf("the final reader's tools are %v", listedTools)
	}
	if want := []string{"trace1|", "trace1|test:a"}; !slices.Equal(reads.requests(), want) {
		t.Fatalf("the service fetched %q, want %q", reads.requests(), want)
	}
	var got [3]struct {
		Fetched  bool   `json:"fetched"`
		Path     string `json:"path"`
		Output   string `json:"output"`
		Recorded bool   `json:"recorded"`
	}
	for i, raw := range results {
		if err := json.Unmarshal([]byte(raw), &got[i]); err != nil || !got[i].Fetched {
			t.Fatalf("result %d: %s: %v", i, raw, err)
		}
	}
	if got[0].Output != "trace trace1 check \n" || got[1].Output != "trace trace1 check test:a\n" || got[0].Path == got[1].Path || got[0].Recorded || got[1].Recorded || !got[2].Recorded || got[2].Path != got[1].Path {
		t.Fatalf("results %+v", got)
	}
	for _, path := range []string{got[0].Path, got[1].Path} {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "traces/"), ".json")
		docs := intakefixture.StreamDocuments(t, d.repository, d.stream, id)
		if len(docs) != 1 || docs[0].Path != path || docs[0].Actor.ID != "final-review" {
			t.Fatalf("recorded %q: %+v", path, docs)
		}
	}

	d.reopen(t)
	got1, found, err := (checkrun.TraceFetcher{Repository: d.repository, Runner: reads}).Lookup(d.stream, "trace1", "test:a")
	if err != nil || !found || got1.Output != "trace trace1 check test:a\n" || len(reads.requests()) != 2 {
		t.Fatalf("after a restart: %+v, found %v, %v, %d fetches", got1, found, err, len(reads.requests()))
	}
	if _, found, err := (checkrun.TraceFetcher{Repository: d.repository, Runner: reads}).Lookup(d.stream, "trace1", "test:b"); err != nil || found {
		t.Fatalf("another check was answered from a record: found %v, %v", found, err)
	}
}
