package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// amendmentOperation identifies the amendment and the debate round of an
// amendment round or reply operation. A missing round is round 1.
type amendmentOperation struct {
	ID    string `json:"id"`
	Round int    `json:"round,omitempty"`
}

func (in amendmentOperation) round() int { return max(in.Round, 1) }

func amendmentRoundPath(id string, round int, member string) string {
	return fmt.Sprintf("amendments/%s/round-%d/%s.json", id, round, member)
}
func amendmentReplyPath(id string, round int) string {
	return fmt.Sprintf("amendments/%s/round-%d/reply.json", id, round)
}
func amendmentPacketPath(id string) string { return "amendments/" + id + "/packet.json" }

// amendmentStep is the ID of the transition that moves an amendment to the
// given state in the given debate round. Round 1 keeps the plain ID.
func amendmentStep(id string, round int, to string) string {
	if round > 1 {
		return fmt.Sprintf("amendment-%s-%s-%d", id, to, round)
	}
	return "amendment-" + id + "-" + to
}

// amendmentReplyPrefix is the turn ID prefix of the architect's reply to one
// debate round of an amendment.
func amendmentReplyPrefix(id string, round int) string {
	if round > 1 {
		return fmt.Sprintf("amend-%s-round-%d-reply-", id, round)
	}
	return "amend-" + id + "-reply-"
}

// amendmentRecords returns the committee's records of every debate round of
// an amendment, ordered by round and member.
func amendmentRecords(repo *trace.Repository, stream config.WorkstreamID, id string) ([]shed.Record, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	prefix := "amendments/" + id + "/round-"
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if strings.HasPrefix(doc.Path, prefix) && !strings.HasSuffix(doc.Path, "/reply.json") {
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
		if amendmentRoundPath(id, r.Round, r.Member) != doc.Path {
			return nil, fmt.Errorf("%s: invalid amendment round record", doc.Path)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Round != out[j].Round {
			return out[i].Round < out[j].Round
		}
		return out[i].Member < out[j].Member
	})
	return out, nil
}

// roundRecords returns the records of one debate round.
func roundRecords(records []shed.Record, round int) []shed.Record {
	var out []shed.Record
	for _, r := range records {
		if r.Round == round {
			out = append(out, r)
		}
	}
	return out
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
	if in.Round > 1 {
		return fmt.Sprintf("This is round %d of the shed debate for amendment %s to sealed documents, run because the owner asked for another round. Read request.json and affected.json with the proposed spec.md and plan.json. Apply the normal charter veto, fit advice, size split and proof tests, and concede what the architect's earlier answers settled. The owner alone decides the amendment.\n\n%s", in.Round, in.Amendment, p)
	}
	return fmt.Sprintf("This is the one automatic shed round for amendment %s to sealed documents. Read request.json and affected.json with the proposed spec.md and plan.json. Apply the normal charter veto, fit advice, size split and proof tests. The owner alone decides the amendment.\n\n%s", in.Amendment, p)
}
func (a amendmentDebate) Pass(ctx context.Context) error {
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
			if err := a.one(ctx, stream, req); err != nil {
				return fmt.Errorf("workstream %s amendment %s: %w", stream, req.ID, err)
			}
		}
	}
	return nil
}

// one moves one amendment on from the state it is in.
func (a amendmentDebate) one(ctx context.Context, stream config.WorkstreamID, req trace.Amendment) error {
	state, err := a.repository.Workflow(stream, amendmentSubject(req.ID))
	if err != nil {
		return err
	}
	round, err := amendmentRound(a.repository, stream, req.ID)
	if err != nil {
		return err
	}
	switch state.Value {
	case "proposed":
		if a.s.options.Committee == nil {
			return nil
		}
		if err := a.ensureCommittee(ctx, stream, a.s.current().Capacity.Committee); err != nil {
			return err
		}
		return a.start(ctx, stream, req.ID, round, state, "proposed", AmendmentRoundAction, "debating")
	case "heard":
		if a.s.options.Architect == nil {
			return nil
		}
		if err := a.drafter().ensureThread(ctx, stream); err != nil {
			return err
		}
		return a.start(ctx, stream, req.ID, round, state, "heard", AmendmentReplyAction, "answering")
	case "answered":
		return a.present(ctx, stream, req, round, state)
	case amendmentApproved:
		return a.reseal(ctx, stream, req, state)
	case amendmentResealed:
		return a.apply(ctx, stream, req, state)
	case amendmentApplied:
		if err := a.follow(ctx, stream, req); err != nil {
			return err
		}
		return a.rule(ctx, stream, req, state)
	case amendmentRejected, amendmentUnapplied:
		return a.rule(ctx, stream, req, state)
	case amendmentRuled:
		return a.follow(ctx, stream, req)
	}
	return nil
}

