package coreadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/ops"
	"github.com/kpenfound/busybees/core/work"
)

// LedgerAdapter must be shared by all users of its file. External writers are
// unsupported. Reads and appends are serialized to avoid observing a partial append.
type LedgerAdapter struct {
	mu   sync.Mutex
	core *ops.Ledger
}

var _ Ledger = (*LedgerAdapter)(nil)

func NewLedger(dir string, now func() time.Time) *LedgerAdapter {
	return &LedgerAdapter{core: &ops.Ledger{Dir: dir, Now: now}}
}

func (a *LedgerAdapter) Append(ctx context.Context, e LedgerEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.AttemptID == "" || !nonnegative(e.Usage.CostUSD) || e.Usage.Turns < 0 {
		return errors.New("invalid ledger entry")
	}
	scope, err := json.Marshal(e.Scope)
	if err != nil {
		return err
	}
	cost := e.Usage.CostUSD
	if !e.Usage.CostKnown {
		cost = 0
	}
	return a.core.AppendLedger(ops.LedgerEntry{Work: work.Ref{Key: work.Key(scope), Tags: map[string]string{"osmia.cost_known": strconv.FormatBool(e.Usage.CostKnown), "osmia.workstream": e.Scope.Workstream}}, Time: e.At, Role: e.Scope.Role, Session: e.AttemptID, Turns: e.Usage.Turns, CostUSD: cost})
}

// Read returns complete validated entries, never partial accounting on error.
func (a *LedgerAdapter) Read(ctx context.Context, since time.Time) ([]LedgerEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.read(ctx, since)
}
func (a *LedgerAdapter) read(ctx context.Context, since time.Time) ([]LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// TODO: Remove this strict scan when busybees/core supplies fail-closed ledger
	// reads. Its current reader silently discards malformed records.
	f, err := os.Open(a.core.LedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 1<<20)
	var entries []LedgerEntry
	for line := 1; scan.Scan(); line++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var raw ops.LedgerEntry
		if err := json.Unmarshal(scan.Bytes(), &raw); err != nil {
			return nil, fmt.Errorf("ledger line %d: %w", line, err)
		}
		var scope Scope
		known, err := strconv.ParseBool(raw.Work.Tags["osmia.cost_known"])
		if err != nil || json.Unmarshal([]byte(raw.Work.Key), &scope) != nil || raw.Time.IsZero() || raw.Session == "" || raw.Turns < 0 || !nonnegative(raw.CostUSD) || (!known && raw.CostUSD != 0) || raw.Work.Tags["osmia.workstream"] != scope.Workstream || raw.Role != scope.Role {
			return nil, fmt.Errorf("ledger line %d: invalid Osmia accounting record", line)
		}
		canonical, _ := json.Marshal(scope)
		if string(canonical) != string(raw.Work.Key) {
			return nil, fmt.Errorf("ledger line %d: invalid scope key", line)
		}
		if !raw.Time.Before(since) {
			entries = append(entries, LedgerEntry{Scope: scope, AttemptID: raw.Session, At: raw.Time, Usage: Usage{raw.CostUSD, known, raw.Turns}})
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (a *LedgerAdapter) Spend(ctx context.Context, query SpendQuery) (Spend, error) {
	entries, err := a.Read(ctx, query.Since)
	if err != nil {
		return Spend{}, err
	}
	selected := []ops.LedgerEntry{}
	total := Spend{}
	for _, e := range entries {
		if len(query.Workstreams) != 0 && !slices.Contains(query.Workstreams, e.Scope.Workstream) {
			continue
		}
		selected = append(selected, ops.LedgerEntry{Time: e.At, CostUSD: e.Usage.CostUSD})
		if !e.Usage.CostKnown {
			total.UnknownCosts++
		}
	}
	total.CostUSD, _ = ops.Spend(selected, "", query.Since)
	if !nonnegative(total.CostUSD) {
		return Spend{}, errors.New("ledger total overflow")
	}
	return total, nil
}
