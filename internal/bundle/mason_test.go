package bundle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const masonSpec = "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume.\n2. Duplicates are dropped.\n3. Seeds are logged.\n"

const masonPlan = `{"version": 1, "units": [
 {"id": "resume", "title": "Resume uploads", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}, {"criterion": "spec#3", "proof": {"kind": "reviewer-judgement", "name": "log lines"}}], "depends_on": [], "footprint": ["internal.kb.seed"]},
 {"id": "dedupe", "addresses": [{"criterion": "spec#2", "proof": {"kind": "existing-test", "name": "TestDedupe"}}], "depends_on": ["resume"], "footprint": ["cmd"]}
]}`

func document(id, path, content string) trace.Document {
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: first, At: timestamp, Actor: owner, Cause: "test"}, Path: path, Content: content}
}

// sealed records the spec, the plan and a seal of both in the first
// workstream.
func sealed(t *testing.T) fixture {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	if err := f.repo.CreateWorkstream(ctx, first, timestamp, owner); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, plan.SpecPath, masonSpec), document(plan.PlanDocument, plan.PlanPath, masonPlan)}); err != nil {
		t.Fatal(err)
	}
	content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(masonSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/w_1", Workspace: "/root/branches/p_1/w_1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.RecordDocuments(ctx, []trace.Document{document(seal.DocumentID, seal.Path, string(content))}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) mason(unit string) (bundle.Mason, error) {
	return f.provider.(bundle.Files).Mason(context.Background(), project, first, unit)
}

func TestMasonBundleHoldsExactlyTheUnit(t *testing.T) {
	t.Parallel()
	f := sealed(t)
	m, err := f.mason("resume")
	if err != nil {
		t.Fatal(err)
	}
	want := []bundle.UnitCriterion{
		{Citation: "spec#1", Text: "Uploads resume.", Proof: plan.Proof{Kind: plan.NewTest, Name: "TestResume"}},
		{Citation: "spec#3", Text: "Seeds are logged.", Proof: plan.Proof{Kind: plan.ReviewerJudgement, Name: "log lines"}},
	}
	if !reflect.DeepEqual(m.Criteria, want) {
		t.Fatalf("criteria = %#v, want %#v", m.Criteria, want)
	}
	if m.Unit != "resume" || m.Title != "Resume uploads" || len(m.DependsOn) != 0 || !reflect.DeepEqual(m.Footprint, []string{"internal.kb.seed"}) {
		t.Fatalf("unit = %q %q depends %v footprint %v", m.Unit, m.Title, m.DependsOn, m.Footprint)
	}
	if m.Seal != 1 || m.Spec.Content != masonSpec || m.Spec.Hash != seal.SpecHash(masonSpec) || m.Spec.Revision != 1 || m.Plan.Revision != 1 {
		t.Fatalf("documents = %+v %+v", m.Spec, m.Plan)
	}
	if len(m.Context.Charter.Rules) != 2 || !reflect.DeepEqual(m.Context.Scope.Entities, []string{"internal.kb.seed"}) || m.Context.Scope.Workstream != first {
		t.Fatalf("context = %+v", m.Context)
	}
	if got := sources(m.Context); !reflect.DeepEqual(got, []string{"kb/internal.md", "kb/internal.kb.seed.md"}) {
		t.Fatalf("knowledge = %v", got)
	}
	r := m.Render()
	for _, s := range []string{"# Unit resume", "- spec#1: Uploads resume.\n  proof: new-test TestResume", "- spec#3: Seeds are logged.", "## Depends on\nNo units.", "## Footprint\n- internal.kb.seed", "1. Uploads resume.", "charter#1", "# Project context"} {
		if !strings.Contains(r, s) {
			t.Errorf("render lacks %q:\n%s", s, r)
		}
	}
	if strings.Contains(r, "Duplicates are dropped.\n  proof") || strings.Contains(r, "spec#2:") {
		t.Errorf("render holds another unit's criterion:\n%s", r)
	}

	d, err := f.mason("dedupe")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Criteria) != 1 || d.Criteria[0].Text != "Duplicates are dropped." || !reflect.DeepEqual(d.DependsOn, []string{"resume"}) || !reflect.DeepEqual(d.Footprint, []string{"cmd"}) {
		t.Fatalf("dedupe = %+v", d)
	}
	if !strings.Contains(d.Render(), "## Depends on\n- plan#resume") {
		t.Errorf("render lacks the dependency:\n%s", d.Render())
	}
}

func TestMasonBundleRefusesASpecEditedAfterTheSeal(t *testing.T) {
	t.Parallel()
	f := sealed(t)
	path := filepath.Join(f.dir, "workstreams", string(first), plan.SpecPath)
	if err := os.WriteFile(path, []byte(masonSpec+"4. Something new.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := f.mason("resume")
	want := "spec does not match its seal: spec.md revision 2 hashes to " + seal.SpecHash(masonSpec+"4. Something new.\n") + ", seal 1 records " + seal.SpecHash(masonSpec)
	if !errors.Is(err, bundle.ErrStaleSpec) || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

func TestMasonBundleRefusals(t *testing.T) {
	t.Parallel()
	f := sealed(t)
	if _, err := f.mason("nope"); err == nil || err.Error() != `unit "nope" is not in plan.json revision 1` {
		t.Fatalf("unknown unit err = %v", err)
	}
	g := setup(t)
	if err := g.repo.CreateWorkstream(context.Background(), first, timestamp, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := g.mason("resume"); err == nil || err.Error() != "workstream "+string(first)+" is not sealed" {
		t.Fatalf("unsealed err = %v", err)
	}
}
