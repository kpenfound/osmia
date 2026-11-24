package checkselect

import "slices"

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

// Leaves returns how many distinct checks listed names: its links that no
// other listed link is narrower than, and each link whose coverage cannot be
// read.
func Leaves(listed []string) int {
	links := distinct(listed)
	n := len(listedCoverage(links).leaves)
	for _, link := range links {
		if parseAddress(link).opaque {
			n++
		}
	}
	return n
}

// Covered returns how many of the checks listed names the selected links run:
// the leaves some selected link covers, and each link whose coverage cannot be
// read that is selected as it was listed.
func Covered(selected, listed []string) int {
	links := distinct(listed)
	chosen := make([]*address, len(selected))
	for i, link := range selected {
		chosen[i] = parseAddress(link)
	}
	n := 0
	for _, leaf := range listedCoverage(links).leaves {
		if slices.ContainsFunc(chosen, func(c *address) bool { return !c.opaque && c.covers(leaf) }) {
			n++
		}
	}
	for _, link := range links {
		if parseAddress(link).opaque && slices.Contains(selected, link) {
			n++
		}
	}
	return n
}
