package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/vcs"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// AssembledState is the feature state of a workstream whose planned units
// have all merged.
const AssembledState = "assembled"

// FinalReviewAction is the runner-boundary operation action that rebases an
// assembled workstream's feature branch onto upstream one last time and runs
// one committee member's read of the resulting branch against the sealed spec
// and the charter.
const FinalReviewAction = "final-review"

// FinalReportTool is the tool the final reader records its report with.
const FinalReportTool = "final_report"

const (
	// finalReviewSubject is the workflow subject that tracks a workstream's
	// final reviews: requested-<k> once review k is asked for, then
	// reviewed-<k> or failed-<k>.
	finalReviewSubject = "final-review"
	// finalRebasePath and finalReportPath are the workstream documents of
	// the final rebase and the final report, with their trace record IDs.
	finalRebasePath     = "final/rebase.json"
	finalRebaseDocument = "final-rebase"
	finalReportPath     = "final/report.json"
	finalReportDocument = "final-report"
	// finalReviewed and finalFailed are the outcomes of a final report.
	finalReviewed = "reviewed"
	finalFailed   = "failed"
)

var finalReviewActor = trace.Actor{Kind: "service", ID: "final-review"}

// finalReviewInput names one final review: its number and the inputs it was
// asked for, the feature branch commit and the seal, spec, plan and charter
// revisions that governed the workstream then.
type finalReviewInput struct {
	Review  int    `json:"review"`
	Commit  string `json:"commit"`
	Seal    int    `json:"seal"`
	Spec    int    `json:"spec"`
	Plan    int    `json:"plan"`
	Charter int    `json:"charter"`
}

// FinalRebase is the document final/rebase.json: the rebase of the feature
// branch that final review Review reads. Before is the branch commit the
// review was asked for, Upstream the upstream commit it was rebased onto and
// Commit the rebased branch commit.
type FinalRebase struct {
	Review    int       `json:"review"`
	Operation string    `json:"operation"`
	Branch    string    `json:"branch"`
	Upstream  seal.Base `json:"upstream"`
	Before    string    `json:"before"`
	Commit    string    `json:"commit"`
}

