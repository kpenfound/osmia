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
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

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
			a, b := intakefixture.SealOf(t, 1, m, tc.a...), intakefixture.SealOf(t, 1, m, tc.b...)
			for _, got := range []footprintOverlap{overlapOf(a, b, m), overlapOf(b, a, m)} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("overlap %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func projectOf(cfg *config.Config, id config.ProjectID) config.Project {
	p := cfg.Project
	p.ID = id
	return p
}

type overlapPlanted struct {
	t       *testing.T
	opts    Options
	cfg     *config.Config
	clock   *fixturekit.FixedClock
	ticks   chan time.Time
	service *Service
	client  *Client
}

func (p *overlapPlanted) header(repo *trace.Repository, stream config.WorkstreamID, schema, id string, revision int) trace.Header {
	return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: stream, At: p.clock.Now(), Actor: ownerActor, Cause: "planted"}
}

func (p *overlapPlanted) plant(repo *trace.Repository, stream config.WorkstreamID, k int, names ...string) {
	t := p.t
	t.Helper()
	footprint, err := json.Marshal(names)
	fixturekit.Must(t, err)
	graph := fmt.Sprintf(`{"version": 1, "units": [{"id": "work", "title": "Do the work", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestWork"}}], "depends_on": [], "footprint": %s}]}`+"\n", footprint)
	parsed, err := plan.Parse([]byte(graph))
	fixturekit.Must(t, err)
	m, err := kb.Load(repo)
	fixturekit.Must(t, err)
	footprints, unresolved := seal.Take(parsed, m)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved %v", unresolved)
	}
	content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: k, Round: 1, Revision: shed.Pin{Spec: 1, Plan: k}, SpecHash: seal.SpecHash(fixturekit.ValidSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(stream), Workspace: "/planted", Footprints: footprints})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{
		{Header: p.header(repo, stream, "osmia.trace.document", plan.PlanDocument, k), Path: plan.PlanPath, Content: graph},
		{Header: p.header(repo, stream, "osmia.trace.document", seal.DocumentID, k), Path: seal.Path, Content: string(content)},
	}))
}

func (p *overlapPlanted) feature(repo *trace.Repository, stream config.WorkstreamID, to string) {
	p.t.Helper()
	_, err := repo.SetFeatureState(context.Background(), p.header(repo, stream, "osmia.trace.transition", "planted-"+to, 1), to, "planted")
	fixturekit.Must(p.t, err)
}

func (p *overlapPlanted) run(check func()) {
	t := p.t
	t.Helper()
	s, err := Start(context.Background(), p.opts)
	fixturekit.Must(t, err)
	p.service, p.client = s, NewClient(s.Socket())
	defer func() {
		p.client.Close()
		fixturekit.Must(t, s.Close())
	}()
	for range 2 {
		select {
		case p.ticks <- p.clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("service did not finish its pass")
		}
	}
	check()
}

func (p *overlapPlanted) advisories(repo *trace.Repository, stream config.WorkstreamID) []string {
	p.t.Helper()
	entries, err := repo.Outbox(stream)
	fixturekit.Must(p.t, err)
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
	fixturekit.Must(p.t, err)
	return st.Advisories
}

