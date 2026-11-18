package jev

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// The decide-transition task: one request that asks several narrow questions
// about one exit of a state machine. Code turns the answers into the next
// state; the task only judges.
const (
	// TransitionTask is the judgment task name of a transition request.
	TransitionTask = "decide-transition"
	// TransitionVersion numbers the task's questions and the meaning of their
	// answers. Bump it whenever either changes; decisions recorded under an
	// older version stay as they were.
	TransitionVersion = 1
	// TransitionKind is the kind of the occurrences a transition request is
	// recorded under.
	TransitionKind = "transition"
)

// The IDs of the questions every transition request asks. The trigger's own
// questions have the IDs OwnQuestionPrefix and the question's name.
const (
	// QuestionFit asks whether the default is the right next state.
	QuestionFit = "fit"
	// QuestionPickA and QuestionPickB ask which legal target fits, with the
	// options in two different orders.
	QuestionPickA = "pick_a"
	QuestionPickB = "pick_b"
	// QuestionStuck asks whether the subject makes no progress where it is.
	QuestionStuck = "stuck"
	// QuestionIntent asks whether any way forward other than the default would
	// change what the owner approved.
	QuestionIntent = "intent"
	// OwnQuestionPrefix starts the ID of a question specific to the trigger.
	OwnQuestionPrefix = "own_"
)

const (
	fitInstructions    = "The default next state is the right next state for this subject, given the evidence."
	stuckInstructions  = "This subject makes no progress if it is left in its current state."
	intentInstructions = "Any way forward other than the default next state would change what the owner approved: its intent, its constraints or its scope."
	pickInstructions   = "Choose the next state that is right for this subject, given the evidence. Each option says what it does."
)

var ownName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// TransitionOption is one legal target, as Jev sees it.
type TransitionOption struct {
	Name        string
	Description string
}

// TransitionQuestion is a question specific to the trigger: a proposition
// whose probability Jev gives. Name is lowercase letters, digits and
// underscores.
type TransitionQuestion struct {
	Name         string
	Instructions string
}

// TransitionRequest asks the narrow questions of one exit.
type TransitionRequest struct {
	// Scope names where the request is made; its project and workstream are
	// required.
	Scope coreadapter.Scope
	// Cause is the trace record that raised the exit.
	Cause string
	// Occurrence is the key of the exit's occurrence.
	Occurrence string
	// State is the structured summary Jev reads, built from records only.
	State any
	// Options are the legal targets in the order of the first pick question.
	// The second pick question asks them in the reverse order. There are at
	// least two.
	Options []TransitionOption
	// Questions are the trigger's own questions.
	Questions []TransitionQuestion
	Sources   []Source
}

// TransitionPick is one pick question's answer: the option chosen and its
// confidence.
type TransitionPick struct {
	Choice     string
	Confidence float64
}

// TransitionAnswers are the answers to one transition request: the
// probabilities of the propositions and the two picks.
type TransitionAnswers struct {
	Fit, Stuck, Intent float64
	PickA, PickB       TransitionPick
	// Own are the probabilities of the trigger's own questions by name.
	Own map[string]float64
}

// TransitionResult is what a transition request produced: Jev's answers, or
// why there are none.
type TransitionResult struct {
	// Answered says Jev's answers are in Answers. Otherwise the request failed
	// and Reason and Detail say how.
	Answered bool
	Answers  TransitionAnswers
	// Judgment is the judgment record, empty when nothing was recorded.
	Judgment string
	Reason   Reason
	Detail   string
	// Recorded says the answers were read from the trace, not asked now.
	Recorded bool
}

// TransitionOccurrenceRecorded reports whether the trace holds an accepted
// answer for the occurrence of a transition request, and returns it.
func TransitionOccurrenceRecorded(repo *trace.Repository, stream config.WorkstreamID, key string) (TransitionResult, bool, error) {
	records, err := occurrenceRecords(repo, stream, DecisionKind(TransitionKind), key)
	if err != nil {
		return TransitionResult{}, false, err
	}
	for _, rec := range records {
		if rec.record.State != StateAccepted {
			continue
		}
		answers, ok := transitionAnswers(rec.record.Answers)
		if !ok {
			continue
		}
		return TransitionResult{Answered: true, Answers: answers, Judgment: rec.id, Recorded: true}, true, nil
	}
	return TransitionResult{}, false, nil
}

