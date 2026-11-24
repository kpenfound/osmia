package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const followUpTool = "follow_up"

const followUpTurnPrefix = "follow_up_"

const (
	minFollowUp = 5 * time.Minute
	maxFollowUp = 24 * time.Hour
)

var followUpActor = trace.Actor{Kind: "service", ID: "follow-up"}

const (
	autoFollowUpReason  = "an action you took started no work"
	stallFollowUpReason = "your turn on a stall notice ended without an action that queued work, a follow_up or an attention item for the owner"
)

const stallTurnRule = "End this turn with one of: an action whose result shows queued work, a follow_up, or a set_status attention item for the owner."

const (
	followUpPending   = "pending"
	followUpFired     = "fired"
	followUpCancelled = "cancelled"
)

type followUp struct {
	ID          string              `json:"id"`
	Workstream  config.WorkstreamID `json:"workstream,omitempty"`
	Reason      string              `json:"reason"`
	ScheduledAt time.Time           `json:"scheduled_at"`
	Due         time.Time           `json:"due"`
	By          string              `json:"by"`
	Turn        string              `json:"turn"`
	State       string              `json:"state"`
	FiredTurn   string              `json:"fired_turn,omitempty"`
	Replaces    string              `json:"replaces,omitempty"`
	Refires     int                 `json:"refires,omitempty"`
}

const maxFollowUpRefires = 3

func followUpRefireDelay(n int) time.Duration {
	return minFollowUp << (n - 1)
}

type followUpActed struct {
	Turn string `json:"turn"`
	By   string `json:"by"`
}

type followUpRecords struct {
	latest   map[string]followUp
	revision map[string]int
	turns    map[string]bool
	acted    map[string]bool
}

func followUpID(thread string, named config.WorkstreamID) string {
	sum := sha256.Sum256([]byte(thread + "\n" + string(named)))
	return "follow-up-" + hex.EncodeToString(sum[:])
}

func followUpActedID(turn string) string {
	sum := sha256.Sum256([]byte(turn))
	return "follow-up-acted-" + hex.EncodeToString(sum[:])
}

func followUpTurnID(id string, revision int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", id, revision)))
	return followUpTurnPrefix + hex.EncodeToString(sum[:20])
}

func followUpWindow(cfg *config.Config) time.Duration {
	if cfg == nil {
		return config.DefaultStallAfter
	}
	return cfg.GiveUpWindow()
}

func readFollowUps(r *trace.Repository, stream config.WorkstreamID) (followUpRecords, error) {
	out := followUpRecords{latest: map[string]followUp{}, revision: map[string]int{}, turns: map[string]bool{}, acted: map[string]bool{}}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return out, err
	}
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "follow-ups/") {
			continue
		}
		if strings.HasPrefix(d.ID, "follow-up-acted-") {
			var a followUpActed
			if err := json.Unmarshal([]byte(d.Content), &a); err != nil {
				return out, err
			}
			out.acted[a.Turn] = true
			continue
		}
		var f followUp
		if err := json.Unmarshal([]byte(d.Content), &f); err != nil {
			return out, err
		}
		out.turns[f.Turn] = true
		if d.Revision > out.revision[d.ID] {
			out.latest[d.ID], out.revision[d.ID] = f, d.Revision
		}
	}
	return out, nil
}

func (f followUpRecords) pending() []followUp {
	var out []followUp
	for _, x := range f.latest {
		if x.State == followUpPending {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Due.Before(out[j].Due) })
	return out
}

func recordFollowUp(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, revision int, f followUp, actor trace.Actor, cause string, at time.Time) error {
	data, _ := json.MarshalIndent(f, "", "  ")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: f.ID, Revision: revision, Project: r.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause}, Path: "follow-ups/" + f.ID + ".json", Content: string(data) + "\n"}
	return r.RecordDocuments(ctx, []trace.Document{doc})
}

