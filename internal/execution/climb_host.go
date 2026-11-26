package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/gitdiff"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// ClimbMasonAgent is the one mason thread of a climb. Every attempt's turns
// run on it.
const ClimbMasonAgent = climb.MasonAgent

const climbMasonSystemPrompt = "You are the mason of a Hill Climb workstream of the %s project (%s). The climb improves one measured number by repeated attempts, and you take every attempt on this one thread, so what you learn from an attempt carries to the next. Each turn is one attempt: your view holds the project's files as the climb's current best has them. You propose, the service measures, and Go decides whether an attempt is kept: you never judge whether a score improved, and your own measurements never count. You hold no version control tool: the service records your work as the attempt's candidate. Change only what the attempt needs and never a protected path, which is frozen. Make a change you can state as a hypothesis, then call done with the hypothesis, the change and the expected effect, and end your turn. Record discovered work with record_discovery and read factory_context for the attempt records."

const climbReviewerSystemPrompt = "You are the reviewer of a Hill Climb attempt. The service already measured the candidate and Go kept it: its score beats the current best by at least the minimum improvement, its guardrails are green and its secondary limits are met. You decide only whether the candidate may land as the new best. Read it for metric gaming (a change that moves the score without improving what the climb is for, such as special-casing the workload, caching across runs, skipping work the oracle does not check, or reading the evaluator's inputs), for breaches of the project's written rules (the charter, AGENTS.md or contributor documents, and the skills configured on your role) and for scope (a change the attempt's hypothesis does not explain). Judge from the diff, the measurement and the climb spec; you never decide whether a score improved. Your read-only view holds the candidate's exact files; call workstream_diff to read its diff, whole or by file and line range. Call verdict: satisfactory lets the candidate land; material_findings rejects it, and each finding says what is wrong and what the next attempt should do instead, because the mason reads your findings."

const reviewTurnAttempts = 3

// ClimbHost is the machinery a Hill Climb loop runs its attempts on: the
// climb's clone, the attempts' workspaces, the one mason thread, the reviewer
// and the guardrail checks. It is built over the unit machinery, so a mason
// turn of an attempt is an ordinary mason turn in a unit workspace.
type ClimbHost struct {
	m        *masons
	stream   config.WorkstreamID
	g        workspace.Provider
	features workspace.Provider
	units    unitWorkspaces
}

var _ climb.Host = (*ClimbHost)(nil)

// ClimbHost returns the host of a Hill Climb workstream.
func (c *Controller) ClimbHost(r *trace.Repository, stream config.WorkstreamID) (*ClimbHost, error) {
	m := c.Units(r)
	units := NewUnitWorkspaces(m.cfg, r)
	g, err := units.Of(stream)
	if err != nil {
		return nil, err
	}
	features, err := featureWorkspaces(m.cfg, r).Of(stream)
	if err != nil {
		return nil, err
	}
	return &ClimbHost{m: m, stream: stream, g: g, features: features, units: units}, nil
}

func (h *ClimbHost) Export(ctx context.Context, commit, dir string) error {
	return h.g.Export(ctx, commit, dir)
}

func (h *ClimbHost) Branch(ctx context.Context, name string) (string, bool, error) {
	return h.g.Branch(ctx, name)
}

func (h *ClimbHost) ChangedPaths(ctx context.Context, base, candidate string) ([]string, error) {
	return h.g.ChangedPaths(ctx, base, candidate)
}

func (h *ClimbHost) Diff(ctx context.Context, base, candidate string) (string, error) {
	return h.g.Diff(ctx, base, candidate)
}

func (h *ClimbHost) Squash(ctx context.Context, base, candidate, message string, at time.Time) (string, error) {
	return h.g.Squash(ctx, base, candidate, message, at)
}

func (h *ClimbHost) Open(ctx context.Context, n int, parent string) (string, error) {
	tip, exists, err := h.g.Branch(ctx, featureBranch(h.stream))
	if err != nil {
		return "", err
	}
	if !exists || tip != parent {
		return "", fmt.Errorf("the climb branch %s is at %q, not at the commit %s attempt %d starts from", featureBranch(h.stream), tip, parent, n)
	}
	w, _, err := h.units.open(ctx, h.stream, climb.AttemptUnit(n))
	if err != nil {
		return "", err
	}
	return w.Branch, nil
}

func (h *ClimbHost) Capture(ctx context.Context, n int, parent string) (string, error) {
	_, _, candidate, err := h.units.Snapshot(ctx, h.stream, climb.AttemptUnit(n))
	return candidate, err
}

