package runtime

import (
	"testing"
	"time"
)

func TestArchiveObservedMergePreservesConcurrentCleanupAcrossRestart(t *testing.T) {
	in := fixture(t)
	store := open(t, in)
	at := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	a := Archive{Project: pid, Workstream: w1, Title: "Upstream", State: "delivered", ResolutionEvidence: "unresolved", ArchivedAt: at, Publication: ArchivePublication{Upstream: "owner/repo", Fork: "owner/fork", Branch: "feature", Commit: "tip", PullRequest: 7}}
	must(t, store.SetArchived(a))
	p := a.Publication
	wrong := p
	wrong.PullRequest = 6
	if err := store.RecordArchivePullRequestMerge(pid, w1, wrong, "merge"); err == nil {
		t.Fatal("accepted older PR")
	}
	must(t, store.RecordArchivePullRequestMerge(pid, w1, p, ""))
	must(t, store.RecordArchivePullRequestMerge(pid, w1, p, "merge"))
	// Cleanup read its snapshot before the observation. Its metadata write must
	// preserve the concrete merge while advancing authorized cleanup progress.
	a.CleanupStartedAt = at.Add(time.Minute)
	a.CleanedAt = at.Add(2 * time.Minute)
	must(t, store.UpdateArchive(a))
	must(t, store.RecordArchivePullRequestMerge(pid, w1, p, "merge"))
	if err := store.RecordArchivePullRequestMerge(pid, w1, p, "different"); err == nil {
		t.Fatal("changed merge pin")
	}
	must(t, store.Close())
	reopened := open(t, in)
	st, _ := reopened.Snapshot()
	got := st.Archived[0]
	if !got.Resolved || got.ResolutionEvidence != "merged" || got.MergeCommit != "merge" || got.MergeReference != "owner/repo#7" || got.CleanedAt != a.CleanedAt || got.CleanupStartedAt != a.CleanupStartedAt || got.ArchivedAt != a.ArchivedAt || got.Publication != p {
		t.Fatalf("recovered archive=%+v", got)
	}
}
