package clientjourney

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
)

// build seeds a sealed plan whose units the masons then implement.
func build(f *journey.ShedFixture, t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	return intakefixture.SeedBuilding(t, f.Clone, key, graph, func() (string, error) {
		b, err := f.S.Intake().NewWorkspaces(context.Background(), f.S.About(f.Repository()))
		return b.Backend, err
	}, func(k, g, b, base string) config.WorkstreamID {
		return journey.RecordBuild(t, f, f.Repository(), k, g, b, base)
	}, f.S.RefreshRuntime)
}

func latestReport(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) execution.UnitReport {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == execution.ReportDocument(unit) {
			latest = d
		}
	}
	var report execution.UnitReport
	if latest.Revision != 0 {
		fixturekit.Must(t, json.Unmarshal([]byte(latest.Content), &report))
	}
	return report
}

// inCommit reads a file as a commit of the project's clone holds it.
func inCommit(f *journey.ShedFixture, commit, name string) (string, error) {
	cmd := exec.Command("git", "-C", f.Clone, "show", commit+":"+name)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(f.Clone), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.Output()
	return string(out), err
}

func write(dir, name, content string) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
