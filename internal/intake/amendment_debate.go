package intake

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

type AmendmentDebate struct{ *Debate }

// amendmentOperation identifies the amendment and the debate round of an
// amendment round or reply operation. A missing round is round 1.
type AmendmentOperation struct {
	ID    string `json:"id"`
	Round int    `json:"round,omitempty"`
}

func (in AmendmentOperation) RoundNumber() int { return max(in.Round, 1) }

func AmendmentRoundPath(id string, round int, member string) string {
	return fmt.Sprintf("amendments/%s/round-%d/%s.json", id, round, member)
}

func AmendmentReplyPath(id string, round int) string {
	return fmt.Sprintf("amendments/%s/round-%d/reply.json", id, round)
}

func AmendmentPacketPath(id string) string { return "amendments/" + id + "/packet.json" }

// amendmentStep is the ID of the transition that moves an amendment to the
// given state in the given debate round. Round 1 keeps the plain ID.
func AmendmentStep(id string, round int, to string) string {
	if round > 1 {
		return fmt.Sprintf("amendment-%s-%s-%d", id, to, round)
	}
	return "amendment-" + id + "-" + to
}

// amendmentReplyPrefix is the turn ID prefix of the architect's reply to one
// debate round of an amendment.
func AmendmentReplyPrefix(id string, round int) string {
	if round > 1 {
		return fmt.Sprintf("amend-%s-round-%d-reply-", id, round)
	}
	return "amend-" + id + "-reply-"
}

// amendmentRecords returns the committee's records of every debate round of
// an amendment, ordered by round and member.
func AmendmentRecords(repo *trace.Repository, stream config.WorkstreamID, id string) ([]shed.Record, error) {
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
		if AmendmentRoundPath(id, r.Round, r.Member) != doc.Path {
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
func RoundRecords(records []shed.Record, round int) []shed.Record {
	var out []shed.Record
	for _, r := range records {
		if r.Round == round {
			out = append(out, r)
		}
	}
	return out
}

func AmendmentDissent(records []shed.Record) []shed.Entry {
	return shed.DissentRecord(records, nil, nil)
}

func AmendmentCommitteeSystemPrompt(project config.Project, in RoundInput) string {
	if in.Amendment == "" {
		return CommitteeSystemPrompt(project)
	}
	return fmt.Sprintf("You are a member of the committee reviewing an amendment to a sealed spec and plan for the %s project (%s). Apply the shed's charter, fit, size and acceptance rules. You read only and contribute through %s and %s; cite every objection. The chief of staff decides plan-only engineering revisions; changed intent requires the owner.", project.Name, project.Upstream, shed.ObjectTool, shed.ConcedeTool)
}

func AmendmentCommitteePrompt(in RoundInput, standing []shed.Dissent, answers []string) string {
	p := CommitteePrompt(in, standing, answers)
	if in.Amendment == "" {
		return p
	}
	if in.Round > 1 {
		return fmt.Sprintf("This is round %d of the shed debate for amendment %s to sealed documents, run because another round was requested. Read request.json and affected.json with the proposed spec.md and plan.json. Apply the normal charter veto and fit, size and acceptance advice, and concede what the architect's earlier answers settled. The chief of staff decides plan-only engineering revisions; changed intent requires the owner.\n\n%s", in.Round, in.Amendment, p)
	}
	return fmt.Sprintf("This is the one automatic shed round for amendment %s to sealed documents. Read request.json and affected.json with the proposed spec.md and plan.json. Apply the normal charter veto and fit, size and acceptance advice. The chief of staff decides plan-only engineering revisions; changed intent requires the owner.\n\n%s", in.Amendment, p)
}

func (a AmendmentDebate) Pass(ctx context.Context) error {
	streams, err := a.Repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		feature, err := a.Repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value != "building" && feature.Value != "assembled" {
			continue
		}
		requests, err := trace.Read[trace.Amendment](a.Repository, stream)
		if err != nil {
			return err
		}
		for _, req := range requests {
			if err := a.One(ctx, stream, req); err != nil {
				return fmt.Errorf("workstream %s amendment %s: %w", stream, req.ID, err)
			}
		}
	}
	return nil
}

// one moves one amendment on from the state it is in.
func (a AmendmentDebate) One(ctx context.Context, stream config.WorkstreamID, req trace.Amendment) error {
	state, err := a.Repository.Workflow(stream, AmendmentSubject(req.ID))
	if err != nil {
		return err
	}
	round, err := CurrentAmendmentRound(a.Repository, stream, req.ID)
	if err != nil {
		return err
	}
	switch state.Value {
	case "proposed":
		if a.S.Options.Committee == nil {
			return nil
		}
		if err := a.EnsureCommittee(ctx, stream, a.S.Current().Capacity.Committee); err != nil {
			return err
		}
		return a.Start(ctx, stream, req.ID, round, state, "proposed", AmendmentRoundAction, "debating")
	case "heard":
		if a.S.Options.Architect == nil {
			return nil
		}
		if err := a.Drafter().EnsureThread(ctx, stream); err != nil {
			return err
		}
		return a.Start(ctx, stream, req.ID, round, state, "heard", AmendmentReplyAction, "answering")
	case "answered":
		return a.Present(ctx, stream, req, round, state)
	case AmendmentApproved:
		return a.Reseal(ctx, stream, req, state)
	case AmendmentResealed:
		return a.ApplyAmendment(ctx, stream, req, state)
	case AmendmentApplied:
		if err := a.Follow(ctx, stream, req); err != nil {
			return err
		}
		return a.Rule(ctx, stream, req, state)
	case AmendmentRejected, AmendmentUnapplied:
		return a.Rule(ctx, stream, req, state)
	case AmendmentRuled:
		return a.Follow(ctx, stream, req)
	}
	return nil
}

func (a AmendmentDebate) Start(ctx context.Context, stream config.WorkstreamID, id string, round int, state trace.WorkflowState, from, action, to string) error {
	// A paused workstream is asked for nothing until the pause is lifted.
	if _, paused := a.S.Pausing(a.Repository.Project(), stream); paused {
		return nil
	}
	in := AmendmentOperation{ID: id}
	if round > 1 {
		in.Round = round
	}
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	transition := AmendmentStep(id, round, to)
	event := trace.EventID(transition, "run")
	op := coreadapter.Operation{ID: trace.OperationID(a.Repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: action, Input: input}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.Header(transition, stream, "amendment_"+id+"_"+from, a.S.Now()), Subject: AmendmentSubject(id), From: from, To: to, Reason: fmt.Sprintf("round %d of the amendment begins %s", round, action)}, Events: []trace.Event{{ID: event, Kind: action, Body: "Debate amendment " + id, Operation: &op}}}
	_, err = a.Repository.Transact(ctx, tx)
	return err
}

