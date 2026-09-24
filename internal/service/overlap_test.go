package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// overlapEntities has the subsystems internal and cmd; internal.trace,
// internal.upload and internal.audit are part of internal, and legacy lies
// under internal/trace without being part of anything.
func overlapEntities() kb.Map {
	entity := func(id, path string, partOf ...string) kb.Entity {
		return kb.Entity{ID: id, Name: path, Aliases: []string{}, Paths: []string{path}, Owners: []string{}, PartOf: append([]string{}, partOf...)}
	}
	return kb.Map{Version: kb.Version, Entities: []kb.Entity{
		entity("internal", "internal"),
		entity("internal.trace", "internal/trace", "internal"),
		entity("internal.upload", "internal/upload", "internal"),
		entity("internal.audit", "internal/audit", "internal"),
		entity("legacy", "internal/trace/legacy"),
		entity("cmd", "cmd"),
	}}
}

// sealOf is a seal of one unit per footprint, resolved against the map.
func sealOf(t *testing.T, k int, m kb.Map, footprints ...[]string) seal.Seal {
	t.Helper()
	var p plan.Plan
	for i, names := range footprints {
		p.Units = append(p.Units, plan.Unit{ID: fmt.Sprintf("u%d", i+1), Footprint: names})
	}
	resolved, unresolved := seal.Take(p, m)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved %v", unresolved)
	}
	return seal.Seal{Seal: k, Footprints: resolved}
}

// Two sealed footprints overlap when both name an entity or when an entity's
// path patterns cover the other's; the advisory names the most specific
// shared entities, their top-level subsystems and the overlapping patterns.
func TestOverlapOfSealedFootprints(t *testing.T) {
	t.Parallel()
	m := overlapEntities()
	for _, tc := range []struct {
		name string
		a, b [][]string
		want footprintOverlap
	}{
		{"same entity", [][]string{{"internal.trace"}}, [][]string{{"internal.upload"}, {"internal.trace"}},
			footprintOverlap{Subsystems: []string{"internal"}, Entities: []string{"internal.trace"}, Paths: []string{"internal/trace"}}},
		{"a subsystem and one of its parts", [][]string{{"internal"}}, [][]string{{"internal.upload"}},
			footprintOverlap{Subsystems: []string{"internal"}, Entities: []string{"internal.upload"}, Paths: []string{"internal", "internal/upload"}}},
		{"nested paths of unrelated entities", [][]string{{"internal.trace"}}, [][]string{{"legacy"}},
			footprintOverlap{Subsystems: []string{"internal", "legacy"}, Entities: []string{"internal.trace", "legacy"}, Paths: []string{"internal/trace", "internal/trace/legacy"}}},
		{"top-level entity", [][]string{{"cmd"}}, [][]string{{"cmd"}, {"internal.audit"}},
			footprintOverlap{Subsystems: []string{"cmd"}, Entities: []string{"cmd"}, Paths: []string{"cmd"}}},
		{"parts of one subsystem that do not overlap", [][]string{{"internal.trace"}}, [][]string{{"internal.upload"}, {"internal.audit"}},
			footprintOverlap{Subsystems: []string{}, Paths: []string{}}},
		{"different subsystems", [][]string{{"cmd"}}, [][]string{{"internal.trace"}},
			footprintOverlap{Subsystems: []string{}, Paths: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := sealOf(t, 1, m, tc.a...), sealOf(t, 1, m, tc.b...)
			for _, got := range []footprintOverlap{overlapOf(a, b, m), overlapOf(b, a, m)} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("overlap %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

// projectOf is the configured project with another ID.
func projectOf(cfg *config.Config, id config.ProjectID) config.Project {
	p := cfg.Project
	p.ID = id
	return p
}

// overlapPlanted is a trace with workstreams planted building, delivered or
// paused on seals of one-unit plans.
type overlapPlanted struct {
	t       *testing.T
	opts    Options
	cfg     *config.Config
	clock   *fixedClock
	ticks   chan time.Time
	service *Service
	client  *Client
}

func (p *overlapPlanted) header(repo *trace.Repository, stream config.WorkstreamID, schema, id string, revision int) trace.Header {
	return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: stream, At: p.clock.Now(), Actor: ownerActor, Cause: "planted"}
}

// plant records seal k of the workstream, and plan revision k it seals, with
// one unit whose footprint names the entities.
func (p *overlapPlanted) plant(repo *trace.Repository, stream config.WorkstreamID, k int, names ...string) {
	t := p.t
	t.Helper()
	footprint, err := json.Marshal(names)
	must(t, err)
	graph := fmt.Sprintf(`{"version": 1, "units": [{"id": "work", "title": "Do the work", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestWork"}}], "depends_on": [], "footprint": %s}]}`+"\n", footprint)
	parsed, err := plan.Parse([]byte(graph))
	must(t, err)
	m, err := kb.Load(repo)
	must(t, err)
	footprints, unresolved := seal.Take(parsed, m)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved %v", unresolved)
	}
	content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: k, Round: 1, Revision: shed.Pin{Spec: 1, Plan: k}, SpecHash: seal.SpecHash(validSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(stream), Workspace: "/planted", Footprints: footprints})
	must(t, err)
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{
		{Header: p.header(repo, stream, "osmia.trace.document", plan.PlanDocument, k), Path: plan.PlanPath, Content: graph},
		{Header: p.header(repo, stream, "osmia.trace.document", seal.DocumentID, k), Path: seal.Path, Content: string(content)},
	}))
}

