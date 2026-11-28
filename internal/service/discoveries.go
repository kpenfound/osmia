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
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const discoveryTool = "record_discovery"

type workDiscovery struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	Criterion     string `json:"criterion"`
	Disposition   string `json:"disposition"`
	Unit          string `json:"unit,omitempty"`
	Reason        string `json:"reason,omitempty"`
	DecidedBy     string `json:"decided_by,omitempty"`
	Judgment      string `json:"judgment,omitempty"`
	HandedToOwner string `json:"handed_to_owner,omitempty"`
}

var discoveryOutcomes = map[string]string{"assigned": "assign", "declined": "decline", "suggestion": "retain"}

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

func unresolvedWork(r *trace.Repository, stream config.WorkstreamID) (bool, error) {
	if due, err := lifecycle.DependencyUpdateDue(r, stream); err != nil || due {
		return due, err
	}
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
		if !slices.Contains([]string{amendmentRuled, intake.AmendmentApplied, "declined", intake.AmendmentRejected, intake.AmendmentUnapplied, "invalid"}, st.Value) {
			return true, nil
		}
	}
	return false, nil
}

func recordDiscovery(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	description := "Record work you found that the plan does not cover, without ending your turn. Give key (a stable name of your own, so recording the same work again does not duplicate it), description, and criterion (the spec#<n> it serves). It is recorded as open. A necessary open discovery blocks completion of the workstream, and an assigned one stays required until its unit lands. " +
		"Do not take another unit's work without coordination: the chief of staff assigns discoveries. " +
		"Only the chief of staff changes a discovery: pass its id and revision, as factory_context reads them, with a disposition (assigned with unit, declined, suggestion for optional work, or open) and a reason against the approved intent. Call decide for the discovery first (kind discovery-disposition, subject its id). When Jev's decision is binding, only the disposition Jev picked is recorded, with your reasoning."
	properties := `"key":{"type":"string"},"id":{"type":"string"},"revision":{"type":"integer"},"description":{"type":"string"},"criterion":{"type":"string"},"disposition":{"type":"string","enum":["open","assigned","declined","suggestion"]},"unit":{"type":"string"},"reason":{"type":"string"}`
	if scope.Role == trace.Manager {
		description += " workstream: the member workstream ID of your workgroup the discovery belongs to, as factory_context read it. Omit it for this conversation's own workstream. owner_decided: true only to record a disposition the owner gave in the message this turn answers; that needs no decide result."
		properties += `,"owner_decided":{"type":"boolean"}`
	}
	return coreadapter.Tool{Name: discoveryTool, Effect: coreadapter.ToolMemory, Description: description, InputSchema: memberSchema(scope, properties), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if err := r.ActiveTurn(scope); err != nil {
			return nil, err
		}
		var in struct {
			workDiscovery
			Key          string              `json:"key"`
			Revision     int                 `json:"revision"`
			Workstream   config.WorkstreamID `json:"workstream"`
			OwnerDecided bool                `json:"owner_decided"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		in.DecidedBy, in.Judgment, in.HandedToOwner = "", "", ""
		if scope.Role != trace.Manager && scope.Role != masonRole && scope.Role != reviewerRole {
			return nil, errors.New("discovery recording is not granted to this role")
		}
		target, stream, refusal, err := chiefMember(r, scope, in.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		project := target.Project()
		st, err := target.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return nil, err
		}
		if intake.Finished(st.Value) {
			return priorityRefusal("terminal workstreams accept no discovered work")
		}
		docs, err := trace.Read[trace.Document](target, stream)
		if err != nil {
			return nil, err
		}
		var prior trace.Document
		requestedID := in.ID
		if in.ID == "" {
			if strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Description) == "" || strings.TrimSpace(in.Criterion) == "" {
				return priorityRefusal("key, description and criterion are all required to record new work")
			}
			in.ID = fmt.Sprintf("discovery-%x", sha256.Sum256([]byte(scope.Thread+"\n"+in.Key)))
		}
		for _, d := range docs {
			if d.ID == in.ID && d.Revision > prior.Revision {
				prior = d
			}
		}
		if !strings.HasPrefix(in.ID, "discovery-") || len(in.ID) != 74 {
			return priorityRefusal("invalid discovery id; pass an id exactly as factory_context lists it under discoveries")
		}
		if prior.Revision > 0 && prior.Path != "discoveries/"+in.ID+".json" {
			return priorityRefusal("that id does not name a discovery; pass an id factory_context lists under discoveries")
		}
		if prior.Revision == 0 {
			if requestedID != "" {
				return priorityRefusal("discovery not found; create it with a stable key, description and criterion")
			}
			if err := questions.Resolve(ctx, target, stream, in.Criterion); err != nil || !strings.HasPrefix(in.Criterion, "spec#") {
				return priorityRefusal("criterion must be an existing spec#<n> of this workstream; read spec.md with factory_context to find it")
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
			return priorityRefusal("discovery changed since you read it; read its latest revision with factory_context and pass that revision")
		}
		if prior.Revision > 0 && scope.Role != trace.Manager {
			return priorityRefusal("only the chief of staff changes a discovery or gives it a disposition other than open; a discovery you record stays open until the chief decides it")
		}
		if in.Disposition == "" {
			in.Disposition = "open"
		}
		if in.Disposition != "open" && scope.Role != trace.Manager {
			return priorityRefusal("only the chief of staff changes a discovery or gives it a disposition other than open; a discovery you record stays open until the chief decides it")
		}
		var old workDiscovery
		if prior.Revision > 0 {
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
			return priorityRefusal("a disposition other than open needs a reason; say why it follows from the approved intent")
		}
		if in.OwnerDecided {
			if prior.Revision == 0 || scope.Role != trace.Manager {
				return priorityRefusal("owner_decided names an existing discovery, and only the chief of staff relays the owner's disposition")
			}
			if _, owner, err := r.OwnerTurn(trace.Manager, scope); err != nil {
				return nil, err
			} else if !owner {
				return priorityRefusal("owner_decided is only for a disposition the owner gave in the message this turn answers")
			}
		}
		var bound *jev.Result
		switch {
		case in.OwnerDecided:
		case in.Disposition == "open":
			in.DecidedBy, in.Judgment, in.HandedToOwner = old.DecidedBy, old.Judgment, old.HandedToOwner
		case prior.Revision > 0:
			if old.HandedToOwner != "" {
				return priorityRefusal("Jev's decision on this discovery could not be carried out; the owner decides it")
			}
			res, err := jev.Recorded(target, stream, jev.KindDiscovery, jev.DiscoveryKey(in.ID))
			if err != nil {
				return nil, err
			}
			if old.Disposition == "open" || res.Status != jev.None {
				var why string
				if bound, why, err = jev.Require(target, stream, jev.KindDiscovery, jev.DiscoveryKey(in.ID), discoveryOutcomes[in.Disposition], "discovery "+in.ID); err != nil {
					return nil, err
				} else if why != "" {
					return priorityRefusal(why)
				}
			}
		}
		var assignment trace.WorkflowState
		handed := ""
		if in.Disposition == "assigned" {
			st, err := target.Workflow(stream, trace.UnitSubject(in.Unit))
			if err != nil {
				return nil, err
			}
			assignment = st
			if st.Value == "" || st.Value == UnitMerged || st.Value == UnitApproved {
				const refused = "assign necessary work to an existing unapproved unit; revise the plan first if needed"
				if bound == nil {
					return priorityRefusal(refused)
				}
				handed = fmt.Sprintf("Jev decided assign with confidence %.2f (judgment %s), but the service refused it: %s. The decision is yours. The chief of staff's reasoning: %s", bound.Confidence, bound.Judgment, refused, in.Reason)
				in.HandedToOwner, in.Disposition, in.Unit = handed, "open", ""
			}
		}
		actor := trace.Actor{Kind: "agent", ID: scope.Thread}
		if bound != nil {
			in.DecidedBy, in.Judgment = trace.DecidedByJev, bound.Judgment
			actor = trace.JevActor
		}
		data, _ := json.MarshalIndent(in.workDiscovery, "", "  ")
		rev := prior.Revision + 1
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: in.ID, Revision: rev, Project: project, Workstream: stream, At: now(), Actor: actor, Cause: scope.Turn}, Path: "discoveries/" + in.ID + ".json", Content: string(data) + "\n"}
		notice := in.Description + "; disposition: " + in.Disposition
		if handed != "" {
			notice += "; handed to the owner with Jev's judgment"
		}
		tx := trace.Transaction{ExpectedVersion: uint64(prior.Revision), Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("%s-%d", in.ID, rev), Revision: 1, Project: project, Workstream: stream, At: now(), Actor: doc.Actor, Cause: scope.Turn}, Subject: in.ID, To: in.Disposition, Reason: in.Description + "; " + in.Reason}, Events: []trace.Event{trace.Notice(fmt.Sprintf("%s-%d", in.ID, rev), "discovery", notice)}}
		if prior.Revision > 0 {
			tx.Transition.From = old.Disposition
		}
		txs := []trace.Transaction{tx}
		if in.Disposition == "assigned" {
			h := tx.Transition.Header
			h.ID += "-assignment"
			h.Unit = in.Unit
			txs = append(txs, trace.Transaction{ExpectedVersion: assignment.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(in.Unit), From: assignment.Value, To: assignment.Value, Reason: "Necessary work assigned: " + in.Description}})
		}
		if _, err := target.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
			return nil, err
		}
		if handed != "" {
			return json.Marshal(map[string]any{"recorded": true, "id": in.ID, "revision": rev, "detail": "the service could not carry out Jev's decision; the discovery is open and in the owner's inbox with Jev's judgment"})
		}
		return json.Marshal(map[string]any{"recorded": true, "id": in.ID, "revision": rev})
	}}
}

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

func discoveryEntries(r *trace.Repository, stream config.WorkstreamID) ([]intake.InboxEntry, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "discoveries/") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out []intake.InboxEntry
	for _, id := range ids {
		d := latest[id]
		var x workDiscovery
		if err := json.Unmarshal([]byte(d.Content), &x); err != nil {
			return nil, err
		}
		if x.Disposition != "open" || x.HandedToOwner == "" {
			continue
		}
		e := intake.Decision(intake.InboxDiscovery, stream, d.At, "/conversation/"+string(stream), map[string]any{"discovery": x.ID})
		e.Question = fmt.Sprintf("What happens to the necessary work %s discovered for %s: %s", x.ID, x.Criterion, x.Description)
		e.Blocked = "The workstream cannot complete while the work is undecided."
		if res, err := jev.Recorded(r, stream, jev.KindDiscovery, jev.DiscoveryKey(x.ID)); err == nil && res.Status == jev.Binding {
			intake.CarriedCard(&e, res, "discovery "+x.ID)
		}
		e.Recommendation = x.HandedToOwner
		out = append(out, e)
	}
	return out, nil
}
