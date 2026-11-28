package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func plantWorkstream(t *testing.T, root config.Root, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	repository, err := trace.OpenByID(root, project)
	fixturekit.Must(t, err)
	defer repository.Close()
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), id, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), serviceActor))
}

func chiefOfStaffAgents(t *testing.T, s *Service, id config.WorkstreamID) []trace.Agent {
	t.Helper()
	agents, err := trace.Read[trace.Agent](s.sole().repository, id)
	fixturekit.Must(t, err)
	var out []trace.Agent
	for _, a := range agents {
		if a.ID == trace.Manager {
			out = append(out, a)
		}
	}
	return out
}

func TestRestartKeepsOneChiefOfStaffThreadPerWorkstream(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	id := added.Project.ID
	fixturekit.Must(t, s.Close())
	const existing config.WorkstreamID = "w_00000000000000000000000000000009"
	plantWorkstream(t, s.cfg.Root, id, existing)

	s, _ = start(t, opts)
	th, err := s.sole().repository.ManagerThread(existing)
	fixturekit.Must(t, err)
	if th.Identity.Role != trace.Manager || th.Identity.Actor != serviceActor || th.Status != "idle" {
		t.Fatalf("thread = %+v", th)
	}
	fixturekit.Must(t, s.Close())

	s, _ = start(t, opts)
	again, err := s.sole().repository.ManagerThread(existing)
	fixturekit.Must(t, err)
	if got := chiefOfStaffAgents(t, s, existing); len(got) != 1 || !again.Identity.At.Equal(th.Identity.At) {
		t.Fatalf("agents after restart = %+v", got)
	}
}
