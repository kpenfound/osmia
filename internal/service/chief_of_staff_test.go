package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// plantWorkstream commits a workstream without a chief-of-staff thread
// straight into the trace's Git history.
func plantWorkstream(t *testing.T, home, traceDir string, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	prefix := filepath.Join(traceDir, "workstreams", string(id))
	must(t, os.MkdirAll(prefix, 0700))
	h := trace.Header{Schema: "osmia.trace.workstream", Version: trace.Version, ID: string(id), Revision: 1, Project: project, Workstream: id, At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Actor: registrationActor, Cause: "workstream-create"}
	data, err := json.Marshal(h)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(prefix, "workstream.json"), append(data, '\n'), 0600))
	for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
		must(t, os.WriteFile(filepath.Join(prefix, name), nil, 0600))
	}
	demoGit(t, home, "-C", traceDir, "add", "workstreams")
	demoGit(t, home, "-C", traceDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "Create workstream trace")
}

func chiefOfStaffAgents(t *testing.T, s *Service, id config.WorkstreamID) []trace.Agent {
	t.Helper()
	agents, err := trace.Read[trace.Agent](s.active.repository, id)
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
	opts, clone := projectFixture(t)
	home := filepath.Dir(opts.Config.Root)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	id := added.Project.ID
	must(t, s.Close())
	const legacy config.WorkstreamID = "w_00000000000000000000000000000009"
	plantWorkstream(t, home, added.Project.Trace, id, legacy)

	s, _ = start(t, opts)
	th, err := s.active.repository.ChiefOfStaffThread(legacy)
	must(t, err)
	if th.Identity.Role != trace.ChiefOfStaff || th.Identity.Actor != serviceActor || th.Status != "idle" {
		t.Fatalf("thread = %+v", th)
	}
	must(t, s.Close())

	s, _ = start(t, opts)
	again, err := s.active.repository.ChiefOfStaffThread(legacy)
	must(t, err)
	if got := chiefOfStaffAgents(t, s, legacy); len(got) != 1 || !again.Identity.At.Equal(th.Identity.At) {
		t.Fatalf("agents after restart = %+v", got)
	}
}