// feature moves the workstream's feature state to the given value.
func (p *overlapPlanted) feature(repo *trace.Repository, stream config.WorkstreamID, to string) {
	p.t.Helper()
	_, err := repo.SetFeatureState(context.Background(), p.header(repo, stream, "osmia.trace.transition", "planted-"+to, 1), to, "planted")
	must(p.t, err)
}

// run starts the service, lets it finish two passes and stops it.
func (p *overlapPlanted) run(check func()) {
	t := p.t
	t.Helper()
	s, err := Start(context.Background(), p.opts)
	must(t, err)
	p.service, p.client = s, NewClient(s.Socket())
	defer func() {
		p.client.Close()
		must(t, s.Close())
	}()
	for range 2 {
		select {
		case p.ticks <- p.clock.Now():
		case <-time.After(demoTimeout):
			t.Fatal("service did not finish its pass")
		}
	}
	check()
}

// advisories returns the sorted bodies of the workstream's overlap advisory
// events.
func (p *overlapPlanted) advisories(repo *trace.Repository, stream config.WorkstreamID) []string {
	p.t.Helper()
	entries, err := repo.Outbox(stream)
	must(p.t, err)
	var out []string
	for _, e := range entries {
		if e.Event.Kind == OverlapKind {
			out = append(out, e.Event.Body)
		}
	}
	slices.Sort(out)
	return out
}

func (p *overlapPlanted) status(stream config.WorkstreamID) []OverlapAdvisory {
	p.t.Helper()
	st, err := p.client.Status(context.Background(), stream)
	must(p.t, err)
	return st.Advisories
}

