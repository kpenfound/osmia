package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// upstream gives the fixture's clone a local bare upstream, named by the
// project's upstream repository, as a second remote, and returns the commit
// its main branch is at.
func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	home := filepath.Dir(f.clone)
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	if _, err := os.Stat(bare); err != nil {
		must(t, os.MkdirAll(filepath.Dir(bare), 0700))
		demoGit(t, home, "-C", f.clone, "branch", "-M", "main")
		demoGit(t, home, "clone", "--quiet", "--bare", f.clone, bare)
	}
	demoGit(t, home, "-C", f.clone, "remote", "add", "upstream", bare)
	return strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", "refs/heads/main"))
}

// awaitFeature waits until the workstream's feature state is the wanted one.
func (f *architectFixture) awaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, trace.FeatureSubject)
		must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s stayed %q, want %q", stream, state.Value, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sealMoves lists the seal subject's values in order.
func (f *architectFixture) sealMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == sealSubject {
			if tr.Actor != sealingActor {
				t.Fatalf("seal transition by %+v", tr)
			}
			moves = append(moves, tr.To)
		}
	}
	return moves
}

// sealOperations returns the workstream's sealing operations in sealing
// order.
func (f *architectFixture) sealOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.repository().Operations(stream)
	must(t, err)
	var out []trace.OperationRecord
	number := map[string]int{}
	for _, o := range ops {
		if o.Operation.Action == SealAction {
			in, err := decodeSeal(o.Operation)
			must(t, err)
			number[o.EventID] = in.Seal
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b trace.OperationRecord) int { return number[a.EventID] - number[b.EventID] })
	return out
}

