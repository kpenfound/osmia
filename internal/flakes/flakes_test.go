package flakes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	origin  config.WorkstreamID = "w_00000000000000000000000000000001"
	groupA                      = "dag+check://?check=test&go-module=.&go-package=internal/a"
	groupB                      = "dag+check://?check=test&go-module=.&go-package=internal/b"
)

var epoch = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t     *testing.T
	root  config.Root
	p     config.Project
	repo  *trace.Repository
	store *Store
	clock time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	if err := os.Mkdir(p.Clone, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := trace.Create(context.Background(), root, p, epoch, trace.Actor{Kind: "owner", ID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, p: p, repo: repo, clock: epoch}
	t.Cleanup(func() { f.repo.Close() })
	f.store = f.newStore(repo)
	return f
}

func (f *fixture) newStore(repo *trace.Repository) *Store {
	return &Store{Repository: repo, Now: func() time.Time { f.clock = f.clock.Add(time.Minute); return f.clock }}
}

func (f *fixture) restart() {
	f.t.Helper()
	f.repo.Close()
	repo, err := trace.Open(f.root, f.p)
	if err != nil {
		f.t.Fatal(err)
	}
	f.repo = repo
	f.store = f.newStore(repo)
}

func unitRun(n string, links ...string) OccurrenceInput {
	return OccurrenceInput{Run: "units/u1/checks-" + n + ".json", Source: Source{Workstream: origin, Unit: "u1"}, Links: links,
		Evidence: Evidence{Engine: EngineLocal, Flaky: links, Parallelism: 4, CoRun: []string{"lint"}, Diagnostics: []string{"--- FAIL: TestOne [passed on rerun]"}, OutputPath: "units/u1/checks-" + n + "-output.txt"}}
}

func in(group string, o OccurrenceInput) OccurrenceInput {
	o.Group = group
	return o
}

func TestRecordingAnOccurrenceTwiceStoresItOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, added, err := f.store.Record(ctx, in(groupA, unitRun("1", groupA+"&go-test=TestOne")))
	if err != nil || !added {
		t.Fatalf("first record: %+v added %v: %v", first, added, err)
	}
	again, added, err := f.store.Record(ctx, in(groupA, unitRun("1", groupA+"&go-test=TestOne", groupA+"&go-test=Other")))
	if err != nil || added {
		t.Fatalf("second record: added %v: %v", added, err)
	}
	if again.ID != first.ID || len(again.Links) != 1 || !again.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("second record changed the occurrence: %+v, was %+v", again, first)
	}
	g, found, err := f.store.Group(groupA)
	if err != nil || !found || len(g.Occurrences) != 1 || g.Occurrences[0].Decision.State != Pending {
		t.Fatalf("group %+v: found %v, %v", g, found, err)
	}
	f.restart()
	if g, _, err = f.store.Group(groupA); err != nil || len(g.Occurrences) != 1 {
		t.Fatalf("after a restart: %+v, %v", g, err)
	}
	if _, added, err = f.store.Record(ctx, in(groupA, unitRun("1", groupA+"&go-test=TestOne"))); err != nil || added {
		t.Fatalf("recording after a restart: added %v, %v", added, err)
	}
}

func TestOneRunInTwoGroupsRecordsTwoOccurrences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	run := unitRun("1", groupA+"&go-test=TestOne", groupB+"&go-test=TestOne")
	for _, group := range []string{groupA, groupB} {
		o := in(group, run)
		o.Links = []string{group + "&go-test=TestOne"}
		if _, added, err := f.store.Record(ctx, o); err != nil || !added {
			t.Fatalf("%s: added %v, %v", group, added, err)
		}
	}
	all, err := f.store.Occurrences()
	if err != nil || len(all) != 2 || all[0].ID == all[1].ID || all[0].Run != all[1].Run {
		t.Fatalf("occurrences %+v, %v", all, err)
	}
	groups, err := f.store.Groups()
	if err != nil || len(groups) != 2 || groups[0].Group != groupA || groups[1].Group != groupB {
		t.Fatalf("groups %+v, %v", groups, err)
	}
}

