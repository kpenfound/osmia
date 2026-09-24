package thread

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// AttemptID is the stable accounting identity of one turn attempt.
func AttemptID(request string, attempt int) string {
	return trace.EventID(request, fmt.Sprintf("attempt-%d", attempt))
}

// costs appends one ledger record per attempt that returned a result. Records
// already present under the same identity are kept, so a retry after restart
// adds only the missing entries.
func (r Runner) costs(ctx context.Context, role string, q trace.QueuedTurn) error {
	existing, err := trace.Read[trace.Cost](r.Store, q.Request.Workstream)
	if err != nil {
		return err
	}
	recorded := map[string]bool{}
	for _, c := range existing {
		recorded[c.ID] = true
	}
	req := q.Request
	for _, a := range q.Attempts {
		if a.Result == nil {
			continue
		}
		h := req.Header
		h.Schema, h.ID, h.Revision, h.At, h.Actor = "osmia.trace.cost", trace.EventID(req.ID, fmt.Sprintf("cost-%d", a.Number)), 1, r.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
		if recorded[h.ID] {
			continue
		}
		scope := coreadapter.Scope{Project: string(req.Project), Workstream: string(req.Workstream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: role}
		cost := trace.Cost{Header: h, Entry: coreadapter.LedgerEntry{Scope: scope, AttemptID: AttemptID(req.ID, a.Number), At: a.At, Usage: a.Result.Usage}}
		if !cost.Entry.Usage.CostKnown {
			cost.Entry.Usage.CostUSD = 0
		}
		if err := r.Store.Append(ctx, cost); err != nil {
			return err
		}
	}
	if q.Response != nil {
		for i, usage := range q.Response.ClassifierUsage {
			h := req.Header
			h.Schema, h.ID, h.Revision, h.At, h.Actor = "osmia.trace.cost", trace.EventID(req.ID, fmt.Sprintf("classifier-cost-%d", i+1)), 1, r.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			if recorded[h.ID] {
				continue
			}
			if !usage.CostKnown {
				usage.CostUSD = 0
			}
			scope := coreadapter.Scope{Project: string(req.Project), Workstream: string(req.Workstream), Unit: req.Unit, Thread: req.ThreadID, Turn: req.TurnID, Role: role}
			cost := trace.Cost{Header: h, Entry: coreadapter.LedgerEntry{Scope: scope, AttemptID: trace.EventID(req.ID, fmt.Sprintf("classifier-attempt-%d", i+1)), At: q.Response.Result.StartedAt, Usage: usage}}
			if err := r.Store.Append(ctx, cost); err != nil {
				return err
			}
		}
	}
	return nil
}
