package lifecycle_test

import (
	"context"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func sealDependent(t *testing.T, f *shedFixture, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	return intakefixture.SealDependent(t, f.s.intake(), repo, backend, base, files)
}

func TestParkedWorkstreamMovesOntoUpstreamOnceItsAbandonedBaseIsIntegrated(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			if backend == config.WorkspacesJujutsu {
				intakefixture.RequireJJ(t)
			}
			f := newDebateFixture(t, 1, 1)
			f.upstream(t)
			f.stop(t)
			ctx := context.Background()
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			base, held := sealDependent(t, f, repo, backend, "", map[string]string{"base.go": "base\n"})
			stream, _ := sealDependent(t, f, repo, backend, base, map[string]string{"dependent.go": "dependent\n"})
			cfg := f.s.about(repo)
			g, err := intake.FeatureWorkspaces(cfg, repo).Of(stream)
			fixturekit.Must(t, err)

			if _, apiFault := f.s.lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, base); apiFault == nil || apiFault.Code != api.Conflict {
				t.Fatalf("an unparked workstream moved onto upstream: %v", apiFault)
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.s.now(), Actor: intake.OwnerActor, Cause: "owner"}
			_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Merged out of factory")
			fixturekit.Must(t, err)
			fixturekit.Must(t, f.s.lifecycle().BaseWaitPass(ctx, repo))
			entry := parkedBaseEntry(t, repo, stream)
			if entry.Kind != api.InboxBase || !slices.Equal(entry.Options, []string{api.BaseDecisionUpstream}) || entry.Answer.Path != api.Prefix+"/base/"+string(stream)+"/upstream" || entry.Answer.Body["base"] != string(base) {
				t.Fatalf("the parked base's inbox entry %+v", entry)
			}

			if _, apiFault := f.s.lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, stream); apiFault == nil || apiFault.Code != api.Conflict {
				t.Fatalf("moved onto upstream for another base: %v", apiFault)
			}
			if _, apiFault := f.s.lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, base); apiFault == nil || apiFault.Code != api.Conflict {
				t.Fatalf("moved onto upstream lacking the base's changes: %v", apiFault)
			}
			advanceUpstream(t, f, map[string]string{"base.go": "upstream's own\n"})
			if _, apiFault := f.s.lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, base); apiFault == nil || apiFault.Code != api.Conflict {
				t.Fatalf("moved onto upstream conflicting with the base's changes: %v", apiFault)
			}
			if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || !waiting {
				t.Fatalf("a refused decision released the park: %v %v", waiting, err)
			}

			upstream := advanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
			out, apiFault := f.s.lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, base)
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			want := seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}
			if out.Workstream != stream || out.Base != base || out.Integrated != held || out.Upstream != want {
				t.Fatalf("the decision %+v", out)
			}
			if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || waiting {
				t.Fatalf("the decision left the park: %v %v", waiting, err)
			}
			released := intakefixture.TransitionByID(t, repo, stream, "base-wait-2")
			if released.Actor != intake.OwnerActor || released.From != "waiting" || released.To != "available" {
				t.Fatalf("the release %+v", released)
			}
			if observed, _, err := lifecycle.BaseObservationAt(repo, stream); err != nil || observed.Base != want || observed.Unavailable {
				t.Fatalf("the base observation %+v: %v", observed, err)
			}
			fixturekit.Must(t, f.s.lifecycle().BaseWaitPass(ctx, repo))
			if state, err := repo.Workflow(stream, lifecycle.BaseWaitSubject); err != nil || state.Value != "available" || state.Version != 2 {
				t.Fatalf("the base wait after its next pass %+v: %v", state, err)
			}
			if parkedBaseEntries(t, repo, stream) != 0 {
				t.Fatal("the released park stayed in the inbox")
			}
			if changed, err := lifecycle.DependentBaseChanged(ctx, cfg, repo, stream); err != nil || !changed {
				t.Fatalf("no drift rebase is due onto upstream: %v %v", changed, err)
			}

			h.Workstream, h.ID = stream, "building"
			_, err = repo.SetFeatureState(ctx, h, intake.BuildingState, "Units are recorded")
			fixturekit.Must(t, err)
			d := drifter{&foreman{masons: newMasonController(f.s, repo)}}
			op := requestDrift(t, d, stream)
			if result := settleOperation(t, f.s, repo, stream, op, d); result.Outcome != "succeeded" {
				t.Fatalf("the drift rebase %+v", result)
			}
			tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream))
			fixturekit.Must(t, err)
			if parentOf(t, f, tip) != upstream || fileAt(t, f, tip, "dependent.go") != "dependent\n" || fileAt(t, f, tip, "base.go") != "base\n" {
				t.Fatalf("the feature branch is at %s, not its own commit replayed onto %s", tip, upstream)
			}
			if sealed, _, _, err := seal.Latest(repo, stream); err != nil || sealed.Base != want {
				t.Fatalf("the seal's base %+v: %v", sealed.Base, err)
			}
		})
	}
}

func TestBaseUpstreamRefusesAWorkstreamThatIsNotParked(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "base-upstream-")
	_, c := start(t, opts)
	_, err := c.BaseUpstream(context.Background(), fixturekit.Stream, fixturekit.Quiet)
	fixturekit.AssertCode(t, err, api.Conflict)
}

func parkedBaseEntries(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	statuses, err := repo.Statuses()
	fixturekit.Must(t, err)
	n := 0
	for _, w := range statuses {
		if w.Workstream != stream {
			continue
		}
		if _, open, err := service.TestBaseEntry(repo, w); err != nil {
			t.Fatal(err)
		} else if open {
			n++
		}
	}
	return n
}

func parkedBaseEntry(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) api.InboxEntry {
	t.Helper()
	statuses, err := repo.Statuses()
	fixturekit.Must(t, err)
	for _, w := range statuses {
		if w.Workstream != stream {
			continue
		}
		e, open, err := service.TestBaseEntry(repo, w)
		fixturekit.Must(t, err)
		if open {
			return e
		}
	}
	t.Fatal("no inbox entry for the parked base")
	return api.InboxEntry{}
}
