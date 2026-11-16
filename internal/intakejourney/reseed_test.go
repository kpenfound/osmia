package intakejourney

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// landUpstream commits a new directory on the fixture's upstream through a
// scratch clone and returns the commit. The service's clone does not fetch it.
func (f *architectFixture) landUpstream(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Dir(f.clone)
	scratch := t.TempDir()
	demoGit(t, home, "clone", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	must(t, os.MkdirAll(filepath.Join(scratch, filepath.FromSlash(dir)), 0700))
	must(t, os.WriteFile(filepath.Join(scratch, filepath.FromSlash(dir), "doc.go"), []byte("package x\n"), 0600))
	demoGit(t, home, "-C", scratch, "add", "--all")
	demoGit(t, home, "-C", scratch, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "land "+dir)
	demoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(demoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

// removeUpstream commits the removal of a directory on the fixture's upstream.
func (f *architectFixture) removeUpstream(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Dir(f.clone)
	scratch := t.TempDir()
	demoGit(t, home, "clone", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	demoGit(t, home, "-C", scratch, "rm", "-rq", dir)
	demoGit(t, home, "-C", scratch, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "remove "+dir)
	demoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(demoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

// upstreamRef returns the commit the clone's remote-tracking ref of upstream's
// main branch is at.
func (f *architectFixture) upstreamRef(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", "refs/remotes/upstream/main"))
}

// entityRevisions returns the revisions of the entity map in order.
func entityRevisions(t *testing.T, repo *trace.Repository) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != trace.EntitiesDocument })
}

// reseeds returns the revisions of the entity map a re-seed recorded.
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
	must(t, err)
	_, ok := m.Lookup(id)
	return ok
}

// recordRatification records a plan revision naming the footprint and the
// owner's ratification of it. The service's sealing pass asks for its sealing.
func recordRatification(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, revision int, footprint string) {
	t.Helper()
	ctx := context.Background()
	drafted := strings.ReplaceAll(validPlan, `"footprint": ["internal.trace"]`, `"footprint": ["`+footprint+`"]`)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: revision, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner-edit"}, Path: plan.PlanPath, Content: drafted}}))
	content, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: revision}, nil))
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
}

// plantRatification is recordRatification and the request of the sealing of
// the ratification. It returns the sealing operation.
func plantRatification(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, revision int, footprint string) trace.OperationRecord {
	t.Helper()
	ctx := context.Background()
	recordRatification(t, f, repo, stream, revision, footprint)
	latest, found, err := latestRatification(repo, stream)
	must(t, err)
	if !found || latest.Recorded != 1 {
		t.Fatalf("latest ratification %+v %v", latest, found)
	}
	k, err := (&sealer{S: f.s.intake().SealerDependencies(), Repository: repo}).Request(ctx, stream, latest)
	must(t, err)
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, o := range ops {
		if in, err := decodeSeal(o.Operation); err == nil && in.Seal == k {
			return o
		}
	}
	t.Fatalf("no sealing %d", k)
	return trace.OperationRecord{}
}

// A draft re-seeds the entity map from the upstream head it fetches. A
// directory that lands upstream afterwards, while the clone's ref of upstream
// is stale, is fetched and re-seeded by the sealing before its footprints
// resolve, so a plan naming it seals.
func TestDraftAndFirstSealReseedFromTheFetchedUpstreamHead(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	f.upstream(t)
	early := f.landUpstream(t, "internal/early")
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, "ratify: no objection stands")

	revisions := entityRevisions(t, f.repository())
	if got := reseeds(revisions); !slices.Equal(got, []string{early}) || !hasEntity(t, revisions[len(revisions)-1], "internal.early") {
		t.Fatalf("the draft's re-seed recorded %v of %d revisions, want one from %s", got, len(revisions), early)
	}
	late := f.landUpstream(t, "internal/late")
	if ref := f.upstreamRef(t); ref != early {
		t.Fatalf("the clone's upstream ref is %s, want the stale %s", ref, early)
	}
	f.stop(t)

	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	must(t, err)
	defer repo.Close()
	op := plantRatification(t, f, repo, stream, 2, "internal.late")
	if got := reseeds(entityRevisions(t, repo)); !slices.Equal(got, []string{early}) {
		t.Fatalf("re-seeds before the sealing: %v", got)
	}
	z := &sealer{S: f.s.intake().SealerDependencies(), Repository: repo}
	result, err := z.Apply(ctx, op.Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("sealing: %+v", result)
	}
	record, _, found, err := seal.Latest(repo, stream)
	must(t, err)
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

	// Another trigger at the same head seeds nothing and records nothing.
	f.s.intake().ReseedEntities(ctx, repo)
	if after := entityRevisions(t, repo); len(after) != len(revisions) {
		t.Fatalf("a repeated trigger recorded %d revisions", len(after)-len(revisions))
	}
}

