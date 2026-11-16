package intakejourney

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	config "github.com/kpenfound/osmia/internal/config"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
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

// plantRatification records a plan revision naming the footprint and the
// owner's ratification of it, and asks for its sealing.
func plantRatification(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, revision int, footprint string) trace.OperationRecord {
	t.Helper()
	ctx := context.Background()
	drafted := strings.ReplaceAll(validPlan, `"footprint": ["internal.trace"]`, `"footprint": ["`+footprint+`"]`)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: revision, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner-edit"}, Path: plan.PlanPath, Content: drafted}}))
	content, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: revision}, nil))
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
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