// FinalReport is the document final/report.json: one final review of an
// assembled workstream. A reviewed report names the exact branch commit the
// committee member read, the governing seal, spec hash, spec, plan and
// charter revisions, and evidence or an explicit gap for every criterion of
// the sealed spec. A failed report says why no such report exists; it never
// authorises approval or delivery.
type FinalReport struct {
	Review    int              `json:"review"`
	Operation string           `json:"operation"`
	Outcome   string           `json:"outcome"`
	Failure   string           `json:"failure,omitempty"`
	Branch    string           `json:"branch"`
	Before    string           `json:"before"`
	Commit    string           `json:"commit,omitempty"`
	Upstream  *seal.Base       `json:"upstream,omitempty"`
	Conflicts []string         `json:"conflicts,omitempty"`
	Seal      int              `json:"seal"`
	SpecHash  string           `json:"spec_hash"`
	Spec      int              `json:"spec"`
	Plan      int              `json:"plan"`
	Charter   int              `json:"charter"`
	Reader    string           `json:"reader,omitempty"`
	Turn      string           `json:"turn,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	Criteria  []FinalCriterion `json:"criteria"`
}

// FinalCriterion is the final reader's account of one sealed criterion:
// Evidence says what in the branch shows it holds, or Gap what does not.
type FinalCriterion struct {
	Criterion string `json:"criterion"`
	Text      string `json:"text"`
	Evidence  string `json:"evidence,omitempty"`
	Gap       string `json:"gap,omitempty"`
}

// finalReportInput is what the final reader's tool call records.
type finalReportInput struct {
	Summary  string `json:"summary,omitempty"`
	Criteria []struct {
		Criterion string `json:"criterion"`
		Evidence  string `json:"evidence,omitempty"`
		Gap       string `json:"gap,omitempty"`
	} `json:"criteria"`
}

func finalReviewIDs(k int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", finalReviewSubject, k)
	return transition, trace.EventID(transition, "run")
}

func finalTurnPrefix(k int, member string) string { return fmt.Sprintf("final-%d-%s-", k, member) }

func finalTurnID(k int, member string, attempt int) string {
	return fmt.Sprintf("%s%d", finalTurnPrefix(k, member), attempt)
}

// finalReviewer is the assembly controller. Its pass moves a building
// workstream whose every planned unit has merged to assembled, and asks for
// a final review of an assembled workstream whose latest review does not read
// its current branch and governing documents; its reconciler runs each final
// review operation.
type finalReviewer struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*finalReviewer)(nil)

// Pass reconciles every workstream of the trace except the librarian's.
func (a *finalReviewer) Pass(ctx context.Context) error {
	streams, err := a.repository.Workstreams()
	if err != nil {
		return err
	}
	librarian := librarianWorkstream(a.repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		switch feature.Value {
		case BuildingState:
			err = a.assemble(ctx, stream)
		case AssembledState:
			err = a.request(ctx, stream)
		}
		if err != nil {
			return fmt.Errorf("workstream %s final review: %w", stream, err)
		}
	}
	return nil
}

// assemble moves a building workstream to assembled once every unit of its
// sealed plan has merged, and does nothing while any has not.
func (a *finalReviewer) assemble(ctx context.Context, stream config.WorkstreamID) error {
	b, found, err := (&masons{s: a.s, cfg: a.s.current(), repository: a.repository}).read(stream)
	if err != nil || !found {
		return err
	}
	if merged, _ := allMerged(b); !merged {
		return nil
	}
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return err
	}
	cause := ""
	var ids []string
	for _, u := range b.plan.Units {
		ids = append(ids, u.ID)
	}
	for _, t := range transitions {
		if t.To == UnitMerged && strings.HasPrefix(t.Subject, "unit") {
			cause = t.ID
		}
	}
	if cause == "" {
		cause = BuildingState
	}
	reason := fmt.Sprintf("every unit of the sealed plan has merged onto %s: %s; the feature branch is rebased onto upstream and read against the sealed spec and the charter next", featureBranch(stream), strings.Join(ids, ", "))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: AssembledState, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: foremanActor, Cause: cause}
	_, err = a.repository.MoveFeatureState(ctx, h, BuildingState, AssembledState, reason)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

// allMerged reports whether every unit of the building workstream's plan
// has merged, and names the first that has not.
func allMerged(b building) (bool, string) {
	for _, u := range b.plan.Units {
		if b.states[trace.UnitSubject(u.ID)].Value != UnitMerged {
			return false, u.ID
		}
	}
	return len(b.plan.Units) > 0, ""
}

// governing returns the inputs a final review of the workstream would read
// now: the feature branch tip and the latest seal with its spec and plan
// revisions and the latest charter revision.
func (a *finalReviewer) governing(ctx context.Context, stream config.WorkstreamID) (finalReviewInput, seal.Seal, error) {
	latest, _, found, err := seal.Latest(a.repository, stream)
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	if !found {
		return finalReviewInput{}, seal.Seal{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	tip, exists, err := featureWorkspaces(a.s.current()).Branch(ctx, featureBranch(stream))
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	if !exists {
		return finalReviewInput{}, seal.Seal{}, fmt.Errorf("the clone has no feature branch %s", featureBranch(stream))
	}
	charter, err := a.repository.Charter(ctx, a.s.now())
	if err != nil {
		return finalReviewInput{}, seal.Seal{}, err
	}
	return finalReviewInput{Commit: tip, Seal: latest.Seal, Spec: latest.Revision.Spec, Plan: latest.Revision.Plan, Charter: charter.Revision}, latest, nil
}

// reads reports whether a review asked for with in, or recorded as report,
// reads the governing inputs now.
func (in finalReviewInput) reads(now finalReviewInput) bool {
	in.Review, now.Review = 0, 0
	return in == now
}

func (r FinalReport) input() finalReviewInput {
	return finalReviewInput{Commit: r.Commit, Seal: r.Seal, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter}
}

// request asks for the next final review of an assembled workstream unless
// one has no result yet, the workstream is paused, the service has no
// committee runner, or a review was already asked for, or a report already
// reads, the current branch, seal, spec, plan and charter. A review that
// failed is asked for again only once one of them changes.
func (a *finalReviewer) request(ctx context.Context, stream config.WorkstreamID) error {
	if a.s.options.Committee == nil {
		return nil
	}
	state, _ := a.s.store.Effective()
	if scheduler.Paused(state.Pauses, a.repository.Project(), stream) {
		return nil
	}
	ops, err := a.repository.Operations(stream)
	if err != nil {
		return err
	}
	var asked []finalReviewInput
	for _, o := range ops {
		if o.Operation.Action != FinalReviewAction {
			continue
		}
		if o.Result == nil {
			return nil
		}
		in, err := decodeFinalReview(o.Operation)
		if err != nil {
			return err
		}
		asked = append(asked, in)
	}
	now, _, err := a.governing(ctx, stream)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(asked, func(in finalReviewInput) bool { return in.reads(now) }) {
		return nil
	}
	if report, found, err := latestFinalReport(a.repository, stream); err != nil {
		return err
	} else if found && report.Outcome == finalReviewed && report.input().reads(now) {
		return nil
	}
	subject, err := a.repository.Workflow(stream, finalReviewSubject)
	if err != nil {
		return err
	}
	now.Review = len(asked) + 1
	transition, event := finalReviewIDs(now.Review)
	input, err := json.Marshal(now)
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(a.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: FinalReviewAction, Input: input}
	reason := fmt.Sprintf("the workstream is assembled; final review %d rebases %s, at %s, onto upstream and reads it against seal %d: spec revision %d, plan revision %d and charter revision %d", now.Review, featureBranch(stream), now.Commit, now.Seal, now.Spec, now.Plan, now.Charter)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: finalReviewActor, Cause: AssembledState}
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: finalReviewSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", now.Review), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: FinalReviewAction, Body: fmt.Sprintf("Run final review %d", now.Review), Operation: &op}}}
	if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func decodeFinalReview(op coreadapter.Operation) (finalReviewInput, error) {
	var in finalReviewInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != FinalReviewAction {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid final review operation input: %w", err)
	}
	if in.Review < 1 || in.Commit == "" || in.Seal < 1 || in.Spec < 1 || in.Plan < 1 || in.Charter < 1 {
		return in, errors.New("final review operation requires a positive review number, a commit and the seal, spec, plan and charter revisions")
	}
	return in, nil
}

// stream returns the workstream a final review operation belongs to: the one
// whose run event derives the operation ID.
func (a *finalReviewer) stream(op coreadapter.Operation, in finalReviewInput) (config.WorkstreamID, error) {
	streams, err := a.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := finalReviewIDs(in.Review)
	for _, stream := range streams {
		if trace.OperationID(a.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("final review operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of final review k: succeeded once its
// report is reviewed, failed once its failure is recorded, nil before either.
func (a *finalReviewer) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := finalReviewIDs(k)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-" + finalReviewed:
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-" + finalFailed:
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions and the reader's thread: a recorded
// outcome completes the operation, a reader turn still running leaves it
// unknown, and otherwise it is absent.
func (a *finalReviewer) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeFinalReview(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := a.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := a.outcome(stream, in.Review)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "final review " + result.Outcome, Result: result}, nil
	}
	t, err := a.repository.Thread(stream, committeeAgent(1))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return coreadapter.Observation{}, err
	}
	if err == nil {
		for _, q := range t.Turns {
			if strings.HasPrefix(q.Request.TurnID, finalTurnPrefix(in.Review, committeeAgent(1))) && q.Claim != nil && q.Response == nil && t.Status != "interrupted" {
				return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "final review turn " + q.Request.TurnID + " is running"}, nil
			}
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("final review %d is not recorded", in.Review)}, nil
}

// Apply runs final review k to a recorded report. The workstream must still
// be assembled and read the seal, spec, plan and charter the review was asked
// for, and its feature branch must still be at the commit it was asked for.
// The foreman then fetches the configured upstream base branch and replays
// the feature branch onto it; final/rebase.json records the result before
// the branch moves, so a retry moves it to the same commit. A replay that
// conflicts leaves the branch where it was. The first committee member then
// reads the rebased branch, the sealed spec and plan and the charter in a
// read-only view and records evidence or a gap for every sealed criterion.
// One commit records final/report.json with the review's move to reviewed or
// failed and a notice for the chief of staff. A review whose inputs changed,
// whose replay conflicted, or whose reader recorded no report fails with the
// reason in its report. Storage, fetch and Git errors, and a missing
// committee runner, leave the operation pending for another attempt.
func (a *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeFinalReview(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := a.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := a.outcome(stream, in.Review); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := a.s.current()
	if !cfg.HasProject() || cfg.Project.ID != a.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	latest, _, found, err := seal.Latest(a.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	report := FinalReport{Review: in.Review, Operation: op.ID, Outcome: finalFailed, Branch: featureBranch(stream), Before: in.Commit, Seal: in.Seal, SpecHash: latest.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Criteria: []FinalCriterion{}}
	fail := func(reason string) (coreadapter.OperationResult, error) {
		report.Failure = reason
		return a.record(ctx, stream, report)
	}
	feature, err := a.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != AssembledState {
		return fail(fmt.Sprintf("the workstream is %s, not assembled", featureState(feature.Value)))
	}
	now, _, err := a.governing(ctx, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	rebase, rebased, err := a.rebase(stream, in.Review)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if now.Seal != in.Seal || now.Spec != in.Spec || now.Plan != in.Plan || now.Charter != in.Charter {
		return fail(fmt.Sprintf("the governing documents changed since the review was asked for: seal %d, spec revision %d, plan revision %d and charter revision %d are current", now.Seal, now.Spec, now.Plan, now.Charter))
	}
	g := featureWorkspaces(cfg)
	if !rebased {
		if now.Commit != in.Commit {
			return fail(fmt.Sprintf("feature branch %s moved to %s since the review was asked for", report.Branch, now.Commit))
		}
		requested, err := (&foreman{masons: &masons{s: a.s, cfg: cfg, repository: a.repository}}).requestedAt(stream, op.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		remote, err := g.Remote(ctx, cfg.Project.Upstream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		fetched, err := g.Fetch(ctx, remote, cfg.Project.BaseBranch)
		if err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("fetch %s of %s: %w", cfg.Project.BaseBranch, remote, err)
		}
		upstream := seal.Base{Remote: remote, Branch: cfg.Project.BaseBranch, Commit: fetched}
		commit, conflicts, err := g.Replay(ctx, in.Commit, fetched, requested)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(conflicts) > 0 {
			report.Upstream, report.Conflicts = &upstream, conflicts
			return fail(fmt.Sprintf("feature branch %s does not rebase cleanly onto %s/%s at %s: %s conflicted; the branch is left at %s", report.Branch, remote, upstream.Branch, fetched, strings.Join(conflicts, ", "), in.Commit))
		}
		rebase = FinalRebase{Review: in.Review, Operation: op.ID, Branch: report.Branch, Upstream: upstream, Before: in.Commit, Commit: commit}
		if err := a.recordRebase(ctx, stream, rebase); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := a.s.step("final-rebase-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	report.Upstream = &rebase.Upstream
	if now.Commit != rebase.Before && now.Commit != rebase.Commit {
		return fail(fmt.Sprintf("feature branch %s moved to %s during the review", report.Branch, now.Commit))
	}
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: report.Branch})
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("feature branch %s: %w", report.Branch, err)
	}
	if err := g.Move(ctx, acquired.(workspace.Worktree), rebase.Before, rebase.Commit); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := a.s.step("final-rebase-moved"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	report.Commit = rebase.Commit
	return a.read(ctx, cfg, stream, in, report)
}

// rebase returns the recorded rebase of final review k, and whether one is.
func (a *finalReviewer) rebase(stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return FinalRebase{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalRebaseDocument {
			continue
		}
		var r FinalRebase
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			return FinalRebase{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if r.Review == k {
			return r, true, nil
		}
	}
	return FinalRebase{}, false, nil
}

// recordRebase records the next revision of final/rebase.json.
func (a *finalReviewer) recordRebase(ctx context.Context, stream config.WorkstreamID, rebase FinalRebase) error {
	content, err := json.MarshalIndent(rebase, "", "  ")
	if err != nil {
		return err
	}
	revision, err := nextRevision(a.repository, stream, finalRebaseDocument)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalRebaseDocument, Revision: revision, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: foremanActor, Cause: rebase.Operation},
		Path: finalRebasePath, Content: string(content) + "\n"}
	return a.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// nextRevision returns the revision the next record of a workstream
// document takes.
func nextRevision(repository *trace.Repository, stream config.WorkstreamID, id string) (int, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return 0, err
	}
	revision := 1
	for _, d := range docs {
		if d.ID == id && d.Revision >= revision {
			revision = d.Revision + 1
		}
	}
	return revision, nil
}

// read drives the first committee member's turn of the review to its end
// and records the report it left. It abandons a turn a previous service stop
// interrupted and starts a new turn while attempts remain.
func (a *finalReviewer) read(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in finalReviewInput, report FinalReport) (coreadapter.OperationResult, error) {
	if err := (&debate{s: a.s, repository: a.repository}).ensureCommittee(ctx, stream, 1); err != nil {
		return coreadapter.OperationResult{}, err
	}
	member := committeeAgent(1)
	report.Reader = member
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.s.turns.add(stream, cancel)()
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err := a.repository.Thread(stream, member)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		var turns []trace.QueuedTurn
		for _, q := range t.Turns {
			if strings.HasPrefix(q.Request.TurnID, finalTurnPrefix(in.Review, member)) {
				turns = append(turns, q)
			}
		}
		var last, pending *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		if i := slices.IndexFunc(turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() }); i >= 0 {
			pending = &turns[i]
		}
		switch {
		case pending != nil && pending.Claim != nil && pending.Response == nil:
			if t.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("final review turn " + pending.Request.TurnID + " is still running")
			}
			if err := a.repository.AbandonTurn(ctx, stream, member, pending.Request.TurnID, a.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case pending != nil:
			gone, err := abandoned(a.repository, stream)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if gone && pending.Response == nil {
				if _, err := a.repository.CancelTurns(ctx, stream, a.s.now(), abandonActor, cancelReason); err != nil {
					return coreadapter.OperationResult{}, err
				}
				continue
			}
			turnCtx := running
			if pending.Response != nil {
				turnCtx = ctx
			} else if a.s.options.Committee == nil {
				return coreadapter.OperationResult{}, errNoCommittee
			}
			if _, err := a.dispatch(turnCtx, stream, in, report, member, pending.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(a.turnDirectory(stream, pending.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last == nil || last.Status() == "interrupted":
			if gone, err := abandoned(a.repository, stream); err != nil || gone {
				if err != nil {
					return coreadapter.OperationResult{}, err
				}
				report.Failure = "the owner abandoned the workstream, so the committee member ran no turn"
				return a.record(ctx, stream, report)
			}
			if len(turns) >= maxRoundAttempts {
				report.Failure = fmt.Sprintf("the committee member's turn was interrupted %d times by service stops", len(turns))
				return a.record(ctx, stream, report)
			}
			if a.s.options.Committee == nil {
				return coreadapter.OperationResult{}, errNoCommittee
			}
			if err := a.enqueue(ctx, cfg, stream, in, report, member, len(turns)+1); err != nil {
				return coreadapter.OperationResult{}, err
			}
		default:
			report.Turn = last.Request.TurnID
			if last.Status() != "idle" {
				report.Failure = "the committee member's turn ended with status " + last.Status()
				if last.Response != nil && last.Response.Failure != "" {
					report.Failure += ": " + last.Response.Failure
				}
				return a.record(ctx, stream, report)
			}
			recorded, found, err := a.recorded(stream, last.Request.TurnID)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			if !found {
				report.Failure = "the committee member ended its turn without recording a report"
				return a.record(ctx, stream, report)
			}
			report.Outcome, report.Summary, report.Criteria = finalReviewed, recorded.Summary, recorded.Criteria
			return a.record(ctx, stream, report)
		}
	}
}

// turnDirectory is the service-owned directory of one final review turn: its
// staged view, its backend session and the report its tool kept.
func (a *finalReviewer) turnDirectory(stream config.WorkstreamID, turn string) string {
	return filepath.Join(a.s.current().Root.String(), "final", string(a.repository.Project()), string(stream), turn)
}

func (a *finalReviewer) output(stream config.WorkstreamID, turn string) string {
	return filepath.Join(a.turnDirectory(stream, turn), "output", "report.json")
}

// recorded returns the report the turn's tool kept, and whether it kept one.
func (a *finalReviewer) recorded(stream config.WorkstreamID, turn string) (FinalReport, bool, error) {
	data, err := os.ReadFile(a.output(stream, turn))
	if errors.Is(err, fs.ErrNotExist) {
		return FinalReport{}, false, nil
	}
	if err != nil {
		return FinalReport{}, false, err
	}
	var report FinalReport
	if err := json.Unmarshal(data, &report); err != nil {
		return FinalReport{}, false, err
	}
	return report, true, nil
}

// enqueue accepts one attempt of the reader's turn, fixing its profile and
// prompts.
func (a *finalReviewer) enqueue(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, in finalReviewInput, report FinalReport, member string, attempt int) error {
	profile, _, err := a.s.roleExecution(cfg, committeeRole)
	if err != nil {
		return err
	}
	t, err := a.repository.Thread(stream, member)
	if err != nil {
		return err
	}
	turn := finalTurnID(in.Review, member, attempt)
	transition, _ := finalReviewIDs(in.Review)
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: a.s.now(), Actor: finalReviewActor, Cause: transition, Depth: 1},
		AgentID: member, ThreadID: t.Identity.ThreadID, TurnID: turn, Profile: profile, SystemPrompt: finalSystemPrompt(cfg.Project), Prompt: finalPrompt(report)}
	_, err = a.repository.EnqueueTurn(ctx, req)
	return err
}

// dispatch runs the turn through the thread dispatcher and runner.
func (a *finalReviewer) dispatch(ctx context.Context, stream config.WorkstreamID, in finalReviewInput, report FinalReport, member, turn string) (coreadapter.OperationResult, error) {
	dispatcher := thread.Dispatcher{Runner: thread.Runner{Store: a.repository, Turns: a.turns(stream, in, report), Now: a.s.now}, Prepare: func(_ context.Context, input thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(a.turnDirectory(input.Workstream, input.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(a.repository.Project(), "final-review-turn", thread.TurnInput{Workstream: stream, Agent: member, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return dispatcher.Apply(ctx, op)
}

// turns is the final reader's isolated turn path: a read-only private view
// of the reviewed branch, its diff from upstream, the sealed spec and plan,
// the charter and the units' reports and landings, the file reading tool and
// the report tool, and no write, execute, network or VCS capability.
func (a *finalReviewer) turns(stream config.WorkstreamID, in finalReviewInput, report FinalReport) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if c := a.s.options.Committee; c != nil {
		engine, hosts = c.Engine, c.Hosts
	}
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(a.s.current().Root.String(), "views")},
		Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			return a.selectView(ctx, scope, stream, report)
		},
		Grants: map[string]coreadapter.Capabilities{committeeRole: {Tools: []string{"file_read", FinalReportTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role != committeeRole || scope.Workstream != string(stream) || scope.Project != string(a.repository.Project()) || !strings.HasPrefix(scope.Turn, finalTurnPrefix(in.Review, report.Reader)) {
				return nil, errors.New("turn scope denied")
			}
			tool, err := a.tool(stream, scope.Turn, report)
			return []coreadapter.Tool{tool}, err
		},
		Hosts:  hosts,
		Engine: engine,
	}
}

// sealedCriteria returns the criteria of the review's sealed spec revision,
// cited as spec#<n>, with their text.
func (a *finalReviewer) sealedCriteria(stream config.WorkstreamID, report FinalReport) ([]FinalCriterion, error) {
	spec, _, err := a.documents(stream, report)
	if err != nil {
		return nil, err
	}
	var criteria []FinalCriterion
	for _, c := range plan.ParseSpec(spec.Content).Criteria {
		criteria = append(criteria, FinalCriterion{Criterion: plan.Cite(c.Number), Text: strings.Join(strings.Fields(c.Text), " ")})
	}
	if len(criteria) == 0 {
		return nil, fmt.Errorf("spec.md revision %d holds no criteria", report.Spec)
	}
	return criteria, nil
}

// tool binds the report tool to the turn: a call that accounts for every
// sealed criterion once, each with evidence or a gap and not both, is kept in
// the turn's output, replacing an earlier call's; any other is refused with
// the reason.
func (a *finalReviewer) tool(stream config.WorkstreamID, turn string, report FinalReport) (coreadapter.Tool, error) {
	sealed, err := a.sealedCriteria(stream, report)
	if err != nil {
		return coreadapter.Tool{}, err
	}
	file := a.output(stream, turn)
	tool := coreadapter.Tool{Name: FinalReportTool, Effect: coreadapter.ToolMemory,
		Description: "Record the final report on the whole branch: for every criterion of the sealed spec, cited as spec#<n>, either evidence (what in the branch shows the criterion holds: files, tests, behaviour) or a gap (what does not show it). A later call replaces an earlier one. End your turn once the report is recorded.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"criteria":{"type":"array","items":{"type":"object","properties":{"criterion":{"type":"string"},"evidence":{"type":"string"},"gap":{"type":"string"}},"required":["criterion"],"additionalProperties":false}}},"required":["criteria"],"additionalProperties":false}`)}
	tool.Handle = func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input finalReportInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		criteria, reason := accountFor(sealed, input)
		if reason != "" {
			return refuseReport("%s", reason)
		}
		data, err := json.Marshal(FinalReport{Summary: strings.TrimSpace(input.Summary), Criteria: criteria})
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return nil, err
		}
		// A rename keeps the previous report whole if the write stops.
		if err := errors.Join(os.WriteFile(file+".tmp", data, 0600), os.Rename(file+".tmp", file)); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"recorded": true, "next": "End your turn now, or call again to replace the report."})
	}
	return tool, nil
}