func TestOverlapAdvisoriesWarnEachChiefOncePerSeal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ov-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	fixturekit.DemoGit(t, home, "init", "-q", filepath.Join(home, "clone"))
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	p := &overlapPlanted{t: t, opts: opts, cfg: cfg, clock: &fixturekit.FixedClock{At: fixturekit.DemoStart}, ticks: make(chan time.Time)}
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

	for _, id := range []config.ProjectID{fixturekit.Project, elsewhere} {
		repo, err := trace.Create(ctx, cfg.Root, projectOf(cfg, id), p.clock.Now(), owner)
		fixturekit.Must(t, err)
		fixturekit.Must(t, kb.Store(ctx, repo, overlapEntities(), p.clock.Now(), ownerActor, "planted"))
		streams := []config.WorkstreamID{a, b, c, paused, delivered}
		if id == elsewhere {
			streams = []config.WorkstreamID{a}
		}
		for _, stream := range streams {
			fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, p.clock.Now(), owner))
			footprint := "internal.trace"
			if stream == c {
				footprint = "internal.upload"
			}
			p.plant(repo, stream, 1, footprint)
			p.feature(repo, stream, BuildingState)
		}
		if id == fixturekit.Project {
			_, err = repo.SetFeatureState(ctx, p.header(repo, delivered, "osmia.trace.transition", "planted-delivered", 1), DeliveredState, "planted")
			fixturekit.Must(t, err)
		}
		fixturekit.Must(t, repo.Close())
	}
	state, err := json.Marshal(runtime.State{Version: runtime.Version, Pauses: []runtime.Pause{{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: paused}, Mode: "soft", Source: runtime.PauseOwner, Reason: "test", SetAt: time.Now().UTC()}}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(cfg.Root.String(), "runtime.json"), state, 0600))

	message := func(other config.WorkstreamID, mine, theirs int, subsystems, entities string) string {
		return fmt.Sprintf("Workstream %s of this project builds in subsystem %s: both sealed footprints cover %s (seal %d of this workstream, seal %d of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.", other, subsystems, entities, mine, theirs)
	}
	advisory := func(other config.WorkstreamID, mine, theirs int, entities, paths []string) OverlapAdvisory {
		return OverlapAdvisory{Workstream: other, Seal: mine, OtherSeal: theirs, Subsystems: []string{"internal"}, Entities: entities, Paths: paths,
			Message: message(other, mine, theirs, "internal", strings.Join(entities, ", "))}
	}
	check := func(want map[config.WorkstreamID][]string, active map[config.WorkstreamID][]OverlapAdvisory) {
		t.Helper()
		repo := p.service.sole().repository
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
		fixturekit.Must(t, err)
		for _, st := range list.Workstreams {
			if len(st.Advisories) != len(active[st.Workstream]) {
				t.Fatalf("listed advisories of %s: %+v", st.Workstream, st.Advisories)
			}
		}
		other, err := trace.Open(cfg.Root, projectOf(cfg, elsewhere))
		fixturekit.Must(t, err)
		defer other.Close()
		if got := p.advisories(other, a); len(got) != 0 {
			t.Fatalf("advisories in another project: %q", got)
		}
	}

	trace1 := []string{"internal/trace"}
	first := map[config.WorkstreamID][]string{a: {message(b, 1, 1, "internal", "internal.trace")}, b: {message(a, 1, 1, "internal", "internal.trace")}}
	firstActive := map[config.WorkstreamID][]OverlapAdvisory{a: {advisory(b, 1, 1, []string{"internal.trace"}, trace1)}, b: {advisory(a, 1, 1, []string{"internal.trace"}, trace1)}}
	p.run(func() { check(first, firstActive) })
	p.run(func() { check(first, firstActive) })

	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	p.plant(repo, a, 2, "internal.trace", "internal.upload")
	fixturekit.Must(t, repo.Close())
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

	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	p.plant(repo, a, 3, "internal.audit")
	fixturekit.Must(t, repo.Close())
	p.run(func() { check(second, nil) })
}

func TestOverlapAdvisoryAboutAGoneWorkstreamIsInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		a config.WorkstreamID = "w_00000000000000000000000000000001"
		b config.WorkstreamID = "w_00000000000000000000000000000002"
	)
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "osmia"), "")
	fixturekit.Must(t, err)
	p := &overlapPlanted{t: t, clock: &fixturekit.FixedClock{At: fixturekit.DemoStart}}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	planted := config.Project{ID: fixturekit.Project, Clone: t.TempDir()}
	repo, err := trace.Create(ctx, root, planted, p.clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, kb.Store(ctx, repo, overlapEntities(), p.clock.Now(), ownerActor, "planted"))
	for _, stream := range []config.WorkstreamID{a, b} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, stream, p.clock.Now(), owner))
		p.plant(repo, stream, 1, "internal.trace")
		p.feature(repo, stream, BuildingState)
	}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: p.header(repo, a, "osmia.trace.transition", "overlap-"+string(b)+"-1-1", 1), Subject: overlapSubject(b), To: "seals-1-1", Reason: "planted"}})
	fixturekit.Must(t, err)
	advisories := func(repo *trace.Repository) []OverlapAdvisory {
		t.Helper()
		subjects, err := repo.WorkflowStates(a)
		fixturekit.Must(t, err)
		out, err := overlapAdvisories(repo, a, subjects)
		fixturekit.Must(t, err)
		return out
	}
	if got := advisories(repo); len(got) != 1 || got[0].Workstream != b {
		t.Fatalf("active advisories: %+v", got)
	}
	fixturekit.Must(t, repo.Close())

	gone, err := root.WorkstreamTrace(planted.ID, b)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.RemoveAll(gone))
	repo, err = trace.Open(root, planted)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	if got := advisories(repo); len(got) != 0 {
		t.Fatalf("advisories about a gone workstream: %+v", got)
	}
}
