package intakefixture

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// DeliveryMaintenance supplies the local execution boundaries of a delivered fixture.
type DeliveryMaintenance struct {
	Config          *config.Config
	Repository      *trace.Repository
	Stream          config.WorkstreamID
	Clone           string
	Now             func() time.Time
	Approve         func()
	Publish         func() (coreadapter.OperationResult, error)
	AdvanceUpstream func(map[string]string)
	ForkTip         func() string
	Refresh         func(*fixturekit.FakePulls, string) coreadapter.OperationResult
	Observation     func(*trace.Repository, config.WorkstreamID) (seal.Base, bool, int, error)
}

func PrepareDeliveryMaintenance(t *testing.T, f DeliveryMaintenance) {
	t.Helper()
	ctx := context.Background()
	upstream := f.Config.Project.Upstream
	// Record a completed delivery on the fork, then configure the canonical
	// upstream for the integration check and second publication.
	f.Config.Project.Upstream = f.Config.Project.Fork
	f.Approve()
	result, err := f.Publish()
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	f.Config.Project.Upstream = upstream
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	repo := f.Repository
	fixturekit.Must(t, repo.CreateWorkstream(ctx, parent, f.Now(), trace.Actor{Kind: "owner", ID: "owner"}))
	sealed, doc, _, err := seal.Latest(repo, f.Stream)
	fixturekit.Must(t, err)
	sealed.Branch = intake.FeatureBranch(parent)
	data, err := seal.Encode(sealed)
	fixturekit.Must(t, err)
	doc.Workstream, doc.Content, doc.Revision = parent, string(data), 1
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	h := doc.Header
	h.Schema, h.ID = "osmia.trace.transition", "parent-delivered"
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "Parent delivered before upstream integration")
	fixturekit.Must(t, err)
	fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "branch", sealed.Branch, sealed.Base.Commit)
	base := trace.WorkstreamBase{Workstream: f.Stream, Base: parent, Revision: 1}
	data, err = json.Marshal(base)
	fixturekit.Must(t, err)
	doc.Workstream, doc.ID, doc.Path, doc.Content = f.Stream, "workstream-base", "base.json", string(data)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	// A second upstream commit forces maintenance to move the reviewed branch.
	f.AdvanceUpstream(map[string]string{"INTEGRATED.md": "dependency is integrated\n"})
	host := &fixturekit.FakePulls{Fork: func() string { return f.ForkTip() }}
	result = f.Refresh(host, upstream)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	observedBase, unavailable, rev, err := f.Observation(repo, f.Stream)
	fixturekit.Must(t, err)
	if rev != 1 || unavailable || observedBase.Workstream != "" {
		t.Fatalf("integration %+v %d", observedBase, rev)
	}

}
