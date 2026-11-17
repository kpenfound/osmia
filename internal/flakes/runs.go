package flakes

import (
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
)

// RunKey is the key of the run record at path in the trace of stream. Two
// workstreams record runs at the same paths, so the run an occurrence is keyed
// by names its workstream as well.
func RunKey(stream config.WorkstreamID, path string) string {
	return string(stream) + "/" + path
}

// RunPath is the path of the occurrence's run record in the trace of its
// originating workstream.
func (o Occurrence) RunPath() string {
	return strings.TrimPrefix(o.Run, string(o.Source.Workstream)+"/")
}

// CheckLinks returns the check links among items, without repeats, in order:
// the items that are Dagger links rather than Dagger Cloud check names.
func CheckLinks(items ...string) []string {
	var out []string
	for _, item := range items {
		if (strings.HasPrefix(item, "dag://") || strings.HasPrefix(item, "dag+check://")) && !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// GroupLinks are the flaky checks of one run that fall in one flake group.
type GroupLinks struct {
	// Group is the group's key, and Unresolved is set when it is a Dagger Cloud
	// check name tied to no known check.
	Group      string
	Unresolved bool
	Links      []string
}

// GroupFlaky splits the flaky checks of a run, local links or Dagger Cloud
// check names, by flake group, in the order each group first appears. known
// are the check links the project is known to run, which tie a Cloud name to
// the check it stands for. A link no group can be derived from is a group of
// its own, unresolved, so no flaky check is lost.
func GroupFlaky(known, flaky []string) []GroupLinks {
	resolver := checkselect.NewFlakeResolver(known)
	var out []GroupLinks
	for _, check := range flaky {
		check = strings.TrimSpace(check)
		if check == "" {
			continue
		}
		group := resolver.CloudName(check)
		i := slices.IndexFunc(out, func(g GroupLinks) bool { return g.Group == group.Key })
		if i < 0 {
			out = append(out, GroupLinks{Group: group.Key, Unresolved: group.Unresolved})
			i = len(out) - 1
		}
		if !slices.Contains(out[i].Links, check) {
			out[i].Links = append(out[i].Links, check)
		}
	}
	return out
}

// FlakeFixWorkstream reports whether workstream is a flake-fix workstream of
// the groups' record: one a group has open, closed, or decided an occurrence
// to open, to be opening or to have joined.
func FlakeFixWorkstream(groups []Group, workstream config.WorkstreamID) bool {
	if workstream == "" {
		return false
	}
	for _, g := range groups {
		if g.Open != nil && g.Open.Workstream == workstream {
			return true
		}
		if slices.ContainsFunc(g.Closed, func(c Closure) bool { return c.Workstream == workstream }) {
			return true
		}
		for _, o := range g.Occurrences {
			switch o.Decision.State {
			case Opening, Opened, Joined:
				if o.Decision.Workstream == workstream {
					return true
				}
			}
		}
	}
	return false
}

// IsFlakeFix reports whether workstream is a flake-fix workstream of the
// project's flake record.
func (s *Store) IsFlakeFix(workstream config.WorkstreamID) (bool, error) {
	groups, err := s.Groups()
	if err != nil {
		return false, err
	}
	return FlakeFixWorkstream(groups, workstream), nil
}
