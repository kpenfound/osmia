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
// straight into its own dedicated trace repository's Git history, which this
// helper creates from scratch since the workstream was never created through
// CreateWorkstream.
func plantWorkstream(t *testing.T, home string, root config.Root, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	dir, err := root.WorkstreamTrace(project, id)
	must(t, err)
	must(t, os.MkdirAll(dir, 0700))
	demoGit(t, home, "init", "--quiet", "--initial-branch=main", dir)
	must(t, os.WriteFile(filepath.Join(dir, ".git", "osmia.lock"), nil, 0600))
	prefix := filepath.Join(dir, "workstreams", string(id))
	must(t, os.MkdirAll(prefix, 0700))
	h := trace.Header{Schema: "osmia.trace.workstream", Version: trace.Version, ID: string(id), Revision: 1, Project: project, Workstream: id, At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Actor: registrationActor, Cause: "workstream-create"}
	data, err := json.Marshal(h)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(prefix, "workstream.json"), append(data, '\n'), 0600))
	for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
		must(t, os.WriteFile(filepath.Join(prefix, name), nil, 0600))
	}
	demoGit(t, home, "-C", dir, "add", "workstreams")
	demoGit(t, home, "-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "Create workstream trace")
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
	home := filepath.Dir(opts.Config.Root)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	id := added.Project.ID
	must(t, s.Close())
	const legacy config.WorkstreamID = "w_00000000000000000000000000000009"
	plantWorkstream(t, home, s.cfg.Root, id, legacy)

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
