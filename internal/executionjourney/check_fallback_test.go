package executionjourney

import (
	"context"
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

const fallbackConfig = "\n[dagger]\ncloud_token_env = 'OSMIA_FALLBACK_TEST_TOKEN'\ncheck_fallback = 'cloud'\n"

var engineFailure = CheckResult{ExitCode: 1, Output: "Error: the engine session was lost\n"}

func appendConfig(t *testing.T, opts *Options, extra string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(extra)...), 0600))
}

// newFallbackChecksFixture is newChecksFixture with the cloud runner cloud
// injected and extra appended to the service configuration.
func newFallbackChecksFixture(t *testing.T, key, extra string, local, cloud ReviewChecks) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", independentPlan, "", func(opts *Options) { appendConfig(t, opts, extra) })
	f.s.setReviewChecks(local)
	service.InspectForTest(f.s.core()).SetCloudChecks(cloud)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func TestServiceUnitChecksFallBackToTheConfiguredCloudRunner(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	cloud := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newFallbackChecksFixture(t, "fallback-unit", fallbackConfig, local, cloud)
	runChecks(t, f.s, repository, stream)
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksPassed || len(run.Engines.CloudRuns()) != 1 || run.Engines.CloudRuns()[0].Reason != api.CloudAfterEngineFailure || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceChecksWithoutTheFallbackKeyNeverReachCloud(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	cloud := &fakeChecks{Outcomes: passing()}
	f, stream, repository := newFallbackChecksFixture(t, "fallback-off", "", local, cloud)
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Engines.UsedCloud() || len(local.Runs()) != 1 || len(cloud.Runs()) != 0 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceCloudEngineKeepsItsPrimaryRunnerUnderTheFallbackKey(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	cloud := &fakeChecks{Outcomes: passing()}
	extra := "\n[dagger]\nengine = 'cloud'\ncloud_token_env = 'OSMIA_FALLBACK_TEST_TOKEN'\ncheck_fallback = 'cloud'\n"
	f, stream, repository := newFallbackChecksFixture(t, "fallback-cloud-engine", extra, local, cloud)
	runChecks(t, f.s, repository, stream)
	run := onlyRun(t, repository, stream)
	if run.Status != ChecksIncomplete || run.Engines.UsedCloud() || len(local.Runs()) != 1 || len(cloud.Runs()) != 0 {
		t.Fatalf("run %+v engines %+v, local %q cloud %q", run, run.Engines, local.Runs(), cloud.Runs())
	}
}

func TestServiceSessionChecksFallBackToTheConfiguredCloudRunner(t *testing.T) {
	t.Parallel()
	local := &fakeChecks{Outcomes: []checkOutcome{{Result: engineFailure}}}
	cloud := &fakeChecks{Outcomes: passing()}
	s, c, _, _, stream, _, _ := newSessionControlsFixture(t, func(opts *Options) { appendConfig(t, opts, fallbackConfig) })
	inspect := service.InspectForTest(s.core())
	inspect.SetReviewChecks(local)
	inspect.SetCloudChecks(cloud)
	run, err := c.SessionCheck(context.Background(), stream)
	must(t, err)
	if run.Status != ChecksPassed || len(local.Runs()) != 1 || len(cloud.Runs()) != 1 {
		t.Fatalf("run %+v, local %q cloud %q", run, local.Runs(), cloud.Runs())
	}
	entries, err := sessionEntriesSince(s.sole().repository, stream, time.Time{})
	must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Text, "the Dagger Cloud engine ran every check") {
		t.Fatalf("session entries %+v", entries)
	}
}
