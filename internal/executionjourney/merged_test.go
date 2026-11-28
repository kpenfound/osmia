package executionjourney

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func assembleForTest(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: intake.AssembledState, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.S.Now(), Actor: execution.ForemanActor, Cause: "test"}
	_, err := repo.SetFeatureState(context.Background(), h, intake.AssembledState, "every unit merged")
	fixturekit.Must(t, err)
}

func requestPublication(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, k int) {
	t.Helper()
	subject, err := repo.Workflow(stream, delivery.PublicationSubject)
	fixturekit.Must(t, err)
	transition, _ := delivery.PublishIDs(k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: transition, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.S.Now(), Actor: execution.ForemanActor, Cause: "test"}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: delivery.PublicationSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", k), Reason: "the owner approved delivery"}})
	fixturekit.Must(t, err)
}

func TestOwnerRecordsAnAssembledWorkstreamMergedOutsideTheFactory(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				intakefixture.RequireJJ(t)
			}
			t.Parallel()
			f := journey.NewDebateFixture(t, 1, 1)
			f.Upstream(t)
			f.Stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			stream, tip := journey.SealDependent(t, f, repo, backend, "", map[string]string{"feature.go": "feature\n"})

			if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream); apiErr == nil || apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "only an assembled workstream") {
				t.Fatalf("a workstream that is not assembled was recorded merged: %v", apiErr)
			}
			assembleForTest(t, f, repo, stream)
			if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream); apiErr == nil || apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "does not hold the changes") {
				t.Fatalf("recorded merged while upstream lacks the branch: %v", apiErr)
			}
			journey.AdvanceUpstream(t, f, map[string]string{"feature.go": "upstream's own\n"})
			if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream); apiErr == nil || apiErr.Code != api.Conflict {
				t.Fatalf("recorded merged while upstream conflicts with the branch: %v", apiErr)
			}
			upstream := journey.AdvanceUpstream(t, f, map[string]string{"feature.go": "feature\n"})
			requestPublication(t, f, repo, stream, 1)
			f.S.Options.PullRequests = &fixturekit.FakePulls{}
			if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream); apiErr == nil || apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "may still be applied") {
				t.Fatalf("recorded merged while a publication could still open a pull request: %v", apiErr)
			}
			if feature, err := repo.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
				t.Fatalf("a refused merge moved the feature: %+v %v", feature, err)
			}

			f.S.Options.PullRequests = &fixturekit.FakePulls{Uncredentialed: true}
			out, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			want := api.DeliveryMerge{Branch: intake.FeatureBranch(stream), Commit: tip, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}}
			if out.Workstream != stream || out.State != intake.DeliveredState || out.Merge != want || out.Refused != 1 {
				t.Fatalf("the recorded merge %+v", out)
			}
			delivered := intakefixture.TransitionByID(t, repo, stream, intake.DeliveredState)
			if delivered.Actor != intake.OwnerActor || delivered.Subject != trace.FeatureSubject || delivered.From != intake.AssembledState || delivered.To != intake.DeliveredState || !strings.Contains(delivered.Reason, upstream) {
				t.Fatalf("the delivery %+v", delivered)
			}
			if refused, err := delivery.PublicationRefused(repo, stream, 1); err != nil || !refused {
				t.Fatalf("the waiting publication was not refused: %v %v", refused, err)
			}
			if subject, err := repo.Workflow(stream, delivery.PublicationSubject); err != nil || subject.Value != "refused-1" {
				t.Fatalf("the publication %+v: %v", subject, err)
			}
			if merge, found, err := delivery.ReadDeliveryMerge(repo, stream); err != nil || !found || merge != want {
				t.Fatalf("the merge record %+v %v: %v", merge, found, err)
			}
			outbox, err := repo.Outbox(stream)
			fixturekit.Must(t, err)
			if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
				return e.TransitionID == intake.DeliveredState && e.Event.Kind == trace.NoticeKind && strings.Contains(e.Event.Body, "No pull request was opened")
			}) {
				t.Fatalf("no delivery notice for the manager in %+v", outbox)
			}

			presented, apiErr := f.S.Delivery().PresentDelivery(ctx, repo.Project(), stream, repo)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			if !presented.Delivered || presented.Merge == nil || *presented.Merge != want || presented.Publication != nil {
				t.Fatalf("the delivery presentation %+v", presented)
			}
			w, err := service.LoadTraceWalkForTest(repo, stream)
			fixturekit.Must(t, err)
			chain, _, gaps := w.Delivery()
			if chain == nil || chain.Merge == nil || chain.Merge.Path != delivery.MergePath || chain.Merged != upstream {
				t.Fatalf("the trace's delivery %+v", chain)
			}
			for _, gap := range gaps {
				if gap.Link == delivery.PublicationPath || gap.Link == delivery.DeliveryPath {
					t.Fatalf("the trace of a merged workstream misses its publication: %+v", gap)
				}
			}
			if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, stream); apiErr == nil || apiErr.Code != api.Conflict {
				t.Fatalf("a delivered workstream was recorded merged again: %v", apiErr)
			}
		})
	}
}

func TestDependentOfAMergedBaseRebasesOntoUpstream(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Upstream(t)
	f.Stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	base, held := journey.SealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	cfg := f.S.About(repo)
	g, err := intake.FeatureWorkspaces(cfg, repo).Of(stream)
	fixturekit.Must(t, err)
	if selected, err := f.S.Intake().ResolveBase(ctx, cfg, repo, stream, g); err != nil || selected.Workstream != base {
		t.Fatalf("before the merge the dependent builds on %+v: %v", selected, err)
	}

	assembleForTest(t, f, repo, base)
	home := filepath.Dir(f.Clone)
	fixturekit.DemoGit(t, home, "-C", f.Clone, "push", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), held+":refs/heads/main")
	if _, apiErr := f.S.Delivery().MergedOutside(ctx, repo, base); apiErr != nil {
		t.Fatal(apiErr)
	}
	if changed, err := service.TestDependentBaseChanged(ctx, cfg, repo, stream); err != nil || !changed {
		t.Fatalf("the merged base did not ask the dependent to rebase: %v %v", changed, err)
	}
	selected, err := f.S.Intake().ResolveBase(ctx, cfg, repo, stream, g)
	if err != nil || selected != (seal.Base{Remote: "upstream", Branch: "main", Commit: held}) {
		t.Fatalf("the dependent of a merged base resolves %+v: %v", selected, err)
	}
}
