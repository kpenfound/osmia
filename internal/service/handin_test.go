package service

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"
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

func newDesignFixture(t *testing.T) (*Service, *Client, config.ProjectID) {
	t.Helper()
	f := newHandInFixture(t)
	return f.s, f.c, f.project
}

var recordDesignDraft = intakefixture.RecordDesignDraft

const (
	designSessionSubject = intake.DesignSessionSubject
	designOpenValue      = intake.DesignOpenValue
	designClosedValue    = intake.DesignClosedValue
)

func (f *handInFixture) handIn(t *testing.T, req HandInRequest) HandInResponse {
	t.Helper()
	return intakefixture.HandIn(t, f.c, f.project, req)
}

func (f *handInFixture) repository() *trace.Repository { return f.s.sole().repository }

// head returns how many commits the project's trace and each of its
// workstreams' traces hold, apart from the librarian's workstream, which the
// project's extraction writes to.
func (f *handInFixture) head(t *testing.T) map[string]int {
	t.Helper()
	return intakefixture.HandInHead(t, f.trace, func() []config.WorkstreamID { return f.streams(t) }, func(stream config.WorkstreamID) string {
		return workstreamTraceDirectory(t, f.s.cfg.Root, f.project, stream)
	})
}

// unchanged reports whether nothing was committed since head outside the
// librarian's workstream, and no workstream was created.
func (f *handInFixture) unchanged(t *testing.T, head map[string]int) bool {
	t.Helper()
	return intakefixture.HandInUnchanged(t, f.head(t), head)
}

// streams lists the project's workstreams other than the librarian's.
func (f *handInFixture) streams(t *testing.T) []config.WorkstreamID {
	t.Helper()
	return intakefixture.Streams(t, f.s.current().Root, f.project, librarianWorkstream(f.project))
}
