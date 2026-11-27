// Package main is the shared web driver: a Dagger module that drives a bound
// Service with Playwright and Chromium and returns a version 1 verification
// report with its evidence files.
//
// Products install it by git ref and call it from their own verify function.
// Run and RunApp take the run ID that Osmia passes to the product's lever, and
// fold it into the driver's inputs so each run executes anew. The step list is
// documented in README.md.
package main

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"dagger/web-driver/internal/dagger"
)

const playwrightImage = "mcr.microsoft.com/playwright:v1.56.1-noble@sha256:f1e7e01021efd65dd1a2c56064be399f3e4de00fd021ac561325f2bfbb2b837a"

const driverDir = "/opt/driver"

//go:embed driver/driver.js
var driverScript string

//go:embed driver/package.json
var driverPackage string

//go:embed driver/package-lock.json
var driverLock string

// WebDriver drives a web service with Playwright and Chromium.
type WebDriver struct{}

// Run drives the bound Service with a step list and returns a Directory with
// report.json (version 1) and the evidence files its claims name.
//
// The Service is bound inside the driver container under the alias app and
// reached at http://app:<port>. No host port is published. The run ID is part
// of the driver container's inputs, so every run ID executes the browser
// anew. The Service must also be derived from the run ID for the app instance
// to start anew: RunApp does that, and a product that builds its own Service
// folds the run ID into the Service's container, for example as an
// environment variable.
//
// +cache="never"
func (m *WebDriver) Run(
	// The Service to drive.
	app *dagger.Service,
	// The run ID the service gave this lever run.
	runID string,
	// The step list: steps separated by ; or newlines, each a verb followed by
	// arguments separated by |. Verbs are goto, click, fill, press, snapshot,
	// screenshot, console and requests.
	steps string,
	// The port the Service listens on.
	// +default=8080
	port int,
	// The flow name that tags every claim. Empty leaves the claims untagged.
	// +optional
	flow string,
	// Seconds each step may take before it fails.
	// +default=10
	stepTimeout int,
) (*dagger.Directory, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run ID must not be empty")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	if stepTimeout < 1 || stepTimeout > 120 {
		return nil, fmt.Errorf("step timeout %d is outside 1 to 120 seconds", stepTimeout)
	}
	driver := dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).
		From(playwrightImage).
		WithWorkdir(driverDir).
		WithNewFile(driverDir+"/package.json", driverPackage).
		WithNewFile(driverDir+"/package-lock.json", driverLock).
		WithExec([]string{"npm", "ci", "--no-audit", "--no-fund"}).
		WithNewFile(driverDir+"/driver.js", driverScript)
	run := driver.
		WithServiceBinding("app", app).
		WithEnvVariable("OSMIA_RUN_ID", runID).
		WithEnvVariable("OSMIA_BASE_URL", fmt.Sprintf("http://app:%d", port)).
		WithEnvVariable("OSMIA_FLOW", flow).
		WithEnvVariable("OSMIA_STEP_TIMEOUT_MS", fmt.Sprint(stepTimeout*1000)).
		WithEnvVariable("OSMIA_OUT", "/out").
		WithEnvVariable("OSMIA_STEPS", steps).
		WithExec([]string{"node", driverDir + "/driver.js"})
	return run.Directory("/out"), nil
}

// RunApp starts the app container as a Service that is unique to the run ID,
// then drives it like Run. The app container needs an entrypoint or Args that
// start its server on the port.
//
// +cache="never"
func (m *WebDriver) RunApp(
	// The built app. Its server starts through Args or its entrypoint.
	app *dagger.Container,
	// The run ID the service gave this lever run.
	runID string,
	// The step list, as for Run.
	steps string,
	// The port the app listens on.
	// +default=8080
	port int,
	// The flow name that tags every claim. Empty leaves the claims untagged.
	// +optional
	flow string,
	// Seconds each step may take before it fails.
	// +default=10
	stepTimeout int,
	// The command that starts the app. Empty uses the container entrypoint.
	// +optional
	args []string,
) (*dagger.Directory, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run ID must not be empty")
	}
	service := app.
		WithEnvVariable("OSMIA_RUN_ID", runID).
		WithExposedPort(port).
		AsService(dagger.ContainerAsServiceOpts{Args: args, UseEntrypoint: len(args) == 0})
	return m.Run(service, runID, steps, port, flow, stepTimeout)
}
