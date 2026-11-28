package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const resumeRun = "units/resume/checks-1.json"

type announceFaults struct {
	mu   sync.Mutex
	fail func(name string) bool
	hits map[string]int
}

func failEveryAnnouncement(string) bool { return true }

func (a *announceFaults) boundary(name string) error {
	if !strings.HasPrefix(name, "flake-announce-") {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hits == nil {
		a.hits = map[string]int{}
	}
	a.hits[name]++
	if a.fail != nil && a.fail(name) {
		return errFlakeStop
	}
	return nil
}

func (a *announceFaults) set(fail func(name string) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = fail
}

func (a *announceFaults) attempts(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits[name]
}

func (l *flakeFixLive) failing(fail func(name string) bool) *announceFaults {
	faults := &announceFaults{fail: fail}
	l.s.setBoundary(faults.boundary)
	return faults
}

func (l *flakeFixLive) pass(t *testing.T) {
	t.Helper()
	repo := l.repository()
	fixturekit.Must(t, (&flakeRecorder{s: l.s, cfg: l.s.about(repo), repository: repo}).Pass(context.Background()))
}

func (l *flakeFixLive) current(t *testing.T, o flakes.Occurrence) flakes.Occurrence {
	t.Helper()
	for _, c := range flakeOccurrences(t, l.repository()) {
		if c.ID == o.ID {
			return c
		}
	}
	t.Fatalf("occurrence %s is no longer recorded", o.ID)
	return flakes.Occurrence{}
}

func (l *flakeFixLive) entries(t *testing.T, o flakes.Occurrence) []trace.Transition {
	t.Helper()
	posted, _ := flakePosts(t, l.repository(), o.Source.Workstream, flakeDecisionSubject(o.ID))
	return posted
}

func (l *flakeFixLive) awaitEntry(t *testing.T, o flakes.Occurrence) trace.Transition {
	t.Helper()
	intakefixture.Soon(t, "the decision of occurrence "+o.ID+" in its originating workstream", func() bool { return len(l.entries(t, o)) > 0 })
	return l.entries(t, o)[0]
}

func (l *flakeFixLive) decide(t *testing.T, stream config.WorkstreamID, name, input string) flakeFixAnswer {
	t.Helper()
	a := callFlakeFix(t, l.manager(t, stream, name), input)
	if a.refused() {
		t.Fatalf("the flake_fix tool refused %s: %+v", input, a)
	}
	return a
}

func declineInput(occurrence, reason string) string {
	return fmt.Sprintf(`{"action":"decline","occurrence":%q,"reason":%q}`, occurrence, reason)
}

func (l *flakeFixLive) awaitState(t *testing.T, o flakes.Occurrence, state string) flakes.Occurrence {
	t.Helper()
	var got flakes.Occurrence
	intakefixture.Soon(t, "occurrence "+o.ID+" to be "+state, func() bool {
		got = l.current(t, o)
		return got.Decision.State == state
	})
	return got
}

func (l *flakeFixLive) footprint(t *testing.T, target config.WorkstreamID, os ...flakes.Occurrence) string {
	t.Helper()
	var parts []string
	for _, o := range os {
		parts = append(parts, fmt.Sprintf("%s entries=%d", o.ID, len(l.entries(t, o))))
	}
	if target == "" {
		return fmt.Sprint(parts)
	}
	return fmt.Sprintf("%v surfacings=%d", parts, len(flakeSurfacings(t, l.repository(), target)))
}

func (l *flakeFixLive) settledFootprint(t *testing.T, target config.WorkstreamID, os ...flakes.Occurrence) {
	t.Helper()
	before := l.footprint(t, target, os...)
	noticesBefore := len(flakeNoticeBodies(t, l.repository(), os[0].Source.Workstream))
	for range 2 {
		l.pass(t)
	}
	l.restart(t)
	for range 2 {
		l.pass(t)
	}
	if after := l.footprint(t, target, os...); after != before {
		t.Fatalf("passes and a restart changed the announcements:\nbefore %s\nafter  %s", before, after)
	}
	if n := len(flakeNoticeBodies(t, l.repository(), os[0].Source.Workstream)); n != noticesBefore {
		t.Fatalf("passes and a restart changed the notices from %d to %d", noticesBefore, n)
	}
}

func TestFlakeDecisionIsAnnouncedAfterAMissedWrite(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"opened", "declined", "joined by the recorder", "joined by the manager"} {
		for _, how := range []string{"write fails", "service restarts"} {
			if decision == "joined by the manager" && how == "service restarts" {
				continue
			}
			t.Run(decision+"/"+how, func(t *testing.T) {
				t.Parallel()
				l := startFlakeFixLive(t, "flake-announce-"+strings.ReplaceAll(decision+how, " ", "-"), flakyOutcome(flakyTraceTest))
				first := l.occurrence(t, l.stream, resumeRun)
				l.awaitNoticeOf(t, l.stream, first.ID)
				want := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 0)
				faults := l.failing(nil)

				var decided flakes.Occurrence
				var target config.WorkstreamID
				var reasonPart string
				switch decision {
				case "opened":
					faults.set(failEveryAnnouncement)
					if a := l.decide(t, l.stream, "open", openInput(first.ID, "")); a.Decision != flakes.Opened || a.Workstream != want {
						t.Fatalf("open answered %+v", a)
					}
					decided, reasonPart = first, string(want)
				case "declined":
					faults.set(failEveryAnnouncement)
					reasonPart = "The flaky test was added by the unit itself."
					if a := l.decide(t, l.stream, "decline", declineInput(first.ID, reasonPart)); a.Decision != flakes.Declined {
						t.Fatalf("decline answered %+v", a)
					}
					decided = first
				case "joined by the recorder":
					l.decide(t, l.stream, "open", openInput(first.ID, ""))
					l.awaitEntry(t, first)
					faults.set(failEveryAnnouncement)
					run := recordUnitFlakyRun(t, l.repository(), l.stream, "extra", 1, flakyTraceOther)
					decided = l.awaitState(t, l.occurrence(t, l.stream, run), flakes.Joined)
					target, reasonPart = want, string(want)
				case "joined by the manager":
					run := recordUnitFlakyRun(t, l.repository(), l.stream, "extra", 1, flakyTraceOther)
					second := l.occurrence(t, l.stream, run)
					l.awaitNoticeOf(t, l.stream, second.ID)
					l.decide(t, l.stream, "open", openInput(first.ID, ""))
					l.awaitEntry(t, first)
					faults.set(failEveryAnnouncement)
					if a := l.decide(t, l.stream, "join", openInput(second.ID, "")); a.Decision != flakes.Joined || a.Workstream != want {
						t.Fatalf("the second open answered %+v", a)
					}
					decided, target, reasonPart = l.current(t, second), want, string(want)
				}

				if len(l.entries(t, decided)) != 0 {
					t.Fatalf("an entry was written while the write failed: %+v", l.entries(t, decided))
				}
				if target != "" && len(flakeSurfacings(t, l.repository(), target)) != 0 {
					t.Fatalf("a surfacing was delivered while the write failed")
				}
				if o := l.current(t, decided); o.IsAnnounced(flakes.DestinationOrigin) || o.IsMoot(flakes.DestinationOrigin) || len(o.Surfaced) != 0 {
					t.Fatalf("the occurrence is marked announced before any write: %+v", o)
				}

				l.pass(t)
				if faults.attempts("flake-announce-origin "+decided.ID) == 0 {
					t.Fatalf("the failing announcement was not tried: %+v", faults.hits)
				}
				if how == "write fails" {
					faults.set(nil)
				} else {
					l.restart(t)
				}

				entry := l.awaitEntry(t, decided)
				if entry.To != strings.Fields(decision)[0] || !strings.Contains(entry.Reason, reasonPart) || entry.Actor != managerActor || !strings.Contains(entry.Reason, decided.ID) {
					t.Fatalf("the decision entry: %+v", entry)
				}
				intakefixture.Soon(t, "the announcement of "+decided.ID+" to be marked", func() bool {
					o := l.current(t, decided)
					return o.IsAnnounced(flakes.DestinationOrigin) && (target == "" || len(o.Surfaced) == 1)
				})
				if target != "" {
					if n := len(flakeSurfacings(t, l.repository(), target)); n != 1 {
						t.Fatalf("%d surfacings to %s", n, target)
					}
					if o := l.current(t, decided); o.Surfaced[0].Workstream != target {
						t.Fatalf("the surfacing is marked for the wrong workstream: %+v", o)
					}
				}
				if o := l.current(t, decided); len(o.Moot) != 0 {
					t.Fatalf("the announcement is not marked: %+v", o)
				}
				feed := intakefixture.ReadFeed(t, l.c, l.stream)
				var shown int
				for _, e := range feed.Entries {
					if e.Transition != nil && e.Transition.Subject == flakeDecisionSubject(decided.ID) {
						shown++
					}
				}
				if shown != 1 {
					t.Fatalf("the feed shows the decision %d times", shown)
				}
				l.settledFootprint(t, target, decided)
				if got := l.entries(t, decided); len(got) != 1 {
					t.Fatalf("%d entries after the restart", len(got))
				}
				wantStreams := []config.WorkstreamID{want}
				if decision == "declined" {
					wantStreams = nil
				}
				if got := flakeFixWorkstreams(t, l.repository()); !slices.Equal(got, wantStreams) {
					t.Fatalf("flake-fix workstreams %v, want %v", got, wantStreams)
				}
			})
		}
	}
}

