// Package kinds is the registry of workstream kinds. A kind is Go code, not
// a plugin or a setting: it supplies the hooks a workstream's work needs
// while it is building, and the fixed capability flags the rest of the
// factory consults instead of assuming Feature. Every kind shares the outer
// states handed, sketched, in-shed, ratified, building, assembled and
// delivered; only the inner machine that runs while a workstream is building
// belongs to the kind.
//
// Sessions and design sessions are not kinds. A session records the trace's
// session kind and a design session is identified by its design-session
// subject, so neither is ever looked up here.
package kinds

import (
	"fmt"
	"slices"
	"strings"
)

// Feature is the kind of an ordinary workstream: a ratified spec and plan,
// built as units. A stored empty kind is Feature.
const Feature = "feature"

// Flags are a kind's fixed capabilities. Code that iterates workstreams
// consults them instead of assuming Feature.
type Flags struct {
	// Units says the kind is drafted as a spec and plan and built as units.
	Units bool
	// MultiProject says the kind may join a workgroup of several projects.
	MultiProject bool
	// UpstreamDrift says upstream drift may be applied while building.
	UpstreamDrift bool
	// Dependencies says the kind may be a base or have dependency edges.
	Dependencies bool
	// NewProject says the kind may target a brand-new project.
	NewProject bool
}

// Documents names what the architect drafts, the committee debates and the
// owner ratifies, by trace document ID.
type Documents struct {
	Drafted  []string
	Debated  []string
	Ratified []string
}

// Loop describes the inner machine a kind runs while a workstream is
// building.
type Loop struct {
	Name   string
	States []string
}

// PullRequest is what a kind's pull-request section is written from.
type PullRequest struct {
	Workstream string
}

// Units counts a workstream's units by state.
type Units map[string]int

// Progress is a kind's reading of how far a workstream has got. Panel names
// the web panel that draws it, and Summary is its one line.
type Progress struct {
	Panel   string
	Summary string
}

// Settings is a kind's `[kinds.<kind>]` table. A nil Validate accepts only
// an empty table; a nil Skills loads no skills.
type Settings struct {
	Validate func(table map[string]any) error
	Skills   func(table map[string]any) []string
}

// Kind is one registered workstream kind.
type Kind struct {
	Name    string
	UseWhen string
	Flags   Flags

	Documents Documents
	Loop      Loop

	// PullRequestSection returns the kind's section of the pull-request body,
	// or the empty string for none.
	PullRequestSection func(PullRequest) string
	// Progress reads the kind's progress from a workstream's unit counts.
	Progress func(Units) Progress
	// ChiefTools names the tools the kind adds to the chief of staff's.
	ChiefTools []string
	Settings   Settings
}

var unitStates = []string{"planned", "ready", "implementing", "checking", "reviewing", "approved", "merged"}

var feature = Kind{
	Name:      Feature,
	UseWhen:   "the work is a change to the project's code, docs or configuration that is specified, planned and built as units, in one project or across several.",
	Flags:     Flags{Units: true, MultiProject: true, UpstreamDrift: true, Dependencies: true, NewProject: true},
	Documents: Documents{Drafted: []string{"spec", "plan"}, Debated: []string{"spec", "plan"}, Ratified: []string{"spec", "plan"}},
	Loop:      Loop{Name: "units", States: unitStates},
	PullRequestSection: func(PullRequest) string {
		return ""
	},
	Progress: func(units Units) Progress {
		return Progress{Panel: "units", Summary: unitsSummary(units)}
	},
	ChiefTools: []string{"move_unit"},
}

