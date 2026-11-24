package intakejourney

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	intake "github.com/kpenfound/osmia/internal/intake"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func entityRevisions(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	fixturekit.Must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != trace.EntitiesDocument })
}

func reseeds(revisions []trace.Document) []string {
	var out []string
	for _, d := range revisions {
		if commit, ok := kb.ReseedCommit(d.Cause); ok {
			out = append(out, commit)
		}
	}
	return out
}

func hasEntity(t *testing.T, d trace.Document, id string) bool {
	t.Helper()
	m, err := kb.Parse([]byte(d.Content))
	fixturekit.Must(t, err)
	_, ok := m.Lookup(id)
	return ok
}

func recordRatification(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, revision int, footprint string) {
	t.Helper()
	ctx := context.Background()
	drafted := strings.ReplaceAll(fixturekit.ValidPlan, `"footprint": ["internal.trace"]`, `"footprint": ["`+footprint+`"]`)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: revision, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner-edit"}, Path: plan.PlanPath, Content: drafted}}))
	content, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: revision}, nil))
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
}

func fileChiefAmendment(t *testing.T, f *journey.ArchitectFixture, repo *trace.Repository, stream config.WorkstreamID, id string, sealed seal.Seal) {
	t.Helper()
	ctx := context.Background()
	_, doc, found, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	header := trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: id, Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: chief, Cause: "fixture"}
	fixturekit.Must(t, repo.Append(ctx, trace.Amendment{Header: header, Requester: chief, Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1",
		Citations: []string{"spec#1"}, Change: "Move a footprint to the new directory", Reason: "The directory landed upstream", Seal: sealed.Seal, SealRevision: doc.Revision, SpecHash: sealed.SpecHash}))
	header.Schema, header.ID = "osmia.trace.transition", "amendment_"+id+"_filed"
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: header, Subject: intake.AmendmentSubject(id), To: "filed", Reason: "filed"}})
	fixturekit.Must(t, err)
}

func awaitSealRevision(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, revision int) seal.Seal {
	t.Helper()
	var current seal.Seal
	wake, release := fixturekit.Ticks(50 * time.Millisecond)
	defer release()
	fixturekit.Until(t, fmt.Sprintf("the workstream to reach seal revision %d", revision), wake, func() bool {
		var doc trace.Document
		var found bool
		var err error
		current, doc, found, err = seal.Latest(repo, stream)
		fixturekit.Must(t, err)
		if found && doc.Revision >= revision {
			return true
		}
		state, err := repo.Workflow(stream, intake.AmendmentSubject("1"))
		fixturekit.Must(t, err)
		if state.Value == intake.AmendmentUnapplied {
			t.Fatalf("the workstream stayed at seal revision %d, want %d, with amendment 1 %s", doc.Revision, revision, state.Value)
		}
		return false
	})
	return current
}

func plantRatification(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, revision int, footprint string) trace.OperationRecord {
	t.Helper()
	ctx := context.Background()
	recordRatification(t, f, repo, stream, revision, footprint)
	latest, found, err := intake.LatestRatification(repo, stream)
	fixturekit.Must(t, err)
	if !found || latest.Recorded != 1 {
		t.Fatalf("latest ratification %+v %v", latest, found)
	}
	k, err := (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}).Request(ctx, stream, latest)
	fixturekit.Must(t, err)
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if in, err := intake.DecodeSeal(o.Operation); err == nil && in.Seal == k {
			return o
		}
	}
	t.Fatalf("no sealing %d", k)
	return trace.OperationRecord{}
}

func TestDraftAndFirstSealReseedFromTheFetchedUpstreamHead(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	f.Upstream(t)
	early := f.LandUpstream(t, "internal/early")
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")

	revisions := entityRevisions(t, f.Repository())
	if got := reseeds(revisions); !slices.Equal(got, []string{early}) || !hasEntity(t, revisions[len(revisions)-1], "internal.early") {
		t.Fatalf("the draft's re-seed recorded %v of %d revisions, want one from %s", got, len(revisions), early)
	}
	late := f.LandUpstream(t, "internal/late")
	if ref := f.UpstreamRef(t); ref != early {
		t.Fatalf("the clone's upstream ref is %s, want the stale %s", ref, early)
	}
	f.Stop(t)

	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	op := plantRatification(t, f, repo, stream, 2, "internal.late")
	if got := reseeds(entityRevisions(t, repo)); !slices.Equal(got, []string{early}) {
		t.Fatalf("re-seeds before the sealing: %v", got)
	}
	z := &intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}
	result, err := z.Apply(ctx, op.Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("sealing: %+v", result)
	}
	record, _, found, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	if !found || record.Base.Commit != late {
		t.Fatalf("the seal %+v is not at %s", record, late)
	}
	for _, footprint := range record.Footprints {
		if !slices.Equal(footprint.Entities, []string{"internal.late"}) || !slices.Equal(footprint.Paths, []string{"internal/late"}) {
			t.Fatalf("footprint of %s: %+v", footprint.Unit, footprint)
		}
	}
	revisions = entityRevisions(t, repo)
	if got := reseeds(revisions); !slices.Equal(got, []string{early, late}) {
		t.Fatalf("re-seeds: %v, want one per upstream commit", got)
	}

	f.S.Intake().ReseedEntities(ctx, repo)
	if after := entityRevisions(t, repo); len(after) != len(revisions) {
		t.Fatalf("a repeated trigger recorded %d revisions", len(after)-len(revisions))
	}
}