func scheduleFollowUp(ctx context.Context, r *trace.Repository, host config.WorkstreamID, thread, turn string, named config.WorkstreamID, reason string, window time.Duration, at time.Time) (time.Time, error) {
	recs, err := readFollowUps(r, host)
	if err != nil || recs.turns[turn] {
		return time.Time{}, err
	}
	id := followUpID(thread, named)
	due := at.Add(window)
	prior, ok := recs.latest[id]
	f := followUp{ID: id, Workstream: named, Reason: reason, ScheduledAt: at, Due: due, By: "service", Turn: turn, State: followUpPending}
	if ok && prior.State == followUpPending {
		if !prior.Due.After(due) {
			return time.Time{}, nil
		}
		f.Replaces = prior.Reason
	}
	if err := recordFollowUp(ctx, r, host, recs.revision[id]+1, f, followUpActor, turn, at); err != nil {
		return time.Time{}, err
	}
	return due, nil
}

func markFollowUpActed(ctx context.Context, r *trace.Repository, host config.WorkstreamID, turn, by string, at time.Time) error {
	th, err := r.Thread(host, trace.ChiefOfStaff)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if i < 0 || !stallTurn(th.Turns[i]) {
		return nil
	}
	recs, err := readFollowUps(r, host)
	if err != nil || recs.acted[turn] {
		return err
	}
	id := followUpActedID(turn)
	data, _ := json.MarshalIndent(followUpActed{Turn: turn, By: by}, "", "  ")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: r.Project(), Workstream: host, At: at, Actor: followUpActor, Cause: turn}, Path: "follow-ups/" + id + ".json", Content: string(data) + "\n"}
	if err := r.RecordDocuments(ctx, []trace.Document{doc}); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func stallTurn(q trace.QueuedTurn) bool {
	return !strings.HasPrefix(q.Request.TurnID, followUpTurnPrefix) && strings.Contains(q.Request.Prompt, stallTurnRule)
}