func transitionAnswers(in map[string]systemone.Answer) (TransitionAnswers, bool) {
	var out TransitionAnswers
	noul := func(id string) (float64, bool) {
		a, ok := in[id]
		return a.Noul, ok && a.Kind == systemone.KindNoul
	}
	pick := func(id string) (TransitionPick, bool) {
		a, ok := in[id]
		return TransitionPick{Choice: a.Choice, Confidence: a.Confidence}, ok && a.Kind == systemone.KindChoice && a.Choice != ""
	}
	var ok [5]bool
	out.Fit, ok[0] = noul(QuestionFit)
	out.Stuck, ok[1] = noul(QuestionStuck)
	out.Intent, ok[2] = noul(QuestionIntent)
	out.PickA, ok[3] = pick(QuestionPickA)
	out.PickB, ok[4] = pick(QuestionPickB)
	if slices.Contains(ok[:], false) {
		return TransitionAnswers{}, false
	}
	for id, a := range in {
		if name, own := strings.CutPrefix(id, OwnQuestionPrefix); own && a.Kind == systemone.KindNoul {
			if out.Own == nil {
				out.Own = map[string]float64{}
			}
			out.Own[name] = a.Noul
		}
	}
	return out, true
}

// transitionQuestions returns the questions of a request, keyed by ID.
func transitionQuestions(req TransitionRequest) (map[string]systemone.Question, []string, error) {
	if len(req.Options) < 2 {
		return nil, nil, errors.New("a transition request needs at least two legal targets")
	}
	options := make([]systemone.Option, len(req.Options))
	reversed := make([]systemone.Option, len(req.Options))
	names := make([]string, len(req.Options))
	seen := map[string]bool{}
	for i, o := range req.Options {
		if strings.TrimSpace(o.Name) == "" || seen[o.Name] {
			return nil, nil, fmt.Errorf("transition option %q is empty or repeated", o.Name)
		}
		seen[o.Name] = true
		opt := systemone.Option{Name: o.Name, Description: o.Description}
		options[i], reversed[len(options)-1-i], names[i] = opt, opt, o.Name
	}
	questions := map[string]systemone.Question{
		QuestionFit:    systemone.Noul(fitInstructions),
		QuestionStuck:  systemone.Noul(stuckInstructions),
		QuestionIntent: systemone.Noul(intentInstructions),
		QuestionPickA:  systemone.Choice(pickInstructions, options...),
		QuestionPickB:  systemone.Choice(pickInstructions, reversed...),
	}
	for _, q := range req.Questions {
		if !ownName.MatchString(q.Name) || strings.TrimSpace(q.Instructions) == "" {
			return nil, nil, fmt.Errorf("transition question %q needs a lowercase name and instructions", q.Name)
		}
		id := OwnQuestionPrefix + q.Name
		if _, dup := questions[id]; dup {
			return nil, nil, fmt.Errorf("transition question %q is repeated", q.Name)
		}
		questions[id] = systemone.Noul(q.Instructions)
	}
	return questions, names, nil
}