func (a AmendmentDebate) Locate(op coreadapter.Operation, action string) (config.WorkstreamID, string, int, error) {
	var in AmendmentOperation
	if op.Action != action || op.Boundary != coreadapter.RunnerBoundary || json.Unmarshal(op.Input, &in) != nil || in.ID == "" || in.Round < 0 {
		return "", "", 0, errors.New("invalid amendment operation")
	}
	streams, err := a.Repository.Workstreams()
	if err != nil {
		return "", "", 0, err
	}
	to := "debating"
	if action == AmendmentReplyAction {
		to = "answering"
	}
	for _, stream := range streams {
		event := trace.EventID(AmendmentStep(in.ID, in.RoundNumber(), to), "run")
		if trace.OperationID(a.Repository.Project(), stream, event) == op.ID {
			return stream, in.ID, in.RoundNumber(), nil
		}
	}
	return "", "", 0, fmt.Errorf("amendment operation %s has no workstream", op.ID)
}

func (a AmendmentDebate) Outcome(stream config.WorkstreamID, id, operation string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.Repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject == AmendmentSubject(id) && t.Cause == operation {
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

func (a AmendmentDebate) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	stream, id, round, err := a.Locate(op, op.Action)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := a.Outcome(stream, id, op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: fmt.Sprintf("round %d of amendment %s moved on: %s", round, id, result.Evidence), Result: result}, nil
	}
	if op.Action == AmendmentReplyAction {
		thread, err := a.Repository.Thread(stream, ArchitectAgent)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		for _, turn := range thread.Turns {
			if strings.HasPrefix(turn.Request.TurnID, AmendmentReplyPrefix(id, round)) && turn.Claim != nil && turn.Response == nil && thread.Status != "interrupted" {
				return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect reply turn is running"}, nil
			}
		}
	} else {
		members, err := a.Committee(stream)
		if err != nil {
			return coreadapter.Observation{}, err
		}
		for _, member := range members {
			thread, err := a.Repository.Thread(stream, member)
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

func (a AmendmentDebate) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	stream, id, round, err := a.Locate(op, op.Action)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := a.Outcome(stream, id, op.ID); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if op.Action == AmendmentReplyAction {
		return a.Reply(ctx, op, stream, id, round)
	}
	return a.RoundNumber(ctx, op, stream, id, round)
}

func (a AmendmentDebate) RoundNumber(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string, n int) (coreadapter.OperationResult, error) {
	members, err := a.Committee(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	all, err := AmendmentRecords(a.Repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records := RoundRecords(all, n)
	if len(records) == 0 {
		in := RoundInput{Round: n, Spec: 1, Plan: 1, Amendment: id}
		cfg := a.S.About(a.Repository)
		records = make([]shed.Record, len(members))
		failures := make([]error, len(members))
		waiting := make([]string, len(members))
		running, cancel := context.WithCancel(ctx)
		defer cancel()
		defer a.S.TrackTurn(stream, cancel)()
		var wg sync.WaitGroup
		for i, member := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				records[i], waiting[i], failures[i] = a.Member(trace.Beside(ctx), trace.Beside(running), cfg, stream, op.ID, in, member)
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
		at := a.S.Now()
		var docs []trace.Document
		for _, r := range records {
			data, err := shed.Encode(r)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("amendment_%s_round_%d_%s", id, n, r.Member), Revision: 1, Project: a.Repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "agent", ID: r.Member}, Cause: op.ID, Depth: 1}, Path: AmendmentRoundPath(id, n, r.Member), Content: string(data)})
		}
		if err := trace.Relock(ctx, func() error { return a.Repository.RecordDocuments(ctx, docs) }); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	var result coreadapter.OperationResult
	err = trace.Relock(ctx, func() error {
		result, err = a.Heard(ctx, op, stream, id, n, members, records, all)
		return err
	})
	return result, err
}

// heard moves the amendment to heard once round n has a record of every
// member.
func (a AmendmentDebate) Heard(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string, n int, members []string, records, all []shed.Record) (coreadapter.OperationResult, error) {
	if len(records) != len(members) {
		return coreadapter.OperationResult{}, fmt.Errorf("amendment %s round %d has %d of %d member records", id, n, len(records), len(members))
	}
	state, err := a.Repository.Workflow(stream, AmendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != "debating" {
		return coreadapter.OperationResult{}, trace.ErrConflict
	}
	reason := fmt.Sprintf("amendment shed round %d heard %d committee members; %s", n, len(records), Standing(AmendmentDissent(append(slices.DeleteFunc(slices.Clone(all), func(r shed.Record) bool { return r.Round >= n }), records...))))
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.Header(AmendmentStep(id, n, "heard"), stream, op.ID, a.S.Now()), Subject: AmendmentSubject(id), From: "debating", To: "heard", Reason: reason}}
	if _, err := a.Repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

func (a AmendmentDebate) Reply(ctx context.Context, op coreadapter.Operation, stream config.WorkstreamID, id string, n int) (coreadapter.OperationResult, error) {
	if a.S.Options.Architect == nil {
		return coreadapter.OperationResult{}, ErrNoArchitect
	}
	records, err := AmendmentRecords(a.Repository, stream, id)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	open := AmendmentDissent(records)
	t, err := a.Repository.Thread(stream, ArchitectAgent)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	prefix := AmendmentReplyPrefix(id, n)
	var queued *trace.QueuedTurn
	// A turn a hard pause stopped is continued within its attempt, so only
	// the attempts' own turns count.
	attempts := 0
	for i := range t.Turns {
		if strings.HasPrefix(t.Turns[i].Request.TurnID, prefix) {
			if !IsContinuation(t.Turns[i].Request.TurnID) {
				attempts++
			}
			queued = &t.Turns[i]
		}
	}
	if queued != nil && queued.Claim != nil && queued.Response == nil && t.Status == "interrupted" {
		if err := a.Repository.AbandonTurn(ctx, stream, ArchitectAgent, queued.Request.TurnID, a.S.Now()); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.Repository.Thread(stream, ArchitectAgent)
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
	if StoppedTurn(queued) {
		if err := a.S.Held(a.Repository, stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
		next, err := a.S.ContinueStopped(ctx, a.Repository, a.S.About(a.Repository), ArchitectRole, *queued)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if t, err = a.Repository.Thread(stream, ArchitectAgent); err != nil {
			return coreadapter.OperationResult{}, err
		}
		for i := range t.Turns {
			if t.Turns[i].Request.TurnID == next {
				queued = &t.Turns[i]
			}
		}
	}
	answered, err := a.Replied(stream, t, queued)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	hasReply := answered != nil
	if queued == nil || queued.Status() == "interrupted" && !hasReply && attempts < MaxDraftAttempts {
		if err := a.S.Held(a.Repository, stream); err != nil {
			return coreadapter.OperationResult{}, err
		}
		turn := fmt.Sprintf("%s%d", prefix, attempts+1)
		profile, _, err := a.S.RoleExecution(a.S.About(a.Repository), ArchitectRole)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		var lines []string
		for _, e := range open {
			lines = append(lines, fmt.Sprintf("%s (%s on %s): %s", e.ID, e.Kind, e.Part, e.Argument))
		}
		prompt := fmt.Sprintf("Answer once for amendment %s, round %d. Read request.json, affected.json, spec.md, plan.json, charter.md and context.md. Use %s to answer each standing objection. You answer this round once; do not redraft or decide for the owner. Dissent: %s", id, n, shed.ReplyTool, strings.Join(lines, "; "))
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.Repository.Project(), Workstream: stream, At: a.S.Now(), Actor: ArchitectActor, Cause: op.ID, Depth: 1}, AgentID: ArchitectAgent, ThreadID: ArchitectThread, TurnID: turn, Profile: profile, SystemPrompt: ArchitectSystemPrompt(a.S.About(a.Repository).Project), Prompt: prompt}
		if _, err := a.Repository.EnqueueTurn(ctx, req); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.Repository.Thread(stream, ArchitectAgent)
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
		if queued.Response == nil {
			if err := a.S.Held(a.Repository, stream); err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
		path := a.ReplyTurns(stream, id, n, open)
		dispatcher := thread.Dispatcher{Runner: a.S.ThreadRunner(a.S.About(a.Repository), a.Repository, &questions.Turns{Turns: path, Repository: a.Repository}, a.S.Now), Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
			dir := filepath.Join(a.Drafter().TurnDirectory(input.Workstream, input.Turn), "session")
			return coreadapter.PreparedTurn{SessionDirectory: dir}, os.MkdirAll(dir, 0700)
		}}
		run, err := thread.TurnOperation(a.Repository.Project(), "amendment-reply-turn", thread.TurnInput{Workstream: stream, Agent: ArchitectAgent, Turn: turn})
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		running, release := a.S.Stoppable(ctx, a.Repository.Project(), stream)
		_, err = dispatcher.Apply(running, run)
		release()
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err = a.Repository.Thread(stream, ArchitectAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		for i := range t.Turns {
			if t.Turns[i].Request.TurnID == turn {
				queued = &t.Turns[i]
				break
			}
		}
		// The next run continues the turn once the pause is lifted.
		if StoppedTurn(queued) {
			return coreadapter.OperationResult{}, fmt.Errorf("%w: a hard pause stopped architect turn %s", ErrPaused, turn)
		}
		if answered, err = a.Replied(stream, t, queued); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	reply := shed.Reply{Version: shed.Version, Round: n, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: turn}
	if answered != nil {
		reply = *answered
	}
	if queued.Status() != "idle" {
		reply.Failure = "architect turn ended with status " + queued.Status()
	}
	encoded, err := shed.EncodeReply(reply)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("amendment_%s_round_%d_reply", id, n), Revision: 1, Project: a.Repository.Project(), Workstream: stream, At: a.S.Now(), Actor: ArchitectActor, Cause: op.ID, Depth: 1}, Path: AmendmentReplyPath(id, n), Content: string(encoded)}
	state, err := a.Repository.Workflow(stream, AmendmentSubject(id))
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.Header(AmendmentStep(id, n, "answered"), stream, op.ID, a.S.Now()), Subject: AmendmentSubject(id), From: "answering", To: "answered", Reason: fmt.Sprintf("the architect answered amendment shed round %d once", n)}}
	if _, err := a.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: tx.Transition.Reason}, nil
}

