package intakejourney

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOwnerCharterEditPinsItsRevision(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	_, c := journey.StartT(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	charterPath := api.Prefix + "/projects/charter/" + string(added.Project.ID)
	var charter trace.Document
	fixturekit.Must(t, c.Do(ctx, http.MethodGet, charterPath, nil, &charter))
	edit := api.CharterEdit{Revision: charter.Revision, Content: "1. Keep changes focused.\n"}
	fixturekit.Must(t, c.Do(ctx, http.MethodPut, charterPath, edit, &charter))
	fixturekit.AssertCode(t, c.Do(ctx, http.MethodPut, charterPath, edit, nil), api.Conflict)
}

func TestOwnerDraftEditPinsRevisionsAndIsRefusedOnceRatified(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "trace.go"), []byte("package trace\n"), 0600))
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "add", "internal")
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	text := "Add a feature"
	handed, err := c.HandIn(ctx, api.HandInRequest{Project: added.Project.ID, Key: "documents", Stdin: &text})
	fixturekit.Must(t, err)
	repo, err := s.Repository(added.Project.ID)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, Revision: 1, Project: added.Project.ID, Workstream: handed.Workstream, At: s.Now(), Actor: intake.OwnerActor, Cause: "draft"}
	spec := trace.Document{Header: h, Path: "spec.md", Content: fixturekit.ValidSpec}
	spec.ID = "spec"
	plan := trace.Document{Header: h, Path: "plan.json", Content: fixturekit.ValidPlan}
	plan.ID = "plan"
	plan.Actor = intake.ManagerActor
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{spec, plan}))
	h.Schema, h.ID = "osmia.trace.transition", "sketch"
	_, err = repo.SetFeatureState(ctx, h, "sketched", "Draft ready")
	fixturekit.Must(t, err)
	draftPath := api.Prefix + "/documents/" + string(handed.Workstream)
	var docs map[string]trace.Document
	fixturekit.Must(t, c.Do(ctx, http.MethodGet, draftPath, nil, &docs))
	if docs["plan"].Actor.ID != trace.ManagerDisplay {
		t.Fatalf("the manager's plan reads as %+v", docs["plan"].Actor)
	}
	draft := api.DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: fixturekit.ValidSpec + "\nAn owner note.\n", Plan: fixturekit.ValidPlan}
	fixturekit.Must(t, c.Do(ctx, http.MethodPut, draftPath, draft, &docs))
	if docs["spec"].Revision != 2 || docs["plan"].Revision != 1 || docs["plan"].Actor.ID != trace.ManagerDisplay {
		t.Fatalf("saved draft: %+v", docs)
	}
	fixturekit.AssertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), api.Conflict)
	draft.SpecRevision = 2
	draft.Plan = "invalid"
	fixturekit.AssertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), api.Validation)
	draft.Plan = fixturekit.ValidPlan
	rh := h
	rh.Schema, rh.ID = "osmia.trace.document", "owner-ratification"
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 2, Plan: 1}, nil))
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: rh, Path: "shed/round-1/ratification.json", Content: string(ratified)}}))
	fixturekit.AssertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), api.Conflict)
	h.ID = "ratify"
	_, err = repo.SetFeatureState(ctx, h, "ratified", "Owner ratified")
	fixturekit.Must(t, err)
	fixturekit.AssertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), api.Conflict)
}
