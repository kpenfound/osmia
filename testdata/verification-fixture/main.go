// Package main is the verification fixture's lever: a Dagger module that
// starts the fixture service and drives it with the shared web-driver module.
//
// The fixture is the product Osmia's own tests verify the lever contract
// against. Its verify function takes the run ID from the constructor, runs a
// named flow or an ad-hoc step list, and returns the report Directory.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dagger/verification-fixture/internal/dagger"
)

const nodeImage = "node:22-bookworm-slim@sha256:be979e5523c76fa4d5ca4ebd3099078f220aa47ec39c48ad5b5df9314debd6eb"

const appPort = 8080

type namedFlow struct {
	name  string
	steps string
}

var flows = []namedFlow{
	{"home-page", "goto .; snapshot; screenshot; console; requests"},
	{"add-note", "goto notes; fill id=note | Remember the milk; click text=Add note; snapshot; screenshot; requests"},
}

// VerificationFixture is the lever of the verification fixture project.
type VerificationFixture struct {
	// +private
	RunID string
}

// New returns the lever for one run. The run ID is the service-generated ID
// that makes the app Service and the browser start anew for the run.
func New(
	// The run ID of this lever run. Only verify needs it.
	// +optional
	runID string,
) *VerificationFixture {
	return &VerificationFixture{RunID: runID}
}

// List returns the named flows as a JSON array of strings.
func (m *VerificationFixture) List() (string, error) {
	names := make([]string, 0, len(flows))
	for _, f := range flows {
		names = append(names, f.name)
	}
	out, err := json.Marshal(names)
	return string(out), err
}

// Verify starts the fixture service anew for the run ID, drives it with the
// named flow or the ad-hoc step list, and returns the report Directory. Give
// exactly one of flow and steps.
func (m *VerificationFixture) Verify(
	// The fixture service source.
	// +defaultPath="app"
	app *dagger.Directory,
	// A named flow from List.
	// +optional
	flow string,
	// An ad-hoc step list in the web-driver step encoding. Its claims are untagged.
	// +optional
	steps string,
) (*dagger.Directory, error) {
	if strings.TrimSpace(m.RunID) == "" {
		return nil, errors.New("run ID is required: pass --run-id before the function")
	}
	if (flow == "") == (steps == "") {
		return nil, errors.New("give exactly one of --flow and --steps")
	}
	if flow != "" {
		found := false
		for _, f := range flows {
			if f.name == flow {
				steps, found = f.steps, true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown flow %q: call list for the flows", flow)
		}
	}
	server := dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).
		From(nodeImage).
		WithDirectory("/app", app).
		WithWorkdir("/app").
		WithEnvVariable("PORT", fmt.Sprint(appPort))
	return dag.WebDriver().RunApp(server, m.RunID, steps, dagger.WebDriverRunAppOpts{
		Port: appPort,
		Flow: flow,
		Args: []string{"node", "/app/server.js"},
	}), nil
}