func unitsSummary(units Units) string {
	total := 0
	for _, n := range units {
		total += n
	}
	if total == 0 {
		return ""
	}
	noun := "units"
	if total == 1 {
		noun = "unit"
	}
	var parts []string
	for _, state := range unitStates {
		if n := units[state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	return fmt.Sprintf("%d %s: %s", total, noun, strings.Join(parts, ", "))
}

var registered = []Kind{feature}

// All lists the registered kinds in registration order.
func All() []Kind { return slices.Clone(registered) }

// Names lists the registered kind names in registration order.
func Names() []string {
	names := make([]string, len(registered))
	for i, k := range registered {
		names[i] = k.Name
	}
	return names
}

// Lookup returns the registered kind a stored kind names. A stored empty kind
// is Feature. A stored kind this build does not register is not found, and is
// never Feature.
func Lookup(stored string) (Kind, bool) {
	if stored == "" {
		stored = Feature
	}
	for _, k := range registered {
		if k.Name == stored {
			return k, true
		}
	}
	return Kind{}, false
}

// Normalize returns the registered name a requested kind names, with the
// empty kind defaulting to Feature. An unregistered kind is refused with a
// reason that lists the kinds that are registered.
func Normalize(requested string) (string, error) {
	k, ok := Lookup(strings.TrimSpace(requested))
	if !ok {
		return "", fmt.Errorf("kind %q is not a registered workstream kind; use one of: %s", requested, strings.Join(Names(), ", "))
	}
	return k.Name, nil
}

// ChiefToolNames lists the chief-of-staff tool names any registered kind adds.
func ChiefToolNames() []string {
	var names []string
	for _, k := range registered {
		for _, n := range k.ChiefTools {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return names
}

// CheckBrief checks a design brief of the kind against its flags and refuses
// with a reason the drafting agent can act on.
func (k Kind) CheckBrief(projects int, newProject bool) error {
	if projects > 1 && !k.Flags.MultiProject {
		return fmt.Errorf("a %s workstream cannot join a multi-project workgroup: draft the brief for exactly one project", k.Name)
	}
	if newProject && !k.Flags.NewProject {
		return fmt.Errorf("a %s workstream cannot target a brand-new project: remove new_project and name one existing project", k.Name)
	}
	return nil
}

// CheckDependencies refuses a base or dependency edge on a workstream of a
// kind that cannot have one.
func (k Kind) CheckDependencies() error {
	if !k.Flags.Dependencies {
		return fmt.Errorf("a %s workstream cannot be a base or have dependency edges", k.Name)
	}
	return nil
}

// CheckWorkgroup refuses a workgroup whose members are not all of one
// registered kind, or whose kind cannot join a workgroup of several members.
// A stored empty kind is Feature.
func CheckWorkgroup(stored []string) error {
	var first Kind
	for i, name := range stored {
		k, ok := Lookup(name)
		if !ok {
			return fmt.Errorf("workstream kind %q is not registered by this build", name)
		}
		if i == 0 {
			first = k
			continue
		}
		if k.Name != first.Name {
			return fmt.Errorf("a workgroup holds one kind of workstream, and it has both %s and %s", first.Name, k.Name)
		}
	}
	if len(stored) > 1 && !first.Flags.MultiProject {
		return fmt.Errorf("a %s workgroup has exactly one member", first.Name)
	}
	return nil
}

// Limits describes in words the drafting-time limits a kind's flags set.
func (k Kind) Limits() []string {
	var limits []string
	if !k.Flags.MultiProject {
		limits = append(limits, "exactly one project, never a multi-project workgroup")
	}
	if !k.Flags.NewProject {
		limits = append(limits, "no new_project")
	}
	if !k.Flags.Dependencies {
		limits = append(limits, "no base and no dependency edges")
	}
	return limits
}

// Catalog renders the registry for the design agent: each kind's name, its
// one-line use-when and the limits its flags set at drafting time.
func Catalog() string {
	var b strings.Builder
	for _, k := range registered {
		limits := "none"
		if l := k.Limits(); len(l) > 0 {
			limits = strings.Join(l, "; ")
		}
		fmt.Fprintf(&b, "- %s: use when %s Limits: %s.\n", k.Name, k.UseWhen, limits)
	}
	return b.String()
}
