package runtime

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

const pidOther config.ProjectID = "p_fedcba9876543210fedcba9876543210"
const wOther config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"

func twoProjects(t *testing.T) Inputs {
	t.Helper()
	in := fixture(t)
	root := in.Config.Root.String()
	register(t, root, pidOther, `version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+filepath.Join(root, "..", "other")+`"
`)
	c, err := projectstore.LoadConfig(config.Options{Root: root})
	must(t, err)
	in.Config = c
	in.Workstreams[pidOther] = []config.WorkstreamID{wOther}
	return in
}

func TestReferencesToEveryActiveProject(t *testing.T) {
	in := twoProjects(t)
	s := open(t, in)
	must(t, s.SetPause(Pause{Target: Target{Scope: "project", Project: pidOther}, Mode: "soft", Source: PauseOwner, Reason: "test"}))
	must(t, s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: w1}, Mode: "soft", Source: PauseOwner, Reason: "test"}))
	must(t, s.SetPriority(Priority{Project: pidOther, Workstreams: []config.WorkstreamID{wOther}}))
	must(t, s.SetPriority(Priority{Project: pid, Workstreams: []config.WorkstreamID{w2, w1}}))
	if err := s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: wOther}, Mode: "soft", Source: PauseOwner, Reason: "test"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("another project's workstream accepted: %v", err)
	}
	effective, ds := s.Effective()
	if len(ds) != 0 || len(effective.Pauses) != 2 || len(effective.Priorities) != 2 {
		t.Fatalf("%+v %v", effective, ds)
	}
	one := in
	one.Config = in.Config.WithoutProjectID(pidOther)
	delete(one.Workstreams, pidOther)
	must(t, s.Resolve(one))
	effective, ds = s.Effective()
	if len(ds) != 2 || len(effective.Pauses) != 1 || effective.Pauses[0].Target.Project != pid || len(effective.Priorities) != 1 || effective.Priorities[0].Project != pid {
		t.Fatalf("%+v %v", effective, ds)
	}
	for _, d := range ds {
		if !strings.Contains(d.Reason, "inactive project "+string(pidOther)) {
			t.Fatalf("diagnostic %+v", d)
		}
	}
	if err := s.Resolve(Inputs{Config: one.Config, Workstreams: map[config.ProjectID][]config.WorkstreamID{pidOther: {wOther}}}); err == nil {
		t.Fatal("workstreams of an inactive project accepted")
	}
}
