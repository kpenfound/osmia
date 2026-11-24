package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type handInFixture struct {
	s       *Service
	c       *Client
	issues  *fixturekit.FakeIssues
	project config.ProjectID
	trace   string
	home    string
}

func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	f := &handInFixture{}
	data := intakefixture.NewHandInSetup(t, clone, func(fake *fixturekit.FakeIssues) intakefixture.ArchitectProjectClient {
		opts.Issues = fake
		f.s, f.c = start(t, opts)
		return f.c
	}, func(t *testing.T, project config.ProjectID, text string) { writeCharter(t, f.c, project, text) })
	f.issues, f.project, f.trace, f.home = data.Issues, data.Project, data.Trace, data.Home
	return f
}

func (f *handInFixture) handIn(t *testing.T, req HandInRequest) HandInResponse {
	t.Helper()
	return intakefixture.HandIn(t, f.c, f.project, req)
}

func (f *handInFixture) repository() *trace.Repository { return f.s.sole().repository }

func (f *handInFixture) head(t *testing.T) map[string]int {
	t.Helper()
	return intakefixture.HandInHead(t, f.trace, func() []config.WorkstreamID { return f.streams(t) }, func(stream config.WorkstreamID) string {
		return fixturekit.WorkstreamTraceDirectory(t, f.s.cfg.Root, f.project, stream)
	})
}

func (f *handInFixture) unchanged(t *testing.T, head map[string]int) bool {
	t.Helper()
	return intakefixture.HandInUnchanged(t, f.head(t), head)
}

func (f *handInFixture) streams(t *testing.T) []config.WorkstreamID {
	t.Helper()
	return intakefixture.Streams(t, f.s.current().Root, f.project)
}