// awaitSealMove waits until the seal subject has reached the wanted value.
func (f *architectFixture) awaitSealMove(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		if slices.Contains(f.sealMoves(t, stream), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s seal subject went %v, want %q", stream, f.sealMoves(t, stream), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitRetry waits until sealing k has recorded a retry whose reason contains
// the given text, and returns the reason.
func (f *architectFixture) awaitRetry(t *testing.T, stream config.WorkstreamID, k int, contains string) string {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		for _, o := range f.sealOperations(t, stream) {
			for _, a := range o.History {
				if a.Kind == "retry" && strings.Contains(a.Failure, contains) {
					return a.Failure
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sealing %d of workstream %s recorded no retry saying %q: %+v", k, stream, contains, f.sealOperations(t, stream))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// notice returns the body of the notice the given transition told the chief
// of staff with.
func (f *architectFixture) notice(t *testing.T, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && entry.Event.Kind == trace.NoticeKind {
			return entry.Event.Body
		}
	}
	t.Fatalf("transition %s of workstream %s told the chief of staff nothing", transition, stream)
	return ""
}

// ratified debates one round to consensus and ratifies it, returning the
// workstream and the ratification response.
func (f *shedFixture) ratified(t *testing.T) (config.WorkstreamID, RatifyResponse) {
	t.Helper()
	p := &faults{}
	f.member(1, 1, 1, concedes(p))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	out, err := f.c.Ratify(context.Background(), stream, 1, 1)
	must(t, err)
	return stream, out
}

// A passing ratification is sealed: upstream is fetched, the seal and the
// footprints are recorded in seal.json, the feature branch is created from
// the sealed commit in the service's workspace on the clone, nothing is
// pushed, and the workstream moves to ratified with a notice for the chief of
// staff.
func TestSealingRecordsTheSealAndCreatesTheFeatureBranch(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	commit := f.upstream(t)
	home := filepath.Dir(f.clone)
	stream, out := f.ratified(t)
	if out.Sealing != "requested" || !strings.HasSuffix(out.Detail, "; the sealing is asked for") {
		t.Fatalf("ratification %+v", out)
	}
	f.awaitFeature(t, stream, RatifiedState)

	// The operation ran once, to success, and is acknowledged.
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	operation := ops[0].Operation.ID
	if in, err := decodeSeal(ops[0].Operation); err != nil || in != (sealInput{Seal: 1, Round: 1, Spec: 1, Plan: 1, Ratification: 1}) {
		t.Fatalf("seal operation %+v: %v", ops[0].Operation, err)
	}
	if moves := f.sealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	if request := f.transition(t, stream, "seal-1"); request.Cause != shed.RatificationDocumentID(1)+"-1" || request.Reason != "the owner ratified spec.md revision 1 and plan.json revision 1 in round 1; sealing 1 fetches upstream, records the seal and the footprints and creates the feature branch" {
		t.Fatalf("the request %+v", request)
	}

	// The seal names the upstream commit, the spec hash, the branch, the
	// workspace and every unit's resolved footprint.
	docs := f.documents(t, stream, seal.DocumentID)
	if len(docs) != 1 || docs[0].Path != seal.Path || docs[0].Revision != 1 || docs[0].Actor != sealingActor || docs[0].Cause != operation {
		t.Fatalf("seal documents %+v", docs)
	}
	record, err := seal.Parse([]byte(docs[0].Content))
	must(t, err)
	branch := "osmia/" + string(stream)
	workspace := filepath.Join(f.opts.Config.Root, "branches", string(f.project), string(stream))
	spec := f.documents(t, stream, plan.SpecDocument)
	entities, err := kb.Load(f.repository())
	must(t, err)
	footprint := entities.ResolveEntities([]string{"internal.trace"})
	want := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(spec[0].Content),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: commit}, Branch: branch, Workspace: workspace,
		Footprints: []seal.Footprint{{Unit: "resume", Entities: footprint.Entities, Paths: footprint.Paths}, {Unit: "dedupe", Entities: footprint.Entities, Paths: footprint.Paths}}}
	if wantContent, err := seal.Encode(want); err != nil || docs[0].Content != string(wantContent) {
		t.Fatalf("seal.json:\n%s\nwant:\n%s\n%v", docs[0].Content, wantContent, err)
	}
	if len(footprint.Entities) == 0 || len(footprint.Paths) == 0 || len(footprint.Unresolved) != 0 || len(record.SpecHash) != 71 {
		t.Fatalf("the footprint %+v of seal %+v", footprint, record)
	}
	if data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), seal.Path)); err != nil || string(data) != docs[0].Content {
		t.Fatalf("seal.json on disk: %q %v", data, err)
	}

	// The clone has the feature branch at the sealed commit, checked out in
	// the workspace, and upstream has no branch of that name.
	if tip := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-parse", "refs/heads/"+branch)); tip != commit {
		t.Fatalf("the feature branch is at %s, want %s", tip, commit)
	}
	if on := strings.TrimSpace(demoGit(t, home, "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD")); on != branch {
		t.Fatalf("the workspace is on %s", on)
	}
	if _, err := os.Stat(filepath.Join(workspace, "internal", "trace", "git.go")); err != nil {
		t.Fatalf("the workspace's files: %v", err)
	}
	if refs := demoGit(t, home, "-C", filepath.Join(home, "remotes", "dagger", "dagger.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(refs) != "refs/heads/main" {
		t.Fatalf("upstream's refs after the seal:\n%s", refs)
	}

	// The move to ratified is the operation's, in one transition with its
	// notice.
	moved := f.transition(t, stream, RatifiedState)
	reason := fmt.Sprintf("sealed spec.md revision 1 and plan.json revision 1 at %s of upstream/main (%s); feature branch %s is checked out in %s; the footprints of 2 units are recorded", commit, record.SpecHash, branch, workspace)
	if moved.From != InShedState || moved.To != RatifiedState || moved.Cause != operation || moved.Actor != sealingActor || moved.Reason != reason {
		t.Fatalf("the move to ratified %+v", moved)
	}
	if body := f.notice(t, stream, RatifiedState); body != "Workstream state changed from in-shed to ratified: "+reason {
		t.Fatalf("the notice %q", body)
	}
	if ops[0].Result.Evidence != reason {
		t.Fatalf("the result's evidence %q", ops[0].Result.Evidence)
	}
	status, err := f.c.Status(ctx, stream)
	must(t, err)
	if status.State == nil || *status.State != RatifiedState {
		t.Fatalf("status %+v", status)
	}
	// A ratified workstream is past the shed.
	if _, err := f.c.Ratify(ctx, stream, 1, 1); !failed(err, Conflict) || !strings.Contains(err.Error(), "is ratified; the owner takes part in the shed while it is in-shed or sketched") {
		t.Fatalf("ratifying a ratified workstream: %v", err)
	}
	if docs := f.documents(t, stream, seal.DocumentID); len(docs) != 1 {
		t.Fatalf("seal documents after: %+v", docs)
	}
	if out := demoGit(t, home, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatalf("worktrees:\n%s", out)
	}
}

