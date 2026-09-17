package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// ReplyAction is the runner-boundary operation action that runs the
// architect's one reply to a heard shed round.
const ReplyAction = "shed-reply"

const (
	// maxRedrafts bounds the redrafts the architect may deliver in one reply
	// before an invalid one is given up, and maxReplyAttempts the turns of one
	// reply that service stops may interrupt: the architect's drafting bounds.
	maxRedrafts      = maxDrafts
	maxReplyAttempts = maxDraftAttempts
	replyFile        = "reply.json"
)

func replyIDs(n int) (transition, event string) {
	transition = fmt.Sprintf("shed-reply-%d", n)
	return transition, trace.EventID(transition, "run")
}
func replyTurnPrefix(n int) string { return fmt.Sprintf("reply-%d-", n) }
func replyTurnID(n, k int) string  { return replyTurnPrefix(n) + strconv.Itoa(k) }

// replier reconciles the architect's reply operations of the shed controller.
type replier struct{ *debate }

var _ coreadapter.Reconciler = replier{}

func (d *debate) drafter() *drafter { return &drafter{s: d.s, repository: d.repository} }

// requestReply publishes the architect's reply to a heard round as a durable
// operation pinned to the revision the round debated.
func (d *debate) requestReply(ctx context.Context, stream config.WorkstreamID, state trace.WorkflowState, in roundInput, open int) error {
	if err := d.drafter().ensureThread(ctx, stream); err != nil {
		return err
	}
	transition, event := replyIDs(in.Round)
	input, err := encodeRound(in)
	if err != nil {
		return err
	}
	round, _ := roundIDs(in.Round)
	op := coreadapter.Operation{ID: trace.OperationID(d.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: input}
	reason := fmt.Sprintf("%d objections stand after round %d; the architect is asked for its reply", open, in.Round)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(transition, stream, round+"-heard", d.s.now()), Subject: shedSubject, From: state.Value, To: fmt.Sprintf("reply-%d", in.Round), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: "shed-reply", Body: fmt.Sprintf("Architect's reply to committee round %d", in.Round), Operation: &op}}}
	_, err = d.repository.Transact(ctx, tx)
	return err
}

// replyOutcome returns the recorded terminal result of the reply to round n:
// succeeded once the reply is recorded, failed when its failed transition is
// recorded, nil before either.
func (d *debate) replyOutcome(stream config.WorkstreamID, n int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](d.repository, stream)
	if err != nil {
		return nil, err
	}
	reply, _ := replyIDs(n)
	for _, t := range transitions {
		switch {
		case t.Subject != shedSubject:
		case t.ID == reply+"-replied":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case t.ID == reply+"-failed":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// replyTurns returns the architect's turns of the reply to round n, in order.
func replyTurns(t trace.Thread, n int) []trace.QueuedTurn {
	prefix := replyTurnPrefix(n)
	var out []trace.QueuedTurn
	for _, q := range t.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) {
			out = append(out, q)
		}
	}
	return out
}

func (r replier) decode(op coreadapter.Operation) (roundInput, config.WorkstreamID, error) {
	in, err := decodeShed(op, ReplyAction)
	if err != nil {
		return in, "", err
	}
	_, event := replyIDs(in.Round)
	stream, err := r.owner(op, event)
	return in, stream, err
}

// Inspect reads the recorded transitions and the architect thread. A recorded
// outcome completes the operation; a turn this session is running is unknown;
// everything else is absent, and Apply decides between running, retrying and
// recording.
func (r replier) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, stream, err := r.decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := r.replyOutcome(stream, in.Round)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "reply " + result.Outcome, Result: result}, nil
	}
	t, err := r.repository.Thread(stream, architectAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if turns := replyTurns(t, in.Round); len(turns) > 0 {
		if last := turns[len(turns)-1]; last.Claim != nil && last.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "architect turn " + last.Request.TurnID + " is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the reply to round %d is not recorded", in.Round)}, nil
}