func TestAmendmentResealReseedsFromTheFetchedUpstreamHead(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "reseal-reseeds")
	defer repository.Close()
	home := filepath.Dir(f.Clone)
	before := len(entityRevisions(t, repository))
	late := f.LandUpstream(t, "internal/late")
	if refs := fixturekit.DemoGit(t, home, "-C", f.Clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, late) {
		t.Fatalf("the clone already has %s", late)
	}
	revised := strings.Replace(amendedIndependentPlan, `"footprint": ["internal"]`, `"footprint": ["internal.late"]`, 1)
	a, req := approveAmendment(t, f, repository, stream, journey.AmendedSpec, revised)
	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)

	current, _, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found || current.Seal != 2 {
		t.Fatalf("the reseal %+v", current)
	}
	i := slices.IndexFunc(current.Footprints, func(fp seal.Footprint) bool { return fp.Unit == "dedupe" })
	if i < 0 || !slices.Equal(current.Footprints[i].Paths, []string{"internal/late"}) {
		t.Fatalf("footprints %+v", current.Footprints)
	}
	revisions := entityRevisions(t, repository)
	if got := reseeds(revisions[before:]); !slices.Equal(got, []string{late}) {
		t.Fatalf("re-seeds during the reseal: %v, want one from %s", got, late)
	}
}

func TestSealRefusesAFootprintTheReseedDropped(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	f.Upstream(t)
	doomed := f.LandUpstream(t, "internal/doomed")
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	revisions := entityRevisions(t, f.Repository())
	if got := reseeds(revisions); !slices.Equal(got, []string{doomed}) || !hasEntity(t, revisions[len(revisions)-1], "internal.doomed") {
		t.Fatalf("the draft's re-seed recorded %v", got)
	}
	gone := f.RemoveUpstream(t, "internal/doomed")
	f.Stop(t)

	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	op := plantRatification(t, f, repo, stream, 2, "internal.doomed")
	result, err := (&intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}).Apply(ctx, op.Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || !strings.Contains(result.Evidence, "the plan's footprints name what the entity map does not resolve: resume: internal.doomed, dedupe: internal.doomed") {
		t.Fatalf("sealing: %+v", result)
	}
	revisions = entityRevisions(t, repo)
	if got := reseeds(revisions); !slices.Equal(got, []string{doomed, gone}) || hasEntity(t, revisions[len(revisions)-1], "internal.doomed") {
		t.Fatalf("re-seeds %v: the entity of the removed directory stayed", got)
	}
	if _, _, found, err := seal.Latest(repo, stream); err != nil || found {
		t.Fatalf("a refused sealing recorded a seal: %v %v", found, err)
	}
}

func TestReseedFailureIsLoggedOnceAndNeverBlocks(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	repo := f.Repository()
	reseeder := f.S.Intake().Reseeder(repo)
	if reseeder == nil {
		t.Fatal("the project has no re-seeder")
	}
	var mu sync.Mutex
	var logged []string
	reseeder.Log = func(reason string) { mu.Lock(); logged = append(logged, reason); mu.Unlock() }
	before := entityRevisions(t, repo)
	for range 3 {
		f.S.Intake().ReseedEntities(ctx, repo)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "no remote whose URL names") {
		t.Fatalf("logged %q", logged)
	}
	if after := entityRevisions(t, repo); len(after) != len(before) {
		t.Fatalf("a failed re-seed recorded %d revisions", len(after)-len(before))
	}
}

