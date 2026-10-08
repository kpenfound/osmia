package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const discoveryTool = "record_discovery"

type workDiscovery struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Criterion   string `json:"criterion"`
	Disposition string `json:"disposition"`
	Unit        string `json:"unit,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func discoveries(r *trace.Repository, stream config.WorkstreamID) (map[string]workDiscovery, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	out := map[string]workDiscovery{}
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "discoveries/") {
			continue
		}
		var x workDiscovery
		if err := json.Unmarshal([]byte(d.Content), &x); err != nil {
			return nil, err
		}
		out[x.ID] = x
	}
	return out, nil
}

// unresolvedWork includes necessary discoveries and revisions still being prepared.
func unresolvedWork(r *trace.Repository, stream config.WorkstreamID) (bool, error) {
	ds, err := discoveries(r, stream)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		switch d.Disposition {
		case "declined", "suggestion":
			continue
		case "assigned":
			st, err := r.Workflow(stream, trace.UnitSubject(d.Unit))
			if err != nil {
				return false, err
			}
			if st.Value == UnitMerged {
				continue
			}
		}
		return true, nil
	}
	requests, err := trace.Read[trace.Amendment](r, stream)
	if err != nil {
		return false, err
	}
	for _, a := range requests {
		st, err := r.Workflow(stream, amendmentSubject(a.ID))
		if err != nil {
			return false, err
		}
		if !slices.Contains([]string{amendmentRuled, amendmentApplied, "declined", amendmentRejected, amendmentUnapplied, "invalid"}, st.Value) {
			return true, nil
		}
	}
	return false, nil
}

func recordDiscovery(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: discoveryTool, Effect: coreadapter.ToolMemory, Description: "Record newly discovered work without ending your turn. Use a stable key to avoid duplicates, description and criterion (spec#n). Default disposition is open. The chief of staff may assign an existing discovery to a unit, decline it with a reason, or retain it as an optional suggestion. For updates pass id and revision from factory_context. Necessary open discoveries block completion; assigned discoveries remain required until their unit lands. Do not take another unit's work without coordination.", InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"},"id":{"type":"string"},"revision":{"type":"integer"},"description":{"type":"string"},"criterion":{"type":"string"},"disposition":{"type":"string","enum":["open","assigned","declined","suggestion"]},"unit":{"type":"string"},"reason":{"type":"string"}},"additionalProperties":false}`), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if err := r.ActiveTurn(scope); err != nil {
			return nil, err
		}
		var in struct {
			workDiscovery
			Key      string `json:"key"`
			Revision int    `json:"revision"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		if scope.Role != trace.ChiefOfStaff && scope.Role != masonRole && scope.Role != reviewerRole {
			return nil, errors.New("discovery recording is not granted")
		}
		stream := config.WorkstreamID(scope.Workstream)
		st, err := r.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return nil, err
		}
		if st.Value == DeliveredState || st.Value == AbandonedState {
			return priorityRefusal("terminal workstreams accept no discovered work")
		}
		docs, err := trace.Read[trace.Document](r, stream)
		if err != nil {
			return nil, err
		}
		var prior trace.Document
		requestedID := in.ID
		if in.ID == "" {
			if strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Description) == "" || strings.TrimSpace(in.Criterion) == "" {
				return priorityRefusal("key, description and criterion are required")
			}
			in.ID = fmt.Sprintf("discovery-%x", sha256.Sum256([]byte(scope.Thread+"\n"+in.Key)))
		}
		for _, d := range docs {
			if d.ID == in.ID && d.Revision > prior.Revision {
				prior = d
			}
		}
		if !strings.HasPrefix(in.ID, "discovery-") || len(in.ID) != 74 {
			return priorityRefusal("invalid discovery identity")
		}
		if prior.Revision > 0 && prior.Path != "discoveries/"+in.ID+".json" {
			return priorityRefusal("invalid discovery record")
		}
		if prior.Revision == 0 {
			if requestedID != "" {
				return priorityRefusal("discovery not found; create it with a stable key, description and criterion")
			}
			if err := questions.Resolve(ctx, r, stream, in.Criterion, now()); err != nil || !strings.HasPrefix(in.Criterion, "spec#") {
				return priorityRefusal("discovery requires an existing spec criterion")
			}
		}
		if prior.Revision > 0 && in.Revision == 0 {
			var existing workDiscovery
			if err := json.Unmarshal([]byte(prior.Content), &existing); err != nil {
				return nil, err
			}
			if in.Description != "" && in.Description != existing.Description || in.Criterion != "" && in.Criterion != existing.Criterion {
				return priorityRefusal("discovery key already names different work; use a distinct stable key")
			}
			return json.Marshal(map[string]any{"recorded": true, "id": in.ID, "revision": prior.Revision})
		}
		if in.Revision != prior.Revision {
			return priorityRefusal("discovery changed; read its latest revision")
		}
		if prior.Revision > 0 && scope.Role != trace.ChiefOfStaff {
			return priorityRefusal("the chief of staff coordinates discovery dispositions")
		}
		if in.Disposition == "" {
			in.Disposition = "open"
		}
		if in.Disposition != "open" && scope.Role != trace.ChiefOfStaff {
			return priorityRefusal("the chief of staff coordinates discovery dispositions")
		}
		if prior.Revision > 0 {
			var old workDiscovery
			if err := json.Unmarshal([]byte(prior.Content), &old); err != nil {
				return nil, err
			}
			in.Description = old.Description
			in.Criterion = old.Criterion
		}
		if !slices.Contains([]string{"open", "assigned", "declined", "suggestion"}, in.Disposition) {
			return nil, errors.New("invalid discovery disposition")
		}
		if in.Disposition != "open" && strings.TrimSpace(in.Reason) == "" {
			return priorityRefusal("a disposition requires reasoning against approved intent")
		}
		var assignment trace.WorkflowState
		if in.Disposition == "assigned" {
			st, err := r.Workflow(stream, trace.UnitSubject(in.Unit))
			if err != nil {
				return nil, err
			}
			assignment = st
			if st.Value == "" || st.Value == UnitMerged || st.Value == UnitApproved {
				return priorityRefusal("assign necessary work to an existing unapproved unit; revise the plan first if needed")
			}
		}
		data, _ := json.MarshalIndent(in.workDiscovery, "", "  ")
		rev := prior.Revision + 1
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: in.ID, Revision: rev, Project: r.Project(), Workstream: stream, At: now(), Actor: trace.Actor{Kind: "agent", ID: scope.Thread}, Cause: scope.Turn}, Path: "discoveries/" + in.ID + ".json", Content: string(data) + "\n"}
		tx := trace.Transaction{ExpectedVersion: uint64(prior.Revision), Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("%s-%d", in.ID, rev), Revision: 1, Project: r.Project(), Workstream: stream, At: now(), Actor: doc.Actor, Cause: scope.Turn}, Subject: in.ID, To: in.Disposition, Reason: in.Description + "; " + in.Reason}, Events: []trace.Event{trace.Notice(fmt.Sprintf("%s-%d", in.ID, rev), "discovery", in.Description+"; disposition: "+in.Disposition)}}
		if prior.Revision > 0 {
			var old workDiscovery
			_ = json.Unmarshal([]byte(prior.Content), &old)
			tx.Transition.From = old.Disposition
		}
		txs := []trace.Transaction{tx}
		if in.Disposition == "assigned" {
			h := tx.Transition.Header
			h.ID += "-assignment"
			h.Unit = in.Unit
			txs = append(txs, trace.Transaction{ExpectedVersion: assignment.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(in.Unit), From: assignment.Value, To: assignment.Value, Reason: "Necessary work assigned: " + in.Description}})
		}
		if _, err := r.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"recorded": true, "id": in.ID, "revision": rev})
	}}
}

// unitDiscoveries pins the necessary work the reviewer must account for.
func unitDiscoveries(r *trace.Repository, stream config.WorkstreamID, unit string) (string, string, error) {
	ds, err := discoveries(r, stream)
	if err != nil {
		return "", "", err
	}
	var assigned []workDiscovery
	for _, d := range ds {
		if d.Disposition == "assigned" && d.Unit == unit {
			assigned = append(assigned, d)
		}
	}
	if len(assigned) == 0 {
		return "", "", nil
	}
	slices.SortFunc(assigned, func(a, b workDiscovery) int { return strings.Compare(a.ID, b.ID) })
	data, err := json.MarshalIndent(assigned, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(data), fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