func (h *ClimbHost) Drop(ctx context.Context, n int) error {
	w, found, err := h.g.Workspace(ctx, UnitName(h.stream, climb.AttemptUnit(n)))
	if err != nil || !found {
		return err
	}
	return h.g.Release(ctx, w)
}

func (h *ClimbHost) Advance(ctx context.Context, key, from, to string) error {
	branch := featureBranch(h.stream)
	return workspace.LocalStep(ctx, key+"/promote", h.m.s.Now(), []workspace.Provider{h.features}, func(local context.Context) error {
		acquired, err := h.features.Acquire(local, vcs.Request{Name: string(h.stream), Branch: branch})
		if err != nil {
			return fmt.Errorf("climb branch %s: %w", branch, err)
		}
		return h.features.Advance(local, acquired.(workspace.Worktree), from, to)
	})
}

func attemptTurns(th trace.Thread, n int) []trace.QueuedTurn {
	prefix := fmt.Sprintf("climb-mason-%d-", n)
	var out []trace.QueuedTurn
	for _, q := range th.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) {
			out = append(out, q)
		}
	}
	return out
}

func cleanTurns(turns []trace.QueuedTurn) int {
	n := 0
	for _, q := range turns {
		if !strings.Contains(q.Request.TurnID, "-recover-") {
			n++
		}
	}
	return n
}

// Mason reports how attempt n's mason turns stand. It queues the attempt's
// first turn on the climb's one mason thread, a continuation when a turn ended
// without a report, and a recovery when the service stopped during a turn.
func (h *ClimbHost) Mason(ctx context.Context, req climb.MasonRequest) (climb.MasonResult, error) {
	m := h.m
	n := req.Journal.Number
	unit := climb.AttemptUnit(n)
	th, err := m.repository.Thread(h.stream, ClimbMasonAgent)
	switch {
	case errors.Is(err, os.ErrNotExist):
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: ClimbMasonAgent, Revision: 1, Project: m.repository.Project(), Workstream: h.stream, At: m.s.Now(), Actor: masonActor, Cause: trace.ClimbJournalID(n)}, Role: masonRole, ThreadID: ClimbMasonAgent}
		if err := m.repository.CreateThread(ctx, identity); err != nil && !errors.Is(err, trace.ErrConflict) {
			return climb.MasonResult{}, err
		}
		if th, err = m.repository.Thread(h.stream, ClimbMasonAgent); err != nil {
			return climb.MasonResult{}, err
		}
	case err != nil:
		return climb.MasonResult{}, err
	}
	turns := attemptTurns(th, n)
	if len(turns) == 0 {
		return climb.MasonResult{Pending: true}, h.enqueueFirst(ctx, req, unit)
	}
	if th.Status == "interrupted" && th.Active != "" && slices.ContainsFunc(turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == th.Active }) {
		if err := m.RecoverView(ctx, h.stream, ClimbMasonAgent, func() (string, error) {
			w, _, found, err := h.units.Find(ctx, h.stream, unit)
			if err != nil {
				return "", err
			}
			if !found {
				return "", fmt.Errorf("attempt %d of workstream %s has no workspace", n, h.stream)
			}
			return w.Path, nil
		}); err != nil {
			return climb.MasonResult{}, err
		}
		if th, err = m.repository.Thread(h.stream, ClimbMasonAgent); err != nil {
			return climb.MasonResult{}, err
		}
		turns = attemptTurns(th, n)
	}
	last := th.Turns[len(th.Turns)-1]
	if last.Response == nil || last.CompletedAt.IsZero() || last.Status() == "waiting" {
		return climb.MasonResult{Pending: true}, nil
	}
	if reportTaken(last) {
		done, err := parseDoneReport(last)
		if err != nil {
			return climb.MasonResult{}, err
		}
		if done.Attempt == nil {
			return climb.MasonResult{NoCandidate: fmt.Sprintf("the report of turn %s names no hypothesis, change or expected effect", last.Request.TurnID)}, nil
		}
		return climb.MasonResult{Report: &climb.AttemptReport{Hypothesis: done.Attempt.Hypothesis, Change: done.Attempt.Change, Expected: done.Attempt.Expected}}, nil
	}
	if last.Status() == "interrupted" {
		return climb.MasonResult{Pending: true}, h.recoverTurn(ctx, last, unit)
	}
	if Exhausted(last) {
		return climb.MasonResult{NoCandidate: fmt.Sprintf("the mason's turn %s failed on infrastructure and its retries are exhausted: %s", last.Request.TurnID, FailureHistory(last))}, nil
	}
	if k := cleanTurns(turns); k >= m.cfg.Mason.MaxCleanTurns {
		return climb.MasonResult{NoCandidate: fmt.Sprintf("the mason ended %d clean turns without a report, reaching mason.max_clean_turns (%d); the last ended %s", k, m.cfg.Mason.MaxCleanTurns, lastEnding(last))}, nil
	}
	return climb.MasonResult{Pending: true}, h.enqueueContinuation(ctx, last, unit, n, cleanTurns(turns)+1)
}