func (a amendmentDebate) start(ctx context.Context, stream config.WorkstreamID, id string, round int, state trace.WorkflowState, from, action, to string) error {
	in := amendmentOperation{ID: id}
	if round > 1 {
		in.Round = round
	}
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	transition := amendmentStep(id, round, to)
	event := trace.EventID(transition, "run")
	op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: action, Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(transition, stream, "amendment_"+id+"_"+from, a.s.now()), Subject: amendmentSubject(id), From: from, To: to, Reason: fmt.Sprintf("round %d of the amendment begins %s", round, action)}, Events: []trace.Event{{ID: event, Kind: action, Body: "Debate amendment " + id, Operation: &op}}}
	_, err = a.repository.Transact(ctx, tx)
	return err
}
func (a amendmentDebate) locate(op coreadapter.Operation, action string) (config.WorkstreamID, string, int, error) {
	var in amendmentOperation
	if op.Action != action || op.Boundary != coreadapter.RunnerBoundary || json.Unmarshal(op.Input, &in) != nil || in.ID == "" || in.Round < 0 {
		return "", "", 0, errors.New("invalid amendment operation")
	}
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", "", 0, err
	}
	to := "debating"
	if action == AmendmentReplyAction {
		to = "answering"
	}
	for _, stream := range streams {
		event := trace.EventID(amendmentStep(in.ID, in.round(), to), "run")
		if trace.OperationID(a.repository.Project(), stream, event) == op.ID {
			return stream, in.ID, in.round(), nil
		}
	}
	return "", "", 0, fmt.Errorf("amendment operation %s has no workstream", op.ID)
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
	stream, id, round, err := a.locate(op, op.Action)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := a.outcome(stream, id, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: fmt.Sprintf("round %d of amendment %s moved on: %s", round, id, result.Evidence), Result: result}, nil
	}
	if op.Action == AmendmentReplyAction {
		thread, err := a.repository.Thread(stream, architectAgent)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		for _, turn := range thread.Turns {
			if strings.HasPrefix(turn.Request.TurnID, amendmentReplyPrefix(id, round)) && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
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
				if strings.HasPrefix(turn.Request.TurnID, fmt.Sprintf("amend-%s-round-%d-", id, round)) && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
					return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "committee turn is running"}, nil
				}
			}
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("round %d of amendment %s has not moved on and no turn of it is running", round, id)}, nil
}
func (a amendmentDebate) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	stream, id, round, err := a.locate(op, op.Action)
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
		return a.reply(ctx, op, stream, id, round)
	}
	return a.round(ctx, op, stream, id, round)
}
func (a amendmentDebate) round(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string, n int) (coreadapter.OperationResult, error) {
	members, err := a.committee(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	all, err := amendmentRecords(a.repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records := roundRecords(all, n)
	if len(records) == 0 {
		in := roundInput{Round: n, Spec: 1, Plan: 1, Amendment: id}
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
			docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("amendment_%s_round_%d_%s", id, n, r.Member), Revision: 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "agent", ID: r.Member}, Cause: op.ID, Depth: 1}, Path: amendmentRoundPath(id, n, r.Member), Content: string(data)})
		}
		if err := a.repository.RecordDocuments(ctx, docs); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if len(records) != len(members) {
		return coreadapter.OperationResult{}, fmt.Errorf("amendment %s round %d has %d of %d member records", id, n, len(records), len(members))
	}
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != "debating" {
		return coreadapter.OperationResult{}, trace.ErrConflict
	}
	reason := fmt.Sprintf("amendment shed round %d heard %d committee members; %s", n, len(records), standing(amendmentDissent(append(slices.DeleteFunc(slices.Clone(all), func(r shed.Record) bool { return r.Round >= n }), records...))))
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(amendmentStep(id, n, "heard"), stream, op.ID, a.s.now()), Subject: amendmentSubject(id), From: "debating", To: "heard", Reason: reason}}
	if _, err := a.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}
