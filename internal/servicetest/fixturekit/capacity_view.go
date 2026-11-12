package fixturekit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func RoleCapacityEntry(t *testing.T, c *api.CapacityStatus, role string) api.RoleCapacity {
	t.Helper()
	for _, r := range c.Roles {
		if r.Role == role {
			return r
		}
	}
	t.Fatalf("capacity has no %s: %+v", role, c)
	return api.RoleCapacity{}
}

// Limit is the Limit of a bounded pool's capacity entry.
func Limit(n int) *int { return &n }

// LimitOf is the numeric limit of a bounded pool's entry. It fails the test
// for a pool that reports none.
func LimitOf(t *testing.T, r api.RoleCapacity) int {
	t.Helper()
	if r.Limit == nil {
		t.Fatalf("%s reports no numeric limit: %+v", r.Role, r)
	}
	return *r.Limit
}

// Unbounded is the capacity entry of a pool with no factory-wide limit and
// the given use.
func Unbounded(role string, used int, waiting []api.SlotWait) api.RoleCapacity {
	return api.RoleCapacity{Role: role, Used: used, Unbounded: true, Scope: api.UnboundedScope, Note: api.UnboundedNote, Waiting: waiting}
}

// CapacityWire checks the JSON a service serialized for its capacity view
// (the "capacity" member of the chief's capacity tool result or of
// /v1/status) and returns the decoded pool entries in order. The pools
// bounded factory-wide carry a numeric limit; architect and librarian carry
// none, say in words that they have no factory-wide limit, run at most one
// turn per workstream and have no configuration setting, and no field of any
// entry is a limit of 0.
func CapacityWire(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Capacity struct {
			Roles []map[string]any `json:"roles"`
		} `json:"capacity"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capacity JSON: %v\n%s", err, raw)
	}
	var order []string
	for _, r := range doc.Capacity.Roles {
		order = append(order, r["role"].(string))
		role := r["role"].(string)
		for key, v := range r {
			if n, ok := v.(float64); ok && strings.Contains(key, "limit") && n == 0 {
				t.Fatalf("%s entry has %s = 0: %v", role, key, r)
			}
		}
		_, limited := r["limit"]
		switch role {
		case "architect", "librarian":
			note, _ := r["note"].(string)
			note = strings.ToLower(note)
			if limited || r["unbounded"] != true || r["scope"] != "per_workstream" {
				t.Fatalf("%s entry is not marked unbounded: %v", role, r)
			}
			for _, words := range []string{"no factory-wide limit", "one turn per workstream", "no configuration setting"} {
				if !strings.Contains(note, words) {
					t.Fatalf("%s note lacks %q: %q", role, words, note)
				}
			}
		default:
			if n, ok := r["limit"].(float64); !ok || n < 1 {
				t.Fatalf("%s entry has no configured numeric limit: %v", role, r)
			}
			for _, key := range []string{"unbounded", "scope", "note"} {
				if _, ok := r[key]; ok {
					t.Fatalf("%s entry has %s: %v", role, key, r)
				}
			}
		}
		if _, ok := r["used"].(float64); !ok {
			t.Fatalf("%s entry has no used: %v", role, r)
		}
		if _, ok := r["waiting"].([]any); !ok {
			t.Fatalf("%s entry has no waiting list: %v", role, r)
		}
	}
	if got, want := strings.Join(order, ","), "mason,reviewer,committee,architect,librarian,checks"; got != want {
		t.Fatalf("capacity pool order %s, want %s", got, want)
	}
	return doc.Capacity.Roles
}
