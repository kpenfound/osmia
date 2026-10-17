package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/trace/legacytrace"
)

// plantWorkstream leaves a workstream without a chief-of-staff thread: a
// Git trace, which the next start migrates, holding only its manifest.
func plantWorkstream(t *testing.T, root config.Root, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	dir, err := root.WorkstreamTrace(project, id)
	must(t, err)
	must(t, os.MkdirAll(dir, 0700))
	must(t, legacytrace.Build(dir, legacytrace.Workstream(string(project), string(id), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))))
}

func chiefOfStaffAgents(t *testing.T, s *Service, id config.WorkstreamID) []trace.Agent {
	t.Helper()
	agents, err := trace.Read[trace.Agent](s.sole().repository, id)
	must(t, err)
	var out []trace.Agent
	for _, a := range agents {
		if a.ID == trace.ChiefOfStaff {
			out = append(out, a)
		}
	}
	return out
}

func TestStartGivesExistingWorkstreamsOneChiefOfStaffThread(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	id := added.Project.ID
	must(t, s.Close())
	const legacy config.WorkstreamID = "w_00000000000000000000000000000009"
	plantWorkstream(t, s.cfg.Root, id, legacy)

	s, _ = start(t, opts)
	th, err := s.sole().repository.ChiefOfStaffThread(legacy)
	must(t, err)
	if th.Identity.Role != trace.ChiefOfStaff || th.Identity.Actor != serviceActor || th.Status != "idle" {
		t.Fatalf("thread = %+v", th)
	}
	must(t, s.Close())

	s, _ = start(t, opts)
	again, err := s.sole().repository.ChiefOfStaffThread(legacy)
	must(t, err)
	if got := chiefOfStaffAgents(t, s, legacy); len(got) != 1 || !again.Identity.At.Equal(th.Identity.At) {
		t.Fatalf("agents after restart = %+v", got)
	}
}
