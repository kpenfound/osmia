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
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// doneTool is the tool a mason ends its unit's work with.
	doneTool = "done"
	// masonDone is the outcome of a mason turn whose done the service
	// accepted. The outcome's report holds the mason's report as JSON.
	masonDone = "done"
	// reportPath is the path of a unit's report in its workstream's trace,
	// with the unit ID in place of %s.
	reportPath = "units/%s/report.json"
)

// MasonReport is what a mason reports with done: its outcome, which says
// what the unit's work now does and how the mason checked its acceptance,
// and any reusable project learnings.
type MasonReport struct {
	Outcome   string   `json:"outcome"`
	Learnings []string `json:"learnings,omitempty"`
}

// doneReport is the report of an outcome masonDone: the mason's report and,
// when the mason gave a needs_you, the text the move to checking tells the
// chief of staff the owner has to act on. It is the needs_you after fitting,
// or ownerActionWording when that cannot be shown, so the signal survives a
// card the service dropped.
type doneReport struct {
	MasonReport
	OwnerAction string `json:"owner_action,omitempty"`
}

const (
	// ownerActionWording is what a notice says when the needs_you the mason
	// gave cannot be shown.
	ownerActionWording = "The developer flagged an owner action, and the unit's report describes it."
	// ownerActionHeadline is the headline of a notice about an owner action
	// when the mason's card is gone.
	ownerActionHeadline = "Owner action in the report"
)

// UnitReport is the document units/<unit>/report.json: the mason's report
// on a unit, the turn and owner-facing card it came from, the seal the unit was built against,
// and the candidate the service made of the unit's workspace, a commit on
// Branch that descends from the feature branch at Base.
type UnitReport struct {
	Unit      string            `json:"unit"`
	Turn      string            `json:"turn"`
	Seal      int               `json:"seal"`
	Outcome   string            `json:"outcome"`
	Learnings []string          `json:"learnings,omitempty"`
	Card      *coreadapter.Card `json:"card,omitempty"`
	Branch    string            `json:"branch"`
	Base      string            `json:"base"`
	Candidate string            `json:"candidate"`
}

// reportDocument is the document ID of a unit's report.
func reportDocument(unit string) string { return trace.UnitSubject(unit) + "-report" }

// checkingTransitionID returns the ID of the transition that moves a unit
// to checking on its report revision k.
func checkingTransitionID(unit string, k int) string {
	return fmt.Sprintf("%s-%s-%d", trace.UnitSubject(unit), UnitChecking, k)
}

// masonReports holds the report each running mason turn's done accepted,
// until the turn ends and reports it as its outcome.
type masonReports struct {
	mu       sync.Mutex
	accepted map[string]reportedDone
}

type reportedDone struct {
	Report MasonReport
	// Card is the fitted card, nil when the mason's card could not be shown.
	Card *coreadapter.Card
	// OwnerAction is the text for the owner action the mason flagged, empty
	// when it flagged none.
	OwnerAction string
}

func turnKey(scope coreadapter.Scope) string {
	return scope.Workstream + "/" + scope.Thread + "/" + scope.Turn
}