// Apply drives the reply to a terminal result. The architect gets one turn
// for the round; a turn that ends normally with an invalid redraft is followed
// by another that returns the problems, while redrafts remain, and an invalid
// redraft is never recorded. The reply and a valid redraft are recorded in one
// commit and the shed moves to replied-<n>. A failed turn and a redraft given
// up are recorded in the reply, so the debate goes on without them.
// Abandoning the workstream cancels the running turn and fails a reply that
// has no record; a reply whose file is committed is replied all the same.
// Storage errors and a missing architect runner leave the operation pending
// for another attempt.
func (r replier) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	d := r.debate
	none := coreadapter.OperationResult{}
	in, stream, err := r.decode(op)
	if err != nil {
		return none, err
	}
	if result, err := d.replyOutcome(stream, in.Round); err != nil || result != nil {
		if err != nil {
			return none, err
		}
		return *result, nil
	}
	cfg := d.s.current()
	if !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return none, errors.New("the project is not active")
	}
	n := in.Round
	// Abandoning the workstream cancels the running turn, not the recording
	// of what it left.
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.s.turns.add(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return none, err
		}
		// A reply whose file is committed was given, whatever happened to the
		// workstream since.
		if recorded, err := d.recordedReply(stream, n); err != nil || recorded != nil {
			if err != nil {
				return none, err
			}
			return d.replied(ctx, op.ID, stream, *recorded)
		}
		t, err := d.repository.Thread(stream, architectAgent)
		if err != nil {
			return none, err
		}
		turns := replyTurns(t, n)
		var last *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		reply := shed.Reply{Version: shed.Version, Round: n, Revision: in.pin()}
		switch {
		case last != nil && last.Claim != nil && last.Response == nil:
			if t.Status != "interrupted" {
				return none, errors.New("architect turn " + last.Request.TurnID + " is still running")
			}
			if err := d.repository.AbandonTurn(ctx, stream, architectAgent, last.Request.TurnID, d.s.now()); err != nil {
				return none, err
			}
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(d.repository, stream); err != nil || gone {
				if err != nil {
					return none, err
				}
				return d.replyFailed(ctx, op.ID, stream, n, abandonedReply(n))
			}
			interrupted := len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.Status() != "interrupted" }))
			if interrupted >= maxReplyAttempts {
				reply.Failure = fmt.Sprintf("the architect's turn was interrupted %d times by service stops", interrupted)
				return d.recordReply(ctx, op.ID, stream, turns, reply, nil)
			}
			if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if err := d.enqueueReply(ctx, cfg, stream, in, turns, op.ID); err != nil {
				return none, err
			}
		case last.CompletedAt.IsZero():
			gone, err := abandoned(d.repository, stream)
			if err != nil {
				return none, err
			}
			if gone && last.Response == nil {
				if _, err := d.repository.CancelTurns(ctx, stream, d.s.now(), abandonActor, cancelReason); err != nil {
					return none, err
				}
				continue
			}
			// Completing a captured turn runs no architect.
			turnCtx := running
			if last.Response != nil {
				turnCtx = ctx
			} else if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if _, err := d.dispatchReply(turnCtx, stream, in, last.Request.TurnID); err != nil {
				return none, err
			}
			if err := os.RemoveAll(filepath.Join(d.drafter().turnDirectory(stream, last.Request.TurnID), "workspace")); err != nil {
				return none, err
			}
		case last.Status() == "idle":
			files, problems, err := d.redraft(stream, last.Request.TurnID)
			if err != nil {
				return none, err
			}
			if len(problems) == 0 {
				return d.recordReply(ctx, op.ID, stream, turns, reply, files)
			}
			if redrafts(turns) >= maxRedrafts {
				reply.Problems = problems
				return d.recordReply(ctx, op.ID, stream, turns, reply, nil)
			}
			if d.s.options.Architect == nil {
				return none, errNoArchitect
			}
			if err := d.enqueueReply(ctx, cfg, stream, in, turns, op.ID); err != nil {
				return none, err
			}
		default:
			reply.Failure = "the turn ended with status " + last.Status()
			if last.Response != nil && last.Response.Failure != "" {
				reply.Failure = last.Response.Failure
			}
			return d.recordReply(ctx, op.ID, stream, turns, reply, nil)
		}
	}
}