func TestFlakeAnnouncementOfOneOccurrenceFailingStopsNoOther(t *testing.T) {
	t.Parallel()
	l := startFlakeFixLive(t, "flake-announce-isolated", flakyOutcome(flakyTraceTest))
	first := l.occurrence(t, l.stream, resumeRun)
	l.awaitNoticeOf(t, l.stream, first.ID)
	want := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 0)
	l.decide(t, l.stream, "open", openInput(first.ID, ""))
	l.awaitEntry(t, first)

	joinedRun := recordUnitFlakyRun(t, l.repository(), l.stream, "extra", 1, flakyTraceOther)
	declinedRun := recordUnitFlakyRun(t, l.repository(), l.stream, "web", 1, flakyWebTest)
	joined := l.awaitState(t, l.occurrence(t, l.stream, joinedRun), flakes.Joined)
	declinedOcc := l.occurrence(t, l.stream, declinedRun)
	l.awaitNoticeOf(t, l.stream, declinedOcc.ID)
	l.awaitEntry(t, joined)
	intakefixture.Soon(t, "the first surfacing", func() bool { return len(flakeSurfacings(t, l.repository(), want)) == 1 })

	thirdID := flakes.OccurrenceID(flakes.RunKey(l.stream, execution.CheckPath("third", 1)), l.group)
	faults := l.failing(func(name string) bool { return name == "flake-announce-origin "+thirdID })
	thirdRun := recordUnitFlakyRun(t, l.repository(), l.stream, "third", 1, flakyTraceOther)
	third := l.occurrence(t, l.stream, thirdRun)
	third = l.awaitState(t, third, flakes.Joined)
	declinedOcc = l.occurrence(t, l.stream, declinedRun)
	l.decide(t, l.stream, "decline", declineInput(declinedOcc.ID, "Added by the unit."))

	entry := l.awaitEntry(t, declinedOcc)
	if entry.To != "declined" || !strings.Contains(entry.Reason, "Added by the unit.") {
		t.Fatalf("the recoverable occurrence's entry: %+v", entry)
	}
	intakefixture.Soon(t, "the failing occurrence's surfacing", func() bool { return len(l.current(t, third).Surfaced) == 1 })
	if surfacings := flakeSurfacings(t, l.repository(), want); len(surfacings) != 2 {
		t.Fatalf("surfacings %q, want one for each joined occurrence", surfacings)
	}
	for range 3 {
		l.pass(t)
	}
	if got := l.entries(t, third); len(got) != 0 {
		t.Fatalf("the failing occurrence got an entry: %+v", got)
	}
	if o := l.current(t, third); o.IsAnnounced(flakes.DestinationOrigin) || o.IsMoot(flakes.DestinationOrigin) {
		t.Fatalf("the failing occurrence is marked announced: %+v", o)
	}
	if faults.attempts("flake-announce-origin "+third.ID) < 2 {
		t.Fatalf("the failing occurrence was not retried: %+v", faults.hits)
	}
	if got := l.entries(t, declinedOcc); len(got) != 1 {
		t.Fatalf("the recoverable occurrence has %d entries", len(got))
	}

	faults.set(nil)
	l.awaitEntry(t, third)
	intakefixture.Soon(t, "the announcements to be marked", func() bool {
		return l.current(t, third).IsAnnounced(flakes.DestinationOrigin) && l.current(t, declinedOcc).IsAnnounced(flakes.DestinationOrigin)
	})
	l.settledFootprint(t, want, first, joined, declinedOcc, third)
	for _, o := range []flakes.Occurrence{first, joined, declinedOcc, third} {
		if got := l.entries(t, o); len(got) != 1 {
			t.Fatalf("occurrence %s has %d entries", o.ID, len(got))
		}
	}
}