func TestSiblingOccurrencesJoinOneGroupsHistoryInOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i, test := range []string{"TestOne", "TestTwo", "TestOne"} {
		if _, added, err := f.store.Record(ctx, in(groupA, unitRun(string(rune('1'+i)), groupA+"&go-test="+test))); err != nil || !added {
			t.Fatalf("run %d: added %v, %v", i, added, err)
		}
	}
	g, _, err := f.store.Group(groupA)
	if err != nil || len(g.Occurrences) != 3 {
		t.Fatalf("group %+v, %v", g, err)
	}
	for i, test := range []string{"TestOne", "TestTwo", "TestOne"} {
		if got := g.Occurrences[i].Links[0]; got != groupA+"&go-test="+test {
			t.Errorf("occurrence %d is %s", i, got)
		}
	}
	if h := g.History(g.Occurrences[2].Run); len(h) != 2 || h[0].Run != g.Occurrences[0].Run || h[1].Run != g.Occurrences[1].Run {
		t.Fatalf("history of the third: %+v", h)
	}
	if h := g.History(g.Occurrences[0].Run); len(h) != 0 {
		t.Fatalf("history of the first: %+v", h)
	}
}

func TestRecordRefusesAnIncompleteOccurrence(t *testing.T) {
	f := newFixture(t)
	for name, o := range map[string]OccurrenceInput{
		"no run":   in(groupA, OccurrenceInput{Source: Source{Workstream: origin}, Links: []string{"l"}}),
		"no links": in(groupA, OccurrenceInput{Run: "r", Source: Source{Workstream: origin}}),
		"no group": {Run: "r", Source: Source{Workstream: origin}, Links: []string{"l"}},
	} {
		if _, _, err := f.store.Record(context.Background(), o); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
}

func recordOne(t *testing.T, f *fixture, group, n string) Occurrence {
	t.Helper()
	o, _, err := f.store.Record(context.Background(), in(group, unitRun(n, group+"&go-test=TestOne")))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestADecisionOnceSetIsReturnedUnchanged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	opened, joined, declined := recordOne(t, f, groupA, "1"), recordOne(t, f, groupA, "2"), recordOne(t, f, groupB, "3")

	reserved, err := f.store.Reserve(ctx, groupA, opened.Run)
	if err != nil || reserved.State != Opening || reserved.Key != HandInKey(project, groupA, 0) || reserved.Generation != 0 || reserved.Workstream == "" {
		t.Fatalf("reserve: %+v, %v", reserved, err)
	}
	if again, err := f.store.Reserve(ctx, groupA, opened.Run); err != nil || again != reserved {
		t.Fatalf("a repeated reservation changed: %+v, %v", again, err)
	}
	done, err := f.store.Opened(ctx, groupA, opened.Run, "")
	if err != nil || done.State != Opened || done.Workstream != reserved.Workstream || done.Key != reserved.Key {
		t.Fatalf("opened: %+v, %v", done, err)
	}
	for name, attempt := range map[string]func() (Decision, error){
		"reserve": func() (Decision, error) { return f.store.Reserve(ctx, groupA, opened.Run) },
		"opened":  func() (Decision, error) { return f.store.Opened(ctx, groupA, opened.Run, "w_other") },
		"join":    func() (Decision, error) { return f.store.Join(ctx, groupA, opened.Run, "w_other") },
		"decline": func() (Decision, error) { return f.store.Decline(ctx, groupA, opened.Run, "no") },
	} {
		if got, err := attempt(); err != nil || got != done {
			t.Errorf("%s of an opened occurrence: %+v, %v; want %+v", name, got, err, done)
		}
	}

	j, err := f.store.Reserve(ctx, groupA, joined.Run)
	if err != nil || j.State != Joined || j.Workstream != done.Workstream {
		t.Fatalf("reserve into an open group: %+v, %v", j, err)
	}
	if got, err := f.store.Decline(ctx, groupA, joined.Run, "late"); err != nil || got != j {
		t.Fatalf("decline of a joined occurrence: %+v, %v", got, err)
	}

	d, err := f.store.Decline(ctx, groupB, declined.Run, " the test was added by this unit ")
	if err != nil || d.State != Declined || d.Reason != "the test was added by this unit" {
		t.Fatalf("decline: %+v, %v", d, err)
	}
	for name, attempt := range map[string]func() (Decision, error){
		"reserve": func() (Decision, error) { return f.store.Reserve(ctx, groupB, declined.Run) },
		"join":    func() (Decision, error) { return f.store.Join(ctx, groupB, declined.Run, "w_other") },
		"decline": func() (Decision, error) { return f.store.Decline(ctx, groupB, declined.Run, "other reason") },
	} {
		if got, err := attempt(); err != nil || got != d {
			t.Errorf("%s of a declined occurrence: %+v, %v; want %+v", name, got, err, d)
		}
	}
	if _, err := f.store.Decline(ctx, groupB, "units/u1/checks-9.json", "reason"); err == nil {
		t.Error("declined an unknown occurrence")
	}

	f.restart()
	for run, want := range map[string]Decision{opened.Run: done, joined.Run: j} {
		g, _, _ := f.store.Group(groupA)
		if o, _ := g.Find(run); o.Decision != want {
			t.Errorf("after a restart %s: %+v, want %+v", run, o.Decision, want)
		}
	}
	if g, _, _ := f.store.Group(groupB); g.Occurrences[0].Decision != d {
		t.Errorf("after a restart the decline is %+v", g.Occurrences[0].Decision)
	}
}

func TestDecliningNeedsAReasonAndJoiningAWorkstream(t *testing.T) {
	f := newFixture(t)
	o := recordOne(t, f, groupA, "1")
	if _, err := f.store.Decline(context.Background(), groupA, o.Run, "  "); err == nil {
		t.Error("declined without a reason")
	}
	if _, err := f.store.Join(context.Background(), groupA, o.Run, ""); err == nil {
		t.Error("joined without a workstream")
	}
	if _, err := f.store.Opened(context.Background(), groupA, o.Run, ""); err == nil {
		t.Error("completed an opening that was never reserved")
	}
	if g, _, _ := f.store.Group(groupA); g.Occurrences[0].Decision.State != Pending {
		t.Errorf("decision %+v", g.Occurrences[0].Decision)
	}
}

func TestClosingTheWorkstreamAdvancesTheGeneration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, second := recordOne(t, f, groupA, "1"), recordOne(t, f, groupA, "2")
	reserved, err := f.store.Reserve(ctx, groupA, first.Run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Opened(ctx, groupA, first.Run, ""); err != nil {
		t.Fatal(err)
	}
	if closed, err := f.store.CloseWorkstream(ctx, "w_00000000000000000000000000000099"); err != nil || len(closed) != 0 {
		t.Fatalf("closing a stranger: %v, %v", closed, err)
	}
	if g, _, _ := f.store.Group(groupA); g.Generation != 0 || g.Open == nil || g.Open.Workstream != reserved.Workstream {
		t.Fatalf("group before closing: %+v", g)
	}

	closed, err := f.store.CloseWorkstream(ctx, reserved.Workstream)
	if err != nil || len(closed) != 1 || closed[0] != groupA {
		t.Fatalf("closing: %v, %v", closed, err)
	}
	if again, err := f.store.CloseWorkstream(ctx, reserved.Workstream); err != nil || len(again) != 0 {
		t.Fatalf("closing twice: %v, %v", again, err)
	}
	g, _, _ := f.store.Group(groupA)
	if g.Generation != 1 || g.Open != nil || len(g.Closed) != 1 || g.Closed[0].Workstream != reserved.Workstream {
		t.Fatalf("group after closing: %+v", g)
	}
	if o, _ := g.Find(first.Run); o.Decision.State != Opened || o.Decision.Key != reserved.Key || o.Decision.Workstream != reserved.Workstream || o.Decision.Generation != 0 {
		t.Fatalf("opened occurrence: %+v", o.Decision)
	}
	if again, err := f.store.Reserve(ctx, groupA, first.Run); err != nil || again.Key != reserved.Key || again.State != Opened {
		t.Fatalf("replaying the open of a closed workstream: %+v, %v", again, err)
	}
	next, err := f.store.Reserve(ctx, groupA, second.Run)
	if err != nil || next.State != Opening || next.Generation != 1 || next.Key != HandInKey(project, groupA, 1) || next.Key == reserved.Key || next.Workstream == reserved.Workstream {
		t.Fatalf("occurrence after closing: %+v, %v", next, err)
	}
	third := recordOne(t, f, groupA, "3")
	if d, err := f.store.Reserve(ctx, groupA, third.Run); err != nil || d.State != Opening || d.Key != next.Key {
		t.Fatalf("third occurrence: %+v, %v", d, err)
	}
	f.restart()
	if g, _, _ := f.store.Group(groupA); g.Generation != 1 || len(g.Occurrences) != 3 {
		t.Fatalf("after a restart: %+v", g)
	}
}

func TestAnOpeningLeftByAnInterruptionStillClosesAndKeepsItsKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := recordOne(t, f, groupA, "1")
	reserved, err := f.store.Reserve(ctx, groupA, o.Run)
	if err != nil {
		t.Fatal(err)
	}
	f.restart()
	if closed, err := f.store.CloseWorkstream(ctx, reserved.Workstream); err != nil || len(closed) != 1 {
		t.Fatalf("closing: %v, %v", closed, err)
	}
	done, err := f.store.Opened(ctx, groupA, o.Run, "")
	if err != nil || done.State != Opened || done.Key != reserved.Key || done.Workstream != reserved.Workstream {
		t.Fatalf("completing: %+v, %v", done, err)
	}
	g, _, _ := f.store.Group(groupA)
	if g.Open != nil || g.Generation != 1 {
		t.Fatalf("group %+v", g)
	}
}

