package lifecycle

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// EdgeWaiverDocument and EdgeWaiverPath are the workstream Document that
// records the dependencies the dependent no longer waits on. It is written only
// to the dependent's own trace: the shared project spec keeps naming the edge,
// and the upstream's own trace is never written. A waiver is not merge
// evidence: the edge observation never records a waived edge merged.
const (
	EdgeWaiverDocument = "edge-waivers"
	EdgeWaiverPath     = "edge-waivers.json"
)

// BaseEdgeType names the dependency of a workstream on the same-repo base it
// builds on, as the Type of a waiver and in the subject of its decision. The
// edge types of the project spec's graph name the cross-repo dependencies.
const BaseEdgeType = "base"

// EdgeWaiver is one dropped dependency.
type EdgeWaiver struct {
	// Upstream is the workstream the dependent no longer waits on and Type
	// the kind of the dependency: an edge type of the graph, or BaseEdgeType.
	Upstream config.WorkstreamID `json:"upstream"`
	Type     string              `json:"type"`
	// Ended says why the dependency could not be met: abandoned, closed, or
	// owner when the owner dropped a dependency that had not ended.
	Ended string `json:"ended,omitempty"`
	// Reason is the decision's reason.
	Reason string `json:"reason"`
	// DecidedBy is who decided the drop (jev or owner) and Judgment the
	// judgment record when Jev did.
	DecidedBy string    `json:"decided_by"`
	Judgment  string    `json:"judgment,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	At        time.Time `json:"at"`
}

// EdgeWaivers is the content of EdgeWaiverDocument: every dependency the
// dependent dropped, in the order they were dropped.
type EdgeWaivers struct {
	Waivers []EdgeWaiver `json:"waivers"`
}

// Waives reports whether the waivers drop the dependency on upstream of the
// given type.
func (w EdgeWaivers) Waives(upstream config.WorkstreamID, edgeType string) bool {
	return slices.ContainsFunc(w.Waivers, func(x EdgeWaiver) bool { return x.Upstream == upstream && x.Type == edgeType })
}

// EdgeWaiversAt reads the latest recorded waivers of stream and the document
// revision they were read at (0 when none is recorded).
func EdgeWaiversAt(repo *trace.Repository, stream config.WorkstreamID) (EdgeWaivers, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return EdgeWaivers{}, 0, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == EdgeWaiverDocument {
			var w EdgeWaivers
			err := json.Unmarshal([]byte(d.Content), &w)
			return w, d.Revision, err
		}
	}
	return EdgeWaivers{}, 0, nil
}

// EdgeWaived reports whether stream dropped its dependency on upstream.
func EdgeWaived(repo *trace.Repository, stream, upstream config.WorkstreamID, edgeType string) (bool, error) {
	waivers, _, err := EdgeWaiversAt(repo, stream)
	return waivers.Waives(upstream, edgeType), err
}

// unwaived returns the edges the dependent still has: those no waiver in its
// trace drops.
func unwaived(repo *trace.Repository, stream config.WorkstreamID, edges []trace.Edge) ([]trace.Edge, error) {
	if len(edges) == 0 {
		return edges, nil
	}
	waivers, _, err := EdgeWaiversAt(repo, stream)
	if err != nil || len(waivers.Waivers) == 0 {
		return edges, err
	}
	var out []trace.Edge
	for _, e := range edges {
		if !waivers.Waives(e.From, e.Type) {
			out = append(out, e)
		}
	}
	return out, nil
}

// EffectiveIncomingEdges returns the incoming hard_block and merge_order edges
// that still bind stream: the edges of the workgroup's project spec minus those
// a waiver in the stream's own trace drops. Every reader of dependency edges
// (readiness, edge-wait reconciliation and polling, merge-order publication and
// status) reads them through it, so a waived edge stops blocking everywhere
// at once and is never reported or used as the upstream having merged.
func EffectiveIncomingEdges(repo *trace.Repository, stream config.WorkstreamID) ([]trace.Edge, error) {
	edges, err := AllIncomingMergeEdges(repo, stream)
	if err != nil {
		return nil, err
	}
	return unwaived(repo, stream, edges)
}

// UnresolvedEdges returns the edges of stream that still block it: effective
// edges whose upstream observed does not record merged.
func UnresolvedEdges(repo *trace.Repository, stream config.WorkstreamID, observed EdgeObservation) ([]trace.Edge, error) {
	edges, err := EffectiveIncomingEdges(repo, stream)
	if err != nil {
		return nil, err
	}
	var out []trace.Edge
	for _, e := range edges {
		if !AllEdgesResolved(observed, []trace.Edge{e}) {
			out = append(out, e)
		}
	}
	return out, nil
}

// WorkgroupMergeOrderWaived returns a deterministic summary of the incoming
// merge_order edges the stream dropped, sorted so the summary is stable. A final
// review pins it beside the merged edges: a waiver changes what the dependent's
// pull request waits on, so a review read before it is stale and a fresh final
// review and delivery approval are due. It is "" while no merge_order edge is
// waived. It never makes an edge merged: a waiver is not merge evidence.
func WorkgroupMergeOrderWaived(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	all, err := AllIncomingMergeEdges(repo, stream)
	if err != nil {
		return "", err
	}
	waivers, _, err := EdgeWaiversAt(repo, stream)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, e := range EdgesOfType(all, trace.EdgeMergeOrder) {
		if waivers.Waives(e.From, e.Type) {
			parts = append(parts, string(e.From))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ","), nil
}
