package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ErrNotRebuilt is what a Rebuilder returns for a record whose exit its pass
// raises again, and not the machine's rebuild: Recover leaves it alone.
var ErrNotRebuilt = errors.New("the exit is raised again by its own pass")

// Rebuilder returns the exit a route record was raised for, with the current
// targets, hard prerequisites and effects of its machine. Recover sets the
// record's subject version, state, trigger, cause and default on the exit, so
// the rebuilt exit is the same occurrence.
type Rebuilder func(ctx context.Context, repo *trace.Repository, rec Record) (Exit, error)

// Rebuilders is the registry of the machines that can rebuild an exit from its
// route record.
type Rebuilders struct {
	mu    sync.RWMutex
	funcs map[string]Rebuilder
}

// Register defines how an exit of machine is rebuilt from its record. A second
// registration of the machine replaces the first.
func (b *Rebuilders) Register(machine string, f Rebuilder) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.funcs == nil {
		b.funcs = map[string]Rebuilder{}
	}
	b.funcs[machine] = f
}

func (b *Rebuilders) get(machine string) Rebuilder {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.funcs[machine]
}

// Pending returns the route records of the workstream whose pick is recorded
// but not applied, in the order they were first written.
func Pending(repo *trace.Repository, stream config.WorkstreamID) ([]Record, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if prior, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
			latest[d.ID] = d
		} else if d.Revision > prior.Revision {
			latest[d.ID] = d
		}
	}
	var out []Record
	for _, id := range order {
		var rec Record
		if json.Unmarshal([]byte(latest[id].Content), &rec) != nil {
			continue
		}
		if rec.State == StateDecided {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Recover finishes the occurrences of the workstream whose pick was recorded
// but not applied when the service stopped. Each is rebuilt by its machine's
// Rebuilder and routed as the occurrence it was: the recorded pick is checked
// against the hard prerequisites again and applied, with no request to Jev. It
// reports how many were finished. A record whose machine cannot rebuild it is
// left for the pass that raised it, which raises the same exit again.
func (r *Router) Recover(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) (int, error) {
	pending, err := Pending(repo, stream)
	if err != nil {
		return 0, err
	}
	finished := 0
	for _, rec := range pending {
		build := r.Rebuild.get(rec.Machine)
		if build == nil {
			continue
		}
		x, err := build(ctx, repo, rec)
		if errors.Is(err, ErrNotRebuilt) {
			continue
		}
		if err != nil {
			r.logger().Warn("a recorded transition pick cannot be rebuilt; its next pass routes it again", "workstream", string(stream), "subject", rec.Subject, "trigger", rec.Trigger, "error", err.Error())
			continue
		}
		x.Version, x.From, x.Trigger, x.Cause, x.Default = rec.Version, rec.From, rec.Trigger, rec.Cause, rec.Default
		x.Actor, x.Note, x.Turn, x.HeaderCause, x.Proposed = rec.Actor, rec.Note, rec.Turn, rec.HeaderCause, rec.Proposed
		if OccurrenceKey(x) != rec.Occurrence {
			r.logger().Warn("a rebuilt exit is not the occurrence that was recorded", "workstream", string(stream), "occurrence", rec.Occurrence)
			continue
		}
		if _, err := r.Route(ctx, x); err != nil {
			if ctx.Err() != nil {
				return finished, ctx.Err()
			}
			return finished, fmt.Errorf("finish the recorded pick of %s: %w", rec.Occurrence, err)
		}
		finished++
	}
	return finished, nil
}
