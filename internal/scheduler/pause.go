package scheduler

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// Held reports whether a pause in force holds the candidate's turn. A pause
// covers the factory, a project or one of its workstreams; chief-of-staff turns
// are never held, so the owner can still reach the chief of staff.
func Held(pauses []runtime.Pause, project config.ProjectID, c Candidate) bool {
	if c.Thread.Identity.Role == trace.ChiefOfStaff {
		return false
	}
	for _, p := range pauses {
		switch p.Target.Scope {
		case "factory":
			return true
		case "project":
			if p.Target.Project == project {
				return true
			}
		case "workstream":
			if p.Target.Project == project && p.Target.Workstream == c.Workstream {
				return true
			}
		}
	}
	return false
}