// DecideTransition asks the narrow questions of one exit and returns Jev's
// answers. The occurrence is asked once: a recorded answer is returned as
// recorded, whatever the request is now and after a restart, and the asks of
// one occurrence are serialised. A request that fails (a timeout, a
// cool-down, an outage, an unusable answer or a request over MaxDecisionBytes)
// is a recorded fallback with no answers, and marks Jev degraded until a
// request is answered. It must not run on the scheduling path.
func (j *Judge) DecideTransition(ctx context.Context, repo *trace.Repository, req TransitionRequest) TransitionResult {
	failed := func(reason Reason, detail, id string) TransitionResult {
		j.noteTransition(reason, detail)
		return TransitionResult{Reason: reason, Detail: detail, Judgment: id}
	}
	switch {
	case strings.TrimSpace(req.Occurrence) == "" || len(req.Occurrence) > maxOccurrenceKey || strings.ContainsFunc(req.Occurrence, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return failed(ReasonRejected, "an occurrence key of 1 to 256 characters is required", "")
	case req.Scope.Workstream == "":
		return failed(ReasonRejected, "transition scope needs a workstream", "")
	}
	questions, names, err := transitionQuestions(req)
	if err != nil {
		return failed(ReasonRejected, err.Error(), "")
	}
	stream := config.WorkstreamID(req.Scope.Workstream)
	unlock := lockOccurrence(repo, stream, DecisionKind(TransitionKind), req.Occurrence)
	defer unlock()
	if res, ok, err := TransitionOccurrenceRecorded(repo, stream, req.Occurrence); err != nil {
		return failed(ReasonUnrecorded, "judgment records cannot be read", "")
	} else if ok {
		return res
	}
	records, err := occurrenceRecords(repo, stream, DecisionKind(TransitionKind), req.Occurrence)
	if err != nil {
		return failed(ReasonUnrecorded, "judgment records cannot be read", "")
	}
	asks := 0
	for _, r := range records {
		if r.record.State == StateFallback {
			asks++
		}
	}
	threshold := j.threshold()
	scope := req.Scope
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(TransitionKind+"\x00"+req.Occurrence)))[:16]
	if scope.Thread == "" {
		scope.Thread = "decisions"
	}
	if scope.Turn == "" {
		scope.Turn = "decision-" + digest
	}
	if scope.Role == "" {
		scope.Role = "service"
	}
	cause := req.Cause
	if cause == "" {
		cause = "decision-" + digest
	}
	judgment := Judgment{
		Scope: scope, Cause: cause, Task: TransitionTask, Version: TransitionVersion,
		Acceptance: acceptance("", threshold), Sources: req.Sources,
		Occurrence: &Occurrence{Kind: TransitionKind, Key: req.Occurrence, Ask: asks, Threshold: threshold},
		MaxBytes:   MaxDecisionBytes,
		Request:    systemone.Request{State: req.State, Questions: questions},
		Accept: func(r systemone.Response) string {
			for id, q := range questions {
				a, ok := r.Answers[id]
				switch {
				case !ok || a.Kind != q.Kind:
					return fmt.Sprintf("question %s is not answered as a %s", id, q.Kind)
				case q.Kind == systemone.KindChoice && !slices.Contains(names, a.Choice):
					return fmt.Sprintf("question %s picks none of the options", id)
				case q.Kind == systemone.KindNoul && (a.Noul < 0 || a.Noul > 1):
					return fmt.Sprintf("question %s answers outside 0 to 1", id)
				}
			}
			return ""
		},
	}
	d := j.Evaluate(ctx, repo, judgment)
	if d.Outcome != Accepted {
		return failed(d.Reason, d.Detail, d.ID)
	}
	answers, ok := transitionAnswers(d.Response.Answers)
	if !ok {
		return failed(ReasonUnusable, "the answers are incomplete", d.ID)
	}
	j.clearTransition()
	return TransitionResult{Answered: true, Answers: answers, Judgment: d.ID, Recorded: d.Recovered}
}

// transitionFailure is the latest failed transition request.
type transitionFailure struct {
	reason Reason
	detail string
	at     time.Time
}

func (j *Judge) noteTransition(reason Reason, detail string) {
	if reason == ReasonCancelled {
		return
	}
	j.failMu.Lock()
	defer j.failMu.Unlock()
	j.failure = &transitionFailure{reason: reason, detail: detail, at: j.now()}
}

func (j *Judge) clearTransition() {
	j.failMu.Lock()
	defer j.failMu.Unlock()
	j.failure = nil
}

// transitionDegraded returns the latest transition request's failure since the
// last answered one, if any.
func (j *Judge) transitionDegraded() *transitionFailure {
	j.failMu.Lock()
	defer j.failMu.Unlock()
	return j.failure
}
