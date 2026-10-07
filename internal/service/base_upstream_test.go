package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// sealDependent creates a workstream on the backend, building on base when
// it is not empty, seals it and commits the files onto its feature branch. It
// returns the workstream and its feature branch tip.
func sealDependent(t *testing.T, f *shedFixture, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repo.CreateWorkstreamOn(ctx, stream, backend, f.s.now(), ownerActor))
	if base != "" {
		_, err = repo.SetWorkstreamBase(ctx, stream, base, 0, f.s.now())
		must(t, err)
	}
	header := trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "owner-ratification"}
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	must(t, err)
	var docs []trace.Document
	for _, d := range []struct{ id, path, content string }{{"spec", "spec.md", validSpec}, {"plan", "plan.json", validPlan}, {shed.RatificationDocumentID(1), shed.RatificationPath(1), string(ratified)}} {
		h := header
		h.ID = d.id
		docs = append(docs, trace.Document{Header: h, Path: d.path, Content: d.content})
	}
	must(t, repo.RecordDocuments(ctx, docs))
	header.Schema, header.ID = "osmia.trace.transition", "in-shed"
	_, err = repo.SetFeatureState(ctx, header, InShedState, "Owner approved the draft")
	must(t, err)
	z := &sealer{s: f.s, repository: repo}
	must(t, z.Pass(ctx))
	if result, err := z.Apply(ctx, actionOperation(t, repo, stream, SealAction)); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("sealing %+v: %v", result, err)
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	must(t, err)
	g, err := featureWorkspaces(f.s.about(repo), repo).of(stream)
	must(t, err)
	w, found, err := g.Workspace(ctx, string(stream))
	must(t, err)
	if !found {
		t.Fatal("no feature workspace")
	}
	for name, content := range files {
		must(t, os.WriteFile(filepath.Join(w.Path, name), []byte(content), 0600))
	}
	commit, err := g.Snapshot(ctx, w, sealed.Base.Commit)
	must(t, err)
	tip, _, err := g.Branch(ctx, sealed.Branch)
	must(t, err)
	if tip != commit {
		must(t, g.Advance(ctx, w, tip, commit))
	}
	return stream, commit
}

