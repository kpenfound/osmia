package runtime

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestArchiveLifecycleEvidenceSurvivesRestartWithoutChangingIdentity(t *testing.T) {
	in := fixture(t)
	store := open(t, in)
	group := config.WorkgroupID("g_0123456789abcdef0123456789abcdef")
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	a := Archive{Project: pid, Workstream: w1, Workgroup: group, Resolved: true, ResolutionEvidence: "cancelled", State: "abandoned", Title: "Original title", ArchivedAt: at}
	must(t, store.SetArchived(a))
	a.CleanupStartedAt = at.Add(time.Minute)
	must(t, store.UpdateArchive(a))
	a.CleanedAt = at.Add(2 * time.Minute)
	must(t, store.UpdateArchive(a))
	reopened := open(t, in)
	state, _ := reopened.Snapshot()
	if len(state.Archived) != 1 || state.Archived[0] != a {
		t.Fatalf("restart changed lifecycle evidence: %+v", state.Archived)
	}
	for _, mutate := range []func(*Archive){func(a *Archive) { a.Workgroup = "g_1123456789abcdef0123456789abcdef" }, func(a *Archive) { a.Resolved = false }} {
		wrong := a
		mutate(&wrong)
		if err := reopened.UpdateArchive(wrong); err == nil {
			t.Fatal("archive identity or resolution changed")
		}
	}
}

func TestLegacyArchiveResolutionCanBeCorrectedFromRetainedHistory(t *testing.T) {
	in := fixture(t)
	store := open(t, in)
	a := Archive{Project: pid, Workstream: w1, Resolved: true, State: "delivered", Title: "Ready PR", ArchivedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
	must(t, store.SetArchived(a))
	store = open(t, in)
	a.Resolved = false
	a.ResolutionEvidence = "unresolved"
	must(t, store.UpdateArchive(a))
	store = open(t, in)
	st, _ := store.Snapshot()
	if len(st.Archived) != 1 || st.Archived[0] != a {
		t.Fatalf("legacy correction lost on restart: %+v", st.Archived)
	}
	a.Resolved = true
	if err := store.UpdateArchive(a); err == nil {
		t.Fatal("unresolved evidence accepted as resolution")
	}
}

func TestArchiveResolutionEvidenceValidation(t *testing.T) {
	for _, tc := range []struct {
		evidence, state string
		resolved        bool
	}{
		{"merged", "delivered", true}, {"cancelled", "abandoned", true}, {"unresolved", "delivered", false},
	} {
		in := fixture(t)
		store := open(t, in)
		a := Archive{Project: pid, Workstream: w1, State: tc.state, Resolved: tc.resolved, ResolutionEvidence: tc.evidence, Title: "Archived feature", ArchivedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
		must(t, store.SetArchived(a))
		reopened := open(t, in)
		st, _ := reopened.Snapshot()
		if st.Archived[0] != a {
			t.Fatal("resolution evidence changed on restart")
		}
	}
	for _, tc := range []struct {
		evidence, state string
		resolved        bool
	}{
		{"merged", "abandoned", true}, {"merged", "delivered", false}, {"cancelled", "delivered", true}, {"unknown", "delivered", true}, {"unresolved", "delivered", true},
	} {
		in := fixture(t)
		store := open(t, in)
		a := Archive{Project: pid, Workstream: w1, State: tc.state, Resolved: tc.resolved, ResolutionEvidence: tc.evidence, Title: "Invalid evidence", ArchivedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
		if err := store.SetArchived(a); err == nil {
			t.Fatalf("invalid evidence accepted: %+v", a)
		}
	}
}

func TestArchiveActorSurvivesRestartAndCleanupUpdates(t *testing.T) {
	in := fixture(t)
	store := open(t, in)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	a := Archive{Project: pid, Workstream: w1, Actor: ArchiveActorChief, Resolved: true, ResolutionEvidence: "merged", State: "delivered", Title: "Merged PR", ArchivedAt: at}
	must(t, store.SetArchived(a))
	// Cleanup updates carry the record it read; the actor stays what was recorded.
	update := a
	update.Actor, update.CleanedAt = "", at.Add(time.Minute)
	must(t, store.UpdateArchive(update))
	reopened := open(t, in)
	state, _ := reopened.Snapshot()
	a.CleanedAt = at.Add(time.Minute)
	if len(state.Archived) != 1 || state.Archived[0] != a {
		t.Fatalf("archive after restart: %+v", state.Archived)
	}
	// An older record names no actor and reads as the owner's.
	if err := reopened.SetArchived(Archive{Project: pid, Workstream: w2, Actor: "somebody", State: "abandoned", Title: "Odd", ArchivedAt: at}); err == nil {
		t.Fatal("an unknown archive actor was accepted")
	}
	must(t, reopened.SetArchived(Archive{Project: pid, Workstream: w2, State: "abandoned", Title: "Owner", ArchivedAt: at}))
	// A second archive never replaces the first's record or actor.
	must(t, reopened.SetArchived(Archive{Project: pid, Workstream: w2, Actor: ArchiveActorChief, State: "abandoned", Title: "Owner", ArchivedAt: at.Add(time.Hour)}))
	state, _ = reopened.Snapshot()
	for _, got := range state.Archived {
		if got.Workstream == w2 && (got.Actor != "" || !got.ArchivedAt.Equal(at)) {
			t.Fatalf("an owner archive was replaced: %+v", got)
		}
	}
}
