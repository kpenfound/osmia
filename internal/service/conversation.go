package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const managerPrompt = "You are the chief of staff for workstream %s. This turn answers a message from the owner, which is the prompt. The project context in the turn context was assembled when the message was accepted."

func managerGuidance() string {
	return strings.Join([]string{intake.ManagerRolePrompt, intake.FollowUpGuidance, intake.ManagerDocumentsGuidance, priorityGuidance, pauseGuidance, lifecycle.ChangeGuidance, amendmentGuidance, intake.CharterGuidance, intake.ContestGuidance, intake.ObjectionGuidance, intake.DriftHandbackGuidance, intake.OwnerWritingGuidance}, "\n\n")
}

func (s *Service) now() time.Time {
	if s.options.Reconciliation.Now != nil {
		return s.options.Reconciliation.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) conversationTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return s.resolveConversationTrace(raw, false)
}

func (s *Service) conversationReadTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return s.resolveConversationTrace(raw, true)
}

func (s *Service) resolveConversationTrace(raw string, readingManagerArchive bool) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return "", "", nil, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	if archive, ok := s.archivedWorkstream(raw); ok && !(readingManagerArchive && archive.Actor == runtime.ArchiveActorManager) {
		return "", "", nil, &APIError{Code: Conflict, Message: "workstream is permanently archived; its history is unavailable"}
	} else if ok {
		cfg, projects := s.runtimes()
		for _, active := range projects {
			if active.id != archive.Project || !cfg.Active(active.id) {
				continue
			}
			streams, err := active.repository.Workstreams()
			if err != nil {
				return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
			}
			if slices.Contains(streams, id) {
				return active.id, id, active.repository, nil
			}
		}
		return "", "", nil, &APIError{Code: Conflict, Message: "workstream is permanently archived; its history is unavailable"}
	}
	cfg, projects := s.runtimes()
	if len(cfg.Projects) == 0 {
		return "", "", nil, &APIError{Code: NoProject, Message: "no project is configured; add one with osmia project add"}
	}
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		streams, err := active.repository.Workstreams()
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.id)}
		}
		if slices.Contains(streams, id) {
			return active.id, id, active.repository, nil
		}
	}
	return "", "", nil, &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s is not in an active project; list workstreams with osmia status", id)}
}

func (s *Service) send(ctx context.Context, raw string, req SendRequest) (ConversationEntry, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return ConversationEntry{}, api
	}
	if design, err := isDesignWorkstream(repository, stream); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
	} else if design {
		return ConversationEntry{}, &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s is a design session; send messages with POST %s/design/%s/message", stream, Prefix, stream)}
	}
	if blocked, err := s.intake().DesignStartPending(repository, stream); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: err.Error()}
	} else if blocked {
		state, err := repository.Workflow(stream, intake.DesignStartSubject)
		if err != nil || (state.Value != "active" && state.Value != "complete" && state.Value != "invalid") {
			return ConversationEntry{}, &APIError{Code: Conflict, Message: "the design workgroup is starting; retry once conversation adoption completes"}
		}
	}
	return s.sendManager(ctx, stream, req, func() (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
		target, host, bundle, close, err := s.managerRepository(ctx, repository, stream)
		if err != nil {
			return nil, "", "", close, apiErrAsError{Api: &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the workgroup of workstream %s; check the trace repository", stream)}}
		}
		return target, host, bundle, close, nil
	})
}