// A workstream parked because its base was abandoned shows in the inbox with
// the decision to continue from upstream. The decision is refused for another
// base and while upstream lacks or conflicts with the base's changes the
// workstream holds. Once upstream holds them as other commits, it releases the
// park as an owner transition and pins the base observation to upstream, and
// the next drift rebase replays only the workstream's own commit onto
// upstream and moves its seal there.
func TestParkedWorkstreamMovesOntoUpstreamOnceItsAbandonedBaseIsIntegrated(t *testing.T) {
	t.Parallel()
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
			base, held := sealDependent(t, f, repo, backend, "", map[string]string{"base.go": "base\n"})
			stream, _ := sealDependent(t, f, repo, backend, base, map[string]string{"dependent.go": "dependent\n"})
			cfg := f.s.about(repo)
			g, err := featureWorkspaces(cfg, repo).of(stream)
			must(t, err)

			if _, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, base); api == nil || api.Code != Conflict {
				t.Fatalf("an unparked workstream moved onto upstream: %v", api)
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.s.now(), Actor: ownerActor, Cause: "owner"}
			_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Merged out of factory")
			must(t, err)
			must(t, f.s.baseWaitPass(ctx, repo))
			entry := parkedBaseEntry(t, repo, stream)
			if entry.Kind != InboxBase || !slices.Equal(entry.Options, []string{BaseDecisionUpstream}) || entry.Answer.Path != Prefix+"/base/"+string(stream)+"/upstream" || entry.Answer.Body["base"] != string(base) {
				t.Fatalf("the parked base's inbox entry %+v", entry)
			}

			if _, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, stream); api == nil || api.Code != Conflict {
				t.Fatalf("moved onto upstream for another base: %v", api)
			}
			if _, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, base); api == nil || api.Code != Conflict {
				t.Fatalf("moved onto upstream lacking the base's changes: %v", api)
			}
			advanceUpstream(t, f, map[string]string{"base.go": "upstream's own\n"})
			if _, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, base); api == nil || api.Code != Conflict {
				t.Fatalf("moved onto upstream conflicting with the base's changes: %v", api)
			}
			if waiting, err := baseWaiting(repo, stream); err != nil || !waiting {
				t.Fatalf("a refused decision released the park: %v %v", waiting, err)
			}

			upstream := advanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
			out, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, base)
			if api != nil {
				t.Fatal(api)
			}
			want := seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}
			if out.Workstream != stream || out.Base != base || out.Integrated != held || out.Upstream != want {
				t.Fatalf("the decision %+v", out)
			}
			if waiting, err := baseWaiting(repo, stream); err != nil || waiting {
				t.Fatalf("the decision left the park: %v %v", waiting, err)
			}
			released := transitionByID(t, repo, stream, "base-wait-2")
			if released.Actor != ownerActor || released.From != "waiting" || released.To != "available" {
				t.Fatalf("the release %+v", released)
			}
			if observed, _, err := baseObservationAt(repo, stream); err != nil || observed.Base != want || observed.Unavailable {
				t.Fatalf("the base observation %+v: %v", observed, err)
			}
			must(t, f.s.baseWaitPass(ctx, repo))
			if state, err := repo.Workflow(stream, baseWaitSubject); err != nil || state.Value != "available" || state.Version != 2 {
				t.Fatalf("the base wait after its next pass %+v: %v", state, err)
			}
			if parkedBaseEntries(t, repo, stream) != 0 {
				t.Fatal("the released park stayed in the inbox")
			}
			if changed, err := dependentBaseChanged(ctx, cfg, repo, stream); err != nil || !changed {
				t.Fatalf("no drift rebase is due onto upstream: %v %v", changed, err)
			}

			h.Workstream, h.ID = stream, "building"
			_, err = repo.SetFeatureState(ctx, h, BuildingState, "Units are recorded")
			must(t, err)
			d := drifter{&foreman{masons: newMasonController(f.s, repo)}}
			op := requestDrift(t, d, stream)
			if result := settleOperation(t, f.s, repo, stream, op, d); result.Outcome != "succeeded" {
				t.Fatalf("the drift rebase %+v", result)
			}
			tip, _, err := g.Branch(ctx, featureBranch(stream))
			must(t, err)
			if parentOf(t, f, tip) != upstream || fileAt(t, f, tip, "dependent.go") != "dependent\n" || fileAt(t, f, tip, "base.go") != "base\n" {
				t.Fatalf("the feature branch is at %s, not its own commit replayed onto %s", tip, upstream)
			}
			if sealed, _, _, err := seal.Latest(repo, stream); err != nil || sealed.Base != want {
				t.Fatalf("the seal's base %+v: %v", sealed.Base, err)
			}
		})
	}
}

// Moving onto upstream is answered over the API, and refused for a
// workstream that is not parked on its base.
func TestBaseUpstreamRefusesAWorkstreamThatIsNotParked(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "base-upstream-")
	_, c := start(t, opts)
	_, err := c.BaseUpstream(context.Background(), stream, quiet)
	assertCode(t, err, Conflict)
}

// parkedBaseEntries counts the inbox entries of the workstream's parked base.
func parkedBaseEntries(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	statuses, err := repo.Statuses()
	must(t, err)
	n := 0
	for _, w := range statuses {
		if w.Workstream != stream {
			continue
		}
		if _, open, err := baseEntry(repo, w); err != nil {
			t.Fatal(err)
		} else if open {
			n++
		}
	}
	return n
}

// parkedBaseEntry returns the inbox entry of the workstream's parked base.
func parkedBaseEntry(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) InboxEntry {
	t.Helper()
	statuses, err := repo.Statuses()
	must(t, err)
	for _, w := range statuses {
		if w.Workstream != stream {
			continue
		}
		e, open, err := baseEntry(repo, w)
		must(t, err)
		if open {
			return e
		}
	}
	t.Fatal("no inbox entry for the parked base")
	return InboxEntry{}
}
