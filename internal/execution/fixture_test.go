package execution

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/workspace"
)

var must = fixturekit.Must
var demoGit = fixturekit.DemoGit
var registrationActor = intake.RegistrationActor

const stream config.WorkstreamID = "w_0123456789abcdef0123456789abcdef"
const reviewSummary = "The candidate does the task and its acceptance holds"

func providerOf(t *testing.T, w streamWorkspaces, s config.WorkstreamID) workspace.Provider {
	t.Helper()
	g, err := w.Of(s)
	must(t, err)
	return g
}

var demoStart = fixturekit.DemoStart

const project config.ProjectID = "p_0123456789abcdef0123456789abcdef"
