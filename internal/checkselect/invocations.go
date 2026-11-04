package checkselect

import (
	"slices"
	"strings"
)

// Invocations splits links into the addresses of separate dagger check runs,
// one address each, that together select exactly the checks the links select.
// No links is one run of every check.
//
// Dagger evaluates one run under a single filtered view of the workspace, so
// it refuses links whose filters differ, and filters ANDed in one address are
// a cross-product. Links therefore share an address only where the
// cross-product adds nothing: when they differ in the values of a single
// filter, or when one link already covers another.
func Invocations(links []string) [][]string {
	if len(links) == 0 {
		return [][]string{nil}
	}
	var addresses []*address
	for _, link := range links {
		addresses = append(addresses, parseAddress(link))
	}
	for combined := true; combined; {
		combined = false
		for i := 0; i < len(addresses); i++ {
			for j := i + 1; j < len(addresses); j++ {
				if union, ok := combine(addresses[i], addresses[j]); ok {
					addresses[i] = union
					addresses = slices.Delete(addresses, j, j+1)
					combined = true
					j--
				}
			}
		}
	}
	out := make([][]string, len(addresses))
	for i, a := range addresses {
		out[i] = []string{a.String()}
	}
	return out
}

// address is a parsed check link: everything before its query, and the values
// of each filter in the order they first appear. An opaque address has a form
// this package does not read, and only combines with an identical link.
type address struct {
	base    string
	filters []filter
	opaque  bool
}

type filter struct {
	key    string
	values []string
}

func parseAddress(link string) *address {
	base, query, _ := strings.Cut(link, "?")
	if !strings.Contains(base, "://") {
		return &address{base: link, opaque: true}
	}
	a := &address{base: base}
	if query == "" {
		return a
	}
	for pair := range strings.SplitSeq(query, "&") {
		key, value, keyed := strings.Cut(pair, "=")
		if !keyed {
			// A filter without a value selects the whole dimension.
			return &address{base: link, opaque: true}
		}
		a.add(key, value)
	}
	return a
}

func (a *address) add(key, value string) {
	for i := range a.filters {
		if a.filters[i].key == key {
			if !slices.Contains(a.filters[i].values, value) {
				a.filters[i].values = append(a.filters[i].values, value)
			}
			return
		}
	}
	a.filters = append(a.filters, filter{key: key, values: []string{value}})
}

func (a *address) values(key string) ([]string, bool) {
	for _, f := range a.filters {
		if f.key == key {
			return f.values, true
		}
	}
	return nil, false
}

func (a *address) String() string {
	if len(a.filters) == 0 {
		return a.base
	}
	var pairs []string
	for _, f := range a.filters {
		for _, value := range f.values {
			pairs = append(pairs, f.key+"="+value)
		}
	}
	return a.base + "?" + strings.Join(pairs, "&")
}

// combine returns the one address selecting exactly what a and b select
// together, and false when no such address exists.
func combine(a, b *address) (*address, bool) {
	if a.opaque || b.opaque {
		return a, a.opaque && b.opaque && a.base == b.base
	}
	if a.covers(b) {
		return a, true
	}
	if b.covers(a) {
		return b, true
	}
	if a.base != b.base || len(a.filters) != len(b.filters) {
		return nil, false
	}
	differing := ""
	for _, f := range a.filters {
		values, ok := b.values(f.key)
		if !ok {
			return nil, false
		}
		if sameValues(f.values, values) {
			continue
		}
		if differing != "" {
			return nil, false
		}
		differing = f.key
	}
	union := &address{base: a.base}
	for _, f := range a.filters {
		union.filters = append(union.filters, filter{key: f.key, values: slices.Clone(f.values)})
	}
	values, _ := b.values(differing)
	for _, value := range values {
		union.add(differing, value)
	}
	return union, true
}

// covers reports whether a selects everything other selects: an address with
// no path or filters selects every check of its scheme, and otherwise each of
// a's filters must allow all of other's values for that filter.
func (a *address) covers(other *address) bool {
	if len(a.filters) == 0 && strings.HasSuffix(a.base, "://") {
		return schemeOf(a.base) == schemeOf(other.base)
	}
	if a.base != other.base {
		return false
	}
	for _, f := range a.filters {
		values, ok := other.values(f.key)
		if !ok {
			return false
		}
		for _, value := range values {
			if !slices.Contains(f.values, value) {
				return false
			}
		}
	}
	return true
}

func schemeOf(base string) string {
	scheme, _, _ := strings.Cut(base, "://")
	return scheme
}

func sameValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !slices.Contains(b, value) {
			return false
		}
	}
	return true
}