// Building workstreams of one project whose sealed footprints overlap each
// warn their chief of staff about the other once per pair of seals, across
// restarts; a new seal warns again, and status lists the advisories whose
// seals are current. Disjoint, paused and delivered workstreams, and those
// of another project, are not warned about.
func TestOverlapAdvisoriesWarnEachChiefOncePerSeal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ov-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	// The landing controller reads each building workstream's feature branch
	// from the clone, which has none.
	demoGit(t, home, "init", "-q", filepath.Join(home, "clone"))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	p := &overlapPlanted{t: t, opts: opts, cfg: cfg, clock: &fixedClock{now: demoStart}, ticks: make(chan time.Time)}
	p.opts.Reconciliation.Now, p.opts.Reconciliation.Ticks = p.clock.Now, p.ticks
	const (
		a         config.WorkstreamID = "w_00000000000000000000000000000001"
		b         config.WorkstreamID = "w_00000000000000000000000000000002"
		c         config.WorkstreamID = "w_00000000000000000000000000000003"
		paused    config.WorkstreamID = "w_00000000000000000000000000000004"
		delivered config.WorkstreamID = "w_00000000000000000000000000000005"
		elsewhere config.ProjectID    = "p_00000000000000000000000000000002"
	)
	owner := trace.Actor{Kind: "owner", ID: "local"}

	// Workstream a builds in internal.trace in both projects, b too in this
	// one; c builds in internal.upload. The paused and the delivered
	// workstreams build in internal.trace.
	for _, id := range []config.ProjectID{project, elsewhere} {
		repo, err := trace.Create(ctx, cfg.Root, projectOf(cfg, id), p.clock.Now(), owner)
		must(t, err)
		must(t, kb.Store(ctx, repo, overlapEntities(), p.clock.Now(), librarianActor, "planted"))
		streams := []config.WorkstreamID{a, b, c, paused, delivered}
		if id == elsewhere {
			streams = []config.WorkstreamID{a}
		}
		for _, stream := range streams {
			must(t, repo.CreateWorkstream(ctx, stream, p.clock.Now(), owner))
			footprint := "internal.trace"
			if stream == c {
				footprint = "internal.upload"
			}
			p.plant(repo, stream, 1, footprint)
			p.feature(repo, stream, BuildingState)
		}
		if id == project {
			_, err = repo.SetFeatureState(ctx, p.header(repo, delivered, "osmia.trace.transition", "planted-delivered", 1), DeliveredState, "planted")
			must(t, err)
		}
		must(t, repo.Close())
	}
	state, err := json.Marshal(runtime.State{Version: runtime.Version, Pauses: []runtime.Pause{{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: paused}, Mode: "soft", Source: "operator"}}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(cfg.Root.String(), "runtime.json"), state, 0600))

	message := func(other config.WorkstreamID, mine, theirs int, subsystems, entities string) string {
		return fmt.Sprintf("Workstream %s of this project builds in subsystem %s: both sealed footprints cover %s (seal %d of this workstream, seal %d of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.", other, subsystems, entities, mine, theirs)
	}
	advisory := func(other config.WorkstreamID, mine, theirs int, entities, paths []string) OverlapAdvisory {
		return OverlapAdvisory{Workstream: other, Seal: mine, OtherSeal: theirs, Subsystems: []string{"internal"}, Entities: entities, Paths: paths,
			Message: message(other, mine, theirs, "internal", strings.Join(entities, ", "))}
	}
	check := func(want map[config.WorkstreamID][]string, active map[config.WorkstreamID][]OverlapAdvisory) {
		t.Helper()
		repo := p.service.active.repository
		for _, stream := range []config.WorkstreamID{a, b, c, paused, delivered} {
			if got := p.advisories(repo, stream); !reflect.DeepEqual(got, slices.Sorted(slices.Values(want[stream]))) {
				t.Fatalf("advisories of %s:\n%q\nwant:\n%q", stream, got, want[stream])
			}
			got := p.status(stream)
			wantActive := active[stream]
			if wantActive == nil {
				wantActive = []OverlapAdvisory{}
			}
			if !reflect.DeepEqual(got, wantActive) {
				t.Fatalf("status advisories of %s:\n%+v\nwant:\n%+v", stream, got, wantActive)
			}
		}
		list, err := p.client.Statuses(context.Background())
		must(t, err)
		for _, st := range list.Workstreams {
			if len(st.Advisories) != len(active[st.Workstream]) {
				t.Fatalf("listed advisories of %s: %+v", st.Workstream, st.Advisories)
			}
		}
		other, err := trace.Open(cfg.Root, projectOf(cfg, elsewhere))
		must(t, err)
		defer other.Close()
		if got := p.advisories(other, a); len(got) != 0 {
			t.Fatalf("advisories in another project: %q", got)
		}
	}

	trace1 := []string{"internal/trace"}
	first := map[config.WorkstreamID][]string{a: {message(b, 1, 1, "internal", "internal.trace")}, b: {message(a, 1, 1, "internal", "internal.trace")}}
	firstActive := map[config.WorkstreamID][]OverlapAdvisory{a: {advisory(b, 1, 1, []string{"internal.trace"}, trace1)}, b: {advisory(a, 1, 1, []string{"internal.trace"}, trace1)}}
	p.run(func() { check(first, firstActive) })
	// A restart warns about nothing again.
	p.run(func() { check(first, firstActive) })

	// Seal 2 of a also builds in internal.upload: a is warned again about
	// b, and about c for the first time, and so are they.
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	p.plant(repo, a, 2, "internal.trace", "internal.upload")
	must(t, repo.Close())
	second := map[config.WorkstreamID][]string{
		a: {first[a][0], message(b, 2, 1, "internal", "internal.trace"), message(c, 2, 1, "internal", "internal.upload")},
		b: {first[b][0], message(a, 1, 2, "internal", "internal.trace")},
		c: {message(a, 1, 2, "internal", "internal.upload")},
	}
	upload := []string{"internal/upload"}
	p.run(func() {
		check(second, map[config.WorkstreamID][]OverlapAdvisory{
			a: {advisory(b, 2, 1, []string{"internal.trace"}, trace1), advisory(c, 2, 1, []string{"internal.upload"}, upload)},
			b: {advisory(a, 1, 2, []string{"internal.trace"}, trace1)},
			c: {advisory(a, 1, 2, []string{"internal.upload"}, upload)},
		})
	})

	// Seal 3 of a builds in internal.audit alone: nothing overlaps, no
	// advisory is active and none is sent.
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	p.plant(repo, a, 3, "internal.audit")
	must(t, repo.Close())
	p.run(func() { check(second, nil) })
}
