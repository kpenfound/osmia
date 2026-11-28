package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

var queuedWorkTools = []string{moveUnitTool, moveWorkstreamTool, resolveContestedTool, requestChangeTool, decideChangeTool, questions.RouteAmendmentTool, decideAmendmentTool, discoveryTool, questions.AnswerTool, questions.RelayRulingTool, handBackDriftTool, triageTool, checksTool, dropDependencyTool, retryChangeTool, resolveObjectionTool}

type queuedItem struct {
	Role string `json:"role"`
	Unit string `json:"unit,omitempty"`
	What string `json:"what"`
}

type queuedReport struct {
	Items []queuedItem
	Why   string
}

func queuedWork(s *Service, r *trace.Repository, stream config.WorkstreamID) (queuedReport, error) {
	var out queuedReport
	busy := map[string]bool{}
	turns, roles := map[string]bool{}, map[string]string{}
	threads, err := r.Threads(stream)
	if err != nil {
		return out, err
	}
	for _, t := range threads {
		for _, q := range t.Turns {
			turns[t.Identity.ID+"\n"+q.Request.TurnID] = true
		}
		roles[t.Identity.ID] = t.Identity.Role
		if trace.ManagerLike(t.Identity.Role) || t.Identity.Role == sessionRole {
			continue
		}
		for _, q := range t.Turns {
			if !q.CompletedAt.IsZero() {
				continue
			}
			state := "queued"
			if q.Claim != nil {
				state = "running"
			}
			out.Items = append(out.Items, queuedItem{Role: trace.DisplayRole(t.Identity.Role), Unit: q.Request.Unit, What: fmt.Sprintf("%s %s turn", state, trace.DisplayRole(t.Identity.Role))})
			busy[q.Request.Unit] = true
			break
		}
	}
	records, err := r.Operations(stream)
	if err != nil {
		return out, err
	}
	var cause func(config.WorkstreamID, coreadapter.Operation) scheduler.HoldCause
	if s != nil {
		cause = s.holdingCause(r)
	}
	var holds []string
	for _, rec := range records {
		if rec.Result != nil || rec.Acknowledged {
			continue
		}
		if _, err := thread.DecodeTurn(rec.Operation); err == nil {
			continue
		}
		if cause != nil {
			if held := cause(stream, rec.Operation); held != scheduler.NotHeld {
				holds = append(holds, fmt.Sprintf("the %s operation is released from its hold (%s)", rec.Operation.Action, strings.ReplaceAll(string(held), "_", " ")))
				continue
			}
		}
		unit := operationUnitOf(rec.Operation)
		out.Items = append(out.Items, queuedItem{Role: "service", Unit: unit, What: rec.Operation.Action + " operation in flight"})
		busy[unit] = true
	}
	feature, err := r.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return out, err
	}
	pause := ""
	if s != nil {
		if err := s.held(r, stream); errors.Is(err, errPaused) {
			pause = err.Error()
		} else if err != nil {
			return out, err
		}
	}
	states, err := r.WorkflowStates(stream)
	if err != nil {
		return out, err
	}
	counts := map[string]int{}
	var units []string
	for subject, st := range states {
		if unit, ok := strings.CutPrefix(subject, "unit-"); ok && st.Value != "" {
			units = append(units, unit)
			counts[st.Value]++
		}
	}
	sort.Strings(units)
	working := feature.Value == BuildingState || feature.Value == AssembledState
	admit := func(role string) bool { return s == nil || s.admitRole(role) }
	if working && pause == "" {
		for _, unit := range units {
			if busy[unit] {
				continue
			}
			var item queuedItem
			switch states[trace.UnitSubject(unit)].Value {
			case UnitReady, UnitImplementing:
				item = queuedItem{Role: masonRole, What: "the scheduler starts a mason turn when capacity allows"}
			case UnitChecking:
				item = queuedItem{Role: "service", What: "the scheduler runs the unit's checks when capacity allows"}
			case UnitReviewing:
				item = queuedItem{Role: reviewerRole, What: "the scheduler starts a reviewer turn when capacity allows"}
			case UnitApproved:
				item = queuedItem{Role: "service", What: "the scheduler lands the unit"}
			default:
				continue
			}
			if item.Role != "service" && !admit(item.Role) {
				holds = append(holds, "the pause on the "+item.Role+" role is lifted")
				continue
			}
			item.Unit = unit
			out.Items = append(out.Items, item)
		}
	}
	asked, err := r.Questions(stream)
	if err != nil {
		return out, err
	}
	for _, q := range asked {
		if q.State != trace.QuestionAnswered || q.Ruling == nil || turns[q.Asked.AskedBy.ID+"\n"+questions.TurnID(q.Asked.ID)] {
			continue
		}
		if q.Asked.Unit != "" && states[trace.UnitSubject(q.Asked.Unit)].Value == UnitMerged {
			continue
		}
		role := roles[q.Asked.AskedBy.ID]
		if role == "" {
			role = q.Asked.AskedBy.ID
		}
		out.Items = append(out.Items, queuedItem{Role: trace.DisplayRole(role), Unit: q.Asked.Unit, What: "the answer to question " + q.Asked.ID + " is delivered as the asker's next turn"})
	}
	if working && pause == "" {
		requests, err := trace.Read[trace.Amendment](r, stream)
		if err != nil {
			return out, err
		}
		for _, a := range requests {
			st, err := r.Workflow(stream, amendmentSubject(a.ID))
			if err != nil {
				return out, err
			}
			if st.Value == "filed" {
				out.Items = append(out.Items, queuedItem{Role: "architect", What: "the architect drafts amendment " + a.ID})
			}
		}
	}
	if len(out.Items) > 0 {
		return out, nil
	}
	var until []string
	if pause != "" {
		until = append(until, "the pause is lifted ("+pause+")")
	}
	until = append(until, holds...)
	if s != nil {
		statuses, err := r.Statuses()
		if err != nil {
			return out, err
		}
		for _, w := range statuses {
			if w.Workstream != stream {
				continue
			}
			for _, g := range s.intake().HeldGates(r, w) {
				until = append(until, fmt.Sprintf("the owner decides %s %s", g.Kind, g.Reference))
			}
		}
	}
	until = append(until, "an event, an owner message or a follow_up gives you another turn")
	where := fmt.Sprintf("workstream %s is %s", stream, emptyAsNone(feature.Value))
	if len(counts) > 0 {
		var parts []string
		for value, n := range counts {
			parts = append(parts, fmt.Sprintf("%d %s", n, value))
		}
		slices.Sort(parts)
		where += " with units " + strings.Join(parts, ", ")
	}
	out.Why = "Nothing will run until " + strings.Join(until, ", or ") + ": " + where + ", and no turn, operation, unit, answer or amendment is waiting to run."
	return out, nil
}

