package service

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// assembleForTest moves a sealed workstream to assembled as the foreman does once
// every unit merged.
func assembleForTest(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: AssembledState, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: foremanActor, Cause: "test"}
	_, err := repo.SetFeatureState(context.Background(), h, AssembledState, "every unit merged")
	must(t, err)
}

// requestPublication records the owner's approval k as asked to publish, as
// the publisher does before its operation runs.
func requestPublication(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, k int) {
	t.Helper()
	subject, err := repo.Workflow(stream, publicationSubject)
	must(t, err)
	transition, _ := publishIDs(k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: transition, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: foremanActor, Cause: "test"}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: publicationSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", k), Reason: "the owner approved delivery"}})
	must(t, err)
}

// The owner records an assembled workstream merged into upstream outside the
// factory. It is refused before the workstream is assembled, while upstream
// lacks or conflicts with the feature branch, and while a publication the
// service could carry out has no outcome. Once upstream holds the branch's
// changes as other commits, one owner commit records the merge, moves the
// workstream to delivered with a notice and refuses the publication waiting
// for a GitHub token. Delivery and the trace show the merge in place of a
// pull request, and the workstream cannot be recorded merged again.
func TestOwnerRecordsAnAssembledWorkstreamMergedOutsideTheFactory(t *testing.T) {
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				requireJJ(t)
			}
			f := newDebateFixture(t, 1, 1)
			f.upstream(t)
			f.stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			stream, tip := sealDependent(t, f, repo, backend, "", map[string]string{"feature.go": "feature\n"})

			if _, api := f.s.mergedOutside(ctx, repo, stream); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "only an assembled workstream") {
				t.Fatalf("a workstream that is not assembled was recorded merged: %v", api)
			}
			assembleForTest(t, f, repo, stream)
			if _, api := f.s.mergedOutside(ctx, repo, stream); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "does not hold the changes") {
				t.Fatalf("recorded merged while upstream lacks the branch: %v", api)
			}
			advanceUpstream(t, f, map[string]string{"feature.go": "upstream's own\n"})
			if _, api := f.s.mergedOutside(ctx, repo, stream); api == nil || api.Code != Conflict {
				t.Fatalf("recorded merged while upstream conflicts with the branch: %v", api)
			}
			upstream := advanceUpstream(t, f, map[string]string{"feature.go": "feature\n"})
			requestPublication(t, f, repo, stream, 1)
			f.s.options.PullRequests = &fakePulls{}
			if _, api := f.s.mergedOutside(ctx, repo, stream); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "may still open a pull request") {
				t.Fatalf("recorded merged while a publication could still open a pull request: %v", api)
			}
			if feature, err := repo.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
				t.Fatalf("a refused merge moved the feature: %+v %v", feature, err)
			}

			f.s.options.PullRequests = &fakePulls{uncredentialed: true}
			out, api := f.s.mergedOutside(ctx, repo, stream)
			if api != nil {
				t.Fatal(api)
			}
			want := DeliveryMerge{Branch: featureBranch(stream), Commit: tip, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}}
			if out.Workstream != stream || out.State != DeliveredState || out.Merge != want || out.Refused != 1 {
				t.Fatalf("the recorded merge %+v", out)
			}
			delivered := transitionByID(t, repo, stream, DeliveredState)
			if delivered.Actor != ownerActor || delivered.Subject != trace.FeatureSubject || delivered.From != AssembledState || delivered.To != DeliveredState || !strings.Contains(delivered.Reason, upstream) {
				t.Fatalf("the delivery %+v", delivered)
			}
			if refused, err := publicationRefused(repo, stream, 1); err != nil || !refused {
				t.Fatalf("the waiting publication was not refused: %v %v", refused, err)
			}
			if subject, err := repo.Workflow(stream, publicationSubject); err != nil || subject.Value != "refused-1" {
				t.Fatalf("the publication %+v: %v", subject, err)
			}
			if merge, found, err := deliveryMerge(repo, stream); err != nil || !found || merge != want {
				t.Fatalf("the merge record %+v %v: %v", merge, found, err)
			}
			outbox, err := repo.Outbox(stream)
			must(t, err)
			if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
				return e.TransitionID == DeliveredState && e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, "No pull request was opened")
			}) {
				t.Fatalf("no delivery notice for the chief of staff in %+v", outbox)
			}

			presented, api := f.s.presentDelivery(ctx, repo.Project(), stream, repo)
			if api != nil {
				t.Fatal(api)
			}
			if !presented.Delivered || presented.Merge == nil || *presented.Merge != want || presented.Publication != nil {
				t.Fatalf("the delivery presentation %+v", presented)
			}
			w, err := loadTraceWalk(repo, stream)
			must(t, err)
			chain, _, gaps := w.delivery()
			if chain == nil || chain.Merge == nil || chain.Merge.Path != mergePath || chain.Merged != upstream {
				t.Fatalf("the trace's delivery %+v", chain)
			}
			for _, gap := range gaps {
				if gap.Link == publicationPath || gap.Link == deliveryPath {
					t.Fatalf("the trace of a merged workstream misses its publication: %+v", gap)
				}
			}
			if _, api := f.s.mergedOutside(ctx, repo, stream); api == nil || api.Code != Conflict {
				t.Fatalf("a delivered workstream was recorded merged again: %v", api)
			}
		})
	}
}

// A workstream whose base workstream the owner merged into upstream, here
// as a fast-forward, rebases onto upstream: the base is integrated.
func TestDependentOfAMergedBaseRebasesOntoUpstream(t *testing.T) {
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	f.stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	base, held := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	cfg := f.s.about(repo)
	g, err := featureWorkspaces(cfg, repo).of(stream)
	must(t, err)
	if selected, err := f.s.resolveBase(ctx, cfg, repo, stream, g); err != nil || selected.Workstream != base {
		t.Fatalf("before the merge the dependent builds on %+v: %v", selected, err)
	}

	assembleForTest(t, f, repo, base)
	home := filepath.Dir(f.clone)
	demoGit(t, home, "-C", f.clone, "push", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), held+":refs/heads/main")
	if _, api := f.s.mergedOutside(ctx, repo, base); api != nil {
		t.Fatal(api)
	}
	if changed, err := dependentBaseChanged(ctx, cfg, repo, stream); err != nil || !changed {
		t.Fatalf("the merged base did not ask the dependent to rebase: %v %v", changed, err)
	}
	selected, err := f.s.resolveBase(ctx, cfg, repo, stream, g)
	if err != nil || selected != (seal.Base{Remote: "upstream", Branch: "main", Commit: held}) {
		t.Fatalf("the dependent of a merged base resolves %+v: %v", selected, err)
	}
}
