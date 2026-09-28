package service

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOwnerDocumentAPIPinsRevisionsAndEnforcesRatification(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "trace.go"), []byte("package trace\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "internal")
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	charterPath := Prefix + "/projects/charter/" + string(added.Project.ID)
	var charter trace.Document
	must(t, c.Do(ctx, http.MethodGet, charterPath, nil, &charter))
	edit := CharterEdit{Revision: charter.Revision, Content: "1. Keep changes focused.\n"}
	must(t, c.Do(ctx, http.MethodPut, charterPath, edit, &charter))
	assertCode(t, c.Do(ctx, http.MethodPut, charterPath, edit, nil), Conflict)
	text := "Add a feature"
	handed, err := c.HandIn(ctx, HandInRequest{Project: added.Project.ID, Key: "documents", Stdin: &text})
	must(t, err)
	repo, err := s.repository(added.Project.ID)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, Revision: 1, Project: added.Project.ID, Workstream: handed.Workstream, At: s.now(), Actor: ownerActor, Cause: "draft"}
	spec := trace.Document{Header: h, Path: "spec.md", Content: validSpec}
	spec.ID = "spec"
	plan := trace.Document{Header: h, Path: "plan.json", Content: validPlan}
	plan.ID = "plan"
	must(t, repo.RecordDocuments(ctx, []trace.Document{spec, plan}))
	h.Schema, h.ID = "osmia.trace.transition", "sketch"
	_, err = repo.SetFeatureState(ctx, h, "sketched", "Draft ready")
	must(t, err)
	draftPath := Prefix + "/documents/" + string(handed.Workstream)
	var docs map[string]trace.Document
	must(t, c.Do(ctx, http.MethodGet, draftPath, nil, &docs))
	draft := DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: validSpec + "\nAn owner note.\n", Plan: validPlan}
	// Unrecorded local edits cannot be overwritten by the browser.
	specPath := filepath.Join(added.Project.Trace, "workstreams", string(handed.Workstream), "spec.md")
	must(t, os.WriteFile(specPath, []byte("local edit"), 0600))
	assertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), Conflict)
	must(t, os.WriteFile(specPath, []byte(validSpec), 0600))
	must(t, c.Do(ctx, http.MethodPut, draftPath, draft, &docs))
	if docs["spec"].Revision != 2 || docs["plan"].Revision != 1 {
		t.Fatalf("saved draft: %+v", docs)
	}
	assertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), Conflict)
	draft.SpecRevision = 2
	draft.Plan = "invalid"
	assertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), Validation)
	draft.Plan = validPlan
	rh := h
	rh.Schema, rh.ID = "osmia.trace.document", "owner-ratification"
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 2, Plan: 1}, nil))
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: rh, Path: "shed/round-1/ratification.json", Content: string(ratified)}}))
	assertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), Conflict)
	h.ID = "ratify"
	_, err = repo.SetFeatureState(ctx, h, "ratified", "Owner ratified")
	must(t, err)
	assertCode(t, c.Do(ctx, http.MethodPut, draftPath, draft, nil), Conflict)
}
