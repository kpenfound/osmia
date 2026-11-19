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

// fileChiefAmendment files amendment 1 as the chief of staff does: a request against
// the current seal, which the service's amendment passes take from there.
func fileChiefAmendment(t *testing.T, f *architectFixture, repo *trace.Repository, stream config.WorkstreamID, id string, sealed seal.Seal) {
	t.Helper()
	ctx := context.Background()
	_, doc, found, err := seal.Latest(repo, stream)
	must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	header := trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: chief, Cause: "fixture"}
	must(t, repo.Append(ctx, trace.Amendment{Header: header, Requester: chief, Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1",
		Citations: []string{"spec#1"}, Change: "Move a footprint to the new directory", Reason: "The directory landed upstream", Seal: sealed.Seal, SealRevision: doc.Revision, SpecHash: sealed.SpecHash}))
	header.Schema, header.ID = "osmia.trace.transition", "amendment_"+id+"_filed"
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: header, Subject: amendmentSubject(id), To: "filed", Reason: "filed"}})
	must(t, err)
}

// awaitSealRevision waits for the workstream's seal.json to reach the given
// revision and returns the seal it holds.
func awaitSealRevision(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, revision int) seal.Seal {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		current, doc, found, err := seal.Latest(repo, stream)
		must(t, err)
		if found && doc.Revision >= revision {
			return current
		}
		state, err := repo.Workflow(stream, amendmentSubject("1"))
		must(t, err)
		if state.Value == amendmentUnapplied || time.Now().After(deadline) {
			transitions, err := trace.Read[trace.Transition](repo, stream)
			must(t, err)
			var moves []string
			for _, tr := range transitions {
				if tr.Subject == amendmentSubject("1") {
					moves = append(moves, tr.To+": "+tr.Reason)
				}
			}
			t.Fatalf("the workstream stayed at seal revision %d, want %d, with amendment 1 %s: %q", doc.Revision, revision, state.Value, moves)
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// A plan edit and a ratification whose upstream fetch fails are validated
// against the latest recorded map: a footprint it resolves is accepted, one it
// does not is still refused, and no revision is recorded.
func TestEditAndRatificationValidateAgainstTheRecordedMapWhenTheFetchFails(t *testing.T) {
	t.Parallel()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { opts.WriteTimeout = time.Minute })
	ctx := context.Background()
	repo := f.repository()
	// The clone has no remote naming the project's upstream, so every fetch fails.
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	// The skip is accepted once the round is recorded, not while it runs.
	f.awaitShed(t, stream, "concluded-1")
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, "ratify: no objection stands")
	before := entityRevisions(t, repo)

	unknown := strings.ReplaceAll(validPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.nowhere"]`)
	for range 2 {
		if _, err := f.c.EditDraft(ctx, stream, DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: validSpec, Plan: unknown}); !failed(err, Validation) || !strings.Contains(err.Error(), "internal.nowhere") {
			t.Fatalf("editing in a footprint no map resolves: %v", err)
		}
	}
	edited, err := f.c.EditDraft(ctx, stream, DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: validSpec + "\nAn owner note.\n", Plan: validPlan})
	must(t, err)
	if edited["spec"].Revision != 2 {
		t.Fatalf("the edit left %+v", edited)
	}
	ratified, err := f.c.Ratify(ctx, stream, 2, 1)
	must(t, err)
	if ratified.Sealing != "requested" {
		t.Fatalf("ratification %+v", ratified)
	}
	if after := entityRevisions(t, repo); len(after) != len(before) {
		t.Fatalf("validations with a failing fetch recorded %d revisions", len(after)-len(before))
	}
}

// oldKnowledgeSentinel marks the prose and source ledger an older trace holds.
const oldKnowledgeSentinel = "OLD-KNOWLEDGE-SENTINEL"