func (s *Service) sendManager(ctx context.Context, stream config.WorkstreamID, req SendRequest, resolve func() (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error)) (ConversationEntry, *APIError) {
	wholeGroup := stream == ""
	if strings.TrimSpace(req.Text) == "" {
		return ConversationEntry{}, &APIError{Code: Validation, Message: "text must not be empty"}
	}
	cfg := s.current()
	name, profile, err := s.managerOverride(cfg)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("role %s has no usable profile %q; check osmia profiles", trace.Manager, name)}
	}
	target, host, bundleStream, closeTarget, err := resolve()
	if err != nil {
		return ConversationEntry{}, workgroupAuthorityError(err)
	}
	if stream == "" {
		stream = host
	}
	project := target.Project()
	bundleProject, err := target.ProjectFor(bundleStream)
	if err != nil {
		closeTarget()
		return ConversationEntry{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if bundleProject.Project() != target.Project() {
		project = bundleProject.Project()
	}
	context, err := managerContext(ctx, s.Context(), target, closeTarget, project, bundleStream)
	if err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot assemble the context of workstream %s; check the charter, the entity map and the trace repository", stream)}
	}
	note := ""
	if host != stream {
		note = fmt.Sprintf(addressedNote+"%s, a member of your workgroup; answer or escalate one of its questions as \"%s:<number>\".", stream, stream)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "cannot generate a message ID"}
	}
	id := hex.EncodeToString(nonce[:])
	at := s.now()
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for workstream %s; check the trace repository", stream)}
	target, _, _, closeTarget, err = resolve()
	if err != nil {
		return ConversationEntry{}, workgroupAuthorityError(err)
	}
	defer closeTarget()
	if _, err := target.EnsureManager(ctx, stream, at, serviceActor); err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, failed
	}
	q, err := target.EnqueueTurn(ctx, trace.TurnRequest{
		Header:       trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: target.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-message"},
		AgentID:      trace.Manager,
		ThreadID:     trace.Manager,
		TurnID:       "message_" + id,
		Profile:      profile,
		SystemPrompt: fmt.Sprintf(managerPrompt, host) + "\n\n" + managerGuidance(),
		Context:      strings.TrimSpace(note) + "\n\n" + context,
		Prompt:       req.Text,
	})
	if err != nil {
		if wholeGroup {
			return ConversationEntry{}, workgroupAuthorityError(err)
		}
		return ConversationEntry{}, failed
	}
	entry := conversation(q)[0]
	if th, err := target.Thread(q.Request.Workstream, q.Request.AgentID); err == nil {
		entry.QueuedReason = s.queuedReason(th, q)
	}
	return entry, nil
}

func (s *Service) conversationList(raw string) (ConversationResponse, *APIError) {
	_, stream, repository, api := s.conversationReadTrace(raw)
	if api != nil {
		return ConversationResponse{}, api
	}
	target, thread, closeTarget, err := s.conversationTarget(context.Background(), repository, stream)
	if err != nil {
		var wrapped apiErrAsError
		if errors.As(err, &wrapped) {
			return ConversationResponse{}, wrapped.Api
		}
		return ConversationResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the workgroup of workstream %s; check the trace repository", stream)}
	}
	defer closeTarget()
	entries, err := readview.Consistent(func() ([]ConversationEntry, error) {
		designed, err := s.designHistory(target, thread)
		if err != nil {
			return nil, err
		}
		entries, err := ConversationEntries(target, thread, designed...)
		if err != nil {
			return nil, err
		}
		archives, err := s.managerArchiveActions(repository, stream)
		if err != nil {
			return nil, err
		}
		actions := make([]ConversationEntry, 0, len(archives))
		for _, a := range archives {
			actions = append(actions, a.ConversationEntry)
		}
		return WithActions(entries, actions), nil
	}, target.Generation, s.storeGeneration)
	if err != nil {
		return ConversationResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the conversation of workstream %s; check the trace repository", stream)}
	}
	return ConversationResponse{Workstream: stream, Entries: entries}, nil
}

func (s *Service) conversationTarget(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (*trace.Repository, config.WorkstreamID, func(), error) {
	if _, archived := s.archivedWorkstream(string(stream)); archived {
		group, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return nil, "", nil, err
		}
		if group != "" {
			a, api := s.openWorkgroupFeedForRead(ctx, string(group))
			if api != nil {
				return nil, "", nil, apiErrAsError{Api: api}
			}
			return a.manager, a.host, a.close, nil
		}
	}
	target, _, _, closeTarget, err := s.managerRepositoryForRead(ctx, repository, stream)
	return target, stream, closeTarget, err
}

