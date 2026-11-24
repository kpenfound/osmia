package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	DoneTool   = "done"
	MasonDone  = "done"
	ReportPath = "units/%s/report.json"
)

// MasonReport is what a mason reports with done: its outcome, which says
// what the unit's work now does and how the mason checked its acceptance.
type MasonReport struct {
	Outcome string `json:"outcome"`
}

type doneReport struct {
	MasonReport
	OwnerAction string `json:"owner_action,omitempty"`
}

const (
	ownerActionWording  = "The developer flagged an owner action, and the unit's report describes it."
	ownerActionHeadline = "Owner action in the report"
)

// UnitReport is the document units/<unit>/report.json: the mason's report
// on a unit, the turn and owner-facing card it came from, the seal the unit
// was built against, and the candidate the service made of the unit's
// workspace, a commit on Branch that descends from the feature branch at Base.
type UnitReport struct {
	Unit      string            `json:"unit"`
	Turn      string            `json:"turn"`
	Seal      int               `json:"seal"`
	Outcome   string            `json:"outcome"`
	Card      *coreadapter.Card `json:"card,omitempty"`
	Branch    string            `json:"branch"`
	Base      string            `json:"base"`
	Candidate string            `json:"candidate"`
}

func ReportDocument(unit string) string { return trace.UnitSubject(unit) + "-report" }

func CheckingTransitionID(unit string, k int) string {
	return fmt.Sprintf("%s-%s-%d", trace.UnitSubject(unit), UnitChecking, k)
}

type masonReports struct {
	mu       sync.Mutex
	accepted map[string]reportedDone
	refused  map[string]string
}

func (r *masonReports) refuse(key, reason string) (json.RawMessage, error) {
	r.mu.Lock()
	if r.refused == nil {
		r.refused = map[string]string{}
	}
	r.refused[key] = reason
	r.mu.Unlock()
	return refuseReport("%s", reason)
}

type reportedDone struct {
	Report      MasonReport
	Card        *coreadapter.Card
	OwnerAction string
}

func TurnKey(scope coreadapter.Scope) string {
	return scope.Workstream + "/" + scope.Thread + "/" + scope.Turn
}

