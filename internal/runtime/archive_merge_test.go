package runtime

import (
	"testing"
	"time"
)

func TestArchiveMergeIdentitySurvivesRestartAndCannotChange(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	a := Archive{Project: pid, Workstream: w1, ArchivedAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Title: "Upstream", State: "delivered", Resolved: true, ResolutionEvidence: "merged", MergeCommit: "merge-tip", MergeReference: "owner/repo#7", Publication: ArchivePublication{Upstream: "owner/repo", Fork: "owner/fork", Branch: "feature", Commit: "feature-tip", PullRequest: 7}}
	must(t, s.SetArchived(a))
	must(t, s.Close())
	reopened := open(t, in)
	state, _ := reopened.Snapshot()
	if len(state.Archived) != 1 || state.Archived[0] != a {
		t.Fatalf("lost archive merge evidence: %+v", state.Archived)
	}
	for _, change := range []func(*Archive){func(a *Archive) { a.MergeCommit = "other" }, func(a *Archive) { a.MergeReference = "owner/repo#8" }, func(a *Archive) { a.Publication.PullRequest = 8 }, func(a *Archive) { a.Publication = ArchivePublication{} }} {
		changed := a
		change(&changed)
		if err := reopened.UpdateArchive(changed); err == nil {
			t.Fatalf("changed pinned identity: %+v", changed)
		}
	}
	a.CleanupStartedAt = a.ArchivedAt.Add(time.Minute)
	a.CleanedAt = a.CleanupStartedAt.Add(time.Minute)
	must(t, reopened.UpdateArchive(a))
	state, _ = reopened.Snapshot()
	if state.Archived[0] != a {
		t.Fatal("cleanup lost merge identity")
	}
}

func TestArchiveRejectsUnverifiedMergeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Archive)
	}{
		{"unresolved", func(a *Archive) { a.ResolutionEvidence = "unresolved"; a.Resolved = false }},
		{"cancelled", func(a *Archive) { a.State = "abandoned"; a.ResolutionEvidence = "cancelled" }},
		{"commit only", func(a *Archive) { a.MergeReference = "" }},
		{"reference only", func(a *Archive) { a.MergeCommit = "" }},
		{"mismatched PR", func(a *Archive) {
			a.Publication = ArchivePublication{Upstream: "owner/repo", Fork: "owner/fork", Branch: "feature", PullRequest: 8}
		}},
		{"incomplete publication", func(a *Archive) { a.Publication = ArchivePublication{PullRequest: 7} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture(t)
			s := open(t, in)
			a := Archive{Project: pid, Workstream: w1, Title: "Upstream", State: "delivered", Resolved: true, ResolutionEvidence: "merged", MergeCommit: "merge-tip", MergeReference: "owner/repo#7"}
			tc.change(&a)
			if err := s.SetArchived(a); err == nil {
				t.Fatalf("accepted invalid archive %+v", a)
			}
		})
	}
}
