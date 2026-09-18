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
	return Paused(pauses, project, c.Workstream)
}

// Paused reports whether a pause in force covers the workstream of the
// project: a factory pause, a pause on the project or one on the workstream.
func Paused(pauses []runtime.Pause, project config.ProjectID, stream config.WorkstreamID) bool {
	for _, p := range pauses {
		switch p.Target.Scope {
		case "factory":
			return true
		case "project":
			if p.Target.Project == project {
				return true
			}
		case "workstream":
			if p.Target.Project == project && p.Target.Workstream == stream {
				return true
			}
		}
	}
	return false
}