// redrafts counts the turns of a reply that ended normally: each delivered
// what the architect wanted to, so each one after the first followed an
// invalid redraft.
func redrafts(turns []trace.QueuedTurn) int {
	return len(slices.DeleteFunc(slices.Clone(turns), func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() || q.Status() != "idle" }))
}

// recordedReply returns the recorded reply to round n, nil when there is none.
func (d *debate) recordedReply(stream config.WorkstreamID, n int) (*shed.Reply, error) {
	replies, err := shed.Replies(d.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, r := range replies {
		if r.Round == n {
			return &r, nil
		}
	}
	return nil, nil
}

// redraft reads the spec.md and plan.json the turn delivered and validates
// them with the latest recorded revision of whichever was not delivered. It
// returns the delivered files that differ from the latest revision, none when
// the architect left the revision as it is, and every problem of an invalid
// redraft.
func (d *debate) redraft(stream config.WorkstreamID, turn string) (map[string]string, []string, error) {
	dr := d.drafter()
	latest, err := dr.latest(stream)
	if err != nil {
		return nil, nil, err
	}
	specDoc, planDoc := latest[plan.SpecDocument], latest[plan.PlanDocument]
	files := map[string]string{}
	var problems []string
	for _, doc := range []struct {
		path   string
		latest *trace.Document
	}{{plan.SpecPath, &specDoc}, {plan.PlanPath, &planDoc}} {
		data, err := os.ReadFile(filepath.Join(dr.turnDirectory(stream, turn), "output", doc.path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, err
		case !utf8.Valid(data):
			problems = append(problems, doc.path+" is not UTF-8 text")
		case string(data) != doc.latest.Content:
			files[doc.path], doc.latest.Content = string(data), string(data)
		}
	}
	if len(problems) == 0 && len(files) > 0 {
		if problems, err = dr.validate(specDoc, planDoc); err != nil {
			return nil, nil, err
		}
	}
	return files, problems, nil
}

// answers merges what the reply's turns answered, a later answer replacing an
// earlier one to the same objection, and names the last turn that ended. An
// interrupted turn contributes nothing.
func (d *debate) answers(stream config.WorkstreamID, turns []trace.QueuedTurn) ([]shed.Answer, string, error) {
	var answers []shed.Answer
	turn := ""
	for _, q := range turns {
		if q.CompletedAt.IsZero() || q.Status() == "interrupted" {
			continue
		}
		turn = q.Request.TurnID
		data, err := os.ReadFile(filepath.Join(d.drafter().turnDirectory(stream, turn), "output", replyFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		kept, err := shed.ParseReply(data)
		if err != nil {
			return nil, "", err
		}
		if kept.Turn != turn {
			return nil, "", fmt.Errorf("answers of turn %s belong to another turn", turn)
		}
		for _, a := range kept.Answers {
			answers = append(slices.DeleteFunc(answers, func(old shed.Answer) bool { return old.Objection == a.Objection }), a)
		}
	}
	return answers, turn, nil
}

// recordReply commits the architect's reply as shed/round-<n>/reply.json and
// the files of a valid redraft as revisions of spec.md and plan.json, all
// authored by the architect and caused by the operation, in one commit, then
// moves the shed to replied-<n>.
func (d *debate) recordReply(ctx context.Context, operation string, stream config.WorkstreamID, turns []trace.QueuedTurn, reply shed.Reply, files map[string]string) (coreadapter.OperationResult, error) {
	// A reply that is not committed yet records nothing for a workstream the
	// owner abandoned while the turn ran.
	if gone, err := abandoned(d.repository, stream); err != nil || gone {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return d.replyFailed(ctx, operation, stream, reply.Round, abandonedReply(reply.Round))
	}
	var err error
	if reply.Answers, reply.Turn, err = d.answers(stream, turns); err != nil {
		return coreadapter.OperationResult{}, err
	}
	latest, err := d.drafter().latest(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := d.s.now()
	header := func(id string, revision int) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: d.repository.Project(), Workstream: stream, At: at, Actor: architectActor, Cause: operation, Depth: 1}
	}
	var docs []trace.Document
	redraft := shed.Pin{Spec: latest[plan.SpecDocument].Revision, Plan: latest[plan.PlanDocument].Revision}
	if content, ok := files[plan.SpecPath]; ok {
		redraft.Spec++
		docs = append(docs, trace.Document{Header: header(plan.SpecDocument, redraft.Spec), Path: plan.SpecPath, Content: content})
	}
	if content, ok := files[plan.PlanPath]; ok {
		redraft.Plan++
		docs = append(docs, trace.Document{Header: header(plan.PlanDocument, redraft.Plan), Path: plan.PlanPath, Content: content})
	}
	if len(docs) > 0 {
		reply.Redraft = &redraft
	}
	data, err := shed.EncodeReply(reply)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs = append(docs, trace.Document{Header: header(shed.ReplyDocumentID(reply.Round), 1), Path: shed.ReplyPath(reply.Round), Content: string(data)})
	if err := d.repository.RecordDocuments(ctx, docs); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return d.replied(ctx, operation, stream, reply)
}

// replied moves the shed to replied-<n> for a recorded reply, with a reason
// that says what the reply holds.
func (d *debate) replied(ctx context.Context, operation string, stream config.WorkstreamID, reply shed.Reply) (coreadapter.OperationResult, error) {
	reason := fmt.Sprintf("the architect answered %d objections after round %d", len(reply.Answers), reply.Round)
	switch {
	case reply.Failure != "":
		reason += "; its turn failed: " + reply.Failure
	case reply.Redraft != nil:
		reason += " and redrafted: " + reply.Redraft.String()
	case len(reply.Problems) > 0:
		reason += fmt.Sprintf("; its redraft was given up as invalid and %s stays:\n- %s", reply.Revision, strings.Join(reply.Problems, "\n- "))
	default:
		reason += " and left " + reply.Revision.String() + " as it is"
	}
	return d.endReply(ctx, operation, stream, reply.Round, "replied", reason)
}

// abandonedReply is why the reply to round n of an abandoned workstream
// failed.
func abandonedReply(n int) string {
	return fmt.Sprintf("the reply to round %d failed: the workstream was abandoned, so the architect's reply is not recorded", n)
}

func (d *debate) replyFailed(ctx context.Context, operation string, stream config.WorkstreamID, n int, reason string) (coreadapter.OperationResult, error) {
	return d.endReply(ctx, operation, stream, n, "failed", reason)
}

// endReply ends the reply to round n with a shed transition, replied-<n> or
// failed-<n>, and returns the matching result. The transition already
// recorded is returned as it is.
func (d *debate) endReply(ctx context.Context, operation string, stream config.WorkstreamID, n int, kind, reason string) (coreadapter.OperationResult, error) {
	outcome := coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
	if kind == "replied" {
		outcome.Outcome = "succeeded"
	}
	state, err := d.repository.Workflow(stream, shedSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if state.Value != fmt.Sprintf("reply-%d", n) {
		result, err := d.replyOutcome(stream, n)
		if err != nil || result == nil {
			return coreadapter.OperationResult{}, errors.Join(err, fmt.Errorf("the reply to round %d is %q, not in progress", n, state.Value))
		}
		return *result, nil
	}
	id, _ := replyIDs(n)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: d.header(id+"-"+kind, stream, operation, d.s.now()), Subject: shedSubject, From: state.Value, To: fmt.Sprintf("%s-%d", kind, n), Reason: reason}}
	if _, err := d.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// standingAfter returns the dissent that stood once round n was heard, and
// that the owner has not dismissed: what the architect answers.
func (d *debate) standingAfter(stream config.WorkstreamID, n int) ([]shed.Entry, error) {
	records, err := d.earlier(stream, n+1)
	if err != nil {
		return nil, err
	}
	rulings, err := shed.AllRulings(d.repository, stream)
	if err != nil {
		return nil, err
	}
	return shed.Standing(shed.DissentRecord(records, rulings)), nil
}

// enqueueReply accepts the next architect turn of the reply, fixing its
// profile and prompts. A turn that follows an invalid redraft lists why it
// was not accepted.
func (d *debate) enqueueReply(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in roundInput, turns []trace.QueuedTurn, operation string) error {
	profile, _, err := d.s.roleExecution(cfg, architectRole)
	if err != nil {
		return err
	}
	open, err := d.standingAfter(stream, in.Round)
	if err != nil {
		return err
	}
	latest, err := latestPin(d.repository, stream)
	if err != nil {
		return err
	}
	var problems []string
	if previous := d.returned(turns); previous != "" {
		if _, problems, err = d.redraft(stream, previous); err != nil {
			return err
		}
	}
	turn := replyTurnID(in.Round, len(turns)+1)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: d.repository.Project(), Workstream: stream, At: d.s.now(), Actor: shedActor, Cause: operation, Depth: 1},
		AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: architectSystemPrompt(cfg.Project), Prompt: replyPrompt(in, latest, open, problems)}
	_, err = d.repository.EnqueueTurn(ctx, req)
	return err
}

// returned names the reply's latest turn that ended normally: the one whose
// redraft the next turn is returned, empty when no turn has.
func (d *debate) returned(turns []trace.QueuedTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if !turns[i].CompletedAt.IsZero() && turns[i].Status() == "idle" {
			return turns[i].Request.TurnID
		}
	}
	return ""
}