// ConversationEntries lists the owner's conversation of stream: the manager of
// staff's thread and, when stream holds or held a design session, the design
// thread. A session opened before design sessions had a thread of their own
// holds its first messages on the manager's thread, so both are read and the
// turns merged by when they were sent.
//
// earlier holds the turns of design sessions whose conversation a started
// workgroup continues from another trace, merged in by when they were sent.
func ConversationEntries(repository *trace.Repository, stream config.WorkstreamID, earlier ...[][]ConversationEntry) ([]ConversationEntry, error) {
	threads := slices.Clone(earlier)
	for _, read := range []func(config.WorkstreamID) (trace.Thread, error){repository.ManagerThread, repository.DesignerThread} {
		t, err := read(stream)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		threads = append(threads, threadTurns(t))
	}
	out := mergeTurns(threads)
	if len(threads) == 0 {
		return out, nil
	}
	actions, err := ManagerActions(repository, stream)
	if err != nil {
		return nil, err
	}
	return WithActions(out, actions), nil
}

// designHistory is the design thread of the design session that a workgroup
// started from, read from the trace the session lives in when that is not
// the trace holding the workgroup's manager. Every endpoint of the
// workgroup shows it ahead of the manager's own thread, so the owner reads one
// conversation from the design through the build. Nothing is returned for a
// workgroup that holds its design session's thread itself.
func (s *Service) designHistory(manager *trace.Repository, host config.WorkstreamID) ([][][]ConversationEntry, error) {
	groups, _, err := trace.Workgroups(manager)
	if err != nil {
		return nil, err
	}
	group, found := trace.WorkgroupOf(groups, host)
	if !found || group.Conversation == nil {
		return nil, nil
	}
	origin, err := s.repository(group.Conversation.Project)
	if errors.Is(err, errNoActiveProject) || errors.Is(err, errNoTrace) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if origin == manager {
		return nil, nil
	}
	design, err := origin.DesignerThread(group.Conversation.Workstream)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return [][][]ConversationEntry{threadTurns(design)}, nil
}

func threadTurns(t trace.Thread) [][]ConversationEntry {
	var turns [][]ConversationEntry
	for _, q := range t.Turns {
		if q.Request.Actor != ownerActor {
			continue
		}
		entries := conversation(q)
		entries[0].QueuedReason = queuedReason(t, q)
		if t.Status == "interrupted" && t.Active == q.Request.TurnID {
			for i := range entries {
				entries[i].State = TurnFailed
			}
		}
		turns = append(turns, entries)
	}
	return turns
}

func mergeTurns(threads [][][]ConversationEntry) []ConversationEntry {
	out := []ConversationEntry{}
	for {
		next := -1
		for i, turns := range threads {
			if len(turns) > 0 && (next < 0 || turns[0][0].At.Before(threads[next][0][0].At)) {
				next = i
			}
		}
		if next < 0 {
			return out
		}
		out = append(out, threads[next][0]...)
		threads[next] = threads[next][1:]
	}
}

type archiveAction struct {
	ConversationEntry
	Workstream config.WorkstreamID
}

func (s *Service) managerArchiveActions(repository *trace.Repository, stream config.WorkstreamID) ([]archiveAction, error) {
	members, err := repository.WorkgroupMembers(stream)
	if err != nil {
		return nil, err
	}
	group, err := repository.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return nil, err
	}
	return managerArchiveActionsOf(s.memberArchives(), func(a runtime.Archive) bool {
		if a.Workstream == stream || group != "" && a.Workgroup == group {
			return true
		}
		return a.Project == repository.Project() && slices.Contains(members, a.Workstream)
	}), nil
}

func managerArchiveActionsOf(archives []runtime.Archive, shared func(runtime.Archive) bool) []archiveAction {
	var out []archiveAction
	for _, a := range archives {
		if a.Actor != runtime.ArchiveActorManager || !shared(a) {
			continue
		}
		title := a.Title
		if title == "" {
			title = string(a.Workstream)
		}
		text := fmt.Sprintf("Archived workstream %s (%s) because its pull request merged", a.Workstream, title)
		if a.DeliveryLink != "" {
			text += ": " + a.DeliveryLink
		}
		out = append(out, archiveAction{ConversationEntry{Turn: "archive-" + string(a.Workstream), Kind: "action", Text: text, At: a.ArchivedAt, State: TurnDone}, a.Workstream})
	}
	return out
}