// A fetch that fails is retried as infrastructure: the sealing stays pending
// with the failure as its reason, the workstream stays in the shed, and the
// retry after the clone is put right succeeds. A ratification the owner
// repeats meanwhile reports the pending sealing and asks for nothing new.
func TestSealingRetriesAFailedFetch(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	stream, _ := f.ratified(t)
	reason := f.awaitRetry(t, stream, 1, "the clone has no remote whose URL names dagger/dagger")
	f.stillInShed(t, stream)
	again, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if again.Sealing != "pending" && again.Sealing != "running" {
		t.Fatalf("ratifying again %+v", again)
	}
	// Between an attempt's claim and its retry the reason is not yet known.
	pending := fmt.Sprintf("workstream %s is ratified at spec.md revision 1 and plan.json revision 1 already; sealing 1 is %s", stream, again.Sealing)
	if again.Detail != pending && again.Detail != pending+" after a failed attempt: "+reason {
		t.Fatalf("ratifying again %+v", again)
	}
	if ops := f.sealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("ratifying again asked for %+v", ops)
	}
	if docs := f.documents(t, stream, shed.RatificationDocumentID(1)); len(docs) != 1 {
		t.Fatalf("ratifying again recorded %+v", docs)
	}
	if moves := f.sealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}

	commit := f.upstream(t)
	f.awaitFeature(t, stream, RatifiedState)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	record, _, found, err := seal.Latest(f.repository(), stream)
	must(t, err)
	if !found || record.Seal != 1 || record.Base.Commit != commit {
		t.Fatalf("the seal %+v", record)
	}
}

// After a restart the sealing resumes from what the clone holds: a feature
// branch an interrupted attempt created is the branch, its commit the seal,
// and no second branch or workspace is made. A branch of that name that is
// not on upstream is not built on: the sealing fails with a recorded reason,
// the chief of staff is told, and ratifying again once it is gone asks for a
// new sealing that succeeds.
func TestSealingResumesFromTheBranchTheCloneHolds(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	commit := f.upstream(t)
	home := filepath.Dir(f.clone)
	p := &faults{}
	f.member(1, 1, 1, concedes(p))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	branch := "osmia/" + string(stream)
	// A branch not on upstream: a commit the clone alone has.
	must(t, os.WriteFile(filepath.Join(f.clone, "local.txt"), []byte("local\n"), 0600))
	demoGit(t, home, "-C", f.clone, "add", "local.txt")
	demoGit(t, home, "-C", f.clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "local")
	local := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-parse", "HEAD"))
	demoGit(t, home, "-C", f.clone, "branch", branch, local)

	_, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	f.awaitSealMove(t, stream, "failed-1")
	reason := fmt.Sprintf("sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the clone has a branch %s at %s that is not on upstream/main; it is not the service's feature branch", branch, local)
	if got := f.transition(t, stream, "seal-1-failed"); got.Reason != reason || got.From != "sealing-1" || got.Cause != "seal-1" {
		t.Fatalf("the failure %+v", got)
	}
	if body := f.notice(t, stream, "seal-1-failed"); body != fmt.Sprintf("The sealing of spec.md revision 1 and plan.json revision 1 failed and the workstream is not ratified: the clone has a branch %s at %s that is not on upstream/main; it is not the service's feature branch. Once that is put right, ratifying the same revisions again asks for the sealing again.", branch, local) {
		t.Fatalf("the notice %q", body)
	}
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != reason {
		t.Fatalf("seal operations %+v", ops)
	}
	f.stillInShed(t, stream)
	if _, err := os.Stat(filepath.Join(f.opts.Config.Root, "branches", string(f.project), string(stream))); !os.IsNotExist(err) {
		t.Fatalf("a workspace was made on the stray branch: %v", err)
	}
	// The controller asks for nothing on its own after a failure; the owner
	// does, once the branch is where an interrupted attempt would have left
	// it: on upstream, without its workspace.
	must(t, (&sealer{s: f.s, repository: f.repository()}).Pass(ctx))
	if ops := f.sealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("the pass asked for %+v", ops)
	}
	demoGit(t, home, "-C", f.clone, "branch", "-f", branch, commit)
	again, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if again.Sealing != "requested" || again.Detail != fmt.Sprintf("workstream %s is ratified at spec.md revision 1 and plan.json revision 1 already; sealing 1 failed and the sealing is asked for again", stream) {
		t.Fatalf("ratifying again %+v", again)
	}
	// The request is the ratification recorded again, as its next revision.
	docs := f.documents(t, stream, shed.RatificationDocumentID(1))
	if len(docs) != 2 || docs[1].Revision != 2 || docs[1].Content != docs[0].Content {
		t.Fatalf("ratification documents %+v", docs)
	}
	if got := f.transition(t, stream, shed.RatificationDocumentID(1)+"-2"); got.To != "ratified-1" || got.Reason != "the owner ratified spec.md revision 1 and plan.json revision 1 after round 1 again; sealing 1 failed and the sealing is asked for again" {
		t.Fatalf("the second ratification %+v", got)
	}
	f.awaitFeature(t, stream, RatifiedState)
	ops = awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) })
	if len(ops) != 2 || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}
	// The inspection saw the branch, and the sealing built on it.
	if i := slices.IndexFunc(ops[1].History, func(a trace.OperationAction) bool { return a.Kind == "observe" }); i < 0 || ops[1].History[i].Observation.Evidence != fmt.Sprintf("the clone has feature branch %s; the sealing resumes from it", branch) {
		t.Fatalf("the observation of sealing 2: %+v", ops[1].History)
	}
	record, doc, found, err := seal.Latest(f.repository(), stream)
	must(t, err)
	if !found || doc.Revision != 1 || record.Seal != 2 || record.Base.Commit != commit {
		t.Fatalf("the seal %+v (revision %d)", record, doc.Revision)
	}
	if moves := f.sealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1", "failed-1", "sealing-2"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	if tip := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-parse", "refs/heads/"+branch)); tip != commit {
		t.Fatalf("the feature branch is at %s, want %s", tip, commit)
	}
	if out := demoGit(t, home, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatalf("worktrees:\n%s", out)
	}
}