func TestEditAndRatificationValidateAgainstTheRecordedMapWhenTheFetchFails(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) { opts.WriteTimeout = time.Minute })
	ctx := context.Background()
	repo := f.Repository()
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	f.AwaitShed(t, stream, "concluded-1")
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	before := entityRevisions(t, repo)

	unknown := strings.ReplaceAll(fixturekit.ValidPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.nowhere"]`)
	for range 2 {
		if _, err := f.C.EditDraft(ctx, stream, api.DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: fixturekit.ValidSpec, Plan: unknown}); !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "internal.nowhere") {
			t.Fatalf("editing in a footprint no map resolves: %v", err)
		}
	}
	edited, err := f.C.EditDraft(ctx, stream, api.DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: fixturekit.ValidSpec + "\nAn owner note.\n", Plan: fixturekit.ValidPlan})
	fixturekit.Must(t, err)
	if edited["spec"].Revision != 2 {
		t.Fatalf("the edit left %+v", edited)
	}
	ratified, err := f.C.Ratify(ctx, stream, 2, 1)
	fixturekit.Must(t, err)
	if ratified.Sealing != "requested" {
		t.Fatalf("ratification %+v", ratified)
	}
	if after := entityRevisions(t, repo); len(after) != len(before) {
		t.Fatalf("validations with a failing fetch recorded %d revisions", len(after)-len(before))
	}
}

type reseededDraft struct {
	f         *journey.ShedFixture
	committee *intake.Committee
	repo      *trace.Repository
	stream    config.WorkstreamID
}

func draftAgainstARegisteredProject(t *testing.T, script func(ctx context.Context, tools *mcp.ClientSession) error) reseededDraft {
	t.Helper()
	ctx := context.Background()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) {
		opts.WriteTimeout = time.Minute
	})
	committee := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	f.Upstream(t)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if script == nil {
			return nil
		}
		return script(ctx, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	return reseededDraft{f: f, committee: committee, repo: f.Repository(), stream: stream}
}

func TestRegisteringAProjectRecordsTheSeededEntityMapOnceAndTheArchitectReadsIt(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) {
		opts.WriteTimeout = time.Minute
	})
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	repo := f.Repository()
	registered := entityRevisions(t, repo)
	if len(registered) != 1 || registered[0].Revision != 1 || len(reseeds(registered)) != 0 || !hasEntity(t, registered[0], "internal.trace") {
		t.Fatalf("registration recorded %d entity map revisions: %+v", len(registered), registered)
	}
	if runs := f.Runs(); len(runs) != 0 {
		t.Fatalf("registering ran %v", runs)
	}

	ctx := context.Background()
	var mu sync.Mutex
	var problems []string
	checked := false
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		mu.Lock()
		checked = true
		mu.Unlock()
		text, err := intakefixture.ReadTool(ctx, tools, "context.md")
		if err != nil {
			fail("read context.md: %v", err)
		}
		if !strings.Contains(text, "source: kb/entities.json (record kb-entities revision ") {
			fail("context.md lacks the entity map:\n%s", text)
		}
		listed, err := fixturekit.CallTool(ctx, tools, service.ProjectContextTool, map[string]any{"prefix": "kb/"})
		var records struct {
			Records []struct{ Path string } `json:"records"`
		}
		if err != nil || json.Unmarshal([]byte(listed), &records) != nil || !slices.ContainsFunc(records.Records, func(r struct{ Path string }) bool { return r.Path == "kb/entities.json" }) {
			fail("project_context lists kb/ as %s: %v", listed, err)
		}
		if entities, err := fixturekit.CallTool(ctx, tools, service.ProjectContextTool, map[string]any{"path": "kb/entities.json"}); err != nil || !strings.Contains(entities, "internal.trace") {
			fail("project_context entity map: %s: %v", entities, err)
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	mu.Lock()
	if !checked || len(problems) != 0 {
		t.Fatalf("the architect's turn ran %t and found: %s", checked, strings.Join(problems, "\n"))
	}
	mu.Unlock()

	assembled, err := f.S.Context().Assemble(ctx, f.Project, bundle.Scope{Role: "architect", Workstream: stream})
	fixturekit.Must(t, err)
	if assembled.Entities.Revision < 1 || len(assembled.Entities.Entities) == 0 {
		t.Errorf("the architect's bundle: %+v", assembled.Entities)
	}
}