func WithActions(entries, actions []ConversationEntry) []ConversationEntry {
	actions = slices.Clone(actions)
	sort.SliceStable(actions, func(i, j int) bool { return actions[i].At.Before(actions[j].At) })
	merged := make([]ConversationEntry, 0, len(entries)+len(actions))
	for _, e := range entries {
		for len(actions) > 0 && actions[0].At.Before(e.At) {
			merged, actions = append(merged, actions[0]), actions[1:]
		}
		merged = append(merged, e)
	}
	return append(merged, actions...)
}

func ManagerActions(repository *trace.Repository, stream config.WorkstreamID) ([]ConversationEntry, error) {
	return managerActionsWithIdentity(repository, stream, nil)
}

func managerActionsWithIdentity(repository *trace.Repository, stream config.WorkstreamID, identify func(ConversationEntry, config.WorkstreamID)) ([]ConversationEntry, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []ConversationEntry
	emit := func(entry ConversationEntry, member config.WorkstreamID) {
		out = append(out, entry)
		if identify != nil {
			identify(entry, member)
		}
	}
	members, err := repository.WorkgroupMembers(stream)
	if err != nil {
		return nil, err
	}
	escalations, err := repository.Inbox()
	if err != nil {
		return nil, err
	}
	for _, escalation := range escalations {
		if !slices.ContainsFunc(escalation.Questions, func(question trace.QuestionState) bool {
			return slices.Contains(members, question.Asked.Workstream)
		}) {
			continue
		}
		member := config.WorkstreamID("")
		for i, q := range escalation.Questions {
			if i == 0 {
				member = q.Asked.Workstream
			} else if member != q.Asked.Workstream {
				member = ""
				break
			}
		}
		text := "Question for you: " + escalation.Rephrasing
		if escalation.Hold != "" {
			text += "\nRecoverable hold: " + escalation.Hold
		}
		if escalation.Blocked != "" {
			text += "\nBlocked: " + escalation.Blocked
		}
		if len(escalation.Options) > 0 {
			text += "\nOptions: " + strings.Join(escalation.Options, "; ")
		}
		if escalation.Recommendation != "" {
			text += "\nRecommendation: " + escalation.Recommendation
		}
		emit(ConversationEntry{Turn: escalation.Batch, Kind: "action", Text: text, At: escalation.EscalatedAt, State: TurnDone}, member)
		if escalation.State != trace.QuestionEscalated && len(escalation.Questions) > 0 {
			q := escalation.Questions[0]
			if q.Ruling != nil && q.Ruling.Decision == trace.DecisionRuling {
				ruling, err := repository.QuestionRuling(q.Asked.Workstream, q.Ruling.ID, 1)
				if err != nil {
					return nil, err
				}
				emit(ConversationEntry{Turn: "ruling_" + escalation.Batch, Kind: "action", Text: "You ruled: " + ruling.OwnerResponse, At: ruling.At, State: TurnDone}, member)
			}
		}
	}
	for _, d := range docs {
		if d.Unit == "" || !strings.HasPrefix(d.Path, trace.ContestDecisionPrefix(trace.UnitSubject(d.Unit))) {
			continue
		}
		var decision ManagerContestDecision
		if err := json.Unmarshal([]byte(d.Content), &decision); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		text := fmt.Sprintf("Raised contested unit %s to you: %s", d.Unit, decision.Note)
		switch decision.Decision {
		case "review":
			text = fmt.Sprintf("Resolved contested unit %s: its reviewer reviews the candidate again. %s", d.Unit, decision.Note)
		case "revise":
			text = fmt.Sprintf("Resolved contested unit %s: its mason revises the unit. %s", d.Unit, decision.Note)
		}
		emit(ConversationEntry{Turn: decision.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
	}
	for _, d := range docs {
		if d.Unit == "" || !strings.HasPrefix(d.Path, execution.MovePrefix(d.Unit)) {
			continue
		}
		var move api.UnitMove
		if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if move.By != rulerName(managerActor) {
			continue
		}
		text := fmt.Sprintf("Moved unit %s from %s to %s. %s", d.Unit, move.From, move.To, move.Note)
		if move.To == UnitContested {
			text = fmt.Sprintf("Held unit %s for you, from %s: %s", d.Unit, move.From, move.Note)
		}
		emit(ConversationEntry{Turn: move.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
	}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, lifecycle.WorkstreamMovePrefix) {
			continue
		}
		var move api.WorkstreamMove
		if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		if move.By != rulerName(managerActor) {
			continue
		}
		text := fmt.Sprintf("Moved the workstream from %s to %s. %s", move.From, move.To, move.Note)
		if move.From == move.To {
			text = fmt.Sprintf("Asked for a fresh final review of the %s workstream. %s", move.To, move.Note)
		}
		emit(ConversationEntry{Turn: move.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
	}
	decided := map[string]bool{}
	for _, d := range docs {
		if !shed.IsManagerPath(d.Path) {
			continue
		}
		decisions, err := shed.ParseManagerDecisions([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		for _, c := range decisions.Decisions {
			if decided[c.Objection] {
				continue
			}
			decided[c.Objection] = true
			text := fmt.Sprintf("Dismissed objection %s on your behalf: %s", c.Objection, c.Note)
			if c.Decision == shed.Escalate {
				text = fmt.Sprintf("Raised objection %s to you: %s", c.Objection, c.Note)
			}
			emit(ConversationEntry{Turn: c.Turn, Kind: "action", Text: text, At: d.At, State: TurnDone}, stream)
		}
	}

	host, err := repository.ManagerHost(stream)
	if err != nil {
		return nil, err
	}
	changes, err := readChanges(repository, host)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.State == "failed" {
			emit(ConversationEntry{Turn: "change-" + c.ID + "-failed", Kind: "action", Text: c.Failure, State: TurnDone}, "")
			continue
		}
		if c.State == "drafting" || c.State == "debating" || c.State == "answering" || c.State == "presenting" {
			continue
		}
		text := c.Summary()
		if c.Decision == nil {
			text += fmt.Sprintf("\nApprove or reject this impact summary (revision %d).", c.Revision)
		}
		if c.Request.Kind == "cancel" && c.Decision == nil {
			text += "\nChoose whether open PRs are closed or left as drafts. Merged work is untouched."
		}
		for _, d := range c.Debate {
			text += "\nCommittee: " + d.Note
			for _, objection := range d.Objections {
				text += "\nObjection: " + objection
			}
		}
		if c.Reply != "" {
			text += "\nArchitect: " + c.Reply
		}
		switch {
		case c.Decision != nil && c.Decision.DecidedBy == trace.DecidedByJev:
			text += fmt.Sprintf("\nDecided by Jev (judgment %s): %s", c.Decision.Judgment, c.Decision.Decision)
			if c.Decision.PullRequests != "" {
				text += ", open PRs " + c.Decision.PullRequests
			}
		case c.Decision != nil:
			text += "\nOwner decision: " + c.Decision.Decision
		case c.Request.Kind == "cancel":
			if res, err := jev.Recorded(repository, host, jev.KindCancellation, jev.CancellationKey(c.ID, c.Revision)); err == nil {
				switch {
				case res.Status == jev.Nonbinding && res.Deferred:
					text += "\nJev deferred this decision to you."
				case res.Status == jev.Nonbinding:
					text += fmt.Sprintf("\nJev recommends %s with confidence %.2f, below the %.2f it needs to decide.", res.Pick, res.Confidence, res.Threshold)
				case res.Status == jev.Binding && res.Refused != "":
					text += fmt.Sprintf("\nJev decided %s with confidence %.2f, but the service refused it: %s. The decision is yours.", res.Pick, res.Confidence, res.Refused)
				}
			}
		}
		docs, err := trace.Read[trace.Document](repository, host)
		if err != nil {
			return nil, err
		}
		var at time.Time
		for _, d := range docs {
			if d.ID == workgroupChangeDocument(c.ID) {
				at = d.At
				break
			}
		}
		emit(ConversationEntry{Turn: "change-" + c.ID + "-impact", Kind: "action", Text: text, At: at, State: TurnDone}, "")
	}
	return out, nil
}