// replied returns what the attempt of the architect's reply that turn q
// belongs to answered, or nil when it answered nothing: the answers every
// turn of the attempt kept, a later answer to an objection replacing an
// earlier one, as the reply of q.
func (a AmendmentDebate) Replied(stream config.WorkstreamID, t trace.Thread, q *trace.QueuedTurn) (*shed.Reply, error) {
	if q == nil {
		return nil, nil
	}
	var reply *shed.Reply
	for _, turn := range t.Turns {
		if ContinuedTurn(turn.Request.TurnID) != ContinuedTurn(q.Request.TurnID) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.Drafter().TurnDirectory(stream, turn.Request.TurnID), "output", "reply.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		kept, err := shed.ParseReply(data)
		if err != nil {
			return nil, err
		}
		if reply == nil {
			reply = &kept
			continue
		}
		for _, answer := range kept.Answers {
			reply.Answers = append(slices.DeleteFunc(reply.Answers, func(old shed.Answer) bool { return old.Objection == answer.Objection }), answer)
		}
	}
	if reply != nil {
		reply.Turn = q.Request.TurnID
	}
	return reply, nil
}

func (a AmendmentDebate) ReplyTurns(stream config.WorkstreamID, id string, n int, open []shed.Entry) *isolation.Turns {
	cfg := a.S.About(a.Repository)
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a.S.Options.Architect != nil {
		engine = a.S.Options.Architect.Engine
		hosts = a.S.Options.Architect.Hosts
	}
	turns := &isolation.Turns{Workspaces: StagedWorkspaces{}, Views: isolation.Views{Directory: filepath.Join(cfg.Root.String(), "views")}, Grants: map[string]coreadapter.Capabilities{ArchitectRole: {Tools: []string{"file_read", shed.ReplyTool}}}, Hosts: hosts, Engine: engine,
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Workstream != string(stream) || scope.Role != ArchitectRole {
				return isolation.Selection{}, errors.New("turn scope denied")
			}
			_, settings, err := a.S.RoleExecution(cfg, ArchitectRole)
			if err != nil {
				return isolation.Selection{}, err
			}
			workspace := filepath.Join(a.Drafter().TurnDirectory(stream, scope.Turn), "workspace")
			paths, err := a.Stage(ctx, cfg.Project.Clone, stream, RoundInput{Round: n, Spec: 1, Plan: 1, Amendment: id}, workspace)
			if err != nil {
				return isolation.Selection{}, err
			}
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
		}, Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) || scope.Role != ArchitectRole || scope.Thread != ArchitectThread {
				return nil, errors.New("turn scope denied")
			}
			standing := make([]shed.Dissent, len(open))
			for i, e := range open {
				standing[i] = e.Dissent
			}
			file := filepath.Join(a.Drafter().TurnDirectory(stream, scope.Turn), "output", "reply.json")
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
	return a.S.MemoryTurns(turns, a.S.About(a.Repository), a.Repository)
}