// A workstream that is delivered or abandoned after the decision commits, and
// stays unarchived, still ends with exactly one entry: a terminal workstream
// takes the decision as an ordinary transition under its own subject.
func TestFlakeAnnouncementReachesADeliveredOrAbandonedOriginatingWorkstream(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"delivered", "abandoned"} {
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			l := startFlakeFixLive(t, "flake-announce-"+terminal, flakyOutcome(flakyTraceTest))
			repo := l.repository()
			other := config.WorkstreamID("w_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
			fixturekit.Must(t, repo.CreateWorkstream(context.Background(), other, time.Now(), trace.Actor{Kind: "service", ID: "fixture"}))
			run := recordUnitFlakyRun(t, repo, other, "resume", 1, flakyWebTest)
			occ := l.occurrence(t, other, run)
			l.awaitNoticeOf(t, other, occ.ID)
			faults := l.failing(failEveryAnnouncement)
			const reason = "Added by the unit itself."
			l.decide(t, other, "decline", declineInput(occ.ID, reason))

			switch terminal {
			case "delivered":
				fixturekit.RecordDeliveredPublication(t, l.repository(), l.s.cfg, other, featureBranch(other), 9, ownerActor)
			case "abandoned":
				if _, err := l.c.Abandon(context.Background(), other, "the owner dropped it"); err != nil {
					t.Fatal(err)
				}
			}
			want := DeliveredState
			if terminal == "abandoned" {
				want = AbandonedState
			}
			if state, err := l.repository().Workflow(other, trace.FeatureSubject); err != nil || state.Value != want {
				t.Fatalf("workstream %s is %q (%v), want %s", other, state.Value, err, want)
			}
			l.pass(t)
			if got := l.entries(t, occ); len(got) != 0 {
				t.Fatalf("an entry was written while the write failed: %+v", got)
			}

			faults.set(nil)
			entry := l.awaitEntry(t, occ)
			if entry.To != "declined" || !strings.Contains(entry.Reason, reason) {
				t.Fatalf("the entry: %+v", entry)
			}
			l.settledFootprint(t, "", occ)
			if got := l.entries(t, occ); len(got) != 1 {
				t.Fatalf("%d entries in the %s workstream", len(got), terminal)
			}
			if _, archived := l.s.archivedWorkstream(string(other)); archived {
				t.Fatalf("workstream %s was archived", other)
			}
			if state, err := l.repository().Workflow(other, trace.FeatureSubject); err != nil || state.Value != want {
				t.Fatalf("recording the decision changed %s to %q (%v)", other, state.Value, err)
			}
			if feed := intakefixture.ReadFeed(t, l.c, other); !slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
				return e.Transition != nil && e.Transition.Subject == flakeDecisionSubject(occ.ID) && strings.Contains(e.Transition.Reason, reason)
			}) {
				t.Fatalf("the feed of the %s workstream does not show the decision: %+v", terminal, feed.Entries)
			}
		})
	}
}

