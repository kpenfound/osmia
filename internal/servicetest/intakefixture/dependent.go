package intakefixture

import (
	context "context"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	os "os"
	filepath "path/filepath"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func SealDependent(t *testing.T, c *intake.Controller, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstreamOn(ctx, stream, backend, c.Now(), intake.OwnerActor))
	if base != "" {
		_, err = repo.SetWorkstreamBase(ctx, stream, base, 0, c.Now())
		fixturekit.Must(t, err)
	}
	header := trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 1, Project: repo.Project(), Workstream: stream, At: c.Now(), Actor: intake.OwnerActor, Cause: "owner-ratification"}
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	fixturekit.Must(t, err)
	var docs []trace.Document
	for _, d := range []struct{ id, path, content string }{{"spec", "spec.md", ValidSpec}, {"plan", "plan.json", ValidPlan}, {shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratified)}} {
		h := header
		h.ID = d.id
		docs = append(docs, trace.Document{Header: h, Path: d.path, Content: d.content})
	}
	fixturekit.Must(t, repo.RecordDocuments(ctx, docs))
	header.Schema, header.ID = "osmia.trace.transition", "in-shed"
	_, err = repo.SetFeatureState(ctx, header, intake.InShedState, "Owner approved the draft")
	fixturekit.Must(t, err)
	z := &intake.Sealer{S: c.SealerDependencies(), Repository: repo}
	fixturekit.Must(t, z.Pass(ctx))
	if result, err := z.Apply(ctx, fixturekit.ActionOperation(t, repo, stream, intake.SealAction)); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("sealing %+v: %v", result, err)
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	g, err := intake.FeatureWorkspaces(c.About(repo), repo).Of(stream)
	fixturekit.Must(t, err)
	w, found, err := g.Workspace(ctx, string(stream))
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("no feature workspace")
	}
	for name, content := range files {
		fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
	}
	commit, err := g.Snapshot(ctx, w, sealed.Base.Commit)
	fixturekit.Must(t, err)
	tip, _, err := g.Branch(ctx, sealed.Branch)
	fixturekit.Must(t, err)
	if tip != commit {
		fixturekit.Must(t, g.Advance(ctx, w, tip, commit))
	}
	return stream, commit
}