func lastEnding(q trace.QueuedTurn) string {
	if reason, ended := endingWithoutReport(q); ended {
		return reason
	}
	return "without calling done"
}

func (h *ClimbHost) request(unit, turn, cause, system, prompt string, depth int) (trace.TurnRequest, error) {
	m := h.m
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return trace.TurnRequest{}, err
	}
	return trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: h.stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: cause, Depth: depth},
		AgentID: ClimbMasonAgent, ThreadID: ClimbMasonAgent, TurnID: turn, Profile: profile, SystemPrompt: system, Prompt: prompt}, nil
}

func (h *ClimbHost) enqueueFirst(ctx context.Context, req climb.MasonRequest, unit string) error {
	m := h.m
	files := bundle.Files{Provider: m.s.ContextFor(m.repository), Role: "mason", Repository: func(config.ProjectID) (*trace.Repository, error) { return m.repository, nil }, Unplanned: unplannedUnit}
	mason, err := files.Mason(ctx, m.repository.Project(), h.stream, unit)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Make attempt %d of this climb.\n\nYour view holds the project's files as the climb's current best, commit %s, has them. Do the attempt's task below, then call done with your hypothesis, the change you made and the effect you expect, and end your turn.\n\n%s\n%s", req.Journal.Number, req.Journal.Parent, mason.Render(), req.Brief.Render())
	turn := fmt.Sprintf("climb-mason-%d-1", req.Journal.Number)
	tr, err := h.request(unit, turn, trace.ClimbJournalID(req.Journal.Number), fmt.Sprintf(climbMasonSystemPrompt, m.cfg.Project.Name, m.cfg.Project.Upstream), prompt, 1)
	if err != nil {
		return err
	}
	return m.EnqueueTurn(ctx, tr)
}

func (h *ClimbHost) enqueueContinuation(ctx context.Context, last trace.QueuedTurn, unit string, n, k int) error {
	prompt := continuationPrompt(last, nil)
	prompt = strings.Replace(prompt, "Continue the unit from there, check its acceptance, and call done with the required report when it holds; call ask if you need a decision.", "Continue the attempt from there and call done with your hypothesis, the change you made and the effect you expect when the change is ready.", 1)
	prompt = strings.Replace(prompt, "check the unit's acceptance, and call done again.", "and call done again with your hypothesis, the change and the expected effect.", 1)
	tr, err := h.request(unit, fmt.Sprintf("climb-mason-%d-%d", n, k), last.Response.ID, fmt.Sprintf(climbMasonSystemPrompt, h.m.cfg.Project.Name, h.m.cfg.Project.Upstream), prompt, last.Request.Depth+1)
	if err != nil {
		return err
	}
	return h.m.EnqueueTurn(ctx, tr)
}

func (h *ClimbHost) recoverTurn(ctx context.Context, last trace.QueuedTurn, unit string) error {
	m := h.m
	req := last.Request
	req.Profile, _, _ = m.s.RoleExecution(m.cfg, masonRole)
	turn := fmt.Sprintf("%s-recover-%d", last.Request.TurnID, last.Sequence)
	req.ID, req.TurnID, req.At = "request_"+turn, turn, m.s.Now()
	if last.Response != nil {
		req.Cause = last.Response.ID
	}
	paths, recorded, err := h.units.Changed(ctx, h.stream, unit, last.Request.TurnID)
	if err != nil {
		return err
	}
	req.Prompt = last.Request.Prompt + "\n\n" + Interruption(last) + " " + kept("that turn", paths, recorded) + " Continue from those files and call done with your hypothesis, the change and the expected effect when the change is ready."
	return m.EnqueueTurn(ctx, req)
}

