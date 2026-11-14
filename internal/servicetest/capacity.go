package servicetest

import (
	"slices"
	"time"
)

// PoolTime is how a pool spent a window, as the capacity query in
// docs/telemetry.md reports it. Running is the time pool limit spans cover;
// the rest divide it. AtCapacity is zero for an unbounded pool, which is only
// ever busy or idle.
type PoolTime struct {
	Running, AtCapacity, Busy, Idle time.Duration
}

// PoolTimes computes, from pool limit and held-slot spans alone, how long each
// pool was at capacity, busy and idle within [from, to). It cuts each pool's
// time wherever a limit or a held slot starts or ends and compares the slots
// held in a piece with the limit in force in that piece, so a limit that
// changes while slots stay held is handled piece by piece. A pool with limit
// spans and no held span is idle throughout them.
func (s Spans) PoolTimes(from, to time.Time) map[string]PoolTime {
	type change struct {
		at                         time.Time
		held, covered, limit, open int
	}
	byPool := map[string][]change{}
	for _, sp := range s {
		if sp.Name != "osmia.slot.held" && sp.Name != "osmia.pool.limit" {
			continue
		}
		start, end := sp.StartTime, sp.EndTime
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !start.Before(end) {
			continue
		}
		pool, _ := Attr(sp, "osmia.pool.id")
		var c change
		if sp.Name == "osmia.slot.held" {
			c.held = 1
		} else {
			c.covered = 1
			if unbounded, _ := Attr(sp, "osmia.pool.unbounded"); unbounded == "true" {
				c.open = 1
			} else {
				for _, kv := range sp.Attributes {
					if string(kv.Key) == "osmia.pool.limit" {
						c.limit = int(kv.Value.AsInt64())
					}
				}
			}
		}
		opening, closing := c, c
		opening.at = start
		closing.at, closing.held, closing.covered, closing.limit, closing.open = end, -c.held, -c.covered, -c.limit, -c.open
		byPool[pool] = append(byPool[pool], opening, closing)
	}
	out := map[string]PoolTime{}
	for pool, changes := range byPool {
		slices.SortStableFunc(changes, func(a, b change) int { return a.at.Compare(b.at) })
		var held, covered, limit, open int
		var t PoolTime
		for i, c := range changes {
			held, covered, limit, open = held+c.held, covered+c.covered, limit+c.limit, open+c.open
			if i+1 == len(changes) {
				break
			}
			piece := changes[i+1].at.Sub(c.at)
			if covered <= 0 || piece <= 0 {
				continue
			}
			t.Running += piece
			if held > 0 {
				t.Busy += piece
			} else {
				t.Idle += piece
			}
			if open == 0 && held >= limit {
				t.AtCapacity += piece
			}
		}
		out[pool] = t
	}
	return out
}