type archivedOrigin struct {
	l        *flakeFixLive
	faults   *announceFaults
	want     config.WorkstreamID
	other    config.WorkstreamID
	opened   flakes.Occurrence
	joined   flakes.Occurrence
	declined flakes.Occurrence
}

func newArchivedOrigin(t *testing.T, key string) *archivedOrigin {
	t.Helper()
	l := startFlakeFixLive(t, key, flakyOutcome(flakyTraceTest))
	first := l.occurrence(t, l.stream, resumeRun)
	l.awaitNoticeOf(t, l.stream, first.ID)
	want := expectedFlakeFixWorkstream(l.repository().Project(), l.group, 0)
	l.decide(t, l.stream, "open", openInput(first.ID, ""))
	l.awaitEntry(t, first)
	faults := l.failing(failEveryAnnouncement)

	other := l.handIn(t, key+"-origin", "# An unrelated change\n\nThe workstream whose flake joins the open one.\n")
	joined := l.awaitState(t, l.occurrence(t, other, recordUnitFlakyRun(t, l.repository(), other, "resume", 1, flakyTraceOther)), flakes.Joined)
	declined := l.occurrence(t, l.stream, recordUnitFlakyRun(t, l.repository(), l.stream, "web", 1, flakyWebTest))
	l.awaitNoticeOf(t, l.stream, declined.ID)
	l.decide(t, l.stream, "decline", declineInput(declined.ID, "Added by the unit."))
	if n := len(flakeSurfacings(t, l.repository(), want)); n != 0 {
		t.Fatalf("%d surfacings while the writes failed", n)
	}
	if len(l.entries(t, joined)) != 0 || len(l.entries(t, declined)) != 0 {
		t.Fatalf("entries were written while the writes failed")
	}
	return &archivedOrigin{l: l, faults: faults, want: want, other: other, opened: first, joined: joined, declined: declined}
}

