package doctor

import (
	"context"
	"os/exec"

	"github.com/kpenfound/osmia/internal/skills"
)

// Tools is what running turns of a profile takes on one machine: the programs
// doctor checks for the service's own profiles.
type Tools struct {
	// Agent is the backend program, such as claude or codex.
	Agent string
	// Container and Sbx say that a role in the profile's chain runs its turns in
	// the container engine or in Docker Sandboxes.
	Container, Sbx bool
	// Dagger says that a role whose main profile this is runs Dagger.
	Dagger bool
	// Skills are the skill references of the roles, which must be fetchable.
	Skills []string
}

// CheckTools runs the tool checks doctor runs for the service's profiles
// against the machine it runs on: the backend's --version, the container
// engine or sbx, dagger, and that each skill's repository answers. A failed
// check blocks the profile; a warning does not. The remediations speak to the
// owner of the machine, who has no profile settings to change.
func CheckTools(ctx context.Context, o Options, need Tools) []Check {
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Command == nil {
		o.Command = command
	}
	r := &run{Options: o}
	if need.Agent != "" {
		r.agentTool(ctx, need.Agent, "install "+need.Agent+" on this machine; profiles that use it cannot run here without it")
	}
	if need.Container {
		r.containerTool(ctx, "install and start the container engine on this machine; roles with sandbox = \"container\" run their turns in it")
	}
	if need.Sbx {
		r.sbxTool(ctx, "install Docker Sandboxes on this machine; roles with sandbox = \"sbx\" run their turns in it")
	}
	if need.Dagger {
		r.daggerTool(ctx, Fail, "install dagger on this machine; the mason runs each change's checks with dagger check")
	}
	for _, ref := range need.Skills {
		r.skill(ctx, ref)
	}
	return r.checks
}

// skill checks that the repository a skill reference names answers.
func (r *run) skill(ctx context.Context, ref string) {
	name := "skill " + ref
	spec, err := skills.Parse(ref)
	if err != nil {
		r.add(GroupToolchain, name, Fail, reason(err), "fix the skill reference in the server's configuration")
		return
	}
	path, err := r.LookPath("git")
	if err != nil {
		r.add(GroupToolchain, name, Fail, "git is not on PATH", "install git; skills are fetched with it")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if _, err := r.Command(ctx, path, "ls-remote", "--exit-code", spec.URL); err != nil {
		r.add(GroupToolchain, name, Fail, spec.URL+" cannot be fetched: "+reason(err), "check that this machine can reach "+spec.URL+" and holds the credentials it needs: git ls-remote "+spec.URL)
		return
	}
	r.add(GroupToolchain, name, Pass, spec.URL+" can be fetched", "")
}