func TestTwoOccurrencesReservingTogetherShareOneWorkstream(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := recordOne(t, f, groupA, "1"), recordOne(t, f, groupA, "2")
	da, err := f.store.Reserve(ctx, groupA, a.Run)
	if err != nil {
		t.Fatal(err)
	}
	db, err := f.store.Reserve(ctx, groupA, b.Run)
	if err != nil || db.Key != da.Key || db.Workstream != da.Workstream || db.State != Opening {
		t.Fatalf("second reservation: %+v, %v; first %+v", db, err, da)
	}
	first, _ := f.store.Opened(ctx, groupA, a.Run, "")
	second, _ := f.store.Opened(ctx, groupA, b.Run, "")
	if first.State != Opened || second.State != Joined || first.Workstream != second.Workstream {
		t.Fatalf("opened %+v, then %+v", first, second)
	}
}

func TestNoticeAndSurfacingMarkersAreIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := recordOne(t, f, groupA, "1")
	for range 2 {
		if err := f.store.MarkNoticed(ctx, groupA, o.Run); err != nil {
			t.Fatal(err)
		}
		if err := f.store.MarkSurfaced(ctx, groupA, o.Run, "w_00000000000000000000000000000002"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.MarkSurfaced(ctx, groupA, o.Run, "w_00000000000000000000000000000003"); err != nil {
		t.Fatal(err)
	}
	f.restart()
	g, _, _ := f.store.Group(groupA)
	got, _ := g.Find(o.Run)
	if got.Noticed == nil || len(got.Surfaced) != 2 {
		t.Fatalf("markers %+v", got)
	}
	if n, _ := f.repo.CommitCount(""); n == 0 {
		t.Fatal("nothing was committed")
	}
	if err := f.store.MarkNoticed(ctx, groupA, "units/none"); err == nil {
		t.Error("marked an unknown occurrence")
	}
}

func TestAnnouncementMarkersAndMootNotesArePerDestinationAndSurviveARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := recordOne(t, f, groupA, "1")
	if _, err := f.store.Decline(ctx, groupA, o.Run, "added by the unit"); err != nil {
		t.Fatal(err)
	}
	moot := Moot{Destination: DestinationTarget, Workstream: "w_00000000000000000000000000000002", ArchivedBy: "chief_of_staff", Note: "archived"}
	for range 2 {
		if err := f.store.MarkAnnounced(ctx, groupA, o.Run, DestinationOrigin); err != nil {
			t.Fatal(err)
		}
		if err := f.store.MarkMoot(ctx, groupA, o.Run, moot); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.MarkMoot(ctx, groupA, o.Run, Moot{Destination: DestinationOrigin, ArchivedBy: "owner"}); err != nil {
		t.Fatal(err)
	}
	f.restart()
	g, _, _ := f.store.Group(groupA)
	got, _ := g.Find(o.Run)
	if len(got.Announced) != 1 || !got.IsAnnounced(DestinationOrigin) || got.IsAnnounced(DestinationTarget) {
		t.Fatalf("announced %+v", got.Announced)
	}
	if len(got.Moot) != 1 || !got.IsMoot(DestinationTarget) || got.IsMoot(DestinationOrigin) || got.Moot[0].ArchivedBy != "chief_of_staff" || got.Moot[0].At.IsZero() {
		t.Fatalf("moot %+v", got.Moot)
	}
	if got.Decision.State != Declined || got.Decision.Reason != "added by the unit" {
		t.Fatalf("the markers changed the decision: %+v", got.Decision)
	}
	if err := f.store.MarkAnnounced(ctx, groupA, "units/none", DestinationOrigin); err == nil {
		t.Error("marked an unknown occurrence")
	}
}

func TestRecordedFromAFlakeFixWorkstreamIsKept(t *testing.T) {
	f := newFixture(t)
	o := unitRun("1", groupA+"&go-test=TestOne")
	o.FromFlakeFix = true
	got, _, err := f.store.Record(context.Background(), in(groupA, o))
	if err != nil || !got.FromFlakeFix {
		t.Fatalf("occurrence %+v, %v", got, err)
	}
}

func TestAReservationKeepsTheChiefsNotesAcrossARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := recordOne(t, f, groupA, "1")
	reserved, err := f.store.ReserveWithNotes(ctx, groupA, o.Run, "Look at the shared fixture.")
	if err != nil || reserved.State != Opening || reserved.Notes != "Look at the shared fixture." {
		t.Fatalf("reserving: %+v, %v", reserved, err)
	}
	f.restart()
	again, err := f.store.ReserveWithNotes(ctx, groupA, o.Run, "Other notes.")
	if err != nil || again.Notes != "Look at the shared fixture." || again.Key != reserved.Key {
		t.Fatalf("reserving again: %+v, %v", again, err)
	}
}