func (a *archivedOrigin) awaitCleaned(t *testing.T) runtime.Archive {
	t.Helper()
	var archive runtime.Archive
	intakefixture.Soon(t, "the archive cleanup of "+string(a.other), func() bool {
		var ok bool
		archive, ok = intakefixture.ArchiveRecord(t, a.l.s.store, a.other)
		if !ok || archive.CleanedAt.IsZero() {
			return false
		}
		gone, err := trace.WorkstreamTraceGone(a.l.s.cfg.Root, a.l.project, a.other)
		return err == nil && gone
	})
	return archive
}

func (a *archivedOrigin) recovers(t *testing.T, archivedBy, noteWho string) {
	t.Helper()
	l := a.l
	a.faults.set(nil)
	intakefixture.Soon(t, "the surfacing to the open workstream", func() bool { return len(flakeSurfacings(t, l.repository(), a.want)) == 1 })
	l.awaitEntry(t, a.declined)
	intakefixture.Soon(t, "the originating destination to close as moot", func() bool { return l.current(t, a.joined).IsMoot(flakes.DestinationOrigin) })
	intakefixture.Soon(t, "the surfacing to be marked", func() bool { return len(l.current(t, a.joined).Surfaced) == 1 })

	check := func() {
		t.Helper()
		o := l.current(t, a.joined)
		if o.Decision.State != flakes.Joined || o.Decision.Workstream != a.want || o.Decision.Key != "" {
			t.Fatalf("the decision changed: %+v", o.Decision)
		}
		if len(o.Moot) != 1 || o.Moot[0].Destination != flakes.DestinationOrigin || o.Moot[0].Workstream != a.other || o.Moot[0].ArchivedBy != archivedBy || !strings.Contains(o.Moot[0].Note, noteWho) || !strings.Contains(o.Moot[0].Note, "moot") {
			t.Fatalf("the moot note: %+v", o.Moot)
		}
		if o.IsAnnounced(flakes.DestinationOrigin) || o.IsMoot(flakes.DestinationTarget) || len(o.Surfaced) != 1 || o.Surfaced[0].Workstream != a.want {
			t.Fatalf("the other destination is not owed and delivered as it should: %+v", o)
		}
		if got := l.entries(t, a.declined); len(got) != 1 {
			t.Fatalf("the declined occurrence has %d entries", len(got))
		}
		if got := len(flakeSurfacings(t, l.repository(), a.want)); got != 1 {
			t.Fatalf("%d surfacings", got)
		}
	}
	check()
	for range 2 {
		l.pass(t)
	}
	l.restart(t)
	l.pass(t)
	check()
	if text := l.footprint(t, a.want, a.declined); !strings.Contains(text, "entries=1") {
		t.Fatalf("footprint %s", text)
	}
}

