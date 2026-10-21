package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type handInFixture struct {
	s       *Service
	c       *Client
	issues  *fakeIssues
	project config.ProjectID
	trace   string
	home    string
}

// newHandInFixture starts a service with an active project whose charter has
// rules.
func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	f := &handInFixture{}
	data := intakefixture.NewHandInSetup(t, clone, func(fake *fakeIssues) intakefixture.ArchitectProjectClient {
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
