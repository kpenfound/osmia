package lifecycle

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// MemberEvidence is read from one document snapshot, including the current
// Publication and its outcome. Publication readiness grants no merge evidence.
type MemberEvidence struct {
	Publication DeliveryPublication
	Published   bool
	Outcome     ownPullRequestOutcome
	Merge       DeliveryMerge
	OwnerMerge  bool
}

func MemberEvidenceFrom(docs []trace.Document) (MemberEvidence, error) {
	var e MemberEvidence
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	for _, d := range latest {
		switch d.ID {
		case publicationDocument:
			var p DeliveryPublication
			if err := json.Unmarshal([]byte(d.Content), &p); err != nil {
				return e, err
			}
			if p.Status == publicationOpened && p.PullRequest > 0 {
				e.Publication, e.Published = p, true
			}
		case ownPullRequestOutcomeDocument:
			if err := json.Unmarshal([]byte(d.Content), &e.Outcome); err != nil {
				return e, err
			}
		case mergeDocument:
			var m DeliveryMerge
			if err := json.Unmarshal([]byte(d.Content), &m); err != nil {
				return e, err
			}
			e.OwnerMerge, e.Merge = true, m
		}
	}
	return e, nil
}
func ReadMemberEvidence(repo *trace.Repository, stream config.WorkstreamID) (MemberEvidence, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return MemberEvidence{}, err
	}
	return MemberEvidenceFrom(docs)
}
func (e MemberEvidence) ResolutionEvidence() trace.MemberResolutionEvidence {
	var pr int
	if e.Published {
		pr = e.Publication.PullRequest
	}
	return trace.MemberResolutionEvidence{OwnerMerge: e.OwnerMerge, Publication: pr, OutcomePullRequest: e.Outcome.PullRequest, Outcome: e.Outcome.Outcome}
}
func (e MemberEvidence) Merged() bool {
	return e.ResolutionEvidence().Merged()
}

// Resolution is the shared member predicate for live history and archive pins.
func (e MemberEvidence) Resolution(state string) string {
	return e.ResolutionEvidence().Resolution(state)
}
func ArchiveResolved(a runtime.Archive) bool {
	if !a.Resolved || a.ResolutionEvidence != "merged" && a.ResolutionEvidence != "cancelled" {
		return false
	}
	return (MemberEvidence{OwnerMerge: a.ResolutionEvidence == "merged"}).Resolution(a.State) == a.ResolutionEvidence
}

// MergeIdentity returns only a verified merge commit, never a published head.
func (e MemberEvidence) MergeIdentity(upstream string) (commit, reference string) {
	if e.OwnerMerge {
		if e.Merge.Upstream.Commit != "" {
			return e.Merge.Upstream.Commit, fmt.Sprintf("%s@%s", upstream, e.Merge.Upstream.Commit)
		}
		return "", ""
	}
	if e.Merged() && e.Outcome.MergeCommit != "" && e.Publication.Upstream != "" && e.Publication.Fork != "" && e.Publication.Branch != "" {
		return e.Outcome.MergeCommit, fmt.Sprintf("%s#%d", e.Publication.Upstream, e.Publication.PullRequest)
	}
	return "", ""
}

// PinArchiveMerge keeps exact available lookup and merge evidence before cleanup.
func (e MemberEvidence) PinArchiveMerge(a *runtime.Archive, upstream string) {
	if e.Published && e.Publication.Upstream != "" && e.Publication.Fork != "" && e.Publication.Branch != "" {
		p := e.Publication
		a.Publication = runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}
	}
	if a.ResolutionEvidence == "merged" {
		a.MergeCommit, a.MergeReference = e.MergeIdentity(upstream)
	}
}
