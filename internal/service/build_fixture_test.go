package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// seedBuild records a sealed plan and builds its unit graph. It returns a
// stopped service's open repository for controller tests.
func seedBuild(t *testing.T, f *shedFixture, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })

	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	must(t, err)
	at := f.s.now()
	must(t, repository.CreateWorkstreamOn(ctx, stream, backend, at, ownerActor))
	w, err := workspaces(f.s.cfg, branchesDirectory, backend).Acquire(ctx, vcs.Request{
		Name: string(stream), Ref: base, Branch: featureBranch(stream),
	})
	must(t, err)
	p, err := plan.Parse([]byte(graph))
	must(t, err)
	entities, err := kb.Load(repository)
	must(t, err)
	footprints, unresolved := seal.Take(p, entities)
	if len(unresolved) != 0 {
		t.Fatalf("fixture has unresolved footprints: %v", unresolved)
	}
	pin := shed.Pin{Spec: 1, Plan: 1}
	sealed, err := seal.Encode(seal.Seal{
		Version: seal.Version, Seal: 1, Round: 1, Revision: pin, SpecHash: seal.SpecHash(validSpec),
		Base:   seal.Base{Remote: "upstream", Branch: f.s.cfg.Project.BaseBranch, Commit: base},
		Branch: featureBranch(stream), Workspace: w.Directory(), Footprints: footprints,
	})
	must(t, err)
	ratification, err := shed.EncodeRatification(shed.Ratify(1, pin, nil))
	must(t, err)
	header := func(schema, id string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1,
			Project: repository.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "fixture-" + key}
	}
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id), Path: path, Content: content}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document(plan.SpecDocument, plan.SpecPath, validSpec),
		document(plan.PlanDocument, plan.PlanPath, graph),
		document(shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratification)),
		document(seal.DocumentID, seal.Path, string(sealed)),
	}, trace.Transaction{Transition: trace.Transition{
		Header: header("osmia.trace.transition", "fixture-ratified"), Subject: trace.FeatureSubject,
		To: RatifiedState, Reason: "fixture with a ratified spec and sealed plan",
	}})
	must(t, err)
	_, event := buildIDs(1)
	input, err := json.Marshal(buildInput{Seal: 1})
	must(t, err)
	result, err := (&builder{s: f.s, repository: repository}).Apply(ctx, coreadapter.Operation{
		ID: trace.OperationID(repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary,
		Action: BuildAction, Input: input,
	})
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("fixture build: %+v", result)
	}
	return stream, repository
}

// newReviewFixture creates a unit whose completed report and candidate await review.
func newReviewFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

// seedReview records a completed mason report and snapshots its candidate.
func seedReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, criterion CriterionReport) {
	t.Helper()
	m := newMasonController(f.s, repository)
	ctx := context.Background()
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	started, blocked, err := m.start(ctx, b, unit)
	must(t, err)
	if !started || blocked {
		t.Fatalf("fixture unit %s did not start", unit)
	}
	w, _, err := newUnitWorkspaces(f.s.cfg, repository).open(ctx, stream, unit)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n"), 0644))
	completeMasonTurn(t, f, repository, stream, unit, criterion)
	b, _, err = m.read(stream)
	must(t, err)
	moved, blocked, err := m.finish(ctx, b, unit)
	must(t, err)
	if !moved || blocked {
		t.Fatalf("fixture unit %s did not enter review", unit)
	}
}