// A ratification the owner supersedes before its sealing runs is not sealed:
// the sealing fails with the later ratification as its reason, and the later
// one is sealed.
func TestSealingSealsTheLatestRatificationOnly(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	stream, _ := f.ratified(t)
	f.awaitRetry(t, stream, 1, "no remote whose URL names")
	// The owner rewords a criterion, which is a new revision to ratify.
	must(t, os.WriteFile(filepath.Join(f.trace, "workstreams", string(stream), plan.SpecPath), []byte(strings.Replace(validSpec, "never sent again", "not sent twice", 1)), 0600))
	f.awaitDocument(t, stream, plan.SpecDocument, 2)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	second, err := f.c.Ratify(ctx, stream, 2, 1)
	must(t, err)
	if second.Sealing != "requested" || !strings.HasSuffix(second.Detail, "; the sealing is asked for") {
		t.Fatalf("the second ratification %+v", second)
	}
	f.awaitSealMove(t, stream, "failed-1")
	if got := f.transition(t, stream, "seal-1-failed"); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the owner's latest ratification is of spec.md revision 2 and plan.json revision 1 in round 1, not of spec.md revision 1 and plan.json revision 1 in round 1" {
		t.Fatalf("the failure %+v", got)
	}
	commit := f.upstream(t)
	f.awaitFeature(t, stream, RatifiedState)
	record, _, found, err := seal.Latest(f.repository(), stream)
	must(t, err)
	spec := f.documents(t, stream, plan.SpecDocument)
	if !found || record.Seal != 2 || record.Revision != (shed.Pin{Spec: 2, Plan: 1}) || record.SpecHash != seal.SpecHash(spec[1].Content) || record.Base.Commit != commit {
		t.Fatalf("the seal %+v", record)
	}
	if ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.sealOperations(t, stream) }); len(ops) != 2 || ops[0].Result.Outcome != "failed" || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("seal operations %+v", ops)
	}

}

