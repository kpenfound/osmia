package delivery

import (
	"context"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// AutoTraceTimeout is how long one automatic trace fetch may hold the final
// review when the service sets no timeout of its own.
const AutoTraceTimeout = checks.TraceFetchTimeout

// tracePair is one failed check of a recorded result with the trace its
// status names: the identity of an automatic trace fetch.
type tracePair struct {
	TraceID string
	Check   string
}

// failedTracePairs lists the failed (trace ID, check) pairs of a recorded
// failed Dagger Cloud result, each once, in the order the record lists them.
// A check without a trace ID, a trace ID the fetch would refuse, and any
// result that is not a failed pull request result have none.
func failedTracePairs(run FinalCheckRun) []tracePair {
	if run.Source != api.CheckSourcePullRequest || run.Status != "failed" {
		return nil
	}
	var pairs []tracePair
	for _, f := range run.FailedChecks {
		pair := tracePair{TraceID: f.TraceID, Check: f.Name}
		if f.TraceID == "" || checks.ValidateTraceID(f.TraceID) != nil || slices.Contains(pairs, pair) {
			continue
		}
		pairs = append(pairs, pair)
	}
	return pairs
}

// finalTraceView is the folder of the reader's view, under the check result's
// own, that holds the recorded trace output of each failed check.
func finalTraceView(k int) string { return fmt.Sprintf("final/checks-%d-traces", k) }

// ReconcileFinalTraces makes sure each failed check of the recorded result run
// that names a trace ID has an output record or a failure record, fetching the
// trace output narrowed to that check only for the pairs that have neither.
// Each pair is recorded as soon as its fetch ends. A result that is not a
// failed pull request result triggers no fetch. It never touches the check
// result: a fetch that fails is recorded with its reason, and any other
// problem leaves the pair for the next call. Only a cancelled context or an
// interruption of the service is returned.
func (a *FinalReviewer) ReconcileFinalTraces(ctx context.Context, stream config.WorkstreamID, run FinalCheckRun) error {
	pairs := failedTracePairs(run)
	if len(pairs) == 0 || a.S.CloudTrace == nil {
		return nil
	}
	timeout := a.S.AutoTraceTimeout
	if timeout <= 0 {
		timeout = AutoTraceTimeout
	}
	fetcher := checks.TraceFetcher{Repository: a.Repository, Runner: checks.WithTraceTimeout(a.S.CloudTrace, timeout), Now: a.S.Now}
	caller := checks.TraceCaller{Workstream: stream, Actor: checksActor, Cause: run.Operation}
	for _, pair := range pairs {
		if err := ctx.Err(); err != nil {
			return err
		}
		ran, err := fetcher.FetchOnce(ctx, caller, pair.TraceID, pair.Check)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || !ran {
			continue
		}
		if err := a.S.Step("final-trace-recorded"); err != nil {
			return err
		}
	}
	return nil
}

// finalTraceFiles returns the files of the reader's view that carry the
// recorded trace output of each failed check of run, by their path in the
// view: the output record of the (trace ID, check), or when it has none its
// failure record. docs are the workstream's documents.
func finalTraceFiles(docs []trace.Document, run FinalCheckRun) map[string]string {
	latest := func(id, path string) (trace.Document, bool) {
		var found *trace.Document
		for i := range docs {
			if docs[i].ID == id && docs[i].Path == path && (found == nil || docs[i].Revision >= found.Revision) {
				found = &docs[i]
			}
		}
		if found == nil {
			return trace.Document{}, false
		}
		return *found, true
	}
	files := map[string]string{}
	for _, pair := range failedTracePairs(run) {
		id, path := checks.TraceDocumentID(pair.TraceID, pair.Check), checks.TraceDocumentPath(pair.TraceID, pair.Check)
		doc, ok := latest(id, path)
		if !ok {
			id, path = checks.TraceFailureDocumentID(pair.TraceID, pair.Check), checks.TraceFailureDocumentPath(pair.TraceID, pair.Check)
			if doc, ok = latest(id, path); !ok {
				continue
			}
		}
		files[finalTraceView(run.Review)+"/"+id+".json"] = doc.Content
	}
	return files
}

// finalTraceEvidence is the line of the reader's prompt that says where the
// trace output of the failed checks is, or "" when the result has no failed
// check that names a trace ID.
func finalTraceEvidence(run FinalCheckRun) string {
	if len(failedTracePairs(run)) == 0 {
		return ""
	}
	return fmt.Sprintf("- %s/: the trace output the service fetched for each failed check that names a trace ID, one file per check, scrubbed and bounded to its last %d KiB; a check whose fetch failed has the reason instead, and a check missing from the folder has neither.\n", finalTraceView(run.Review), checks.MaxTraceOutput/1024)
}