func (a amendmentDebate) reply(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string, n int) (coreadapter.OperationResult, error) {
	if a.s.options.Architect == nil {
		return coreadapter.OperationResult{}, errNoArchitect
	}
	records, err := amendmentRecords(a.repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	open := amendmentDissent(records)
	t, err := a.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	prefix := amendmentReplyPrefix(id, n)
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
		prompt := fmt.Sprintf("Answer once for amendment %s, round %d. Read request.json, affected.json, spec.md, plan.json, charter.md and context.md. Use %s to answer each standing objection. You answer this round once; do not redraft or decide for the owner. Dissent: %s", id, n, shed.ReplyTool, strings.Join(lines, "; "))
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
		path := a.replyTurns(stream, id, n, open)
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
	reply := shed.Reply{Version: shed.Version, Round: n, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: turn}
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
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("amendment_%s_round_%d_reply", id, n), Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: architectActor, Cause: op.ID, Depth: 1}, Path: amendmentReplyPath(id, n), Content: string(encoded)}
	state, err := a.repository.Workflow(stream, amendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(amendmentStep(id, n, "answered"), stream, op.ID, a.s.now()), Subject: amendmentSubject(id), From: "answering", To: "answered", Reason: fmt.Sprintf("the architect answered amendment shed round %d once", n)}}
	if _, err := a.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: tx.Transition.Reason}, nil
}
func (a amendmentDebate) replyTurns(stream config.WorkstreamID, id string, n int, open []shed.Entry) *isolation.Turns {
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
			paths, err := a.stage(ctx, cfg.Project.Clone, stream, roundInput{Round: n, Spec: 1, Plan: 1, Amendment: id}, workspace)
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
			return shed.ReplyTools(shed.ReplyTurn{Reply: shed.Reply{Version: shed.Version, Round: n, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: scope.Turn}, Standing: standing, Save: func(r shed.Reply) error {
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
func (a amendmentDebate) present(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, round int, state trace.WorkflowState) error {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return err
	}
	var spec, graph, affected, reply string
	packetRevision := 1
	for _, doc := range docs {
		switch doc.Path {
		case "amendments/" + req.ID + "/spec.md":
			spec = doc.Content
		case "amendments/" + req.ID + "/plan.json":
			graph = doc.Content
		case "amendments/" + req.ID + "/affected.json":
			affected = doc.Content
		case amendmentReplyPath(req.ID, round):
			reply = doc.Content
		case amendmentPacketPath(req.ID):
			packetRevision = max(packetRevision, doc.Revision+1)
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
		Round          int             `json:"round"`
		Request        trace.Amendment `json:"request"`
		Spec           string          `json:"spec"`
		Plan           string          `json:"plan"`
		Affected       json.RawMessage `json:"affected"`
		Dissent        []shed.Entry    `json:"dissent"`
		Reply          json.RawMessage `json:"reply"`
		Recommendation string          `json:"recommendation"`
	}{round, req, spec, graph, json.RawMessage(affected), dissent, json.RawMessage(reply), recommendation}
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return err
	}
	id := amendmentStep(req.ID, round, "presented")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "amendment-" + req.ID + "-presented-packet", Revision: packetRevision, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: shedActor, Cause: id, Depth: 1}, Path: amendmentPacketPath(req.ID), Content: string(data) + "\n"}
	body := fmt.Sprintf("Present amendment %s to the owner for a decision. Request: %s. Reason: %s. Proposed change: %s. Affected units and proofs: %s. Dissent: %s. Recommendation: %s. The round cap approves nothing. Packet: %s, revision %d, after round %d. The owner approves, rejects, asks for another round or overrules the objections that stand, with osmia amendment %s %s or through you.", req.ID, strings.Join(req.Citations, ", "), req.Reason, req.Change, affected, standing(dissent), recommendation, amendmentPacketPath(req.ID), packetRevision, round, stream, req.ID)
	for _, e := range dissent {
		body += fmt.Sprintf("\n- %s (%s, blocking=%t): %s", e.ID, e.Kind, e.Blocking, e.Argument)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.header(id, stream, amendmentStep(req.ID, round, "answered"), a.s.now()), Subject: amendmentSubject(req.ID), From: "answered", To: "presented", Reason: "the chief of staff presents the amendment and dissent to the owner"}, Events: []trace.Event{trace.Notice(id, "amendment", body)}}
	_, err = a.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	return err
}
