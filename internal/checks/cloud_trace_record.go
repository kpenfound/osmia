package checks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// TracePathPrefix is the folder of a workstream's recorded trace outputs.
const TracePathPrefix = "traces/"

// TraceCitationPrefix starts the citation of a recorded trace output:
// trace#<record>.
const TraceCitationPrefix = "trace#"

// TraceNarrowing names what a trace record covers: the check it is narrowed
// to, or the whole trace when check is empty.
func TraceNarrowing(check string) string {
	if check == "" {
		return "all"
	}
	sum := sha256.Sum256([]byte(check))
	return "check-" + hex.EncodeToString(sum[:8])
}

// TraceDocumentID identifies the record of a trace ID together with its
// narrowing. It is the name of the record's file under TracePathPrefix and the
// record cited as trace#<id>. A record for one check never shares it with
// another check's or with the whole trace's.
func TraceDocumentID(traceID, check string) string {
	return traceID + "-" + TraceNarrowing(check)
}

// TraceDocumentPath is the path of the record of a trace ID with its
// narrowing.
func TraceDocumentPath(traceID, check string) string {
	return TracePathPrefix + TraceDocumentID(traceID, check) + ".json"
}

// TraceRecord is the content of a recorded trace output.
type TraceRecord struct {
	TraceID string `json:"trace_id"`
	// Check is the check the output is narrowed to; empty when the record
	// covers the whole trace.
	Check      string    `json:"check,omitempty"`
	WholeTrace bool      `json:"whole_trace"`
	FetchedAt  time.Time `json:"fetched_at"`
	// Truncated reports that Output is only the end of the output the CLI
	// wrote, Size bytes long.
	Truncated bool   `json:"truncated"`
	Size      int    `json:"size"`
	Output    string `json:"output"`
}

// TraceFetch is a recorded trace output as the action returns it.
type TraceFetch struct {
	// Path is the record's document path, readable with factory_context.
	Path string `json:"path"`
	// Citation cites the record: trace#<record>.
	Citation   string    `json:"citation"`
	TraceID    string    `json:"trace_id"`
	Check      string    `json:"check,omitempty"`
	WholeTrace bool      `json:"whole_trace"`
	FetchedAt  time.Time `json:"fetched_at"`
	Output     string    `json:"output"`
	Truncated  bool      `json:"truncated"`
	Size       int       `json:"size"`
	// Recorded is true when the record already existed and no command ran.
	Recorded bool `json:"recorded"`
}

// TraceFetcher fetches and records the trace output of one project's
// workstreams.
type TraceFetcher struct {
	Repository *trace.Repository
	Runner     TraceRunner
	Now        func() time.Time
}

// TraceCaller is the provenance of the records a fetch writes.
type TraceCaller struct {
	Workstream config.WorkstreamID
	Actor      trace.Actor
	// Cause is what asked for the fetch, such as a turn's ID.
	Cause string
}

// Lookup returns the recorded output of traceID narrowed to check, or false
// when none is recorded. Only a record of exactly that trace ID and narrowing
// answers; a record narrowed to another check, or to the whole trace, never
// does.
func (f TraceFetcher) Lookup(stream config.WorkstreamID, traceID, check string) (TraceFetch, bool, error) {
	if err := ValidateTraceID(traceID); err != nil {
		return TraceFetch{}, false, err
	}
	if check != "" {
		if err := ValidateTraceCheck(check); err != nil {
			return TraceFetch{}, false, err
		}
	}
	docs, err := trace.Read[trace.Document](f.Repository, stream)
	if err != nil {
		return TraceFetch{}, false, err
	}
	id, path := TraceDocumentID(traceID, check), TraceDocumentPath(traceID, check)
	var found *trace.Document
	for i := range docs {
		if docs[i].ID == id && docs[i].Path == path && (found == nil || docs[i].Revision >= found.Revision) {
			found = &docs[i]
		}
	}
	if found == nil {
		return TraceFetch{}, false, nil
	}
	var rec TraceRecord
	if err := json.Unmarshal([]byte(found.Content), &rec); err != nil || rec.TraceID != traceID || rec.Check != check || rec.WholeTrace != (check == "") {
		return TraceFetch{}, false, fmt.Errorf("the recorded trace output %s is damaged", path)
	}
	return TraceFetch{Path: path, Citation: TraceCitationPrefix + id, TraceID: traceID, Check: check, WholeTrace: check == "", FetchedAt: rec.FetchedAt, Output: rec.Output, Truncated: rec.Truncated, Size: rec.Size, Recorded: true}, true, nil
}

// Fetch returns the recorded output of traceID narrowed to check when one
// exists. Otherwise it runs the fetch and records its output as a workstream
// document. A non-nil failure says why the fetch produced nothing, and records
// nothing; a non-nil error is a refused value or a trace that cannot be read
// or written. An empty check covers the whole trace.
func (f TraceFetcher) Fetch(ctx context.Context, caller TraceCaller, traceID, check string) (TraceFetch, *TraceFailure, error) {
	if got, ok, err := f.Lookup(caller.Workstream, traceID, check); err != nil || ok {
		return got, nil, err
	}
	if f.Runner == nil {
		return TraceFetch{}, traceFailure(TraceNoToken, "no Dagger Cloud API token is configured for the service"), nil
	}
	out, failure := f.Runner.FetchTrace(ctx, traceID, check)
	if failure != nil {
		return TraceFetch{}, failure, nil
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	fetched := now().UTC()
	rec := TraceRecord{TraceID: traceID, Check: check, WholeTrace: check == "", FetchedAt: fetched, Truncated: out.Truncated, Size: out.Size, Output: out.Output}
	content, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return TraceFetch{}, nil, err
	}
	id := TraceDocumentID(traceID, check)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.Repository.Project(), Workstream: caller.Workstream, At: fetched, Actor: caller.Actor, Cause: caller.Cause}, Path: TraceDocumentPath(traceID, check), Content: string(content) + "\n"}
	if err := f.Repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if got, ok, lookup := f.Lookup(caller.Workstream, traceID, check); lookup == nil && ok {
				return got, nil, nil
			}
		}
		return TraceFetch{}, nil, err
	}
	return TraceFetch{Path: doc.Path, Citation: TraceCitationPrefix + id, TraceID: traceID, Check: check, WholeTrace: check == "", FetchedAt: fetched, Output: out.Output, Truncated: out.Truncated, Size: out.Size}, nil, nil
}
