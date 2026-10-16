package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// edgeWaitSubject is the workflow subject that parks a hard-blocked
// workstream: waiting while an incoming hard_block edge's upstream pull
// request has not merged, available once it has. A merge-order edge never
// sets this subject to waiting: that edge type lets both workstreams build
// in parallel, and only the dependent's own pull request's readiness waits
// on the merge (spec#11). A contract edge is never recorded here either: it
// resolves the moment the ratified project spec that carries it exists, so
// there is nothing to poll or wait for.
const edgeWaitSubject = "edge-wait"

// edgeObservationDocument and edgeObservationPath are the workstream
// document that records the latest known merge resolution of every
// incoming hard_block and merge_order edge, written only to the dependent's
// own trace: the upstream's own trace is only ever read, never written,
// so nothing about an edge's resolution becomes authoritative in two
// places (charter#2).
const (
	edgeObservationDocument = "edge-observation"
	edgeObservationPath     = "edge-observation.json"
)

// edgeMergeStatus is one incoming edge's last observed merge resolution.
type edgeMergeStatus struct {
	Upstream    config.WorkstreamID `json:"upstream"`
	Type        string              `json:"type"`
	Merged      bool                `json:"merged"`
	Reference   string              `json:"reference,omitempty"`
	Commit      string              `json:"commit,omitempty"`
	BuildBranch string              `json:"build_branch,omitempty"`
}

// edgeObservation is the content of edgeObservationDocument.
type edgeObservation struct {
	Edges []edgeMergeStatus `json:"edges"`
}

// edgeObservationAt reads the latest recorded edge observation of stream,
// the document revision it holds (0 when none is recorded yet) and when it
// was recorded.
func edgeObservationAt(repo *trace.Repository, stream config.WorkstreamID) (edgeObservation, int, time.Time, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return edgeObservation{}, 0, time.Time{}, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == edgeObservationDocument {
			var o edgeObservation
			err := json.Unmarshal([]byte(d.Content), &o)
			return o, d.Revision, d.At, err
		}
	}
	return edgeObservation{}, 0, time.Time{}, nil
}

// incomingMergeEdges returns stream's incoming hard_block and merge_order
// edges from the ratified project spec of the workgroup it belongs to, or
// none while it belongs to no workgroup or that workgroup's graph names
// none. A contract edge is never returned: it resolves the moment the
// project spec that carries it is ratified, which is already true by the
// time any caller can read it, so it never needs to wait or be observed.
func incomingMergeEdges(repo *trace.Repository, stream config.WorkstreamID) ([]trace.Edge, error) {
	approved, err := repo.GroupApproved(stream)
	if err != nil || !approved {
		return nil, err
	}
	spec, _, found, err := repo.ProjectSpecOf(stream)
	if err != nil || !found {
		return nil, err
	}
	var out []trace.Edge
	for _, e := range spec.Graph {
		if e.To == stream && (e.Type == trace.EdgeHardBlock || e.Type == trace.EdgeMergeOrder) {
			out = append(out, e)
		}
	}
	return out, nil
}