// An amendment re-seal fetches upstream and re-seeds before it resolves the
// revised plan's footprints, so a plan naming a directory that landed after
// the first seal re-seals.
func TestAmendmentResealReseedsFromTheFetchedUpstreamHead(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "reseal-reseeds")
	defer repository.Close()
	home := filepath.Dir(f.clone)
	before := len(entityRevisions(t, repository))
	late := f.landUpstream(t, "internal/late")
	if refs := demoGit(t, home, "-C", f.clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, late) {
		t.Fatalf("the clone already has %s", late)
	}
	revised := strings.Replace(amendedIndependentPlan, `"footprint": ["internal"]`, `"footprint": ["internal.late"]`, 1)
	a, req := approveAmendment(t, f, repository, stream, amendedSpec, revised)
	step(t, a, stream, req, amendmentResealed)

	current, _, found, err := seal.Latest(repository, stream)
	must(t, err)
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

// A plan naming what the pure seed no longer produces is refused with the
// unresolved-footprint failure: the directory of an entity is gone upstream,
// so the sealing's re-seed drops the entity.
func TestSealRefusesAFootprintTheReseedDropped(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	f.upstream(t)
	doomed := f.landUpstream(t, "internal/doomed")
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, "ratify: no objection stands")
	revisions := entityRevisions(t, f.repository())
	if got := reseeds(revisions); !slices.Equal(got, []string{doomed}) || !hasEntity(t, revisions[len(revisions)-1], "internal.doomed") {
		t.Fatalf("the draft's re-seed recorded %v", got)
	}
	gone := f.removeUpstream(t, "internal/doomed")
	f.stop(t)

	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	must(t, err)
	defer repo.Close()
	op := plantRatification(t, f, repo, stream, 2, "internal.doomed")
	result, err := (&sealer{S: f.s.intake().SealerDependencies(), Repository: repo}).Apply(ctx, op.Operation)
	must(t, err)
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