func TestAGroupIsAssociatedWithItsOpenOrReservedWorkstream(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := recordOne(t, f, groupA, "1")
	if g, _, _ := f.store.Group(groupA); len(g.Associated()) != 0 {
		t.Fatalf("a group with no workstream is associated with %v", g.Associated())
	}
	reserved, err := f.store.Reserve(ctx, groupA, o.Run)
	if err != nil {
		t.Fatal(err)
	}
	if g, _, _ := f.store.Group(groupA); len(g.Associated()) != 1 || g.Associated()[0] != reserved.Workstream {
		t.Fatalf("a reserved opening is associated with %v, want %s", g.Associated(), reserved.Workstream)
	}
	if _, err := f.store.Opened(ctx, groupA, o.Run, ""); err != nil {
		t.Fatal(err)
	}
	if g, _, _ := f.store.Group(groupA); len(g.Associated()) != 1 || g.Associated()[0] != reserved.Workstream {
		t.Fatalf("an open workstream is associated with %v, want %s", g.Associated(), reserved.Workstream)
	}
	if _, err := f.store.CloseWorkstream(ctx, reserved.Workstream); err != nil {
		t.Fatal(err)
	}
	if g, _, _ := f.store.Group(groupA); len(g.Associated()) != 0 {
		t.Fatalf("a closed group is associated with %v", g.Associated())
	}
}