// accountFor returns the sealed criteria in spec order with the evidence or
// gap the input gives each, or why the input does not account for every
// sealed criterion exactly once.
func accountFor(sealed []FinalCriterion, input finalReportInput) ([]FinalCriterion, string) {
	given := map[string]FinalCriterion{}
	for _, c := range input.Criteria {
		i := slices.IndexFunc(sealed, func(s FinalCriterion) bool { return s.Criterion == c.Criterion })
		if i < 0 {
			return nil, fmt.Sprintf("%q is not a criterion of the sealed spec; cite criteria as spec#<n>", c.Criterion)
		}
		if _, repeated := given[c.Criterion]; repeated {
			return nil, fmt.Sprintf("%s is accounted for more than once", c.Criterion)
		}
		evidence, gap := strings.TrimSpace(c.Evidence), strings.TrimSpace(c.Gap)
		if (evidence == "") == (gap == "") {
			return nil, fmt.Sprintf("%s needs either evidence or a gap, not both and not neither", c.Criterion)
		}
		criterion := sealed[i]
		criterion.Evidence, criterion.Gap = evidence, gap
		given[c.Criterion] = criterion
	}
	var out []FinalCriterion
	var missing []string
	for _, s := range sealed {
		c, ok := given[s.Criterion]
		if !ok {
			missing = append(missing, s.Criterion)
		}
		out = append(out, c)
	}
	if len(missing) > 0 {
		return nil, "every sealed criterion needs evidence or a gap; missing " + strings.Join(missing, ", ")
	}
	return out, ""
}

