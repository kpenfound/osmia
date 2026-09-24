package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	AmendmentRoundAction = "amendment-round"
	AmendmentReplyAction = "amendment-reply"
)

type amendmentDebate struct{ *debate }

type amendmentOperation struct {
	ID string `json:"id"`
}

func amendmentRoundPath(id string, member string) string {
	return "amendments/" + id + "/round-1/" + member + ".json"
}
func amendmentReplyPath(id string) string  { return "amendments/" + id + "/round-1/reply.json" }
func amendmentPacketPath(id string) string { return "amendments/" + id + "/packet.json" }

func amendmentRecords(repo *trace.Repository, stream config.WorkstreamID, id string) ([]shed.Record, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	prefix := "amendments/" + id + "/round-1/"
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if strings.HasPrefix(doc.Path, prefix) && doc.Path != amendmentReplyPath(id) {
			if doc.Revision >= latest[doc.Path].Revision {
				latest[doc.Path] = doc
			}
		}
	}
	var out []shed.Record
	for _, doc := range latest {
		r, err := shed.Parse([]byte(doc.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc.Path, err)
		}
		if amendmentRoundPath(id, r.Member) != doc.Path || r.Round != 1 {
			return nil, fmt.Errorf("%s: invalid amendment round record", doc.Path)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out, nil
}
func amendmentDissent(records []shed.Record) []shed.Entry { return shed.DissentRecord(records, nil) }
func amendmentCommitteeSystemPrompt(project config.Project, in roundInput) string {
	if in.Amendment == "" {
		return committeeSystemPrompt(project)
	}
	return fmt.Sprintf("You are a member of the committee reviewing an amendment to a sealed spec and plan for the %s project (%s). Apply the shed's charter, fit, size and proof rules. You read only and contribute through %s and %s; cite every objection. The owner decides the amendment.", project.Name, project.Upstream, shed.ObjectTool, shed.ConcedeTool)
}

func amendmentCommitteePrompt(in roundInput, standing []shed.Dissent, answers []string) string {
	p := committeePrompt(in, standing, answers)
	if in.Amendment == "" {
		return p
	}
	return fmt.Sprintf("This is the single bounded shed round for amendment %s to sealed documents. Read request.json and affected.json with the proposed spec.md and plan.json. Apply the normal charter veto, fit advice, size split and proof tests. The owner alone decides the amendment.\n\n%s", in.Amendment, p)
}
func (a amendmentDebate) Pass(ctx context.Context) error {
	if a.s.options.Committee == nil {
		return nil
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value != "building" && feature.Value != "assembled" {
			continue
		}
		requests, err := trace.Read[trace.Amendment](a.repository, stream)
		if err != nil {
			return err
		}
		for _, req := range requests {
			state, err := a.repository.Workflow(stream, amendmentSubject(req.ID))
			if err != nil {
				return err
			}
			switch state.Value {
			case "proposed":
				if err := a.ensureCommittee(ctx, stream, a.s.current().Capacity.Committee); err != nil {
					return err
				}
				if err := a.start(ctx, stream, req.ID, state, "proposed", AmendmentRoundAction, "debating"); err != nil {
					return err
				}
			case "heard":
				if a.s.options.Architect != nil {
					if err := a.drafter().ensureThread(ctx, stream); err != nil {
						return err
					}
					if err := a.start(ctx, stream, req.ID, state, "heard", AmendmentReplyAction, "answering"); err != nil {
						return err
					}
				}
			case "answered":
				if err := a.present(ctx, stream, req, state); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (a amendmentDebate) start(ctx context.Context, stream config.WorkstreamID, id string, state trace.WorkflowState, from, action, to string) error {
	input, err := json.Marshal(amendmentOperation{id})
	if err != nil {
		return err
	}
	transition := "amendment-" + id + "-" + to
	event := trace.EventID(transition, "run")
	op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: action, Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(transition, stream, "amendment_"+id+"_"+from, a.s.now()), Subject: amendmentSubject(id), From: from, To: to, Reason: "the amendment begins " + action}, Events: []trace.Event{{ID: event, Kind: action, Body: "Debate amendment " + id, Operation: &op}}}
	_, err = a.repository.Transact(ctx, tx)
	return err
}
func (a amendmentDebate) locate(op coreadapter.Operation, action string) (config.WorkstreamID, string, error) {
	var in amendmentOperation
	if op.Action != action || op.Boundary != coreadapter.RunnerBoundary || json.Unmarshal(op.Input, &in) != nil || in.ID == "" {
		return "", "", errors.New("invalid amendment operation")
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", "", err
	}
	to := "debating"
	if action == AmendmentReplyAction {
		to = "answering"
	}
	for _, stream := range streams {
		event := trace.EventID("amendment-"+in.ID+"-"+to, "run")
		if trace.OperationID(a.repository.Project(), stream, event) == op.ID {
			return stream, in.ID, nil
		}
	}
	return "", "", fmt.Errorf("amendment operation %s has no workstream", op.ID)
}
func (a amendmentDebate) outcome(stream config.WorkstreamID, id, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject == amendmentSubject(id) && t.Cause == operation {
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}
func (a amendmentDebate) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	stream, id, err := a.locate(op, op.Action)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := a.outcome(stream, id, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Result: result}, nil
	}
	if op.Action == AmendmentReplyAction {
		thread, err := a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		for _, turn := range thread.Turns {
			if strings.HasPrefix(turn.Request.TurnID, "amend-"+id+"-reply-") && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
				return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect reply turn is running"}, nil
			}
		}
	} else {
		members, err := a.committee(stream)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		for _, member := range members {
			thread, err := a.repository.Thread(stream, member)
			if err != nil {
				return coreadapter.Observation{}, err
			}
			for _, turn := range thread.Turns {
				if strings.HasPrefix(turn.Request.TurnID, "amend-"+id+"-round-1-") && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
					return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "committee turn is running"}, nil
				}
			}
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent}, nil
}
func (a amendmentDebate) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	stream, id, err := a.locate(op, op.Action)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := a.outcome(stream, id, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if op.Action == AmendmentReplyAction {
		return a.reply(ctx, op, stream, id)
	}
	return a.round(ctx, op, stream, id)
}
func (a amendmentDebate) round(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string) (coreadapter.OperationResult, error) {
	members, err := a.committee(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records, err := amendmentRecords(a.repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if len(records) == 0 {
		in := roundInput{Round: 1, Spec: 1, Plan: 1, Amendment: id}
		cfg := a.s.current()
		records = make([]shed.Record, len(members))
		failures := make([]error, len(members))
		waiting := make([]string, len(members))
		running, cancel := context.WithCancel(ctx)
		defer cancel()
		defer a.s.turns.add(stream, cancel)()
		var wg sync.WaitGroup
		for i, member := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				records[i], waiting[i], failures[i] = a.member(ctx, running, cfg, stream, op.ID, in, member)
			}()
		}
		wg.Wait()
		if err := errors.Join(failures...); err != nil {
			return coreadapter.OperationResult{}, err
		}
		for _, q := range waiting {
			if q != "" {
				return coreadapter.OperationResult{}, fmt.Errorf("amendment round waits for question %s", q)
			}
		}
		at := a.s.now()
		var docs []trace.Document
		for _, r := range records {
			data, err := shed.Encode(r)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "amendment_" + id + "_round_1_" + r.Member, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "agent", ID: r.Member}, Cause: op.ID, Depth: 1}, Path: amendmentRoundPath(id, r.Member), Content: string(data)})
		}
		if err := a.repository.RecordDocuments(ctx, docs); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if len(records) != len(members) {
		return coreadapter.OperationResult{}, fmt.Errorf("amendment %s round has %d of %d member records", id, len(records), len(members))
	}
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != "debating" {
		return coreadapter.OperationResult{}, trace.ErrConflict
	}
	reason := fmt.Sprintf("one amendment shed round heard %d committee members; %s", len(records), standing(amendmentDissent(records)))
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header("amendment-"+id+"-heard", stream, op.ID, a.s.now()), Subject: amendmentSubject(id), From: "debating", To: "heard", Reason: reason}}
	if _, err := a.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}
