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
	_, ok := Pausing(pauses, project, stream)
	return ok
}

// Pausing returns the first pause in force that covers the workstream of the
// project, and whether there is one.
func Pausing(pauses []runtime.Pause, project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool) {
	for _, p := range pauses {
		if covers(p, project, stream) {
			return p, true
		}
	}
	return runtime.Pause{}, false
}

// Stopping returns the first hard pause in force that covers the workstream of
// the project, and whether there is one. A hard pause also stops the turns
// already running on the workstream's threads, except chief-of-staff turns.
func Stopping(pauses []runtime.Pause, project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool) {
	for _, p := range pauses {
		if p.Mode == "hard" && covers(p, project, stream) {
			return p, true
		}
	}
	return runtime.Pause{}, false
}

// covers reports whether the pause covers the workstream of the project: a
// factory pause, a pause on the project or one on the workstream.
func covers(p runtime.Pause, project config.ProjectID, stream config.WorkstreamID) bool {
	switch p.Target.Scope {
	case "factory":
		return true
	case "project":
		return p.Target.Project == project
	case "workstream":
		return p.Target.Project == project && p.Target.Workstream == stream
	}
	return false
}