// Review reports how the review of a kept candidate stands. It queues a review
// turn on the attempt's own reviewer thread, and rejects the candidate when
// the reviewer ends several turns without a verdict, so that a reviewer that
// cannot judge never approves a candidate.
func (h *ClimbHost) Review(ctx context.Context, req climb.ReviewRequest) (climb.ReviewResult, error) {
	m := h.m
	j := req.Journal
	unit := climb.AttemptUnit(j.Number)
	agent := reviewerAgent(unit)
	th, err := m.repository.Thread(h.stream, agent)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := m.repository.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: m.repository.Project(), Workstream: h.stream, Unit: unit, At: m.s.Now(), Actor: ReviewerActor, Cause: trace.ClimbJournalID(j.Number)}, Role: reviewerRole, ThreadID: agent}); err != nil && !errors.Is(err, trace.ErrConflict) {
			return climb.ReviewResult{}, err
		}
		if th, err = m.repository.Thread(h.stream, agent); err != nil {
			return climb.ReviewResult{}, err
		}
	case err != nil:
		return climb.ReviewResult{}, err
	}
	base := fmt.Sprintf("%s-review", agent)
	var turns []trace.QueuedTurn
	for _, q := range th.Turns {
		if strings.HasPrefix(q.Request.TurnID, base) {
			turns = append(turns, q)
		}
	}
	if len(turns) == 0 {
		return climb.ReviewResult{Pending: true}, h.enqueueReview(ctx, req, agent, base+"-1")
	}
	last := turns[len(turns)-1]
	if th.Status == "interrupted" && th.Active == last.Request.TurnID && last.Response == nil {
		if err := m.repository.AbandonTurn(ctx, h.stream, agent, last.Request.TurnID, m.s.Now()); err != nil {
			return climb.ReviewResult{}, err
		}
		if th, err = m.repository.Thread(h.stream, agent); err != nil {
			return climb.ReviewResult{}, err
		}
		last = th.Turns[len(th.Turns)-1]
	}
	if last.Response == nil || last.CompletedAt.IsZero() {
		return climb.ReviewResult{Pending: true}, nil
	}
	if last.Response.Result.Outcome != nil && last.Response.Result.Outcome.Status == VerdictOutcome && last.Status() == "idle" {
		var verdict UnitVerdict
		if err := json.Unmarshal([]byte(last.Response.Result.Outcome.Report), &verdict); err != nil {
			return climb.ReviewResult{}, fmt.Errorf("the verdict of turn %s: %w", last.Request.TurnID, err)
		}
		return climb.ReviewResult{Review: climb.Review{Approved: verdict.Decision == "satisfactory", Summary: verdict.Summary, Notes: findingsText(verdict), Turn: last.Request.TurnID}}, nil
	}
	if last.Status() == "interrupted" {
		return climb.ReviewResult{Pending: true}, (&reviewers{masons: m}).Recover(ctx, last)
	}
	if k := len(turns); k >= reviewTurnAttempts {
		return climb.ReviewResult{Review: climb.Review{Approved: false, Notes: fmt.Sprintf("the reviewer ended %d turns without a verdict, so the candidate was not approved; the last ended %s", k, lastEnding(last)), Turn: last.Request.TurnID}}, nil
	}
	return climb.ReviewResult{Pending: true}, h.enqueueReview(ctx, req, agent, fmt.Sprintf("%s-%d", base, len(turns)+1))
}

func findingsText(v UnitVerdict) string {
	var b strings.Builder
	if v.Decision != "satisfactory" && strings.TrimSpace(v.Summary) != "" {
		b.WriteString(strings.TrimSpace(v.Summary))
	}
	for _, f := range v.Findings {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s: %s Next: %s", f.Severity, strings.TrimSpace(f.Evidence), strings.TrimSpace(f.Action))
	}
	return b.String()
}