func (a amendmentDebate) reply(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string) (coreadapter.OperationResult, error) {
	records, err := amendmentRecords(a.repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	open := amendmentDissent(records)
	t, err := a.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	prefix := "amend-" + id + "-reply-"
	var queued *trace.QueuedTurn
	attempts := 0
	for i := range t.Turns {
		if strings.HasPrefix(t.Turns[i].Request.TurnID, prefix) {
			attempts++
			queued = &t.Turns[i]
		}
	}
	if queued != nil && queued.Claim != nil && queued.Response == nil && t.Status == "interrupted" {
		if err := a.repository.AbandonTurn(ctx, stream, architectAgent, queued.Request.TurnID, a.s.now()); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		for i := range t.Turns {
			if t.Turns[i].Request.TurnID == queued.Request.TurnID {
				queued = &t.Turns[i]
				break
			}
		}
	}
	hasReply := false
	if queued != nil {
		_, err := os.Stat(filepath.Join(a.drafter().turnDirectory(stream, queued.Request.TurnID), "output", "reply.json"))
		hasReply = err == nil
		if err != nil && !os.IsNotExist(err) {
			return coreadapter.OperationResult{}, err
		}
	}
	if queued == nil || queued.Status() == "interrupted" && !hasReply && attempts < maxDraftAttempts {
		turn := fmt.Sprintf("%s%d", prefix, attempts+1)
		profile, _, err := a.s.roleExecution(a.s.current(), architectRole)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		var lines []string
		for _, e := range open {
			lines = append(lines, fmt.Sprintf("%s (%s on %s): %s", e.ID, e.Kind, e.Part, e.Argument))
		}
		prompt := fmt.Sprintf("Answer once for amendment %s, round 1. Read request.json, affected.json, spec.md, plan.json, charter.md and context.md. Use %s to answer each standing objection. This round is capped at one; do not redraft or decide for the owner. Dissent: %s", id, shed.ReplyTool, strings.Join(lines, "; "))
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: architectActor, Cause: op.ID, Depth: 1}, AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: architectSystemPrompt(a.s.current().Project), Prompt: prompt}
		if _, err := a.repository.EnqueueTurn(ctx, req); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		for i := range t.Turns {
			if t.Turns[i].Request.TurnID == turn {
				queued = &t.Turns[i]
				break
			}
		}
	}
	if queued == nil {
		return coreadapter.OperationResult{}, errors.New("amendment reply turn missing")
	}
	turn := queued.Request.TurnID
	if queued.CompletedAt.IsZero() {
		path := a.replyTurns(stream, id, open)
		dispatcher := thread.Dispatcher{Runner: thread.Runner{Store: a.repository, Turns: &questions.Turns{Turns: path, Repository: a.repository}, Now: a.s.now}, Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
			dir := filepath.Join(a.drafter().turnDirectory(input.Workstream, input.Turn), "session")
			return coreadapter.PreparedTurn{SessionDirectory: dir}, os.MkdirAll(dir, 0700)
		}}
		run, err := thread.TurnOperation(a.repository.Project(), "amendment-reply-turn", thread.TurnInput{Workstream: stream, Agent: architectAgent, Turn: turn})
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if _, err := dispatcher.Apply(ctx, run); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		for i := range t.Turns {
			if t.Turns[i].Request.TurnID == turn {
				queued = &t.Turns[i]
				break
			}
		}
	}
	reply := shed.Reply{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: turn}
	data, err := os.ReadFile(filepath.Join(a.drafter().turnDirectory(stream, turn), "output", "reply.json"))
	if err == nil {
		reply, err = shed.ParseReply(data)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
	} else if !os.IsNotExist(err) {
		return coreadapter.OperationResult{}, err
	}
	if queued.Status() != "idle" {
		reply.Failure = "architect turn ended with status " + queued.Status()
	}
	encoded, err := shed.EncodeReply(reply)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "amendment_" + id + "_round_1_reply", Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: architectActor, Cause: op.ID, Depth: 1}, Path: amendmentReplyPath(id), Content: string(encoded)}
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header("amendment-"+id+"-answered", stream, op.ID, a.s.now()), Subject: amendmentSubject(id), From: "answering", To: "answered", Reason: "the architect answered the amendment shed round once"}}
	if _, err := a.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: tx.Transition.Reason}, nil
}
func (a amendmentDebate) replyTurns(stream config.WorkstreamID, id string, open []shed.Entry) *isolation.Turns {
	cfg := a.s.current()
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if cfg != nil && a.s.options.Architect != nil {
		engine = a.s.options.Architect.Engine
		hosts = a.s.options.Architect.Hosts
	}
	return &isolation.Turns{Workspaces: stagedWorkspaces{}, Views: isolation.Views{Directory: filepath.Join(cfg.Root.String(), "views")}, Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{"file_read", shed.ReplyTool}}}, Hosts: hosts, Engine: engine,
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Workstream != string(stream) || scope.Role != architectRole {
				return isolation.Selection{}, errors.New("turn scope denied")
			}
			_, settings, err := a.s.roleExecution(cfg, architectRole)
			if err != nil {
				return isolation.Selection{}, err
			}
			workspace := filepath.Join(a.drafter().turnDirectory(stream, scope.Turn), "workspace")
			paths, err := a.stage(ctx, cfg.Project.Clone, stream, roundInput{Round: 1, Spec: 1, Plan: 1, Amendment: id}, workspace)
			if err != nil {
				return isolation.Selection{}, err
			}
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
		}, Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) || scope.Role != architectRole || scope.Thread != architectThread {
				return nil, errors.New("turn scope denied")
			}
			standing := make([]shed.Dissent, len(open))
			for i, e := range open {
				standing[i] = e.Dissent
			}
			file := filepath.Join(a.drafter().turnDirectory(stream, scope.Turn), "output", "reply.json")
			return shed.ReplyTools(shed.ReplyTurn{Reply: shed.Reply{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: scope.Turn}, Standing: standing, Save: func(r shed.Reply) error {
				data, err := shed.EncodeReply(r)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					return err
				}
				if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
					return err
				}
				return os.Rename(file+".tmp", file)
			}})
		}}
}
func (a amendmentDebate) present(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return err
	}
	var spec, graph, affected, reply string
	for _, doc := range docs {
		switch doc.Path {
		case "amendments/" + req.ID + "/spec.md":
			spec = doc.Content
		case "amendments/" + req.ID + "/plan.json":
			graph = doc.Content
		case "amendments/" + req.ID + "/affected.json":
			affected = doc.Content
		case amendmentReplyPath(req.ID):
			reply = doc.Content
		}
	}
	records, err := amendmentRecords(a.repository, stream, req.ID)
	if err != nil {
		return err
	}
	dissent := amendmentDissent(records)
	recommendation := shed.Recommend(dissent)
	for _, record := range records {
		if record.Failure != "" {
			recommendation = "do not approve yet: a committee member failed to complete its review"
			break
		}
	}
	packet := struct {
		Request        trace.Amendment `json:"request"`
		Spec           string          `json:"spec"`
		Plan           string          `json:"plan"`
		Affected       json.RawMessage `json:"affected"`
		Dissent        []shed.Entry    `json:"dissent"`
		Reply          json.RawMessage `json:"reply"`
		Recommendation string          `json:"recommendation"`
	}{req, spec, graph, json.RawMessage(affected), dissent, json.RawMessage(reply), recommendation}
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return err
	}
	id := "amendment-" + req.ID + "-presented"
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id + "-packet", Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: shedActor, Cause: id, Depth: 1}, Path: amendmentPacketPath(req.ID), Content: string(data) + "\n"}
	body := fmt.Sprintf("Present amendment %s to the owner for a decision. Request: %s. Reason: %s. Proposed change: %s. Affected units and proofs: %s. Dissent: %s. Recommendation: %s. The round cap approves nothing. Packet: %s", req.ID, strings.Join(req.Citations, ", "), req.Reason, req.Change, affected, standing(dissent), recommendation, amendmentPacketPath(req.ID))
	for _, e := range dissent {
		body += fmt.Sprintf("\n- %s (%s, blocking=%t): %s", e.ID, e.Kind, e.Blocking, e.Argument)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(id, stream, "amendment-"+req.ID+"-answered", a.s.now()), Subject: amendmentSubject(req.ID), From: "answered", To: "presented", Reason: "the chief of staff presents the amendment and dissent to the owner"}, Events: []trace.Event{trace.Notice(id, "amendment", body)}}
	_, err = a.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	return err
}
