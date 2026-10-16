package service

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// memberEvidence is read from one document snapshot, including the current
// publication and its outcome. Publication readiness grants no merge evidence.
type memberEvidence struct {
	publication DeliveryPublication
	published   bool
	outcome     ownPullRequestOutcome
	merge       DeliveryMerge
	ownerMerge  bool
}

func memberEvidenceFrom(docs []trace.Document) (memberEvidence, error) {
	var e memberEvidence
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
				e.publication, e.published = p, true
			}
		case ownPullRequestOutcomeDocument:
			if err := json.Unmarshal([]byte(d.Content), &e.outcome); err != nil {
				return e, err
			}
		case mergeDocument:
			var m DeliveryMerge
			if err := json.Unmarshal([]byte(d.Content), &m); err != nil {
				return e, err
			}
			e.ownerMerge, e.merge = true, m
		}
	}
	return e, nil
}

func readMemberEvidence(repo *trace.Repository, stream config.WorkstreamID) (memberEvidence, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return memberEvidence{}, err
	}
	return memberEvidenceFrom(docs)
}

func (e memberEvidence) resolutionEvidence() trace.MemberResolutionEvidence {
	var pr int
	if e.published {
		pr = e.publication.PullRequest
	}
	return trace.MemberResolutionEvidence{OwnerMerge: e.ownerMerge, Publication: pr, OutcomePullRequest: e.outcome.PullRequest, Outcome: e.outcome.Outcome}
}

func (e memberEvidence) merged() bool {
	return e.resolutionEvidence().Merged()
}

// resolution is the shared member predicate for live history and archive pins.
func (e memberEvidence) resolution(state string) string {
	return e.resolutionEvidence().Resolution(state)
}

func archiveResolved(a runtime.Archive) bool {
	if !a.Resolved || a.ResolutionEvidence != "merged" && a.ResolutionEvidence != "cancelled" {
		return false
	}
	return (memberEvidence{ownerMerge: a.ResolutionEvidence == "merged"}).resolution(a.State) == a.ResolutionEvidence
}

// mergeIdentity returns only a verified merge commit, never a published head.
func (e memberEvidence) mergeIdentity(upstream string) (commit, reference string) {
	if e.ownerMerge {
		if e.merge.Upstream.Commit != "" {
			return e.merge.Upstream.Commit, fmt.Sprintf("%s@%s", upstream, e.merge.Upstream.Commit)
		}
		return "", ""
	}
	if e.merged() && e.outcome.MergeCommit != "" && e.publication.Upstream != "" && e.publication.Fork != "" && e.publication.Branch != "" {
		return e.outcome.MergeCommit, fmt.Sprintf("%s#%d", e.publication.Upstream, e.publication.PullRequest)
	}
	return "", ""
}

// pinArchiveMerge keeps exact available lookup and merge evidence before cleanup.
func (e memberEvidence) pinArchiveMerge(a *runtime.Archive, upstream string) {
	if e.published && e.publication.Upstream != "" && e.publication.Fork != "" && e.publication.Branch != "" {
		p := e.publication
		a.Publication = runtime.ArchivePublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}
	}
	if a.ResolutionEvidence == "merged" {
		a.MergeCommit, a.MergeReference = e.mergeIdentity(upstream)
	}
}