func newFollowUpTool(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	description := "Schedule a turn of your own to check back later. after: a Go duration from 5m to 24h, such as 45m or 3h. reason: what to check, which the turn's prompt repeats with what changed since. The service keeps one pending follow-up per workstream: a newer one replaces an unfired one. Use it whenever you expect work to start or finish later, instead of telling the owner it will happen; owner pauses do not stop it."
	if scope.Role == trace.ChiefOfStaff {
		description += " workstream: the member of your workgroup to check on; omit to check on the whole workgroup."
	}
	return coreadapter.Tool{Name: followUpTool, Effect: coreadapter.ToolMemory, Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"after":{"type":"string"},"reason":{"type":"string"},"workstream":{"type":"string"}},"required":["after","reason"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			if scope.Role != trace.ChiefOfStaff {
				return nil, errors.New("follow_up is granted to the chief of staff only")
			}
			var in struct {
				After      string              `json:"after"`
				Reason     string              `json:"reason"`
				Workstream config.WorkstreamID `json:"workstream"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			after, err := time.ParseDuration(strings.TrimSpace(in.After))
			if err != nil || after < minFollowUp || after > maxFollowUp {
				return priorityRefusal("after must be a duration from 5m to 24h, such as 45m or 3h")
			}
			reason := strings.TrimSpace(in.Reason)
			if reason == "" || len(reason) > 1000 {
				return priorityRefusal("reason must say in at most 1000 characters what to check")
			}
			if in.Workstream != "" {
				if _, _, refusal, err := chiefMember(r, scope, in.Workstream); err != nil {
					return nil, err
				} else if refusal != "" {
					return priorityRefusal(refusal)
				}
			}
			host := config.WorkstreamID(scope.Workstream)
			recs, err := readFollowUps(r, host)
			if err != nil {
				return nil, err
			}
			at := now()
			id := followUpID(scope.Thread, in.Workstream)
			f := followUp{ID: id, Workstream: in.Workstream, Reason: reason, ScheduledAt: at, Due: at.Add(after), By: "chief", Turn: scope.Turn, State: followUpPending}
			prior, replaced := recs.latest[id]
			replaced = replaced && prior.State == followUpPending
			if replaced {
				f.Replaces = prior.Reason
			}
			actor := trace.Actor{Kind: "agent", ID: scope.Thread}
			if err := recordFollowUp(ctx, r, host, recs.revision[id]+1, f, actor, scope.Turn, at); err != nil {
				return nil, err
			}
			for other, x := range recs.latest {
				if other == id || x.State != followUpPending || x.By != "service" || x.Turn != scope.Turn {
					continue
				}
				x.State = followUpCancelled
				if err := recordFollowUp(ctx, r, host, recs.revision[other]+1, x, actor, scope.Turn, at); err != nil {
					return nil, err
				}
			}
			result := map[string]any{"recorded": true, "due": f.Due.UTC().Format(time.RFC3339)}
			if in.Workstream != "" {
				result["workstream"] = in.Workstream
			}
			if replaced {
				result["replaced"] = prior.Reason
			}
			return json.Marshal(result)
		}}
}

func renderFollowUps(r *trace.Repository, host config.WorkstreamID) (string, error) {
	recs, err := readFollowUps(r, host)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("\n## Pending follow-ups\n")
	pending := recs.pending()
	if len(pending) == 0 {
		out.WriteString("None.\n")
	}
	for _, f := range pending {
		about := "the whole thread"
		if f.Workstream != "" {
			about = "workstream " + string(f.Workstream)
		}
		by := "you"
		if f.By == "service" {
			by = "the service"
		}
		fmt.Fprintf(&out, "- Due %s, about %s, scheduled by %s: %s\n", f.Due.UTC().Format(time.RFC3339), about, by, f.Reason)
	}
	return out.String(), nil
}

type followUps struct {
	now      func() time.Time
	window   time.Duration
	profile  func() (coreadapter.Profile, error)
	system   func(ctx context.Context, r *trace.Repository, host config.WorkstreamID) (string, error)
	service  *Service
	archived func(raw string) (runtime.Archive, bool)
}

const stallLookback = 24 * time.Hour

func (f followUps) pass(ctx context.Context, r *trace.Repository, streams []config.WorkstreamID) error {
	var failed []error
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.stream(ctx, r, stream); err != nil {
			failed = append(failed, fmt.Errorf("follow-ups of workstream %s: %w", stream, err))
		}
	}
	return errors.Join(failed...)
}

func (f followUps) stream(ctx context.Context, r *trace.Repository, host config.WorkstreamID) error {
	th, err := r.Thread(host, trace.ChiefOfStaff)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	recs, err := readFollowUps(r, host)
	if err != nil {
		return err
	}
	now := f.now()
	var failed []error
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || q.Status() != "idle" || !stallTurn(q) || now.Sub(q.CompletedAt) > stallLookback || recs.turns[q.Request.TurnID] || recs.acted[q.Request.TurnID] {
			continue
		}
		if _, err := scheduleFollowUp(ctx, r, host, th.Identity.ThreadID, q.Request.TurnID, "", stallFollowUpReason, f.window, now); err != nil {
			failed = append(failed, fmt.Errorf("check-back of turn %s: %w", q.Request.TurnID, err))
			continue
		}
		recs.turns[q.Request.TurnID] = true
		if recs, err = readFollowUps(r, host); err != nil {
			return errors.Join(append(failed, err)...)
		}
	}
	ids := make([]string, 0, len(recs.latest))
	for id := range recs.latest {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		x, err := f.refire(ctx, r, host, th, recs.latest[id], recs.revision[id])
		if err != nil {
			failed = append(failed, fmt.Errorf("follow-up %s: %w", id, err))
			continue
		}
		if x.State != recs.latest[id].State {
			recs.latest[id], recs.revision[id] = x, recs.revision[id]+1
		}
	}
	for _, id := range ids {
		x := recs.latest[id]
		if x.State != followUpPending || now.Before(x.Due) {
			continue
		}
		if err := f.fire(ctx, r, host, th, x, recs.revision[id], now); err != nil {
			failed = append(failed, fmt.Errorf("follow-up %s: %w", id, err))
		}
	}
	return errors.Join(failed...)
}

func (f followUps) refire(ctx context.Context, r *trace.Repository, host config.WorkstreamID, th trace.Thread, x followUp, revision int) (followUp, error) {
	if x.State != followUpFired || x.FiredTurn == "" || x.Refires >= maxFollowUpRefires {
		return x, nil
	}
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == x.FiredTurn })
	if i < 0 || th.Turns[i].CompletedAt.IsZero() || f.now().Sub(th.Turns[i].CompletedAt) > stallLookback {
		return x, nil
	}
	ended := th.Turns[i]
	if status := ended.Status(); status != "failed" && status != "interrupted" {
		return x, nil
	}
	again := x
	again.State, again.FiredTurn, again.Refires = followUpPending, "", x.Refires+1
	again.Due = ended.CompletedAt.Add(followUpRefireDelay(again.Refires))
	err := recordFollowUp(ctx, r, host, revision+1, again, followUpActor, x.FiredTurn, f.now())
	if errors.Is(err, trace.ErrConflict) {
		return x, nil
	}
	if err != nil {
		return x, err
	}
	return again, nil
}

func (f followUps) fire(ctx context.Context, r *trace.Repository, host config.WorkstreamID, th trace.Thread, x followUp, revision int, now time.Time) error {
	turn := followUpTurnID(x.ID, revision)
	queued := slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if !queued {
		prompt, err := f.prompt(r, host, x, now)
		if err != nil {
			return err
		}
		profile, err := f.profile()
		if err != nil {
			return err
		}
		system := ""
		if f.system != nil {
			if system, err = f.system(ctx, r, host); err != nil {
				return err
			}
		}
		req := trace.TurnRequest{
			Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: r.Project(), Workstream: host, At: now, Actor: followUpActor, Cause: x.ID},
			AgentID: trace.ChiefOfStaff, ThreadID: th.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: system, Prompt: prompt}
		if _, err := r.EnqueueTurn(ctx, req); err != nil && !errors.Is(err, trace.ErrConflict) {
			return err
		}
	}
	x.State, x.FiredTurn = followUpFired, turn
	err := recordFollowUp(ctx, r, host, revision+1, x, followUpActor, turn, now)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (f followUps) prompt(r *trace.Repository, host config.WorkstreamID, x followUp, now time.Time) (string, error) {
	var b strings.Builder
	who := "You scheduled"
	if x.By == "service" {
		who = "The service scheduled"
	}
	fmt.Fprintf(&b, "Follow-up. %s this check-back at %s, %s ago. Reason: %s\n", who, x.ScheduledAt.UTC().Format(time.RFC3339), now.Sub(x.ScheduledAt).Round(time.Minute), x.Reason)
	members := []config.WorkstreamID{x.Workstream}
	if x.Workstream == "" {
		var err error
		if members, err = r.WorkgroupMembers(host); err != nil {
			return "", err
		}
	}
	for _, member := range members {
		if f.archived != nil {
			if a, ok := f.archived(string(member)); ok {
				fmt.Fprintf(&b, "\nWorkstream %s: archived (%s).\n", member, archiveResolution(a))
				continue
			}
		}
		target, stream, refusal, err := r.ChiefMember(host, member)
		if err != nil {
			return "", err
		}
		if refusal != "" {
			fmt.Fprintf(&b, "\nWorkstream %s: %s.\n", member, refusal)
			continue
		}
		if retired, err := r.MemberRetired(member); err != nil {
			return "", err
		} else if retired {
			fmt.Fprintf(&b, "\nWorkstream %s: archived (retired from the workgroup).\n", member)
			continue
		}
		summary, err := f.summary(target, stream, x.ScheduledAt)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, trace.ErrMemberRetired) {
			fmt.Fprintf(&b, "\nWorkstream %s: archived (its trace was removed).\n", member)
			continue
		}
		if err != nil {
			return "", err
		}
		b.WriteString(summary)
	}
	b.WriteString("\n" + stallTurnRule + "\n")
	return b.String(), nil
}

func archiveResolution(a runtime.Archive) string {
	parts := []string{emptyAsNone(a.State)}
	if a.ResolutionEvidence != "" {
		parts = append(parts, a.ResolutionEvidence)
	}
	return strings.Join(parts, ", ")
}

const maxFollowUpChanges = 20

func (f followUps) summary(r *trace.Repository, stream config.WorkstreamID, since time.Time) (string, error) {
	var b strings.Builder
	feature, err := r.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\nWorkstream %s is %s.\n", stream, emptyAsNone(feature.Value))
	transitions, err := trace.Read[trace.Transition](r, stream)
	if err != nil {
		return "", err
	}
	var changed []trace.Transition
	for _, t := range transitions {
		if t.At.After(since) {
			changed = append(changed, t)
		}
	}
	sort.SliceStable(changed, func(i, j int) bool { return changed[i].At.Before(changed[j].At) })
	if len(changed) == 0 {
		b.WriteString("Nothing changed since then.\n")
	} else {
		b.WriteString("Changes since then:\n")
		if len(changed) > maxFollowUpChanges {
			fmt.Fprintf(&b, "- %d earlier changes are not listed\n", len(changed)-maxFollowUpChanges)
			changed = changed[len(changed)-maxFollowUpChanges:]
		}
		for _, t := range changed {
			reason := strings.Join(strings.Fields(t.Reason), " ")
			if len(reason) > 200 {
				reason = reason[:200] + "…"
			}
			fmt.Fprintf(&b, "- %s %s: %s -> %s (%s)\n", t.At.UTC().Format(time.RFC3339), t.Subject, emptyAsNone(t.From), t.To, reason)
		}
	}
	work, err := queuedWork(f.service, r, stream)
	if err != nil {
		return "", err
	}
	if len(work.Items) == 0 {
		b.WriteString(work.Why + "\n")
		return b.String(), nil
	}
	b.WriteString("Queued or running now:\n")
	for _, item := range work.Items {
		unit := ""
		if item.Unit != "" {
			unit = " (unit " + item.Unit + ")"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", item.Role, unit, item.What)
	}
	return b.String(), nil
}

const followUpPrompt = "You are the chief of staff for workstream %s. The prompt is a follow-up that came due: why it was scheduled, when, and what changed since. The project context below was assembled when it came due."

const followUpEvery = 5 * time.Second

func (p *conversationPath) followUps(ctx context.Context, repository *trace.Repository) error {
	if p.followUpRan == nil {
		p.followUpRan = map[*trace.Repository]time.Time{}
	}
	if last, ok := p.followUpRan[repository]; ok && time.Since(last) < followUpEvery {
		return nil
	}
	p.followUpRan[repository] = time.Now()
	var streams []config.WorkstreamID
	var err error
	if repository != p.repository {
		streams, err = p.streams(repository)
	} else {
		streams, err = intake.NonSessionWorkstreams(repository)
	}
	if err != nil {
		return err
	}
	s := p.s
	cfg := s.current()
	f := followUps{now: s.now, window: followUpWindow(cfg), service: s, system: s.followUpSystem, archived: s.archivedWorkstream,
		profile: func() (coreadapter.Profile, error) {
			_, profile, err := s.chiefOverride(cfg)
			return profile, err
		}}
	return f.pass(ctx, repository, streams)
}

func (s *Service) followUpSystem(ctx context.Context, target *trace.Repository, host config.WorkstreamID) (string, error) {
	bundle, err := target.ProjectFor(host)
	if err != nil {
		return "", err
	}
	context, err := chiefContext(ctx, s.Context(), target, func() {}, bundle.Project(), host)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(followUpPrompt, host) + "\n\n" + chiefGuidance() + "\n\n" + context, nil
}