// edgesOfType filters edges to those of one type.
func edgesOfType(edges []trace.Edge, kind string) []trace.Edge {
	var out []trace.Edge
	for _, e := range edges {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

// firstUnresolvedEdge returns the reference to show the owner for the
// first of edges that observed does not yet record merged, and whether one
// was found. The reference prefers the upstream's own pull request
// ("owner/repo#123"), recorded the first time the upstream's own
// publication is read, and falls back to the bare upstream workstream ID
// before that is known.
func firstUnresolvedEdge(edges []trace.Edge, observed edgeObservation) (reference string, found bool) {
	for _, e := range edges {
		i := slices.IndexFunc(observed.Edges, func(s edgeMergeStatus) bool { return s.Upstream == e.From && s.Type == e.Type })
		if i >= 0 && observed.Edges[i].Merged {
			continue
		}
		reference = string(e.From)
		if i >= 0 && observed.Edges[i].Reference != "" {
			reference = observed.Edges[i].Reference
		}
		return reference, true
	}
	return "", false
}

// edgeWaitMessage is the plain-words wait status an unresolved edge shows
// in status and API: never an inbox entry or a notification (spec#15).
func edgeWaitMessage(reference string) string {
	return fmt.Sprintf("waiting on owner: %s merge", reference)
}

// edgeWaiting reports whether stream is held by an unresolved incoming
// hard_block edge: the only edge type that stops a dependent from starting
// any unit before its upstream merges (spec#11).
func edgeWaiting(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	approved, err := repo.GroupApproved(stream)
	if err != nil || !approved {
		return true, err
	}
	edges, err := incomingMergeEdges(repo, stream)
	if err != nil {
		return true, err
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	_, waiting := firstUnresolvedEdge(edgesOfType(edges, trace.EdgeHardBlock), observed)
	return waiting, err
}

// edgeWaitDisplay returns the plain-words wait status of stream's first
// unresolved incoming edge of any waiting type (hard_block or
// merge_order), or "" when it has none. It is read fresh from the latest
// edge observation on every call, so it never depends on edgeWaitPass
// having just run.
func edgeWaitDisplay(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	edges, err := incomingMergeEdges(repo, stream)
	if err != nil || len(edges) == 0 {
		return "", err
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	if err != nil {
		return "", err
	}
	if reference, found := firstUnresolvedEdge(edges, observed); found {
		return edgeWaitMessage(reference), nil
	}
	return "", nil
}

// unresolvedMergeOrderEdge reports whether stream has an incoming
// merge_order edge whose upstream has not yet merged, and the reference to
// show the owner for the first one found.
func pendingMergeOrderReferences(repo *trace.Repository, stream config.WorkstreamID) ([]string, error) {
	edges, err := incomingMergeEdges(repo, stream)
	if err != nil {
		return nil, err
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	if err != nil {
		return nil, err
	}
	var references []string
	for _, edge := range edgesOfType(edges, trace.EdgeMergeOrder) {
		if reference, waiting := firstUnresolvedEdge([]trace.Edge{edge}, observed); waiting {
			references = append(references, reference)
		}
	}
	return references, nil
}

func edgePublicationWait(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	references, err := pendingMergeOrderReferences(repo, stream)
	if err != nil {
		return "", err
	}
	for _, reference := range references {
		if !strings.Contains(reference, "#") {
			return edgeWaitMessage(reference), nil
		}
	}
	return "", nil
}

func unresolvedMergeOrderEdge(repo *trace.Repository, stream config.WorkstreamID) (reference string, waiting bool, err error) {
	edges, err := incomingMergeEdges(repo, stream)
	if err != nil {
		return "", false, err
	}
	order := edgesOfType(edges, trace.EdgeMergeOrder)
	if len(order) == 0 {
		return "", false, nil
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	if err != nil {
		return "", false, err
	}
	reference, found := firstUnresolvedEdge(order, observed)
	return reference, found, nil
}

// workgroupMergeOrderResolved returns a deterministic summary of which of
// stream's incoming merge_order edges the latest edge observation records
// merged, and at what reference, sorted so the summary is stable. A final
// review's EdgeResolved field pins this: when it later differs, a
// merge_order edge has merged since that review, which the dependent must
// update its dependency reference for and a fresh final review and owner
// approval must authorise (spec#13). It is "" while no merge_order edge has
// merged, so an ordinary review of a workstream with no such edge, or
// whose edges are all still unresolved, is never treated as edge-dependent.
func workgroupMergeOrderResolved(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	edges, err := incomingMergeEdges(repo, stream)
	if err != nil {
		return "", err
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, e := range edgesOfType(edges, trace.EdgeMergeOrder) {
		i := slices.IndexFunc(observed.Edges, func(s edgeMergeStatus) bool { return s.Upstream == e.From && s.Type == e.Type })
		if i < 0 || !observed.Edges[i].Merged {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s@%s", e.From, observed.Edges[i].Reference, observed.Edges[i].Commit))
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}

// workgroupEdgePending reports whether a delivered workstream's own current
// pull request is still recorded a draft from a workgroup merge-order edge
// that has since merged, so a fresh maintenance review, delivery approval
// and publication are due to mark it ready. Like the same-repo drift
// pending flag it mirrors, this stays true until the dependent's own
// publication is refreshed, never merely until a fresh final review is
// recorded: recording that review alone (before the fresh delivery approval
// that actually re-publishes) must still present as pending maintenance, so
// the owner can approve it. This same pending state admits in-scope
// follow-up repairs and their landing. Once the approved publication is
// ready, later same-repo delivery maintenance requires a new workstream
// for repairs even though the edge's merge remains recorded.
func workgroupEdgePending(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	resolved, err := workgroupMergeOrderResolved(repo, stream)
	if err != nil || resolved == "" {
		return false, err
	}
	publication, found, err := currentOwnPublication(repo, stream)
	if err != nil || !found {
		return false, err
	}
	return publication.Draft, nil
}

// upstreamMergeStatus reads verified upstream merge evidence or polls its
// current publication. Archived members use pinned merge identity after cleanup
// or their retained trace while available. Retirement itself grants no merge.
// The caller records the observation only in the dependent's trace.
func (s *Service) upstreamMergeStatus(ctx context.Context, repo *trace.Repository, edge trace.Edge) (edgeMergeStatus, error) {
	out := edgeMergeStatus{Upstream: edge.From, Type: edge.Type}
	archive, archived := s.archivedWorkstream(string(edge.From))
	// Cleanup can evict and close a scoped child between separate reads. Only
	// durable cleanup evidence or confirmed deletion explains a failed read;
	// a retained, unreadable upstream must still report its original error.
	readError := func(readErr error) (edgeMergeStatus, error) {
		if !archived {
			return out, readErr
		}
		latest, found := s.archivedWorkstream(string(edge.From))
		if !found {
			return out, readErr
		}
		if latest.CleanupStartedAt.IsZero() && latest.CleanedAt.IsZero() {
			gone, err := trace.WorkstreamTraceGone(s.current().Root, latest.Project, edge.From)
			if err != nil {
				return out, errors.Join(readErr, err)
			}
			if !gone {
				return out, readErr
			}
		}
		return s.archivedEdgeMergeStatus(ctx, edgeMergeStatus{Upstream: edge.From, Type: edge.Type}, latest)
	}
	upstream := repo
	if archived {
		if (archiveResolved(archive) && archive.ResolutionEvidence == "merged" && archive.MergeCommit != "" && archive.MergeReference != "") || !archive.CleanupStartedAt.IsZero() || !archive.CleanedAt.IsZero() {
			return s.archivedEdgeMergeStatus(ctx, out, archive)
		}
		var err error
		upstream, err = s.repository(archive.Project)
		if err != nil {
			return readError(err)
		}
	}
	if streams, err := upstream.Workstreams(); err != nil {
		return readError(err)
	} else if !slices.Contains(streams, edge.From) {
		if archived {
			return readError(fmt.Errorf("archived upstream %s is absent from its retained trace", edge.From))
		}
		_, _, found, api := s.conversationTrace(string(edge.From))
		if api != nil {
			return out, nil
		}
		upstream = found
	}
	if archived {
		if err := s.step("edge-archive-retained-read"); err != nil {
			return out, err
		}
	}
	feature, err := upstream.Workflow(edge.From, trace.FeatureSubject)
	if err != nil {
		return readError(err)
	}
	if feature.Value != DeliveredState {
		if edge.Type == trace.EdgeMergeOrder {
			out.BuildBranch = fmt.Sprintf("%s@%s", s.about(upstream).Project.PushRepository(), featureBranch(edge.From))
		}
		return out, nil
	}
	if merge, ok, err := deliveryMerge(upstream, edge.From); err != nil {
		return readError(err)
	} else if ok {
		out.Merged, out.Commit = true, merge.Upstream.Commit
		out.Reference = fmt.Sprintf("%s@%s", s.about(upstream).Project.Upstream, merge.Upstream.Commit)
		return out, nil
	}
	publication, found, err := currentOwnPublication(upstream, edge.From)
	if err != nil {
		return readError(err)
	}
	if !found {
		return out, nil
	}
	out.Reference = fmt.Sprintf("%s#%d", publication.Upstream, publication.PullRequest)
	out.BuildBranch = fmt.Sprintf("%s@%s", publication.Fork, publication.Commit)
	if outcome, revision, err := ownPullRequestOutcomeAt(upstream, edge.From); err != nil {
		return readError(err)
	} else if revision > 0 && outcome.PullRequest == publication.PullRequest {
		if outcome.Outcome == ownPullRequestOutcomeClosed {
			return out, nil
		}
		if outcome.Outcome == ownPullRequestOutcomeMerged {
			out.Merged, out.Commit, out.BuildBranch = true, outcome.MergeCommit, ""
			if out.Commit != "" {
				return out, nil
			}
		}
	}
	return s.pollEdgeMerge(ctx, out, publication)
}

// archivedEdgeMergeStatus uses only the archive's verified merge pins or its
// exact publication. Archive and cleanup alone never release a dependency.
func (s *Service) archivedEdgeMergeStatus(ctx context.Context, out edgeMergeStatus, archive runtime.Archive) (edgeMergeStatus, error) {
	if archiveResolved(archive) && archive.ResolutionEvidence == "merged" && archive.MergeCommit != "" && archive.MergeReference != "" {
		out.Merged, out.Commit, out.Reference = true, archive.MergeCommit, archive.MergeReference
		return out, nil
	}
	if p := archive.Publication; p.PullRequest > 0 {
		publication := DeliveryPublication{Upstream: p.Upstream, Fork: p.Fork, Branch: p.Branch, Commit: p.Commit, PullRequest: p.PullRequest}
		return s.pollEdgeMerge(ctx, out, publication)
	}
	return out, nil
}

// pollEdgeMerge also supports archived publications whose trace is gone.
func (s *Service) pollEdgeMerge(ctx context.Context, out edgeMergeStatus, publication DeliveryPublication) (edgeMergeStatus, error) {
	out.Reference = fmt.Sprintf("%s#%d", publication.Upstream, publication.PullRequest)
	if !out.Merged {
		out.BuildBranch = fmt.Sprintf("%s@%s", publication.Fork, publication.Commit)
	}
	client := s.options.PullRequests
	if client == nil {
		return out, nil
	}
	found2, err := client.Find(ctx, publication.Upstream, publication.Fork, publication.Branch)
	if err != nil {
		return out, err
	}
	for _, pr := range found2 {
		if pr.Number != publication.PullRequest {
			continue
		}
		out.Merged = out.Merged || pr.Merged
		if out.Merged {
			out.Commit = pr.MergeCommit
			out.BuildBranch = ""
		}
	}
	return out, nil
}

// edgeResolverPass refreshes, for every workstream of repository with an
// incoming hard_block or merge_order edge, the latest known merge
// resolution of each: it polls, through upstreamMergeStatus, no more often
// than once a minute, and never again once every one of a workstream's
// incoming edges is recorded merged, since merge is a one-way signal that
// never needs to be observed again (owner ruling 3).
func (s *Service) edgeResolverPass(ctx context.Context, repo *trace.Repository) error {
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, archived := s.archivedWorkstream(string(stream)); archived {
			continue
		}
		if session, err := isSession(repo, stream); err != nil {
			return err
		} else if session {
			continue
		}
		edges, err := incomingMergeEdges(repo, stream)
		if err != nil {
			return err
		}
		if len(edges) == 0 {
			continue
		}
		observed, revision, at, err := edgeObservationAt(repo, stream)
		if err != nil {
			return err
		}
		commitsKnown := !slices.ContainsFunc(observed.Edges, func(edge edgeMergeStatus) bool {
			return edge.Type == trace.EdgeMergeOrder && edge.Merged && edge.Commit == ""
		})
		if revision > 0 && allEdgesResolved(observed, edges) && commitsKnown {
			if err := s.edgeUpdatePass(ctx, repo, stream); err != nil {
				return err
			}
			continue
		}
		if revision > 0 && s.now().Sub(at) < time.Minute {
			continue
		}
		var next edgeObservation
		for _, e := range edges {
			prior := slices.IndexFunc(observed.Edges, func(status edgeMergeStatus) bool { return status.Upstream == e.From && status.Type == e.Type })
			if prior >= 0 && observed.Edges[prior].Merged && (e.Type != trace.EdgeMergeOrder || observed.Edges[prior].Commit != "") {
				next.Edges = append(next.Edges, observed.Edges[prior])
				continue
			}
			status, err := s.upstreamMergeStatus(ctx, repo, e)
			if err != nil {
				return err
			}
			if status.Merged {
				if err := s.step("edge-merge-observed"); err != nil {
					return err
				}
			}
			if prior >= 0 {
				status.Merged = status.Merged || observed.Edges[prior].Merged
			}
			next.Edges = append(next.Edges, status)
		}
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: edgeObservationDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: s.now(), Actor: foremanActor, Cause: "edge-refresh"}
		if err := repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: edgeObservationPath, Content: string(data)}}); err != nil {
			return err
		}
		if err := s.edgeUpdatePass(ctx, repo, stream); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) edgeUpdatePass(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) error {
	resolved, err := workgroupMergeOrderResolved(repo, stream)
	if err != nil || resolved == "" {
		return err
	}
	latest, _, found, err := seal.Latest(repo, stream)
	if err != nil || !found {
		return err
	}
	digest := sha256.Sum256([]byte(resolved))
	id := fmt.Sprintf("dependency-update-%x", digest[:6])
	added, err := followup.Read(repo, stream)
	if err != nil || slices.ContainsFunc(added, func(unit followup.Unit) bool { return unit.Unit.ID == id }) {
		return err
	}
	graph, err := sealedPlan(repo, stream, latest.Revision.Plan)
	if err != nil {
		return err
	}
	sealedPlan, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return err
	}
	var footprint, criteria []string
	for _, unit := range sealedPlan.Units {
		for _, entity := range unit.Footprint {
			if !slices.Contains(footprint, entity) {
				footprint = append(footprint, entity)
			}
		}
		for _, criterion := range unit.Criteria {
			if !slices.Contains(criteria, criterion) {
				criteria = append(criteria, criterion)
			}
		}
	}
	if len(criteria) == 0 {
		return fmt.Errorf("dependent %s has no sealed criteria for its dependency update", stream)
	}
	observed, _, _, err := edgeObservationAt(repo, stream)
	if err != nil {
		return err
	}
	for _, edge := range observed.Edges {
		if edge.Type == trace.EdgeMergeOrder && edge.Merged && edge.Commit == "" {
			return fmt.Errorf("merged upstream %s has no recorded merge commit", edge.Reference)
		}
	}
	unit := followup.Unit{Criterion: criteria[0], Gap: "Update dependency references to the merged upstream commits: " + resolved,
		Unit: plan.Unit{ID: id, Title: "Update merged dependencies", Task: "Update dependency references to the merged upstream commits: " + resolved + ". Repair within the ratified slice; ask the chief of staff only if the repair contradicts or extends the design.", Criteria: criteria, Footprint: footprint, DependsOn: []string{}, Acceptance: []string{"Dependency references use the merged upstream commits", "Checks and the ratified slice still hold"}}}
	content, err := json.Marshal([]followup.Unit{unit})
	if err != nil {
		return err
	}
	revision, err := nextRevision(repo, stream, followup.DocumentID)
	if err != nil {
		return err
	}
	at := s.now()
	header := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: followup.DocumentID, Revision: revision, Project: repo.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: id}
	subject := trace.UnitSubject(id)
	transition := header
	transition.Schema, transition.Unit, transition.Revision = "osmia.trace.transition", id, 1
	transition.ID = subject + "-" + UnitPlanned
	planned := trace.Transaction{Transition: trace.Transition{Header: transition, Subject: subject, To: UnitPlanned, Reason: unit.Gap}}
	transition.ID = subject + "-" + UnitReady
	ready := trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: transition, Subject: subject, From: UnitPlanned, To: UnitReady, Reason: unit.Gap}}
	_, err = repo.RecordDocumentsWith(ctx, []trace.Document{{Header: header, Path: followup.Path, Content: string(content)}}, planned, ready)
	return err
}

// allEdgesResolved reports whether observed records every one of edges
// merged.
func allEdgesResolved(observed edgeObservation, edges []trace.Edge) bool {
	for _, e := range edges {
		i := slices.IndexFunc(observed.Edges, func(s edgeMergeStatus) bool { return s.Upstream == e.From && s.Type == e.Type })
		if i < 0 || !observed.Edges[i].Merged {
			return false
		}
	}
	return true
}

// edgeWaitPass keeps edgeWaitSubject current for every workstream of
// repository: waiting while an incoming hard_block edge is unresolved,
// available once every one is. The transition carries the owner-facing
// wait message as its reason, and a feed notice, exactly as baseWaitPass
// records a same-repo base's availability; neither creates an inbox entry,
// because waiting on a merge is status, never an escalation (spec#15).
func (s *Service) edgeWaitPass(ctx context.Context, repo *trace.Repository) error {
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		edges, err := incomingMergeEdges(repo, stream)
		if err != nil {
			return err
		}
		blocking := edgesOfType(edges, trace.EdgeHardBlock)
		observed, _, _, err := edgeObservationAt(repo, stream)
		if err != nil {
			return err
		}
		reason := ""
		if reference, found := firstUnresolvedEdge(blocking, observed); found {
			reason = edgeWaitMessage(reference)
		}
		state, err := repo.Workflow(stream, edgeWaitSubject)
		if err != nil {
			return err
		}
		next := "available"
		if reason != "" {
			next = "waiting"
		}
		if state.Value == next || state.Value == "" && next == "available" {
			continue
		}
		id := fmt.Sprintf("%s-%d", edgeWaitSubject, state.Version+1)
		if reason == "" {
			reason = "the workstream's blocking edges resolved"
		}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: s.now(), Actor: foremanActor, Cause: "edge-availability"}
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: edgeWaitSubject, From: state.Value, To: next, Reason: reason}, Events: []trace.Event{trace.Notice(id, "edge", reason)}})
		if err != nil {
			return err
		}
	}
	return nil
}