func (c *runtimeControls) reportQueued(cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, now func() time.Time, tools []coreadapter.Tool) []coreadapter.Tool {
	for i, tool := range tools {
		handle := tool.Handle
		switch {
		case slices.Contains(queuedWorkTools, tool.Name):
			tools[i].Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				out, err := handle(ctx, raw)
				if err != nil {
					return out, err
				}
				return c.withQueuedWork(ctx, cfg, r, scope, now, raw, out), nil
			}
		case tool.Name == status.ToolName:
			tools[i].Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				out, err := handle(ctx, raw)
				if err != nil {
					return out, err
				}
				var in struct {
					Attention string `json:"attention"`
				}
				if json.Unmarshal(raw, &in) == nil && strings.TrimSpace(in.Attention) != "" && !refusedResult(out) {
					if err := markFollowUpActed(ctx, r, config.WorkstreamID(scope.Workstream), scope.Turn, "attention", now()); err != nil {
						return nil, err
					}
				}
				return out, nil
			}
		}
	}
	return tools
}

func refusedResult(out json.RawMessage) bool {
	var result struct {
		Recorded *bool `json:"recorded"`
	}
	return json.Unmarshal(out, &result) == nil && result.Recorded != nil && !*result.Recorded
}

func (c *runtimeControls) withQueuedWork(ctx context.Context, cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, now func() time.Time, raw, out json.RawMessage) json.RawMessage {
	var result map[string]json.RawMessage
	if json.Unmarshal(out, &result) != nil || result == nil {
		return out
	}
	var in struct {
		Workstream config.WorkstreamID `json:"workstream"`
	}
	_ = json.Unmarshal(raw, &in)
	set := func(key string, v any) {
		data, _ := json.Marshal(v)
		result[key] = data
	}
	done := func() json.RawMessage {
		data, err := json.Marshal(result)
		if err != nil {
			return out
		}
		return data
	}
	target, stream, refusal, err := managerMember(r, scope, in.Workstream)
	if err != nil || refusal != "" {
		return out
	}
	work, err := queuedWork(c.service.Load(), target, stream)
	if err != nil {
		set("queued_unknown", "the service could not read what is queued: "+err.Error())
		return done()
	}
	items := work.Items
	if items == nil {
		items = []queuedItem{}
	}
	set("queued", items)
	if len(items) == 0 {
		set("nothing_will_run", work.Why)
	}
	if refusedResult(out) {
		return done()
	}
	host := config.WorkstreamID(scope.Workstream)
	if len(items) > 0 {
		if err := markFollowUpActed(ctx, r, host, scope.Turn, "queued work", now()); err != nil {
			set("check_back_unrecorded", err.Error())
		}
		return done()
	}
	scheduled, err := scheduleFollowUp(ctx, r, host, scope.Thread, scope.Turn, in.Workstream, autoFollowUpReason, followUpWindow(cfg), now())
	switch {
	case err != nil:
		set("check_back_unrecorded", err.Error())
	case !scheduled.IsZero():
		set("check_back", "the service gives you a turn at "+scheduled.UTC().Format(time.RFC3339)+" to check on this; call follow_up to choose another time")
	}
	return done()
}