// documents returns the review's sealed spec and plan revisions.
func (a *finalReviewer) documents(stream config.WorkstreamID, report FinalReport) (spec, graph trace.Document, err error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return spec, graph, err
	}
	for _, doc := range docs {
		switch {
		case doc.ID == plan.SpecDocument && doc.Revision == report.Spec:
			spec = doc
		case doc.ID == plan.PlanDocument && doc.Revision == report.Plan:
			graph = doc
		}
	}
	if spec.Revision == 0 || graph.Revision == 0 {
		return spec, graph, fmt.Errorf("workstream %s does not record spec revision %d and plan revision %d", stream, report.Spec, report.Plan)
	}
	return spec, graph, nil
}

// selectView stages the reader's view for the claimed turn and selects all of
// it, read-only.
func (a *finalReviewer) selectView(ctx context.Context, scope coreadapter.Scope, stream config.WorkstreamID, report FinalReport) (isolation.Selection, error) {
	cfg := a.s.current()
	if scope.Role != committeeRole || scope.Workstream != string(stream) || scope.Project != string(a.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != a.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	_, settings, err := a.s.roleExecution(cfg, committeeRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(a.turnDirectory(stream, scope.Turn), "workspace")
	paths, err := a.stage(ctx, cfg, stream, report, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings}, nil
}

// stage builds the view: the reviewed commit's tracked files under branch/,
// its diff from the upstream commit it was rebased onto as branch.diff, the
// sealed spec.md and plan.json, the charter revision the review reads as
// charter.md, and each unit's latest report and landing under units/. It
// returns the paths to select.
func (a *finalReviewer) stage(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	g := featureWorkspaces(cfg)
	if err := g.Export(ctx, report.Commit, filepath.Join(workspace, "branch")); err != nil {
		return nil, err
	}
	diff, err := g.Diff(ctx, report.Upstream.Commit, report.Commit)
	if err != nil {
		return nil, err
	}
	spec, graph, err := a.documents(stream, report)
	if err != nil {
		return nil, err
	}
	project, err := trace.Read[trace.Document](a.repository, "")
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(project, func(d trace.Document) bool { return d.ID == "charter" && d.Revision == report.Charter })
	if i < 0 {
		return nil, fmt.Errorf("the project records no charter revision %d", report.Charter)
	}
	files := map[string]string{"branch.diff": diff, plan.SpecPath: spec.Content, plan.PlanPath: graph.Content, "charter.md": project[i].Content}
	paths := []string{"branch", "branch.diff", plan.SpecPath, plan.PlanPath, "charter.md"}
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc.Unit != "" && (doc.ID == reportDocument(doc.Unit) || doc.ID == landingDocument(doc.Unit)) {
			files[doc.Path] = doc.Content
			if !slices.Contains(paths, "units") {
				paths = append(paths, "units")
			}
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

// record commits the next revision of final/report.json with the review's
// move to reviewed or failed and a notice for the chief of staff, and
// returns the review's result. A review already recorded returns its result
// and records nothing more.
func (a *finalReviewer) record(ctx context.Context, stream config.WorkstreamID, report FinalReport) (coreadapter.OperationResult, error) {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	revision, err := nextRevision(a.repository, stream, finalReportDocument)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := a.s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: revision, Project: a.repository.Project(), Workstream: stream, At: at, Actor: finalReviewActor, Cause: report.Operation},
		Path: finalReportPath, Content: string(content) + "\n"}
	transition, _ := finalReviewIDs(report.Review)
	id := transition + "-" + report.Outcome
	outcome := coreadapter.OperationResult{Outcome: "failed"}
	var reason, body string
	if report.Outcome == finalReviewed {
		outcome.Outcome = "succeeded"
		var gaps []string
		for _, c := range report.Criteria {
			if c.Gap != "" {
				gaps = append(gaps, c.Criterion)
			}
		}
		reason = fmt.Sprintf("final review %d read %s at %s against seal %d (spec revision %d, plan revision %d, charter revision %d): %d of %d criteria shown; gaps: %s; report %s revision %d",
			report.Review, report.Branch, report.Commit, report.Seal, report.Spec, report.Plan, report.Charter, len(report.Criteria)-len(gaps), len(report.Criteria), list(gaps), finalReportPath, revision)
		body = fmt.Sprintf("The final review of the assembled branch is recorded: %d of %d criteria are shown; not shown: %s.", len(report.Criteria)-len(gaps), len(report.Criteria), list(gaps))
	} else {
		reason = fmt.Sprintf("final review %d failed: %s; report %s revision %d", report.Review, report.Failure, finalReportPath, revision)
		body = fmt.Sprintf("The final review of the assembled branch failed: %s. The failed review authorises nothing; a new review runs once the branch or its governing documents change.", report.Failure)
	}
	outcome.Evidence = reason
	state, err := a.repository.Workflow(stream, finalReviewSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: finalReviewActor, Cause: report.Operation}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: finalReviewSubject, From: state.Value, To: fmt.Sprintf("%s-%d", report.Outcome, report.Review), Reason: reason},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := a.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := a.outcome(stream, report.Review); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return outcome, nil
}

// latestFinalReport returns the workstream's latest final report, and
// whether one is recorded.
func latestFinalReport(repository *trace.Repository, stream config.WorkstreamID) (FinalReport, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return FinalReport{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != finalReportDocument {
			continue
		}
		var report FinalReport
		if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
			return FinalReport{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		return report, true, nil
	}
	return FinalReport{}, false, nil
}

// finalGate returns the workstream's latest final report and why it cannot
// authorise approval or delivery: none is recorded, the latest failed, or it
// no longer reads the workstream's feature branch tip, latest seal and its
// spec hash, latest spec and plan revisions and latest charter revision. The
// reason is empty when the report can.
func (a *finalReviewer) finalGate(ctx context.Context, stream config.WorkstreamID) (FinalReport, string, error) {
	report, found, err := latestFinalReport(a.repository, stream)
	if err != nil || !found {
		return report, "no final review is recorded", err
	}
	if report.Outcome != finalReviewed {
		return report, fmt.Sprintf("final review %d failed: %s", report.Review, report.Failure), nil
	}
	now, latest, err := a.governing(ctx, stream)
	if err != nil {
		return report, "", err
	}
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return report, "", err
	}
	revisions := map[string]int{}
	for _, d := range docs {
		revisions[d.ID] = max(revisions[d.ID], d.Revision)
	}
	for _, field := range []struct {
		name     string
		reviewed any
		current  any
	}{
		{"feature branch commit", report.Commit, now.Commit},
		{"seal", report.Seal, now.Seal},
		{"spec hash", report.SpecHash, latest.SpecHash},
		{"spec revision", report.Spec, revisions[plan.SpecDocument]},
		{"plan revision", report.Plan, revisions[plan.PlanDocument]},
		{"charter revision", report.Charter, now.Charter},
	} {
		if field.reviewed != field.current {
			return report, fmt.Sprintf("final review %d is stale: it read %s %v, and %v is current; a new final review is required", report.Review, field.name, field.reviewed, field.current), nil
		}
	}
	return report, "", nil
}

func finalSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are the committee member who gives an assembled feature its final read for the %s project (%s): every planned unit has landed on the feature branch, and you read the whole branch against the feature's sealed spec and the project's charter before the owner decides whether it is delivered. You read; you hold no tool that writes, runs or fetches. You record your report with %s: for every criterion, what in the branch shows it holds, or the gap. A criterion you cannot see shown is a gap, never a guess.", p.Name, p.Upstream, FinalReportTool)
}

func finalPrompt(report FinalReport) string {
	return fmt.Sprintf(`Final review %d of the assembled feature branch at commit %s, rebased onto %s/%s at %s. The spec is revision %d, the plan revision %d, sealed as seal %d; the charter is revision %d.

Your view holds:
- branch/: every tracked file of the reviewed commit.
- branch.diff: the whole change the branch makes to upstream.
- spec.md and plan.json: the sealed spec and plan. Cite a criterion as spec#<n>.
- charter.md: the owner's rules for contributing to this project.
- units/<unit>/report.json and landing.json: each unit's last report and how it landed.

Read the whole branch, not unit by unit. For every criterion of spec.md, call %s once with all of them: evidence names what in the branch shows the criterion holds, such as files, tests and behaviour; a gap says what is missing, wrong or not shown, including anything that breaks a charter rule. Then end your turn.
`, report.Review, report.Commit, report.Upstream.Remote, report.Upstream.Branch, report.Upstream.Commit, report.Spec, report.Plan, report.Seal, report.Charter, FinalReportTool)
}