func (a AmendmentDebate) Present(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, round int, state trace.WorkflowState) error {
	docs, err := trace.Read[trace.Document](a.Repository, stream)
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
		case AmendmentReplyPath(req.ID, round):
			reply = doc.Content
		case AmendmentPacketPath(req.ID):
			packetRevision = max(packetRevision, doc.Revision+1)
		}
	}
	records, err := AmendmentRecords(a.Repository, stream, req.ID)
	if err != nil {
		return err
	}
	dissent := AmendmentDissent(records)
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
	id := AmendmentStep(req.ID, round, "presented")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "amendment-" + req.ID + "-presented-packet", Revision: packetRevision, Project: a.Repository.Project(), Workstream: stream, At: a.S.Now(), Actor: ShedActor, Cause: id, Depth: 1}, Path: AmendmentPacketPath(req.ID), Content: string(data) + "\n"}
	cited := strings.Join(req.Citations, ", ")
	if m := req.Upstream; m != nil {
		cited += fmt.Sprintf(", after drift rebase %d moved upstream from %s to %s", m.Drift, m.From, m.To)
	}
	body := fmt.Sprintf("Inspect amendment %s and decide it internally if it only revises engineering assignments while preserving approved intent; otherwise present the intent decision to the owner. Request: %s. Reason: %s. Proposed change: %s. Affected units: %s. Dissent: %s. Recommendation: %s. The round cap approves nothing. Packet: %s, revision %d, after round %d. The owner approves, rejects, asks for another round or overrules the objections that stand, with osmia amendment %s %s or through you.", req.ID, cited, req.Reason, req.Change, affected, Standing(dissent), recommendation, AmendmentPacketPath(req.ID), packetRevision, round, stream, req.ID)
	for _, e := range dissent {
		body += fmt.Sprintf("\n- %s (%s, blocking=%t): %s", e.ID, e.Kind, e.Blocking, e.Argument)
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: a.Header(id, stream, AmendmentStep(req.ID, round, "answered"), a.S.Now()), Subject: AmendmentSubject(req.ID), From: "answered", To: "presented", Reason: "the amendment and dissent are ready for a decision"}, Events: []trace.Event{trace.Notice(id, "amendment", body)}}
	_, err = a.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	return err
}