// tool returns the done tool of the claimed mason turn scope names. Input
// its schema refuses is a tool error. A report the service refuses is an
// ordinary result, {"recorded":false,"reason":...}, so the mason reads why,
// fixes it and calls done again; its unit does not move. A card that does not
// fit never refuses a report: the service fits it, and the result names each
// adjustment.
func (r *masonReports) tool(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	done := coreadapter.Tool{Name: doneTool, Effect: coreadapter.ToolMemory,
		Description: "Report your unit's work done, once its task is done and every acceptance item holds. Give the outcome: what the work now does and how you checked the acceptance. Put any new project knowledge in learnings. Include an owner-facing headline (64 characters), what happened (140 characters), and needs_you (140 characters, empty unless the owner has an action). Write a single line per card field, in words without IDs, paths, URLs or model names. The service shortens a card field that is too long, joins one written over several lines, and leaves out one it cannot show; without a headline or what happened it records no card. A needs_you always tells the owner there is an action. The service records the report, takes your workspace as the unit's candidate, runs the project's checks on it and sends it to review; end your turn as soon as this returns.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"outcome":{"type":"string"},"learnings":{"type":"array","items":{"type":"string"}},"headline":{"type":"string"},"happened":{"type":"string"},"needs_you":{"type":"string"}},"required":["outcome"],"additionalProperties":false}`)}
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
			return refuseReport("unit %s is %s, not implementing", scope.Unit, state.Value)
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
			return refuseReport("%s", reason)
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
		key := turnKey(scope)
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

// driftTool returns the done tool of the claimed drift mason turn scope
// names: it takes the outcome of the mason's resolution, which the turn
// then ends with as its report. Whether the resolution is complete is the
// service's to check once the turn ends.
func (r *masonReports) driftTool(scope coreadapter.Scope) coreadapter.Tool {
	done := coreadapter.Tool{Name: doneTool, Effect: coreadapter.ToolMemory,
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
		key := turnKey(scope)
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

// checkReport returns why a report is incomplete: an empty outcome or an
// empty learning. It returns "" for a complete report.
func checkReport(report MasonReport) string {
	if strings.TrimSpace(report.Outcome) == "" {
		return "outcome is required: say what the unit's work now does and how you checked its acceptance"
	}
	for i, learning := range report.Learnings {
		if strings.TrimSpace(learning) == "" {
			return fmt.Sprintf("learning %d is empty", i+1)
		}
	}
	return ""
}

// reportingTurns ends every mason turn whose done the service accepted with
// the outcome masonDone and the report, with its card when it has one,
// whatever the agent reported.
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
	key := turnKey(prepared.Scope)
	report, ok := t.reports.accepted[key]
	delete(t.reports.accepted, key)
	if ok {
		data, encodeErr := json.Marshal(doneReport{MasonReport: report.Report, OwnerAction: report.OwnerAction})
		if encodeErr != nil {
			return result, errors.Join(err, encodeErr)
		}
		result.Outcome = &coreadapter.Outcome{Status: masonDone, Report: string(data)}
		result.Outcome.Card = report.Card
	}
	return result, err
}

// CheckResume defers to the wrapped runner; one that cannot check resumes
// makes the turn replay.
func (t *reportingTurns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}

// reported returns the report of the unit's mason when the last turn of its
// thread ended cleanly with done, and that turn.
func (m *masons) reported(stream config.WorkstreamID, unit string) (MasonReport, trace.QueuedTurn, bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	if err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, err
	}
	last := th.Turns[len(th.Turns)-1]
	if last.Status() != "idle" || last.Response.Result.Outcome == nil || last.Response.Result.Outcome.Status != masonDone {
		return MasonReport{}, trace.QueuedTurn{}, false, nil
	}
	report, err := parseDoneReport(last)
	if err != nil {
		return MasonReport{}, trace.QueuedTurn{}, false, err
	}
	return report.MasonReport, last, true, nil
}

// parseDoneReport returns the report of a turn that ended with masonDone.
func parseDoneReport(turn trace.QueuedTurn) (doneReport, error) {
	var report doneReport
	if err := json.Unmarshal([]byte(turn.Response.Result.Outcome.Report), &report); err != nil {
		return doneReport{}, fmt.Errorf("the report of turn %s: %w", turn.Request.TurnID, err)
	}
	return report, nil
}

// finish moves an implementing unit whose mason reported done to checking:
// it snapshots the unit's workspace as its candidate, and records the report
// with the candidate as the next revision of units/<unit>/report.json and the
// move in one commit. It reports whether the unit moved. A unit whose
// candidate cannot be made stays implementing, and finish records why it is
// blocked. A unit whose workspace is behind its feature branch waits for the
// foreman to rebase it. A candidate that holds a stored conflict, or whose
// files a rebase left conflicted still carry conflict markers, stays
// implementing, and its mason gets a turn that names those files. A unit whose state moved since it was read is left to the
// next pass.
func (m *masons) finish(ctx context.Context, b building, unit string) (moved, blocked bool, err error) {
	// Queue an ordinary send-back before looking at the last done turn. The
	// move may have committed after the review pass or before a restart.
	if ordinarySendBack(b.reviewTransitions[trace.UnitSubject(unit)]) {
		if err := (&reviewers{masons: m}).resumeFindings(ctx, b.stream, unit, b.states[trace.UnitSubject(unit)]); err != nil {
			return false, false, err
		}
	}
	report, turn, found, err := m.reported(b.stream, unit)
	if err != nil || !found {
		return false, false, err
	}
	units := newUnitWorkspaces(m.cfg, m.repository)
	if behind, err := units.behind(ctx, b.stream, unit); err != nil || behind {
		return false, false, err
	}
	w, base, candidate, err := units.snapshot(ctx, b.stream, unit)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, err
		}
		return false, true, m.block(ctx, b.stream, unit, turn.Response.ID, fmt.Sprintf("unit %s stays implementing: its mason reported done, and its candidate cannot be made: %v", unit, err))
	}
	conflicted, rebase, err := m.conflicts(b.stream, unit)
	if err != nil {
		return false, false, err
	}
	g, err := units.Of(b.stream)
	if err != nil {
		return false, false, err
	}
	if len(conflicted) != 0 {
		th, err := m.repository.Thread(b.stream, masonAgent(unit))
		if err != nil {
			return false, false, err
		}
		resolution := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool {
			return q.Request.TurnID == resolveTurnID(unit, rebase.Rebase)
		})
		// A rebase may finish between scheduling hooks. Its conflicts are
		// new work, so a done response from before the resolution was queued
		// cannot count as an attempt to resolve them.
		if resolution < 0 || turn.Sequence < th.Turns[resolution].Sequence {
			return false, false, nil
		}
	}
	stored, err := g.StoredConflicts(ctx, candidate)
	if err != nil {
		return false, false, err
	}
	marked, err := g.Markers(ctx, candidate, conflicted)
	if err != nil {
		return false, false, err
	}
	for _, p := range marked {
		if !slices.Contains(stored, p) {
			stored = append(stored, p)
		}
	}
	if len(stored) != 0 {
		slices.Sort(stored)
		moved, err := m.remind(ctx, b.stream, unit, b.states[trace.UnitSubject(unit)], turn, stored)
		return moved, false, err
	}
	latest, _, _, err := seal.Latest(m.repository, b.stream)
	if err != nil {
		return false, false, err
	}
	docs, err := trace.Read[trace.Document](m.repository, b.stream)
	if err != nil {
		return false, false, err
	}
	k := 1
	for _, d := range docs {
		if d.ID == reportDocument(unit) {
			k = d.Revision + 1
		}
	}
	card := turn.Response.Result.Outcome.Card
	done, err := parseDoneReport(turn)
	if err != nil {
		return false, false, err
	}
	content, err := json.MarshalIndent(UnitReport{Unit: unit, Turn: turn.Request.TurnID, Seal: latest.Seal, Outcome: report.Outcome, Learnings: report.Learnings, Card: card, Branch: w.Branch, Base: base, Candidate: candidate}, "", "  ")
	if err != nil {
		return false, false, err
	}
	at := m.s.Now()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reportDocument(unit), Revision: k, Project: m.repository.Project(), Workstream: b.stream, Unit: unit, At: at, Actor: masonActor, Cause: turn.Response.ID, Depth: turn.Request.Depth}
	doc := trace.Document{Header: h, Path: fmt.Sprintf(reportPath, unit), Content: string(content) + "\n"}
	subject := trace.UnitSubject(unit)
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", checkingTransitionID(unit, k), 1
	tr := trace.Transition{Header: h, Subject: subject, From: UnitImplementing, To: UnitChecking,
		Reason: fmt.Sprintf("the mason of unit %s reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is %s revision %d", unit, turn.Request.TurnID, candidate, w.Branch, featureBranch(b.stream), base, doc.Path, k)}
	// A mason that names something the owner must act on is news, whatever
	// survived of its card; otherwise the unit's move to checking is routine
	// progress.
	ownerAction := ownerActionOf(done, card)
	body := finishNotice(unit, turn.Request.TurnID, doc.Path, k, card, ownerAction)
	event := trace.Progress(checkingTransitionID(unit, k), "unit", body)
	if ownerAction != "" {
		event = trace.Notice(checkingTransitionID(unit, k), "unit", body)
	}
	tx := trace.Transaction{ExpectedVersion: b.states[subject].Version, Transition: tr,
		Events: []trace.Event{event}}
	if _, err := m.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); errors.Is(err, trace.ErrConflict) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	return true, false, nil
}

// ownerActionOf returns the text of the owner action the mason flagged, or
// "" when it flagged none. A report recorded before reports carried the text
// has it in its card.
func ownerActionOf(report doneReport, card *coreadapter.Card) string {
	if report.OwnerAction != "" {
		return report.OwnerAction
	}
	if card != nil {
		return card.NeedsYou
	}
	return ""
}

// finishNotice says that a unit is checking. It carries the card's headline
// and the owner action when there are any; an owner action with no card has
// the headline ownerActionHeadline.
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
