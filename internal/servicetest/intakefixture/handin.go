package intakefixture

import (
	"context"
	"maps"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type HandInSetup struct {
	Issues      *fixturekit.FakeIssues
	Project     config.ProjectID
	Trace, Home string
}

// NewHandInSetup starts a project with a fake issue source and a nonempty charter.
func NewHandInSetup(t *testing.T, clone string, start func(*fixturekit.FakeIssues) ArchitectProjectClient, writeCharter func(*testing.T, config.ProjectID, string)) HandInSetup {
	t.Helper()
	fake := &fixturekit.FakeIssues{Token: "ghp_handin_secret", Text: map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: "# Issue title\n\nIssue body\n"}}
	client := start(fake)
	project, trace := AddArchitectProject(t, client, clone, "1. Keep changes small.\n", writeCharter)
	return HandInSetup{Issues: fake, Project: project, Trace: trace, Home: filepath.Dir(clone)}
}

type HandInClient interface {
	HandIn(context.Context, api.HandInRequest) (api.HandInResponse, error)
}

func HandIn(t *testing.T, client HandInClient, project config.ProjectID, req api.HandInRequest) api.HandInResponse {
	t.Helper()
	req.Project = project
	out, err := client.HandIn(context.Background(), req)
	Must(t, err)
	return out
}

func (f *ArchitectScenario) HandIn(t *testing.T, project config.ProjectID, key, content string) config.WorkstreamID {
	t.Helper()
	out := HandIn(t, f.Client(), project, api.HandInRequest{Key: key, Stdin: &content})
	return out.Workstream
}

// HandInHead counts commits outside the librarian's workstream.
func HandInHead(t *testing.T, projectTrace string, streams func() []config.WorkstreamID, directory func(config.WorkstreamID) string) map[string]int {
	t.Helper()
	commits := map[string]int{"project": fixturekit.TraceCommits(t, projectTrace)}
	for _, stream := range streams() {
		commits[string(stream)] = fixturekit.TraceCommits(t, directory(stream))
	}
	return commits
}

func HandInUnchanged(t *testing.T, now, head map[string]int) bool {
	t.Helper()
	if !maps.Equal(now, head) {
		t.Logf("commits went from %v to %v", head, now)
		return false
	}
	return true
}
