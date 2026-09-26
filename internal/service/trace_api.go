package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
)

// TraceSummary identifies the sealed revisions, criteria and units of a
// workstream. Unit walks retain their evidence and explicit missing links.
type TraceSummary struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Feature    string              `json:"feature"`
	Seal       *TraceSeal          `json:"seal,omitempty"`
	Spec       *TraceRef           `json:"spec,omitempty"`
	Plan       *TraceRef           `json:"plan,omitempty"`
	Criteria   []TraceCriterion    `json:"criteria"`
	Units      []UnitTrace         `json:"units"`
	Delivery   *TraceDelivery      `json:"delivery,omitempty"`
	Gaps       []TraceGap          `json:"gaps"`
	Complete   bool                `json:"complete"`
}

// changeReadTimeout bounds the read of a commit's change ID in a trace walk.
const changeReadTimeout = 5 * time.Second

func (s *Service) traceView(ctx context.Context, raw, kind, selector string) (any, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return nil, api
	}
	if kind != "" && selector == "" {
		return nil, &APIError{Validation, "trace selector must not be empty"}
	}
	var out any
	var err error
	switch kind {
	case "":
		var w *traceWalk
		w, err = loadTraceWalk(repository, stream)
		if err == nil {
			v := TraceSummary{Project: w.project, Workstream: stream, Feature: w.feature, Seal: w.seal, Criteria: []TraceCriterion{}, Units: []UnitTrace{}, Gaps: append([]TraceGap{}, w.gaps...)}
			if w.specDoc != nil {
				ref := refOf(*w.specDoc)
				v.Spec = &ref
				for _, c := range w.spec.Criteria {
					v.Criteria = append(v.Criteria, TraceCriterion{Criterion: plan.Cite(c.Number), Text: c.Text})
				}
			}
			if w.planDoc != nil {
				ref := refOf(*w.planDoc)
				v.Plan = &ref
			}
			for _, unit := range w.units {
				u, _ := w.unit(unit.unit.ID, "")
				u.Gaps = append(append([]TraceGap{}, w.gaps...), u.Gaps...)
				u.Complete = len(u.Gaps) == 0
				v.Units = append(v.Units, u)
			}
			if w.planDoc != nil {
				for _, c := range v.Criteria {
					if !slices.ContainsFunc(v.Units, func(u UnitTrace) bool {
						return slices.ContainsFunc(u.Addresses, func(a TraceAddress) bool { return a.Criterion == c.Criterion })
					}) {
						v.Gaps = append(v.Gaps, TraceGap{Link: "units addressing " + c.Criterion, State: LinkUnavailable, Reason: fmt.Sprintf("no unit of plan.json revision %d or a follow-up addresses the criterion", w.planDoc.Revision)})
					}
				}
			}
			var gaps []TraceGap
			v.Delivery, _, gaps = w.delivery()
			v.Gaps = append(v.Gaps, gaps...)
			v.Complete = len(v.Gaps) == 0
			for _, u := range v.Units {
				v.Complete = v.Complete && u.Complete
			}
			out = v
		}
	case "unit":
		out, err = traceUnit(repository, stream, selector)
	case "criterion":
		if _, ok := plan.ParseCitation(selector); !ok {
			return nil, &APIError{Validation, "criterion must be cited as spec#<n>"}
		}
		out, err = traceCriterion(repository, stream, selector)
	case "commit":
		if !commitPattern.MatchString(selector) {
			return nil, &APIError{Validation, "commit must be a full lowercase commit ID"}
		}
		// A rebased or squashed commit can be found by its operation trailer.
		// A recorded commit may no longer be present in the clone, so the
		// trace walk still searches its durable records in that case.
		message := ""
		if g, readErr := featureWorkspaces(s.current(), repository).of(stream); readErr == nil {
			if commit, readErr := g.Commit(ctx, selector); readErr == nil {
				message = commit.Message
			}
		}
		// On Jujutsu a unit's commits carry the unit's change ID, which
		// finds the unit of a commit no record names.
		change := ""
		if g, readErr := newUnitWorkspaces(s.current(), repository).of(stream); readErr == nil {
			read, cancel := context.WithTimeout(ctx, changeReadTimeout)
			if c, readErr := g.ChangeOf(read, selector); readErr == nil {
				change = c
			}
			cancel()
		}
		out, err = traceCommit(repository, stream, selector, message, change)
	default:
		return nil, &APIError{Validation, "trace selector must be unit, criterion or commit"}
	}
	if errors.Is(err, errTraceNotFound) {
		return nil, &APIError{NotFound, fmt.Sprintf("%s %s is not in the workstream trace", kind, selector)}
	}
	if err != nil {
		return nil, &APIError{Internal, fmt.Sprintf("cannot read trace of workstream %s; check the trace repository", stream)}
	}
	return out, nil
}

func splitTracePath(path string) (string, string, string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "", "", true
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}
