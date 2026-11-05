package checkselect

// coverage holds the original listing's leaves. Links that cover each other
// represent the same check; only strictly narrower links make a link a parent.
// Opaque links stay options because their coverage cannot be determined.
type coverage struct {
	leaves []*address
	roots  map[string][]*address
}

func listedCoverage(links []string) *coverage {
	var addresses []*address
	for _, link := range links {
		if a := parseAddress(link); !a.opaque {
			addresses = append(addresses, a)
		}
	}
	c := &coverage{roots: map[string][]*address{}}
	for _, a := range addresses {
		leaf := true
		for _, b := range addresses {
			if a.covers(b) && !b.covers(a) {
				leaf = false
				break
			}
		}
		if leaf {
			c.leaves = append(c.leaves, a)
		}
	}
	return c
}

// broad reports whether link has narrower listed checks and holds all the
// leaves of its root (the same base and only the check-name filters). Roots
// need not be listed, so narrowing cannot introduce a whole collection.
func (c *coverage) broad(link string) bool {
	a := parseAddress(link)
	if a.opaque {
		return false
	}
	root := &address{base: a.base}
	if names, ok := a.values("check"); ok {
		root.filters = []filter{{key: "check", values: names}}
	}
	key := root.String()
	leaves, ok := c.roots[key]
	if !ok {
		for _, leaf := range c.leaves {
			if root.covers(leaf) {
				leaves = append(leaves, leaf)
			}
		}
		c.roots[key] = leaves
	}
	narrower := false
	for _, leaf := range leaves {
		if !a.covers(leaf) {
			return false
		}
		if !leaf.covers(a) {
			narrower = true
		}
	}
	return narrower
}
