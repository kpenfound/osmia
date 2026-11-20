package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/trace"
)

// TraceFailureDocumentID identifies the record of an automatic fetch of a
// trace ID with its narrowing that failed: the identity of the output record
// with a "-failed" suffix. No output record's identity ends in it.
func TraceFailureDocumentID(traceID, check string) string {
	return TraceDocumentID(traceID, check) + "-failed"
}

// TraceFailureDocumentPath is the path of the failure record of a trace ID
// with its narrowing.
func TraceFailureDocumentPath(traceID, check string) string {
	return TracePathPrefix + TraceFailureDocumentID(traceID, check) + ".json"
}

// TraceFailureRecord is the content of a recorded automatic fetch that
// produced no output.
type TraceFailureRecord struct {
	TraceID string `json:"trace_id"`
	// Check is the check the fetch was narrowed to; empty for the whole trace.
	Check      string             `json:"check,omitempty"`
	WholeTrace bool               `json:"whole_trace"`
	FailedAt   time.Time          `json:"failed_at"`
	Reason     TraceFailureReason `json:"reason"`
	// Message says the cause in words, scrubbed and bounded.
	Message string `json:"message"`
}

// traceFailureMessageLimit bounds the message a failure record keeps.
const traceFailureMessageLimit = traceFailureExcerpt + 512

// LookupFailure returns the recorded failure of the automatic fetch of
// traceID narrowed to check, or false when none is recorded.
func (f TraceFetcher) LookupFailure(stream config.WorkstreamID, traceID, check string) (TraceFailureRecord, bool, error) {
	if err := ValidateTraceID(traceID); err != nil {
		return TraceFailureRecord{}, false, err
	}
	docs, err := trace.Read[trace.Document](f.Repository, stream)
	if err != nil {
		return TraceFailureRecord{}, false, err
	}
	id, path := TraceFailureDocumentID(traceID, check), TraceFailureDocumentPath(traceID, check)
	var found *trace.Document
	for i := range docs {
		if docs[i].ID == id && docs[i].Path == path && (found == nil || docs[i].Revision >= found.Revision) {
			found = &docs[i]
		}
	}
	if found == nil {
		return TraceFailureRecord{}, false, nil
	}
	var rec TraceFailureRecord
	if err := json.Unmarshal([]byte(found.Content), &rec); err != nil || rec.TraceID != traceID || rec.Check != check || rec.WholeTrace != (check == "") {
		return TraceFailureRecord{}, false, fmt.Errorf("the recorded trace failure %s is damaged", path)
	}
	return rec, true, nil
}

// FetchOnce fetches and records the output of traceID narrowed to check
// unless an output record or a failure record of that identity exists. A fetch
// that produces nothing is recorded as a failure record naming its reason, so
// the identity is never fetched again. It reports whether a fetch ran. The
// error is a value that was refused or a trace that cannot be read or
// written; a fetch cut short by ctx records nothing.
func (f TraceFetcher) FetchOnce(ctx context.Context, caller TraceCaller, traceID, check string) (bool, error) {
	if err := ValidateTraceID(traceID); err != nil {
		return false, err
	}
	refused := ""
	if check != "" {
		if err := ValidateTraceCheck(check); err != nil {
			refused = "the check name was refused: " + err.Error()
		}
	}
	if refused == "" {
		if _, ok, err := f.Lookup(caller.Workstream, traceID, check); err != nil || ok {
			return false, err
		}
	}
	if _, ok, err := f.LookupFailure(caller.Workstream, traceID, check); err != nil || ok {
		return false, err
	}
	failure := traceFailure(TraceFailed, refused)
	ran := false
	if refused == "" {
		ran = true
		var err error
		_, failure, err = f.Fetch(ctx, caller, traceID, check)
		if err != nil || failure == nil {
			return ran, err
		}
		if ctx.Err() != nil {
			return ran, ctx.Err()
		}
	}
	return ran, f.recordFailure(ctx, caller, traceID, check, failure)
}

func (f TraceFetcher) recordFailure(ctx context.Context, caller TraceCaller, traceID, check string, failure *TraceFailure) error {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	at := now().UTC()
	rec := TraceFailureRecord{TraceID: traceID, Check: check, WholeTrace: check == "", FailedAt: at, Reason: failure.Reason, Message: tailText(redact.Scrub(failure.Message), traceFailureMessageLimit)}
	content, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: TraceFailureDocumentID(traceID, check), Revision: 1, Project: f.Repository.Project(), Workstream: caller.Workstream, At: at, Actor: caller.Actor, Cause: caller.Cause}, Path: TraceFailureDocumentPath(traceID, check), Content: string(content) + "\n"}
	if err := f.Repository.RecordDocuments(context.WithoutCancel(ctx), []trace.Document{doc}); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// WithTraceTimeout bounds each fetch of runner by timeout, whether or not the
// runner honours its context: a fetch still running at the deadline is
// abandoned and fails as a timeout. A runner of nil stays nil.
func WithTraceTimeout(runner TraceRunner, timeout time.Duration) TraceRunner {
	if runner == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = TraceFetchTimeout
	}
	return timedTraceRunner{runner: runner, timeout: timeout}
}

type timedTraceRunner struct {
	runner  TraceRunner
	timeout time.Duration
}

func (t timedTraceRunner) FetchTrace(ctx context.Context, traceID, check string) (TraceOutput, *TraceFailure) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	type answer struct {
		out     TraceOutput
		failure *TraceFailure
	}
	done := make(chan answer, 1)
	go func() {
		out, failure := t.runner.FetchTrace(ctx, traceID, check)
		done <- answer{out, failure}
	}()
	select {
	case a := <-done:
		return a.out, a.failure
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return TraceOutput{}, traceFailure(TraceTimeout, fmt.Sprintf("the fetch timed out after %s", t.timeout))
		}
		return TraceOutput{}, traceFailure(TraceFailed, "the fetch was cancelled")
	}
}