func (r *masonReports) tool(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	done := coreadapter.Tool{Name: DoneTool, Effect: coreadapter.ToolMemory,
		Description: "Report your unit's work done, once its task is done and every acceptance item holds. Give the outcome: what the work now does and how you checked the acceptance. Include an owner-facing headline (64 characters), what happened (140 characters), and needs_you (140 characters, empty unless the owner has an action). Write a single line per card field, in words without IDs, paths, URLs or model names. The service shortens a card field that is too long, joins one written over several lines, and leaves out one it cannot show; without a headline or what happened it records no card. A needs_you always tells the owner there is an action. The service records the report, takes your workspace as the unit's candidate, runs the project's checks on it and sends it to review; end your turn as soon as this returns.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"outcome":{"type":"string"},"headline":{"type":"string"},"happened":{"type":"string"},"needs_you":{"type":"string"}},"required":["outcome"],"additionalProperties":false}`)}
	done.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			MasonReport
			coreadapter.Card
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		state, err := repository.Workflow(config.WorkstreamID(scope.Workstream), trace.UnitSubject(scope.Unit))
		if err != nil {
			return nil, err
		}
		canReport := state.Value == UnitImplementing
		if state.Value == UnitWaiting {
			stream := config.WorkstreamID(scope.Workstream)
			own, err := questionWaitOwner(repository, stream, scope.Unit, scope.Thread, masonActor)
			if err != nil {
				return nil, err
			}
			if own {
				th, err := repository.Thread(stream, scope.Thread)
				if err != nil {
					return nil, err
				}
				asked, err := repository.Questions(stream)
				if err != nil {
					return nil, err
				}
				waited, delivered, reason, err := waitedQuestion(repository, stream, scope.Unit, th, asked)
				if err != nil {
					return nil, err
				}
				canReport = delivered && reason == "" && scope.Role == masonRole && scope.Thread == masonAgent(scope.Unit) && scope.Turn == questions.TurnID(waited.Asked.ID)
			}
		}
		if !canReport {
			return r.refuse(TurnKey(scope), fmt.Sprintf("unit %s is %s, not implementing", scope.Unit, state.Value))
		}
		known := []string{scope.Project, scope.Workstream, scope.Unit, scope.Thread, scope.Turn}
		threads, err := repository.Threads(config.WorkstreamID(scope.Workstream))
		if err != nil {
			return nil, err
		}
		for _, thread := range threads {
			known = append(known, thread.Identity.ID, thread.Identity.ThreadID, thread.Identity.Session.ID)
			for _, turn := range thread.Turns {
				known = append(known, turn.Request.TurnID, turn.Request.Profile.Model)
				if turn.Response != nil {
					known = append(known, turn.Response.Result.Session.ID)
				}
			}
		}
		if reason := checkReport(input.MasonReport); reason != "" {
			return r.refuse(TurnKey(scope), reason)
		}
		fitted := status.FitCard(input.Card, known)
		accepted := reportedDone{Report: input.MasonReport, Card: fitted.Card}
		if fitted.OwnerAction {
			accepted.OwnerAction = fitted.NeedsYou
			if accepted.OwnerAction == "" {
				accepted.OwnerAction = ownerActionWording
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		key := TurnKey(scope)
		if _, ok := r.accepted[key]; ok {
			return refuseReport("this turn already reported its unit done; end the turn")
		}
		if r.accepted == nil {
			r.accepted = map[string]reportedDone{}
		}
		r.accepted[key] = accepted
		return json.Marshal(struct {
			Recorded    bool     `json:"recorded"`
			Next        string   `json:"next"`
			Adjustments []string `json:"adjustments,omitempty"`
		}{true, "End your turn now. The service takes your workspace as the unit's candidate and sends it to review.", fitted.Adjustments})
	}
	return done
}

func (r *masonReports) DriftTool(scope coreadapter.Scope) coreadapter.Tool {
	done := coreadapter.Tool{Name: DoneTool, Effect: coreadapter.ToolMemory,
		Description: "Report your resolution done, once no conflicted file carries a conflict marker. Give the outcome: what you resolved and how. The service checks the files and goes on with the rebase; end your turn as soon as this returns.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"outcome":{"type":"string"}},"required":["outcome"],"additionalProperties":false}`)}
	done.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Outcome string `json:"outcome"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if strings.TrimSpace(input.Outcome) == "" {
			return refuseReport("outcome is required: say what you resolved and how")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		key := TurnKey(scope)
		if _, ok := r.accepted[key]; ok {
			return refuseReport("this turn already reported its resolution done; end the turn")
		}
		if r.accepted == nil {
			r.accepted = map[string]reportedDone{}
		}
		r.accepted[key] = reportedDone{Report: MasonReport{Outcome: input.Outcome}}
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Next     string `json:"next"`
		}{true, "End your turn now. The service checks the conflicted files and goes on with the rebase."})
	}
	return done
}

func checkReport(report MasonReport) string {
	if strings.TrimSpace(report.Outcome) == "" {
		return "outcome is required: say what the unit's work now does and how you checked its acceptance"
	}
	return ""
}

type reportingTurns struct {
	Turns   coreadapter.Turns
	reports *masonReports
}

var _ coreadapter.Turns = (*reportingTurns)(nil)

var _ coreadapter.ResumeChecker = (*reportingTurns)(nil)

func (t *reportingTurns) Run(ctx context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	result, err := t.Turns.Run(ctx, prepared)
	t.reports.mu.Lock()
	defer t.reports.mu.Unlock()
	key := TurnKey(prepared.Scope)
	report, ok := t.reports.accepted[key]
	refusal := t.reports.refused[key]
	delete(t.reports.accepted, key)
	delete(t.reports.refused, key)
	if !ok {
		result.Refusal = refusal
	}
	if ok {
		data, encodeErr := json.Marshal(doneReport{MasonReport: report.Report, OwnerAction: report.OwnerAction})
		if encodeErr != nil {
			return result, errors.Join(err, encodeErr)
		}
		result.Outcome = &coreadapter.Outcome{Status: MasonDone, Report: string(data)}
		result.Outcome.Card = report.Card
	}
	return result, err
}

func (t *reportingTurns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}

func reportTaken(q trace.QueuedTurn) bool {
	switch q.Status() {
	case "idle":
	case "failed":
		if Exhausted(q) {
			return false
		}
	default:
		return false
	}
	return q.Response.Result.Outcome != nil && q.Response.Result.Outcome.Status == MasonDone
}

func (m *masons) Reported(stream config.WorkstreamID, unit string) (MasonReport, trace.QueuedTurn, bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	if err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, err
	}
	last := th.Turns[len(th.Turns)-1]
	if !reportTaken(last) {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	report, err := parseDoneReport(last)
	if err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, err
	}
	return report.MasonReport, last, true, nil
}

func parseDoneReport(turn trace.QueuedTurn) (doneReport, error) {
	var report doneReport
	if err := json.Unmarshal([]byte(turn.Response.Result.Outcome.Report), &report); err != nil {
		return doneReport{}, fmt.Errorf("the report of turn %s: %w", turn.Request.TurnID, err)
	}
	return report, nil
}

func (m *masons) finish(ctx context.Context, b building, unit string) (moved, blocked bool, err error) {
	moved, blocked, _, err = m.finishReport(ctx, b, unit)
	return moved, blocked, err
}

func (m *masons) finishReport(ctx context.Context, b building, unit string) (moved, blocked bool, uncollected error, err error) {
	if ordinarySendBack(b.reviewTransitions[trace.UnitSubject(unit)]) {
		if err := (&reviewers{masons: m}).resumeFindings(ctx, b.stream, unit, b.states[trace.UnitSubject(unit)]); err != nil {
			return false, false, nil, err
		}
	}
	report, turn, found, err := m.Reported(b.stream, unit)
	if err != nil || !found {
		return false, false, nil, err
	}
	units := NewUnitWorkspaces(m.cfg, m.repository)
	if behind, err := units.Behind(ctx, b.stream, unit); err != nil || behind {
		return false, false, nil, err
	}
	w, base, candidate, err := units.Snapshot(ctx, b.stream, unit)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, nil, err
		}
		if turn.Status() == "failed" {
			return false, false, err, nil
		}
		return false, true, nil, m.block(ctx, b.stream, unit, turn.Response.ID, fmt.Sprintf("unit %s stays implementing: its mason reported done, and its candidate cannot be made: %v", unit, err))
	}
	conflicted, rebase, err := m.Conflicts(b.stream, unit)
	if err != nil {
		return false, false, nil, err
	}
	g, err := units.Of(b.stream)
	if err != nil {
		return false, false, nil, err
	}
	if len(conflicted) != 0 {
		th, err := m.repository.Thread(b.stream, masonAgent(unit))
		if err != nil {
			return false, false, nil, err
		}
		resolution := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.TurnID == ResolveTurnID(unit, rebase.Rebase)
		})
		if resolution < 0 || turn.Sequence < th.Turns[resolution].Sequence {
			return false, false, nil, nil
		}
	}
	stored, err := g.StoredConflicts(ctx, candidate)
	if err != nil {
		return false, false, nil, err
	}
	marked, err := g.Markers(ctx, candidate, conflicted)
	if err != nil {
		return false, false, nil, err
	}
	for _, p := range marked {
		if !slices.Contains(stored, p) {
			stored = append(stored, p)
		}
	}
	if len(stored) != 0 {
		slices.Sort(stored)
		moved, err := m.remind(ctx, b.stream, unit, b.states[trace.UnitSubject(unit)], turn, stored)
		return moved, false, nil, err
	}
	latest, _, _, err := seal.Latest(m.repository, b.stream)
	if err != nil {
		return false, false, nil, err
	}
	docs, err := trace.Read[trace.Document](m.repository, b.stream)
	if err != nil {
		return false, false, nil, err
	}
	k := 1
	for _, d := range docs {
		if d.ID == ReportDocument(unit) {
			k = d.Revision + 1
		}
	}
	card := turn.Response.Result.Outcome.Card
	done, err := parseDoneReport(turn)
	if err != nil {
		return false, false, nil, err
	}
	content, err := json.MarshalIndent(UnitReport{Unit: unit, Turn: turn.Request.TurnID, Seal: latest.Seal, Outcome: report.Outcome, Card: card, Branch: w.Branch, Base: base, Candidate: candidate}, "", "  ")
	if err != nil {
		return false, false, nil, err
	}
	at := m.s.Now()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ReportDocument(unit), Revision: k, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: at, Actor: masonActor, Cause: turn.Response.ID, Depth: turn.Request.Depth}
	doc := trace.Document{Header: h, Path: fmt.Sprintf(ReportPath, unit), Content: string(content) + "\n"}
	subject := trace.UnitSubject(unit)
	reason := fmt.Sprintf("the mason of unit %s reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is %s revision %d", unit, turn.Request.TurnID, candidate, w.Branch, featureBranch(b.stream), base, doc.Path, k)
	ownerAction := done.OwnerAction
	body := finishNotice(unit, turn.Request.TurnID, doc.Path, k, card, ownerAction)
	event := trace.Progress(CheckingTransitionID(unit, k), "unit", body)
	if ownerAction != "" {
		event = trace.Notice(CheckingTransitionID(unit, k), "unit", body)
	}
	res, err := m.s.raise(ctx, true, UnitExitInput{
		Repo: m.repository, Stream: b.stream, Unit: unit, State: b.states[subject], Trigger: TriggerMasonDone, Cause: turn.Response.ID,
		Default: UnitChecking, Targets: []string{UnitChecking, UnitContested}, Actor: masonActor,
		ID: CheckingTransitionID(unit, k), HeaderCause: turn.Response.ID, Reason: reason, Events: []trace.Event{event}, Documents: []trace.Document{doc},
		Candidate: func(context.Context) (string, error) { return "", nil },
		Evidence: router.Evidence{Facts: map[string]any{"report_revision": k},
			Records: []router.Entry{{Kind: "report", ID: doc.ID, Text: reportText(report.Outcome, card, ownerAction)}}},
	})
	return err == nil && (res.Committed || res.Handed), false, nil, err
}

func reportText(outcome string, card *coreadapter.Card, ownerAction string) string {
	text := "Outcome: " + outcome
	if card != nil {
		text += "\nHeadline: " + card.Headline
	}
	if ownerAction != "" {
		text += "\nNeeds the owner: " + ownerAction
	}
	return text
}

func finishNotice(unit, turn, path string, revision int, card *coreadapter.Card, ownerAction string) string {
	message := fmt.Sprintf("Unit %s is checking: its mason reported done on turn %s; its report is %s revision %d.", unit, turn, path, revision)
	switch {
	case card != nil:
		message += " Headline: " + card.Headline
	case ownerAction != "":
		message += " Headline: " + ownerActionHeadline
	}
	if ownerAction != "" {
		message += "; Needs you: " + ownerAction
	}
	return message
}