func TestDirectoryLandedAfterDraftingIsReseededByTheEditTheSealAndAnAmendment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := draftAgainstARegisteredProject(t, nil)
	f, repo, stream := d.f, d.repo, d.stream

	before := entityRevisions(t, repo)
	late := f.LandUpstream(t, "internal/late")
	if ref := f.UpstreamRef(t); ref == late {
		t.Fatalf("the clone's upstream ref is already at %s", late)
	}
	if refs := fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, late) {
		t.Fatalf("the clone already has %s", late)
	}

	named := strings.ReplaceAll(fixturekit.ValidPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.late"]`)
	unknown := strings.ReplaceAll(fixturekit.ValidPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.nowhere"]`)
	if _, err := f.C.EditDraft(ctx, stream, api.DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: fixturekit.ValidSpec, Plan: unknown}); !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "internal.nowhere") {
		t.Fatalf("editing in a footprint no map resolves: %v", err)
	}
	if got := entityRevisions(t, repo); len(got) != len(before)+1 || !slices.Equal(reseeds(got[len(before):]), []string{late}) {
		t.Fatalf("the refused edit left %d revisions with re-seeds %v, want one from %s", len(got)-len(before), reseeds(got[len(before):]), late)
	}
	edited, err := f.C.EditDraft(ctx, stream, api.DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: fixturekit.ValidSpec, Plan: named})
	fixturekit.Must(t, err)
	if edited["plan"].Revision != 2 || edited["plan"].Content != named || edited["spec"].Revision != 1 {
		t.Fatalf("the edit left %+v", edited)
	}
	if ref := f.UpstreamRef(t); ref != late {
		t.Fatalf("the edit left the clone's upstream ref at %s, want the fetched %s", ref, late)
	}
	revisions := entityRevisions(t, repo)
	added := revisions[len(before):]
	if got := reseeds(added); len(added) != 1 || !slices.Equal(got, []string{late}) || !hasEntity(t, added[0], "internal.late") || added[0].Revision != before[len(before)-1].Revision+1 {
		t.Fatalf("the edit recorded %d revisions with re-seeds %v, want one from %s: %+v", len(added), got, late, added)
	}

	ratified, err := f.C.Ratify(ctx, stream, 1, 2)
	fixturekit.Must(t, err)
	if ratified.Sealing != "requested" {
		t.Fatalf("ratification %+v", ratified)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	record, _, found, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	if !found || record.Seal != 1 || record.Base.Commit != late {
		t.Fatalf("the seal %+v is not the first, at %s", record, late)
	}
	if len(record.Footprints) != 2 {
		t.Fatalf("footprints %+v", record.Footprints)
	}
	for _, footprint := range record.Footprints {
		if !slices.Equal(footprint.Entities, []string{"internal.late"}) || !slices.Equal(footprint.Paths, []string{"internal/late"}) {
			t.Fatalf("footprint of %s: %+v", footprint.Unit, footprint)
		}
	}
	if got := entityRevisions(t, repo); len(got) != len(revisions) {
		t.Fatalf("the ratification and the sealing recorded %d more revisions", len(got)-len(revisions))
	}
	sealings := f.SealOperations(t, stream)
	if len(sealings) != 1 || sealings[0].Result == nil || sealings[0].Result.Outcome != "succeeded" {
		t.Fatalf("sealings %+v", sealings)
	}

	later := f.LandUpstream(t, "internal/later")
	if refs := fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, later) {
		t.Fatalf("the clone already has %s", later)
	}
	amended := strings.Replace(named, `"footprint": ["internal.late"]`, `"footprint": ["internal.later"]`, 1)
	f.Stop(t)
	f.Opts.Committee = d.committee
	f.Start(t)
	repo = f.Repository()
	f.Script("amend-1-1", map[string]string{plan.PlanPath: amended}, nil)
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(1)+"-1", nil, nil)
	f.Script("amend-1-reply-1", nil, nil)
	fileChiefAmendment(t, f.ArchitectFixture, repo, stream, "1", record)
	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	view, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	_, err = f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Packet: view.Revision, Note: "The footprint names the new directory."})
	fixturekit.Must(t, err)
	resealed := awaitSealRevision(t, repo, stream, 2)
	i := slices.IndexFunc(resealed.Footprints, func(fp seal.Footprint) bool { return fp.Unit == "resume" })
	if i < 0 || !slices.Equal(resealed.Footprints[i].Entities, []string{"internal.later"}) || !slices.Equal(resealed.Footprints[i].Paths, []string{"internal/later"}) {
		t.Fatalf("footprints of the reseal %+v", resealed.Footprints)
	}
	if ref := f.UpstreamRef(t); ref != later {
		t.Fatalf("the amendment left the clone's upstream ref at %s, want the fetched %s", ref, later)
	}
	amendments := entityRevisions(t, repo)[len(revisions):]
	if got := reseeds(amendments); len(amendments) != 1 || !slices.Equal(got, []string{later}) || !hasEntity(t, amendments[0], "internal.later") {
		t.Fatalf("the amendment recorded %d revisions with re-seeds %v, want one from %s", len(amendments), got, later)
	}
}
