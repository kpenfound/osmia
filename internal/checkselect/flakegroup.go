package checkselect

import (
	"errors"
	"slices"
	"strings"
)

// The item filters of a Go test collection, coarsest first. A flaky test
// groups with its package.
const (
	goModuleFilter  = "go-module"
	goPackageFilter = "go-package"
	goTestFilter    = "go-test"
)

// ErrBroadFlakeCheck is the error of a link that names a whole collection or
// module rather than one check. No flake group is that broad.
var ErrBroadFlakeCheck = errors.New("the check is a collection root or module, broader than a package")

// FlakeGroup is the unit flakes are grouped by: the address of one package or
// standalone check. Key is that address in a canonical form, so the same
// check yields the same Key whichever way it was reported. A group tied to no
// known check is Unresolved, and its Key is the name Dagger Cloud reported.
type FlakeGroup struct {
	Key        string
	Unresolved bool
}

// FlakeResolver derives flake groups from the links of a project's known
// checks: the listing of its checks, collections expanded. A flaky link and a
// Dagger Cloud check name for the same check resolve to the same group.
type FlakeResolver struct {
	known []string
	// full and suffix map a Cloud check name to the groups of the known checks
	// it can stand for; the first is built from whole check names and the
	// second from their trailing path segments.
	full, suffix map[string]map[string]bool
}

// NewFlakeResolver builds a resolver for the project's known check links.
func NewFlakeResolver(known []string) *FlakeResolver {
	r := &FlakeResolver{known: distinct(known), full: map[string]map[string]bool{}, suffix: map[string]map[string]bool{}}
	for _, link := range r.known {
		r.index(link)
	}
	return r
}

// Link returns the group of a check link. A link with item filters groups
// with its enclosing package (for Go, its go-package: the go-test filter is
// dropped), and a package or standalone check is its own group. A link that
// names a collection root or a module, as the known checks show it, returns
// ErrBroadFlakeCheck.
func (r *FlakeResolver) Link(link string) (FlakeGroup, error) {
	a := parseAddress(link)
	if a.opaque {
		return FlakeGroup{Key: strings.TrimSpace(link)}, nil
	}
	group, _, err := r.cut(a)
	if err != nil {
		return FlakeGroup{}, err
	}
	return FlakeGroup{Key: group.canonical()}, nil
}

// CloudName returns the group of a Dagger Cloud check name. The name is tied
// to a known check when it is the check's name, or the check's name followed
// by the values of some of its item filters (a test, or a package and a test),
// or just its name when one group is all it has. A name that fits no known check, fits several
// groups, or names a collection or module, is a group of its own, Unresolved.
func (r *FlakeResolver) CloudName(name string) FlakeGroup {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "dag://") || strings.HasPrefix(name, "dag+check://") {
		if g, err := r.Link(name); err == nil {
			return g
		}
		return FlakeGroup{Key: name, Unresolved: true}
	}
	alias := aliasName(name)
	for _, index := range []map[string]map[string]bool{r.full, r.suffix} {
		if groups := index[alias]; len(groups) == 1 {
			for key := range groups {
				return FlakeGroup{Key: key}
			}
		} else if len(groups) > 1 {
			break
		}
	}
	return FlakeGroup{Key: name, Unresolved: true}
}

// cut returns a cut to its group: the package or standalone check, and how
// many of its item filters it keeps.
func (r *FlakeResolver) cut(a *address) (*address, int, error) {
	// The filters after the package's, and the test, are below the group.
	// Go's own levels are dropped by name, so the order a link lists its
	// filters in does not matter.
	items := itemFilters(a)
	pkg := slices.Index(items, goPackageFilter)
	group := &address{base: a.base}
	kept, position := 0, 0
	for _, f := range a.filters {
		if f.key != "check" {
			below := f.key == goTestFilter || pkg >= 0 && position > pkg && f.key != goModuleFilter
			position++
			if below {
				continue
			}
			kept++
		}
		group.filters = append(group.filters, f)
	}
	if values, ok := group.values(goPackageFilter); ok && len(values) > 1 {
		return nil, 0, ErrBroadFlakeCheck
	}
	if r.broadGroup(group) {
		return nil, 0, ErrBroadFlakeCheck
	}
	return group, kept, nil
}

// broadGroup reports whether group is a collection root or module the known
// checks list narrower checks under. A package or any collection item with nothing narrower is not.
func (r *FlakeResolver) broadGroup(group *address) bool {
	// A package, or an item of a collection that has no Go levels, is the group.
	items := itemFilters(group)
	if _, packaged := group.values(goPackageFilter); packaged || len(items) > 0 && !slices.Equal(items, []string{goModuleFilter}) {
		return false
	}
	for _, link := range r.known {
		b := parseAddress(link)
		if !b.opaque && group.covers(b) && !b.covers(group) {
			return true
		}
	}
	return false
}

// canonical writes the address with its filters in key order, so the same
// check has one spelling whatever order its filters were listed in.
func (a *address) canonical() string {
	if len(a.filters) == 0 {
		return a.base
	}
	filters := slices.Clone(a.filters)
	slices.SortFunc(filters, func(x, y filter) int { return strings.Compare(x.key, y.key) })
	var pairs []string
	for _, f := range filters {
		for _, value := range slices.Sorted(slices.Values(f.values)) {
			pairs = append(pairs, f.key+"="+value)
		}
	}
	return a.base + "?" + strings.Join(pairs, "&")
}

// index records the names a known link can be reported under, if it has a group.
func (r *FlakeResolver) index(link string) {
	a := parseAddress(link)
	if a.opaque {
		return
	}
	group, kept, err := r.cut(a)
	if err != nil {
		return
	}
	var values []string
	for _, f := range a.filters {
		if f.key == "check" {
			continue
		}
		if len(f.values) != 1 {
			values = nil
			break
		}
		values = append(values, f.values[0])
	}
	add := func(index map[string]map[string]bool, name string) {
		if index[name] == nil {
			index[name] = map[string]bool{}
		}
		index[name][group.canonical()] = true
	}
	full, suffix := checkNames(a)
	for _, tier := range []struct {
		index map[string]map[string]bool
		names []string
	}{{r.full, full}, {r.suffix, suffix}} {
		for _, name := range tier.names {
			// The bare name is the check's when only one group is under it.
			add(tier.index, name)
			// A name that stops above the group's own item names a collection
			// or module, which is no group's.
			for from := range min(kept, len(values)) {
				for to := kept; to <= len(values); to++ {
					add(tier.index, name+"/"+strings.Join(values[from:to], "/"))
				}
			}
		}
	}
}

// checkNames are the names a link's check goes by: its check filter values or
// its path, and then each trailing run of their path segments.
func checkNames(a *address) (full, suffix []string) {
	names, ok := a.values("check")
	if !ok {
		names = []string{strings.TrimPrefix(strings.TrimPrefix(a.base, "dag+check://"), "dag://")}
	}
	for _, name := range names {
		name = aliasName(name)
		full = append(full, name)
		segments := strings.Split(name, "/")
		for i := 1; i < len(segments); i++ {
			suffix = append(suffix, strings.Join(segments[i:], "/"))
		}
	}
	return full, suffix
}

// aliasName writes a check or Cloud name with one separator between segments.
func aliasName(name string) string {
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(name), ":", "/"), "/")
}