// A fetch or re-seed that fails is logged once per distinct reason, never
// returned, and leaves the latest recorded map.
func TestReseedFailureIsLoggedOnceAndNeverBlocks(t *testing.T) {
	t.Parallel()
	f := newSealingRestartFixture(t)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	repo := f.repository()
	reseeder := f.s.intake().Reseeder(repo)
	if reseeder == nil {
		t.Fatal("the project has no re-seeder")
	}
	var mu sync.Mutex
	var logged []string
	reseeder.Log = func(reason string) { mu.Lock(); logged = append(logged, reason); mu.Unlock() }
	before := entityRevisions(t, repo)
	// The clone has no remote naming the project's upstream: the fetch fails.
	for range 3 {
		f.s.intake().ReseedEntities(ctx, repo)
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

// oldKnowledgeSentinel marks the prose and source ledger an older trace holds.
const oldKnowledgeSentinel = "OLD-KNOWLEDGE-SENTINEL"

// The whole feature, in a running service. Registering a project records the
// seeded map as its first revision and starts no librarian. A workstream
// drafts with the entity map and no prose in its context, even though the
// trace holds old prose and a source ledger. A directory then lands upstream
// while the clone's ref of upstream is stale, and the workstream, ratified
// with a plan naming that directory, is sealed by the service: the sealing
// fetches upstream, re-seeds once for that commit, and resolves the footprint.
func TestRunningServiceSealsAFootprintNamingADirectoryThatLandedAfterDrafting(t *testing.T) {
	t.Parallel()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) {
		opts.WriteTimeout = time.Minute
		opts.Committee = nil
	})
	ctx := context.Background()
	repo := f.repository()
	librarian := intake.LibrarianWorkstream(f.project)
	noLibrarian := func(when string) {
		t.Helper()
		streams, err := repo.Workstreams()
		must(t, err)
		if slices.Contains(streams, librarian) {
			t.Fatalf("%s: the trace holds the librarian workstream %s", when, librarian)
		}
		for _, stream := range streams {
			ops, err := repo.Operations(stream)
			must(t, err)
			for _, op := range ops {
				if strings.HasPrefix(op.Operation.Action, "kb-") {
					t.Fatalf("%s: workstream %s holds the operation %s", when, stream, op.Operation.Action)
				}
			}
		}
		for _, run := range f.runs() {
			if strings.Contains(run, "librarian") || strings.HasPrefix(run, "kb-") {
				t.Fatalf("%s: the factory ran %s", when, run)
			}
		}
	}

	// Registering recorded the seeded map as the first revision and asked for
	// nothing else.
	registered := entityRevisions(t, repo)
	if len(registered) != 1 || registered[0].Revision != 1 || len(reseeds(registered)) != 0 || !hasEntity(t, registered[0], "internal.trace") {
		t.Fatalf("registration recorded %d entity map revisions: %+v", len(registered), registered)
	}
	noLibrarian("after registering")
	if runs := f.runs(); len(runs) != 0 {
		t.Fatalf("registering ran %v", runs)
	}

	f.upstream(t)
	oldProse := trace.Actor{Kind: "agent", ID: "agent_librarian"}
	oldDocument := func(id, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.project, At: f.clock.Now(), Actor: oldProse, Cause: "extract"}, Path: path, Content: content}
	}
	must(t, repo.RecordDocuments(ctx, []trace.Document{
		oldDocument("subsystem-internal", "kb/internal.md", "# Internal\n\n"+oldKnowledgeSentinel+"\n"),
		oldDocument("kb-sources", "kb/sources.json", `["`+oldKnowledgeSentinel+`"]`+"\n"),
	}))

	var mu sync.Mutex
	var problems []string
	checked := false
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		mu.Lock()
		checked = true
		mu.Unlock()
		text, err := readTool(ctx, tools, "context.md")
		if err != nil {
			fail("read context.md: %v", err)
		}
		if !strings.Contains(text, "source: kb/entities.json (record kb-entities revision ") {
			fail("context.md lacks the entity map:\n%s", text)
		}
		for _, banned := range []string{"## Knowledge base", "nowledge-base", "KB files", "- missing:", oldKnowledgeSentinel} {
			if strings.Contains(text, banned) {
				fail("context.md carries %q:\n%s", banned, text)
			}
		}
		listed, err := callTool(ctx, tools, projectContextTool, map[string]any{"prefix": "kb/"})
		var records struct {
			Records []struct{ Path string } `json:"records"`
		}
		if err != nil || json.Unmarshal([]byte(listed), &records) != nil || len(records.Records) != 1 || records.Records[0].Path != "kb/entities.json" {
			fail("project_context lists kb/ as %s: %v", listed, err)
		}
		if found, err := callTool(ctx, tools, projectContextTool, map[string]any{"query": oldKnowledgeSentinel}); err != nil || !strings.Contains(found, `"total":0`) {
			fail("project_context finds old prose: %s: %v", found, err)
		}
		for _, args := range []map[string]any{{"path": "kb/internal.md"}, {"path": "kb/sources.json"}, {"path": "kb/internal.md", "revision": 1}, {"path": "kb/sources.json", "revision": 1}} {
			if out, err := callTool(ctx, tools, projectContextTool, args); err == nil || !strings.Contains(err.Error(), "project document revision not found") {
				fail("project_context read %v: %s: %v", args, out, err)
			}
		}
		if entities, err := callTool(ctx, tools, projectContextTool, map[string]any{"path": "kb/entities.json"}); err != nil || !strings.Contains(entities, "internal.trace") {
			fail("project_context entity map: %s: %v", entities, err)
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, "ratify: no objection stands")
	mu.Lock()
	if !checked || len(problems) != 0 {
		t.Fatalf("the architect's turn ran %t and found: %s", checked, strings.Join(problems, "\n"))
	}
	mu.Unlock()

	// The same bundle, assembled by the service, has an entity map and no
	// knowledge or missing field.
	assembled, err := f.s.Context().Assemble(ctx, f.project, bundle.Scope{Role: "architect", Workstream: stream})
	must(t, err)
	data, err := json.Marshal(assembled)
	must(t, err)
	var fields map[string]json.RawMessage
	must(t, json.Unmarshal(data, &fields))
	for _, field := range []string{"knowledge", "missing"} {
		if _, ok := fields[field]; ok {
			t.Errorf("the architect's bundle has a %s field", field)
		}
	}
	if strings.Contains(assembled.Render(), oldKnowledgeSentinel) || strings.Contains(assembled.Brief(), oldKnowledgeSentinel) || assembled.Entities.Revision < 1 || len(assembled.Entities.Entities) == 0 {
		t.Errorf("the architect's bundle: %+v", assembled.Entities)
	}

	// A directory lands upstream after the draft. The clone does not fetch it.
	before := entityRevisions(t, repo)
	late := f.landUpstream(t, "internal/late")
	if ref := f.upstreamRef(t); ref == late {
		t.Fatalf("the clone's upstream ref is already at %s", late)
	}
	if refs := demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, late) {
		t.Fatalf("the clone already has %s", late)
	}

	// The owner ratifies a plan naming it, and the service seals the workstream.
	recordRatification(t, f, repo, stream, 2, "internal.late")
	f.awaitFeature(t, stream, BuildingState)

	record, _, found, err := seal.Latest(repo, stream)
	must(t, err)
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
	if ref := f.upstreamRef(t); ref != late {
		t.Fatalf("the sealing left the clone's upstream ref at %s, want the fetched %s", ref, late)
	}
	revisions := entityRevisions(t, repo)
	added := revisions[len(before):]
	if got := reseeds(added); len(added) != 1 || !slices.Equal(got, []string{late}) || !hasEntity(t, added[0], "internal.late") || added[0].Revision != before[len(before)-1].Revision+1 {
		t.Fatalf("the sealing recorded %d revisions with re-seeds %v, want one from %s: %+v", len(added), got, late, added)
	}
	sealings := f.sealOperations(t, stream)
	if len(sealings) != 1 || sealings[0].Result == nil || sealings[0].Result.Outcome != "succeeded" {
		t.Fatalf("sealings %+v", sealings)
	}

	// No librarian workstream, thread, operation or turn came of any of it.
	noLibrarian("after sealing")
	if ran := slices.DeleteFunc(f.runs(), func(name string) bool { return name == "events" }); !slices.Equal(ran, []string{"draft-1-1"}) {
		t.Fatalf("the factory ran %v", ran)
	}
}
