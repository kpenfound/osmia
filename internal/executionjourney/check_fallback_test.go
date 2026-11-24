package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

const fallbackConfig = "\n[dagger]\ncloud_compute_token_env = 'OSMIA_FALLBACK_TEST_TOKEN'\ncheck_fallback = 'cloud'\n"

var engineFailure = api.CheckResult{ExitCode: 1, Output: "Error: the engine session was lost\n"}

func appendConfig(t *testing.T, opts *Options, extra string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(data, []byte(extra)...), 0600))
}

func newFallbackChecksFixture(t *testing.T, key, extra string, local, cloud checks.ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", independentPlan, func(opts *Options) { appendConfig(t, opts, extra) })
	f.s.setReviewChecks(local)
	service.InspectForTest(f.s.core()).SetCloudChecks(cloud)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func TestServiceUnitChecksFallBackToTheConfiguredCloudRunner(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	f, stream, repository := newFallbackChecksFixture(t, "fallback-unit", fallbackConfig, local, cloud)
	runChecks(t, f.s, repository, stream)
	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != intake.UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != checks.ChecksPassed || len(run.Engines.CloudRuns()) != 1 || run.Engines.CloudRuns()[0].Reason != api.CloudAfterEngineFailure || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceChecksWithoutTheFallbackKeyNeverReachCloud(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	f, stream, repository := newFallbackChecksFixture(t, "fallback-off", "", local, cloud)
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != checks.ChecksIncomplete || run.Engines.UsedCloud() || len(local.Runs()) != 1 || len(cloud.Runs()) != 0 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceCloudEngineKeepsItsPrimaryRunnerUnderTheFallbackKey(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	extra := "\n[dagger]\nengine = 'cloud'\ncloud_compute_token_env = 'OSMIA_FALLBACK_TEST_TOKEN'\ncheck_fallback = 'cloud'\n"
	f, stream, repository := newFallbackChecksFixture(t, "fallback-cloud-engine", extra, local, cloud)
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != checks.ChecksIncomplete || run.Engines.UsedCloud() || len(local.Runs()) != 1 || len(cloud.Runs()) != 0 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceSessionChecksFallBackToTheConfiguredCloudRunner(t *testing.T) {
	t.Parallel()
	local := &fixturekit.FakeChecks{Outcomes: []fixturekit.CheckOutcome{{Result: engineFailure}}}
	cloud := &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()}
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t, func(opts *Options) { appendConfig(t, opts, fallbackConfig) })
	inspect := service.InspectForTest(s.core())
	inspect.SetReviewChecks(local)
	inspect.SetCloudChecks(cloud)
	run, err := c.SessionCheck(context.Background(), stream)
	fixturekit.Must(t, err)
	if run.Status != checks.ChecksPassed || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("run %+v, local %q cloud %q", run, local.Runs(), cloud.Runs())
	}
	entries, err := service.SessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Text, "the Dagger Cloud engine ran every check") {
		t.Fatalf("session entries %+v", entries)
	}
}