// dispatchReply runs the turn through the thread dispatcher and runner.
func (d *debate) dispatchReply(ctx context.Context, stream config.WorkstreamID, in roundInput, turn string) (coreadapter.OperationResult, error) {
	dr := d.drafter()
	dispatcher := thread.Dispatcher{Runner: thread.Runner{Store: d.repository, Turns: d.replyPath(stream, in), Now: d.s.now}, Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(dr.turnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(d.repository.Project(), "reply-turn", thread.TurnInput{Workstream: stream, Agent: architectAgent, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// replyPath is the architect's isolated turn path for a reply: a read-only
// private view of the latest spec and plan, the handed input, the charter,
// the context bundle and the shed's records, the file reading tool, the reply
// tool and the draft delivery tool, and no notes, write, execute, network or
// VCS capability.
func (d *debate) replyPath(stream config.WorkstreamID, in roundInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := d.s.options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	denied := errors.New("turn scope denied")
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(d.s.current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return d.selectReplyView(ctx, scope, in)
		},
		Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{"file_read", shed.ReplyTool, DraftTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(stream) || scope.Role != architectRole || scope.Project != string(d.repository.Project()) || scope.Thread != architectThread || !strings.HasPrefix(scope.Turn, replyTurnPrefix(in.Round)) {
				return nil, denied
			}
			open, err := d.standingAfter(stream, in.Round)
			if err != nil {
				return nil, err
			}
			standing := make([]shed.Dissent, len(open))
			for i, e := range open {
				standing[i] = e.Dissent
			}
			file := filepath.Join(d.drafter().turnDirectory(stream, scope.Turn), "output", replyFile)
			tools, err := shed.ReplyTools(shed.ReplyTurn{Reply: shed.Reply{Round: in.Round, Revision: in.pin(), Turn: scope.Turn}, Standing: standing,
				Save: func(r shed.Reply) error {
					data, err := shed.EncodeReply(r)
					if err != nil {
						return err
					}
					if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
						return err
					}
					// A rename keeps the previous answers whole if the write stops.
					if err := os.WriteFile(file+".tmp", data, 0600); err != nil {
						return err
					}
					return os.Rename(file+".tmp", file)
				}})
			return append(tools, d.drafter().draftTool(scope)), err
		},
		Hosts:  hosts,
		Engine: engine,
	}
}

// selectReplyView stages the architect's view for the claimed turn and
// selects all of it, read-only.
func (d *debate) selectReplyView(ctx context.Context, scope coreadapter.Scope, in roundInput) (isolation.Selection, error) {
	cfg := d.s.current()
	if scope.Role != architectRole || scope.Project != string(d.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != d.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return isolation.Selection{}, err
	}
	_, settings, err := d.s.roleExecution(cfg, architectRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(d.drafter().turnDirectory(stream, scope.Turn), "workspace")
	paths, err := d.stageReply(ctx, stream, in, scope.Turn, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stageReply builds the view: the latest spec.md and plan.json, the handed
// input under handed/, charter.md, the rendered context bundle as context.md,
// the shed's records under shed/ and, after an invalid redraft, what the
// architect delivered under redraft/. It returns the paths to select.
func (d *debate) stageReply(ctx context.Context, stream config.WorkstreamID, in roundInput, turn, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	dr := d.drafter()
	latest, err := dr.latest(stream)
	if err != nil {
		return nil, err
	}
	charter, err := d.repository.Charter(ctx, d.s.now())
	if err != nil {
		return nil, err
	}
	b, err := d.s.Context().Assemble(ctx, d.repository.Project(), bundle.Scope{Workstream: stream})
	if err != nil {
		return nil, err
	}
	files := map[string]string{plan.SpecPath: latest[plan.SpecDocument].Content, plan.PlanPath: latest[plan.PlanDocument].Content, "charter.md": charter.Content, "context.md": b.Render()}
	paths := []string{plan.SpecPath, plan.PlanPath, "charter.md", "context.md"}
	for _, doc := range latest {
		if directory, _, _ := strings.Cut(doc.Path, "/"); directory == "handed" || directory == "shed" {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, directory) {
				paths = append(paths, directory)
			}
		}
	}
	t, err := d.repository.Thread(stream, architectAgent)
	if err != nil {
		return nil, err
	}
	earlier := slices.DeleteFunc(replyTurns(t, in.Round), func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if previous := d.returned(earlier); previous != "" {
		delivered, _, err := d.redraft(stream, previous)
		if err != nil {
			return nil, err
		}
		for path, content := range delivered {
			files["redraft/"+path] = content
		}
		if len(delivered) > 0 {
			paths = append(paths, "redraft")
		}
	}
	for name, content := range files {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func replyPrompt(in roundInput, latest shed.Pin, open []shed.Entry, problems []string) string {
	var b strings.Builder
	redraft := ""
	if len(problems) > 0 {
		fmt.Fprintf(&b, "Your redraft was not accepted, and the committee will not read it:\n- %s\n\nWhat you delivered is in redraft/. Deliver every file of the redraft again with %s, corrected: only what this turn delivers counts. Deliver nothing to leave the revision as it is. Your answers so far are kept; answer an objection again only to replace what you said.\n\n", strings.Join(problems, "\n- "), DraftTool)
		redraft = "- redraft/: the files of your redraft that was not accepted.\n"
	}
	fmt.Fprintf(&b, "Round %d of the shed is heard. The committee debated %s. %d objections stand:\n", in.Round, in.pin(), len(open))
	for _, e := range open {
		weight := "advisory"
		if e.Blocking {
			weight = "blocking"
		}
		// The owner's own objection names no part and cites nothing.
		about := " on " + e.Part
		if e.Part == "" {
			about = ""
		}
		citing := ", citing " + strings.Join(e.Citations, ", ")
		if len(e.Citations) == 0 {
			citing = ""
		}
		fmt.Fprintf(&b, "- %s (%s, %s, by %s in round %d%s%s): %s\n", e.ID, e.Kind, weight, e.Member, e.Round, about, citing, e.Argument)
	}
	fmt.Fprintf(&b, `
What each kind asks of you:
- charter: a veto on the part it names. It blocks until its member concedes it after a redraft, or the owner disposes of it.
- size: split the unit by what it addresses. It blocks until its member concedes it.
- proof: name a proof that can show the criterion. It blocks until its member concedes it.
- fit: advice to the owner. It never blocks; answer it.
- owner: the owner's own objection. It blocks until the owner disposes of it; answer it as you would a member's.

Your view holds:
- spec.md and plan.json: the latest recorded revision, %s.
- handed/, charter.md and context.md: what you drafted from.
- shed/round-<n>/: what every member contributed to each round, and your earlier replies as reply.json.
%s
This is your one reply to this round. Answer each objection with %s, giving its ID and your answer. When the objections call for a change, also deliver the changed spec.md, plan.json or both with %s: the redraft is validated by the rules your draft was, and a valid one is the revision the committee debates next. An invalid one comes back to you. Deliver nothing to leave the revision as it is.`, latest, redraft, shed.ReplyTool, DraftTool)
	return b.String()
}