func TestFlakeJoinedDecisionOfAnOwnerArchivedOriginStillSurfacesToTheOpenWorkstream(t *testing.T) {
	t.Parallel()
	a := newArchivedOrigin(t, "flake-announce-owner-archive")
	ctx := context.Background()
	fixturekit.RecordDeliveredPublication(t, a.l.repository(), a.l.s.cfg, a.other, featureBranch(a.other), 7, ownerActor)
	if _, err := a.l.c.Archive(ctx, a.other); err != nil {
		t.Fatal(err)
	}
	if archive := a.awaitCleaned(t); archive.Actor == runtime.ArchiveActorManager {
		t.Fatalf("archive %+v", archive)
	}
	a.recovers(t, api.ArchivedByOwner, "the owner")
}

func TestFlakeDecisionOfAWorkstreamTheManagerArchivedOnMergeClosesAsMoot(t *testing.T) {
	t.Parallel()
	a := newArchivedOrigin(t, "flake-announce-merge-archive")
	repo := a.l.repository()
	fixturekit.RecordDeliveredPublication(t, repo, a.l.s.cfg, a.other, featureBranch(a.other), 7, ownerActor)
	fixturekit.ResolutionDocument(t, repo, a.other, delivery.OwnPullRequestOutcomeDocument, delivery.OwnPullRequestOutcomePath,
		delivery.OwnPullRequestOutcome{PullRequest: 7, Outcome: delivery.OwnPullRequestOutcomeMerged, MergeCommit: "merge-commit"}, time.Now().UTC(), ownerActor)
	archive := a.awaitCleaned(t)
	if archive.Actor != runtime.ArchiveActorManager || archivedBy(archive) != api.ArchivedByManager {
		t.Fatalf("archive %+v, want the manager's archive-on-merge", archive)
	}
	if n := len(flakeSurfacings(t, a.l.repository(), a.want)); n != 0 {
		t.Fatalf("%d surfacings before reconciliation was allowed", n)
	}
	a.recovers(t, trace.Manager, "the manager")
}

// While announcements fail, the originating workstream's units and transitions
// are exactly those it has without flakes.
func TestFlakeAnnouncementFailuresNeverChangeTheOriginatingFlow(t *testing.T) {
	t.Parallel()
	control, controlStream, controlRepository := newFlakeFixture(t, "flake-announce-control", fixturekit.CheckOutcome{Result: api.CheckResult{Output: "CHECKS  ✔ 1 passed\n✔ " + flakyTraceTest + " 1.0s OK\n"}})
	control.s.cfg.Project.FlakeFixes = false
	runChecks(t, control.s, controlRepository, controlStream)
	want := unitHistory(t, controlRepository, controlStream)

	f, stream, repository := newFlakeFixture(t, "flake-announce-failing", flakyOutcome(flakyTraceTest))
	f.s.setBoundary((&announceFaults{fail: failEveryAnnouncement}).boundary)
	runChecks(t, f.s, repository, stream)
	passFlakes(t, f, repository)
	occ := flakeOccurrences(t, repository)[0]
	res, why, err := f.s.actOnFlake(context.Background(), repository, stream, "decline", occ.ID, "", "Added by the unit.")
	if err != nil || why != "" || res.Decision != flakes.Declined {
		t.Fatalf("the decision was not committed while the announcement failed: %+v %q %v", res, why, err)
	}
	for range 3 {
		passFlakes(t, f, repository)
	}
	if got := unitHistory(t, repository, stream); !slices.Equal(got, want) {
		t.Fatalf("with announcements failing, the unit's transitions are %q, want %q", got, want)
	}
	if posted, _ := flakePosts(t, repository, stream, "flake-decision-"); len(posted) != 0 {
		t.Fatalf("an entry was written while the write failed: %+v", posted)
	}
	f.s.setBoundary(nil)
	passFlakes(t, f, repository)
	passFlakes(t, f, repository)
	if posted, _ := flakePosts(t, repository, stream, "flake-decision-"); len(posted) != 1 {
		t.Fatalf("%d entries once the failure cleared", len(posted))
	}
	if got := unitHistory(t, repository, stream); !slices.Equal(got, want) {
		t.Fatalf("after the announcement, the unit's transitions are %q, want %q", got, want)
	}
}