// The whole feature, in a running service. Registering a project records the
// seeded map as its first revision and starts no librarian. A workstream
// drafts with the entity map and no prose in its context, even though the
// trace holds old prose and a source ledger. A directory then lands upstream
// while the clone's ref of upstream is stale, and the workstream, ratified
// with a plan naming that directory, is sealed by the service: the owner's
// edit fetches upstream and re-seeds once for that commit, so the edit and
// the ratification are accepted while the clone's ref of upstream was stale,
// and the sealing finds the map seeded already. A second directory then lands
// after the seal: an amendment draft naming it is accepted and re-seals with
// its footprint resolved. The plan edit, the ratification, the amendment
// draft, its debate and the owner's decision all go through the production
// actions; only the amendment request is filed into the trace.
func TestRunningServiceSealsAFootprintNamingADirectoryThatLandedAfterDrafting(t *testing.T) {
	t.Parallel()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) {
		opts.WriteTimeout = time.Minute
	})
	ctx := context.Background()
	// The draft's debate is skipped, so no committee runs until the amendment.
	committee := f.opts.Committee
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
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

	// The owner edits the plan to name it. A plan naming what no map resolves
	// is refused, and the refusal does not stop the edit that follows.
	named := strings.ReplaceAll(validPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.late"]`)
	unknown := strings.ReplaceAll(validPlan, `"footprint": ["internal.trace"]`, `"footprint": ["internal.nowhere"]`)
	if _, err := f.c.EditDraft(ctx, stream, DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: validSpec, Plan: unknown}); !failed(err, Validation) || !strings.Contains(err.Error(), "internal.nowhere") {
		t.Fatalf("editing in a footprint no map resolves: %v", err)
	}
	if got := entityRevisions(t, repo); len(got) != len(before)+1 || !slices.Equal(reseeds(got[len(before):]), []string{late}) {
		t.Fatalf("the refused edit left %d revisions with re-seeds %v, want one from %s", len(got)-len(before), reseeds(got[len(before):]), late)
	}
	edited, err := f.c.EditDraft(ctx, stream, DraftEdit{SpecRevision: 1, PlanRevision: 1, Spec: validSpec, Plan: named})
	must(t, err)
	if edited["plan"].Revision != 2 || edited["plan"].Content != named || edited["spec"].Revision != 1 {
		t.Fatalf("the edit left %+v", edited)
	}
	if ref := f.upstreamRef(t); ref != late {
		t.Fatalf("the edit left the clone's upstream ref at %s, want the fetched %s", ref, late)
	}
	revisions := entityRevisions(t, repo)
	added := revisions[len(before):]
	if got := reseeds(added); len(added) != 1 || !slices.Equal(got, []string{late}) || !hasEntity(t, added[0], "internal.late") || added[0].Revision != before[len(before)-1].Revision+1 {
		t.Fatalf("the edit recorded %d revisions with re-seeds %v, want one from %s: %+v", len(added), got, late, added)
	}

	// The owner ratifies it, and the service seals the workstream. Neither
	// computes another seed.
	ratified, err := f.c.Ratify(ctx, stream, 1, 2)
	must(t, err)
	if ratified.Sealing != "requested" {
		t.Fatalf("ratification %+v", ratified)
	}
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
	if got := entityRevisions(t, repo); len(got) != len(revisions) {
		t.Fatalf("the ratification and the sealing recorded %d more revisions", len(got)-len(revisions))
	}
	sealings := f.sealOperations(t, stream)
	if len(sealings) != 1 || sealings[0].Result == nil || sealings[0].Result.Outcome != "succeeded" {
		t.Fatalf("sealings %+v", sealings)
	}

	// A second directory lands upstream after the seal. An amendment draft
	// naming it is validated against the re-seeded map, debated, approved by
	// the owner and re-sealed with the footprint resolved.
	later := f.landUpstream(t, "internal/later")
	if refs := demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "for-each-ref", "--format=%(objectname)", "refs/remotes/upstream"); strings.Contains(refs, later) {
		t.Fatalf("the clone already has %s", later)
	}
	amended := strings.Replace(named, `"footprint": ["internal.late"]`, `"footprint": ["internal.later"]`, 1)
	f.stop(t)
	f.opts.Committee = committee
	f.start(t)
	repo = f.repository()
	f.script("amend-1-1", map[string]string{plan.PlanPath: amended}, nil)
	f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, nil)
	f.script("amend-1-reply-1", nil, nil)
	fileChiefAmendment(t, f.architectFixture, repo, stream, "1", record)
	f.awaitAmendment(t, stream, amendmentPresented)
	view, err := f.c.Amendment(ctx, stream, "1")
	must(t, err)
	_, err = f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: view.Revision, Note: "The footprint names the new directory."})
	must(t, err)
	resealed := awaitSealRevision(t, repo, stream, 2)
	i := slices.IndexFunc(resealed.Footprints, func(fp seal.Footprint) bool { return fp.Unit == "resume" })
	if i < 0 || !slices.Equal(resealed.Footprints[i].Entities, []string{"internal.later"}) || !slices.Equal(resealed.Footprints[i].Paths, []string{"internal/later"}) {
		t.Fatalf("footprints of the reseal %+v", resealed.Footprints)
	}
	if ref := f.upstreamRef(t); ref != later {
		t.Fatalf("the amendment left the clone's upstream ref at %s, want the fetched %s", ref, later)
	}
	amendments := entityRevisions(t, repo)[len(revisions):]
	if got := reseeds(amendments); len(amendments) != 1 || !slices.Equal(got, []string{later}) || !hasEntity(t, amendments[0], "internal.later") {
		t.Fatalf("the amendment recorded %d revisions with re-seeds %v, want one from %s", len(amendments), got, later)
	}

	// No librarian workstream, thread, operation or turn came of any of it.
	noLibrarian("after sealing")
	ran := slices.DeleteFunc(f.runs(), func(name string) bool { return name == "events" })
	slices.Sort(ran)
	if want := []string{"amend-1-1", "amend-1-reply-1", "amend-1-round-1-" + committeeAgent(1) + "-1", "draft-1-1"}; !slices.Equal(slices.Compact(ran), want) {
		t.Fatalf("the factory ran %v, want %v", ran, want)
	}
}