func (h *ClimbHost) enqueueReview(ctx context.Context, req climb.ReviewRequest, agent, turn string) error {
	m := h.m
	j := req.Journal
	unit := climb.AttemptUnit(j.Number)
	latest, _, found, err := seal.Latest(m.repository, h.stream)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("workstream %s has no seal", h.stream)
	}
	sum := sha256.Sum256([]byte(req.Diff))
	identity := UnitReviewIdentity{Subject: string(h.stream) + "/" + unit,
		Candidate:  coreadapter.Candidate{Revision: j.Candidate, BaseRevision: j.Parent, SpecRevision: fmt.Sprint(latest.Revision.Spec), PlanRevision: fmt.Sprint(latest.Revision.Plan)},
		DiffSHA256: hex.EncodeToString(sum[:]), Report: trace.ClimbJournalPath(j.Number), Seal: latest.Seal}
	content, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	profile, _, err := m.s.RoleExecution(m.cfg, reviewerRole)
	if err != nil {
		return err
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Review this exact candidate of attempt %d: read it for metric gaming, breaches of the project's written rules and scope, then record your verdict. Go already decided by arithmetic that the candidate is kept; you decide only whether it may land.\n\nThe candidate identity is:\n%s\n\nChanged files, with added and removed lines; read the diff with %s:\n%s\n", j.Number, content, reviewtools.DiffToolName, gitdiff.ChangedFiles(req.Diff))
	fmt.Fprintf(&prompt, "\nThe mason's hypothesis: %s\nThe change it reports: %s\nThe effect it expects: %s\n", strings.TrimSpace(j.Hypothesis), strings.TrimSpace(j.ChangeSummary), strings.TrimSpace(j.Expected))
	if j.Measurement != nil && j.Decision != nil {
		fmt.Fprintf(&prompt, "\nThe service's measurement: %s %v over %d run(s) (runs %v), against the best %v: a change of %+v, an improvement of %v in the metric's direction.\n", req.Brief.Spec.Metric.Name, j.Measurement.Score, len(j.Measurement.Runs), j.Measurement.Scores, j.Best, j.Decision.Change, j.Decision.Improvement)
	}
	fmt.Fprintf(&prompt, "\nThe climb's goal: %s\nThe protected paths, which no attempt may change: %s.\n", strings.TrimSpace(req.Brief.Spec.Goal), strings.Join(req.Brief.Protected, ", "))
	fmt.Fprintf(&prompt, "\n%s", climbSpecText(req.Brief))
	tr := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: m.repository.Project(), Workstream: h.stream, Unit: unit, At: m.s.Now(), Actor: ReviewerActor, Cause: trace.ClimbJournalID(j.Number), Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: climbReviewerSystemPrompt + " " + unitReviewSkills + CheckGateReviewInstructions, Prompt: prompt.String()}
	_, err = m.repository.EnqueueTurn(ctx, tr)
	return err
}

func climbSpecText(b climb.Brief) string {
	data, err := json.MarshalIndent(b.Spec, "", "  ")
	if err != nil {
		return ""
	}
	return "The climb spec:\n" + string(data) + "\n"
}

// Guardrails runs the climb spec's guardrail checks on the candidate in a fresh
// export, holding a capacity.checks slot, under the project's checks timeout.
// A run that cannot be carried out is an error and is tried again; a check
// that fails is a red guardrail.
func (h *ClimbHost) Guardrails(ctx context.Context, req climb.GuardrailRequest) (climb.Guardrails, error) {
	m := h.m
	checks := m.s.Checks
	if checks == nil {
		return climb.Guardrails{}, errors.New("the candidate check runner is unavailable")
	}
	base := filepath.Join(m.cfg.Root.String(), "checks")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return climb.Guardrails{}, err
	}
	dir, err := os.MkdirTemp(base, "guardrail-")
	if err != nil {
		return climb.Guardrails{}, err
	}
	defer os.RemoveAll(dir)
	if err := h.g.Export(ctx, req.Candidate, dir); err != nil {
		return climb.Guardrails{}, fmt.Errorf("candidate export: %w", err)
	}
	unit := climb.AttemptUnit(req.Number)
	release, err := m.s.AcquireCheckSlot(ctx, m.repository.Project(), h.stream, unit)
	if err != nil {
		return climb.Guardrails{}, err
	}
	defer release()
	identity := checkrun.Identity{Candidate: req.Candidate, Branch: UnitBranch(h.stream, unit), Remote: "https://github.com/" + m.cfg.Project.Upstream, Repository: m.cfg.Project.Clone}
	selection := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: slices.Clone(req.Checks), Candidates: len(req.Checks), Reason: "guardrail"}
	timeout := m.s.About(m.repository).Project.CheckTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, ran, err := checkrun.RunSelection(runCtx, checks, dir, identity, selection)
	if ctx.Err() != nil {
		return climb.Guardrails{}, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("the guardrail checks did not finish within checks_timeout %s", timeout)
	}
	status, failed, cerr := checkrun.ClassifyChecks(result, err)
	switch status {
	case ChecksPassed:
		return climb.Guardrails{Green: true, Detail: checkrun.DescribeSelection(ran)}, nil
	case ChecksFailed:
		return climb.Guardrails{Green: false, Detail: "the guardrail checks failed: " + strings.Join(failed, ", ")}, nil
	}
	return climb.Guardrails{}, cerr
}