// A footprint the entity map no longer resolves when the seal is taken is a
// recorded failure, not a retry.
func TestSealingFailsOnAFootprintTheMapDoesNotResolve(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	stream, _ := f.ratified(t)
	f.awaitRetry(t, stream, 1, "no remote whose URL names")
	must(t, kb.Store(ctx, f.repository(), kb.Map{Version: kb.Version, Entities: []kb.Entity{{ID: "other", Name: "Other", Paths: []string{"other/**"}}}}, f.clock.Now(), ownerActor, "test"))
	f.awaitSealMove(t, stream, "failed-1")
	if got := f.transition(t, stream, "seal-1-failed"); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the plan's footprints name what the entity map does not resolve: resume: internal.trace, dedupe: internal.trace" {
		t.Fatalf("the failure %+v", got)
	}
	f.stillInShed(t, stream)
}

// A service stop between the record of a ratification and the request of its
// sealing leaves the sealing owed, and the next service's first pass asks for
// it. A skipped debate on a workstream that never entered the shed is sealed
// from sketched. A sealing asked for before the owner abandoned the
// workstream fails once it runs, and makes no branch.
func TestSealingIsAskedForAfterARestartAndSealsFromSketched(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	commit := f.upstream(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	gone := f.handIn(t, "second", handedDesign)
	f.await(t, gone, sketched)
	f.stop(t)

	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	// The owner skipped debate and ratified, and the service stopped before
	// asking for the sealing.
	ratify := func(stream config.WorkstreamID) {
		owner, err := repo.Workflow(stream, ownerSubject)
		must(t, err)
		skip := trace.Transaction{ExpectedVersion: owner.Version, Transition: trace.Transition{Header: ownerHeader(skipTransition, f.project, stream, "owner-shed", f.clock.Now()), Subject: ownerSubject, From: "", To: skippedValue, Reason: "planted"}}
		_, err = repo.Transact(ctx, skip)
		must(t, err)
		content, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
		must(t, err)
		must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.RatificationDocumentID(1), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner-shed"}, Path: shed.RatificationPath(1), Content: string(content)}}))
	}
	ratify(stream)
	// The other one was asked for, and then the owner abandoned the
	// workstream before it ran.
	ratify(gone)
	latest, found, err := latestRatification(repo, gone)
	must(t, err)
	if !found {
		t.Fatal("no ratification planted")
	}
	if _, err := (&sealer{s: f.s, repository: repo}).request(ctx, gone, latest); err != nil {
		t.Fatal(err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: f.project, Workstream: gone, At: f.clock.Now(), Actor: ownerActor, Cause: abandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Not needed.")
	must(t, err)
	must(t, repo.Close())

	f.start(t)
	defer f.stop(t)
	f.awaitFeature(t, stream, RatifiedState)
	f.awaitSealMove(t, gone, "failed-1")
	if got := f.transition(t, gone, "seal-1-failed"); got.Reason != "sealing 1 of spec.md revision 1 and plan.json revision 1 failed: the workstream is abandoned, not in the shed" {
		t.Fatalf("the failure %+v", got)
	}
	if state, err := f.repository().Workflow(gone, trace.FeatureSubject); err != nil || state.Value != AbandonedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if _, found, err := (&sealer{s: f.s, repository: f.repository()}).git().Branch(ctx, "osmia/"+string(gone)); err != nil || found {
		t.Fatalf("branch of the abandoned workstream: %v %v", found, err)
	}
	if moves := f.sealMoves(t, stream); !slices.Equal(moves, []string{"sealing-1"}) {
		t.Fatalf("seal subject went %v", moves)
	}
	moved := f.transition(t, stream, RatifiedState)
	if moved.From != SketchedState || moved.To != RatifiedState {
		t.Fatalf("the move to ratified %+v", moved)
	}
	record, _, found, err := seal.Latest(f.repository(), stream)
	must(t, err)
	if !found || record.Base.Commit != commit || record.Round != 1 {
		t.Fatalf("the seal %+v", record)
	}
	// The ratified workstream is at rest: further passes ask for nothing.
	must(t, (&sealer{s: f.s, repository: f.repository()}).Pass(ctx))
	if ops := f.sealOperations(t, stream); len(ops) != 1 {
		t.Fatalf("seal operations %+v", ops)
	}
}
