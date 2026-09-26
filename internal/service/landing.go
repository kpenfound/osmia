package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/vcs"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// LandAction is the repository-boundary operation action that lands one
// approved unit: it squashes the approved candidate onto the feature branch
// as one commit and records the unit merged.
const LandAction = "land"

// landingTrailer is the commit message trailer that names the operation
// that made a landing commit.
const landingTrailer = "Osmia-Operation"

var foremanActor = trace.Actor{Kind: "service", ID: "foreman"}

// landInput names the approval a landing lands: the unit, the revision of
// its units/<unit>/review.json that approved it, and the candidate and base
// that revision records.
type landInput struct {
	Unit      string `json:"unit"`
	Review    int    `json:"review"`
	Candidate string `json:"candidate"`
	Base      string `json:"base"`
}

// UnitLanding is the document units/<unit>/landing.json: the approval a
// unit landed on, with the reviewed candidate and base, the governing spec
// and plan revisions and seal, the criteria the unit addresses, and the
// feature branch commit that landed it with its message.
type UnitLanding struct {
	Unit      string   `json:"unit"`
	Operation string   `json:"operation"`
	Approval  string   `json:"approval"`
	Turn      string   `json:"turn"`
	Candidate string   `json:"candidate"`
	Base      string   `json:"base"`
	Spec      string   `json:"spec"`
	Plan      string   `json:"plan"`
	Seal      int      `json:"seal"`
	Criteria  []string `json:"criteria"`
	Branch    string   `json:"branch"`
	Commit    string   `json:"commit"`
	Message   string   `json:"message"`
}

// landingSubject is the workflow subject that tracks the landings of one
// unit: requested-<k> once landing the approval of review revision k is
// asked for, then landed-<k> or refused-<k>.
func landingSubject(unit string) string {
	return "landing" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func landingDocument(unit string) string { return trace.UnitSubject(unit) + "-landing" }

func landIDs(unit string, review int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", landingSubject(unit), review)
	return transition, trace.EventID(transition, "run")
}

// featureWorkspaces returns the feature branch workspaces of the configured
// project, each workstream's on the backend the repository records for it.
func featureWorkspaces(cfg *config.Config, repository *trace.Repository) streamWorkspaces {
	return streamWorkspaces{cfg: cfg, repository: repository, directory: branchesDirectory}
}

// foreman is the landing controller. Its pass asks to land one approved unit
// at a time per project and to rebase the unit workspaces a landing left
// behind; its reconciler runs each landing operation, and rebaser each
// rebase operation. nextDrift is when its drift schedule is next read.
type foreman struct {
	*masons
	nextDrift time.Time
}

var _ coreadapter.Reconciler = (*foreman)(nil)

// Pass runs one landing and rebase sequence at a time per project. While a
// landing or drift rebase of the project has no result, it does nothing. Otherwise it keeps
// the unfinished units of the building and assembled workstreams that are not paused on
// their feature branches, rebasing each unit whose workspace a landing left
// behind and routing rebase conflicts to masons. Once every such unit is
// current and every rebase has its result, it asks to land the first approved
// unit, in the workstreams' priority order and each plan's dependency order,
// whose approval no landing was asked for.
func (f *foreman) Pass(ctx context.Context) error {
	pending, err := refreshPending(f.repository)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	streams, err := f.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := f.s.effective()
	librarian := librarianWorkstream(f.repository.Project())
	requested := map[config.WorkstreamID][]landInput{}
	rebasing := map[config.WorkstreamID]map[string]bool{}
	rebased := map[config.WorkstreamID]map[string][]string{}
	dispatched := map[config.WorkstreamID]map[string]bool{}
	var candidates []building
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ops, err := f.repository.Operations(stream)
		if err != nil {
			return err
		}
		rebasing[stream], rebased[stream], dispatched[stream] = map[string]bool{}, map[string][]string{}, map[string]bool{}
		for _, o := range ops {
			switch o.Operation.Action {
			case DriftAction:
				if o.Result == nil {
					return nil
				}
			case LandAction:
				if o.Result == nil {
					return nil
				}
				in, err := decodeLand(o.Operation)
				if err != nil {
					return err
				}
				requested[stream] = append(requested[stream], in)
			case RebaseAction:
				in, err := decodeRebase(o.Operation)
				if err != nil {
					return err
				}
				if o.Result == nil {
					rebasing[stream][in.Unit] = true
				} else {
					rebased[stream][in.Unit] = append(rebased[stream][in.Unit], in.Onto)
				}
			default:
				if in, err := thread.DecodeTurn(o.Operation); err == nil {
					dispatched[stream][in.Agent+"/"+in.Turn] = true
				}
			}
		}
		b, found, err := f.read(stream)
		if err != nil {
			return fmt.Errorf("workstream %s landing: %w", stream, err)
		}
		if found && !scheduler.Paused(state.Pauses, f.cfg.Project.ID, stream) {
			candidates = append(candidates, b)
		}
	}
	settled := true
	for _, b := range candidates {
		current, err := f.refresh(ctx, b, rebasing[b.stream], dispatched[b.stream], rebased[b.stream])
		if err != nil {
			return fmt.Errorf("workstream %s rebase: %w", b.stream, err)
		}
		settled = settled && current
	}
	if !settled {
		return nil
	}
	for _, b := range startOrder(candidates, state.Priorities, f.cfg.Project.ID) {
		for _, u := range dependencyOrder(b.plan) {
			if b.states[trace.UnitSubject(u.ID)].Value != UnitApproved {
				continue
			}
			review, result, ok, err := f.approval(b.stream, u.ID, 0)
			if err != nil {
				return err
			}
			if !ok || slices.ContainsFunc(requested[b.stream], func(in landInput) bool { return in.Unit == u.ID && in.Review == review.Revision }) {
				continue
			}
			if err := f.request(ctx, b.stream, u.ID, review, result); err != nil {
				return fmt.Errorf("workstream %s unit %s landing: %w", b.stream, u.ID, err)
			}
			return nil
		}
	}
	return nil
}

// approval returns the given revision of the unit's review.json, its latest
// when revision is zero, with the review result it records, and whether that
// is a satisfactory verdict.
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, UnitReviewResult, bool, error) {
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return trace.Document{}, UnitReviewResult{}, false, err
	}
	var review trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) && (revision == 0 || d.Revision == revision) {
			review = d
		}
	}
	var result UnitReviewResult
	if review.Revision == 0 || json.Unmarshal([]byte(review.Content), &result) != nil {
		return review, result, false, nil
	}
	return review, result, result.Turn != "" && result.Verdict.Decision == "satisfactory", nil
}

func (f *foreman) header(id string, stream config.WorkstreamID, unit, cause string, at time.Time) trace.Header {
	return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repository.Project(), Workstream: stream, Unit: unit, At: at, Actor: foremanActor, Cause: cause}
}

// request publishes the landing of the approval that review records as a
// durable operation.
func (f *foreman) request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result UnitReviewResult) error {
	subject := landingSubject(unit)
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	transition, event := landIDs(unit, review.Revision)
	candidate := result.Identity.Candidate
	input, err := json.Marshal(landInput{Unit: unit, Review: review.Revision, Candidate: candidate.Revision, Base: candidate.BaseRevision})
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: LandAction, Input: input}
	reason := fmt.Sprintf("unit %s is approved by %s revision %d; landing squashes candidate %s onto %s at %s", unit, review.Path, review.Revision, candidate.Revision, featureBranch(stream), candidate.BaseRevision)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(transition, stream, unit, fmt.Sprintf("%s-%d", reviewDocument(unit), review.Revision), f.s.now()), Subject: subject, From: state.Value, To: fmt.Sprintf("requested-%d", review.Revision), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: LandAction, Body: fmt.Sprintf("Land unit %s", unit), Operation: &op}}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func decodeLand(op coreadapter.Operation) (landInput, error) {
	var in landInput
	if op.Boundary != coreadapter.RepositoryBoundary || op.Action != LandAction {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid land operation input: %w", err)
	}
	if in.Unit == "" || in.Review < 1 || in.Candidate == "" || in.Base == "" {
		return in, errors.New("land operation requires a unit, a positive review revision, a candidate and a base")
	}
	return in, nil
}

// stream returns the workstream a landing operation belongs to: the one whose
// run event derives the operation ID.
func (f *foreman) stream(op coreadapter.Operation, in landInput) (config.WorkstreamID, error) {
	streams, err := f.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := landIDs(in.Unit, in.Review)
	for _, stream := range streams {
		if trace.OperationID(f.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("land operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of a landing: succeeded once it
// recorded the unit merged, failed once its refusal is recorded, nil before
// either.
func (f *foreman) outcome(stream config.WorkstreamID, in landInput) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](f.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := landIDs(in.Unit, in.Review)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-landed":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-refused":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// Inspect reads the recorded transitions and the clone. A recorded outcome
// completes the operation; otherwise it is absent, with evidence of whether
// the feature branch already holds this operation's landing commit, which
// Apply then records without committing again.
func (f *foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeLand(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := f.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := f.outcome(stream, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "landing " + result.Outcome, Result: result}, nil
	}
	branch := featureBranch(stream)
	g, err := featureWorkspaces(f.cfg, f.repository).of(stream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	tip, exists, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if !exists {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("the clone has no feature branch %s", branch)}, nil
	}
	if landed, err := landingCommit(ctx, g, tip, in, op.ID); err != nil {
		return coreadapter.Observation{}, err
	} else if landed {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s, this operation's landing commit on %s; the landing records it without committing again", branch, tip, in.Base)}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("feature branch %s is at %s and holds no commit of this landing; the landing checks its approval before committing", branch, tip)}, nil
}

// landingCommit reports whether tip is the commit the landing operation
// made: its only parent is the approved base and its message names the
// operation.
func landingCommit(ctx context.Context, g workspace.Provider, tip string, in landInput, operation string) (bool, error) {
	if tip == in.Base {
		return false, nil
	}
	c, err := g.Commit(ctx, tip)
	if err != nil {
		return false, err
	}
	return slices.Equal(c.Parents, []string{in.Base}) && slices.Contains(strings.Split(c.Message, "\n"), landingTrailer+": "+operation), nil
}

// Apply lands the approved unit. A feature branch whose tip is this
// operation's commit on the approved base is an interrupted landing: the
// commit is recorded and not made again. Otherwise the approval must still be
// current: the unit approved by this review revision, and its candidate,
// base, report, seal, spec and plan unchanged since. The candidate's tree is
// then committed once on the base with a message derived from the criteria
// the unit addresses, and the feature branch and its workspace move to it.
// One commit then records units/<unit>/landing.json, the unit's move to
// merged and every dependent unit whose dependencies have all merged moving
// to ready. A stale approval is refused with the reason as its result, and
// nothing is committed.
func (f *foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := f.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := f.outcome(stream, in); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	review, result, satisfactory, err := f.approval(stream, in.Unit, in.Review)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !satisfactory || result.Identity.Candidate.Revision != in.Candidate || result.Identity.Candidate.BaseRevision != in.Base {
		return coreadapter.OperationResult{}, fmt.Errorf("%s revision %d does not record the approval of candidate %s from %s", reviewDocument(in.Unit), in.Review, in.Candidate, in.Base)
	}
	requested, err := f.requestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	g, err := featureWorkspaces(f.cfg, f.repository).of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	branch := featureBranch(stream)
	acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: branch})
	if err != nil {
		return coreadapter.OperationResult{}, fmt.Errorf("feature branch %s: %w", branch, err)
	}
	w := acquired.(workspace.Worktree)
	tip, _, err := g.Branch(ctx, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	commit := ""
	if landed, err := landingCommit(ctx, g, tip, in, op.ID); err != nil {
		return coreadapter.OperationResult{}, err
	} else if landed {
		commit = tip
	}
	if commit == "" {
		if reason, err := f.current(ctx, stream, in, result); err != nil {
			return coreadapter.OperationResult{}, err
		} else if reason != "" {
			return f.refuse(ctx, stream, in, reason)
		}
		message, err := f.message(stream, in, review, result, op.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.step("land-committing"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if commit, err = g.Squash(ctx, in.Base, in.Candidate, message, requested); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.step("land-committed"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := g.Advance(ctx, w, in.Base, commit); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := f.s.step("land-advanced"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	return f.record(ctx, stream, in, op.ID, review, result, commit)
}

// requestedAt returns when the operation was asked for, the time the commit
// it makes is stamped with.
func (f *foreman) requestedAt(stream config.WorkstreamID, operation string) (time.Time, error) {
	ops, err := f.repository.Operations(stream)
	if err != nil {
		return time.Time{}, err
	}
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == operation })
	if i < 0 {
		return time.Time{}, fmt.Errorf("operation %s is not recorded", operation)
	}
	return ops[i].Transition.At, nil
}

// current returns why the approval can no longer land, or "" when the
// workstream is building or assembled, the unit is approved by this review
// revision, its candidate holds no stored conflict and every reviewed input
// is current.
func (f *foreman) current(ctx context.Context, stream config.WorkstreamID, in landInput, result UnitReviewResult) (string, error) {
	feature, err := f.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return fmt.Sprintf("the workstream is %s, not building or assembled", featureState(feature.Value)), nil
	}
	state, err := f.repository.Workflow(stream, trace.UnitSubject(in.Unit))
	if err != nil {
		return "", err
	}
	if state.Value != UnitApproved {
		return fmt.Sprintf("the unit is %s, not approved", state.Value), nil
	}
	latest, _, _, err := f.approval(stream, in.Unit, 0)
	if err != nil {
		return "", err
	}
	if latest.Revision != in.Review {
		return fmt.Sprintf("%s revision %d replaced the approval", latest.Path, latest.Revision), nil
	}
	g, err := newUnitWorkspaces(f.cfg, f.repository).of(stream)
	if err != nil {
		return "", err
	}
	if stored, err := g.StoredConflicts(ctx, in.Candidate); err != nil {
		return "", err
	} else if len(stored) != 0 {
		return fmt.Sprintf("candidate %s holds unresolved conflicts in %s", in.Candidate, strings.Join(stored, ", ")), nil
	}
	reason, err := f.staleInputs(ctx, stream, in.Unit, result.Identity)
	if err != nil || reason == "" {
		return "", err
	}
	return "stale approval: " + reason, nil
}

// landingCriteria returns the criteria the unit addresses, in plan order.
func landingCriteria(unit plan.Unit) []string {
	var criteria []string
	for _, a := range unit.Addresses {
		if !slices.Contains(criteria, a.Criterion) {
			criteria = append(criteria, a.Criterion)
		}
	}
	return criteria
}

// message returns the landing commit's message: its subject is the text of
// the criteria the unit addresses, taken from the sealed spec, its body lists
// each criterion, and its trailers name the workstream, unit, candidate, base,
// approval and operation.
func (f *foreman) message(stream config.WorkstreamID, in landInput, review trace.Document, result UnitReviewResult, operation string) (string, error) {
	unit, err := sealedUnit(f.repository, coreadapter.Scope{Workstream: string(stream), Unit: in.Unit})
	if err != nil {
		return "", err
	}
	latest, _, found, err := seal.Latest(f.repository, stream)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("workstream %s has no seal", stream)
	}
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == plan.SpecDocument && d.Revision == latest.Revision.Spec })
	if i < 0 {
		return "", fmt.Errorf("workstream %s has no recorded spec.md revision %d", stream, latest.Revision.Spec)
	}
	return landingMessage(stream, unit, plan.ParseSpec(docs[i].Content), in, fmt.Sprintf("%s revision %d", review.Path, review.Revision), operation), nil
}

// subjectLength is the longest landing commit subject, in runes.
const subjectLength = 72

func landingMessage(stream config.WorkstreamID, unit plan.Unit, spec plan.Spec, in landInput, approval, operation string) string {
	var texts, lines []string
	for _, c := range landingCriteria(unit) {
		text := c
		if n, ok := plan.ParseCitation(c); ok {
			if criterion, ok := spec.Criterion(n); ok {
				text = strings.Join(strings.Fields(criterion.Text), " ")
			}
		}
		texts = append(texts, strings.TrimSuffix(text, "."))
		lines = append(lines, fmt.Sprintf("- %s: %s", c, text))
	}
	subject := []rune(strings.Join(texts, "; "))
	if len(subject) > subjectLength {
		subject = append(subject[:subjectLength-1], '…')
	}
	title := unit.ID
	if unit.Title != "" {
		title += " (" + unit.Title + ")"
	}
	return fmt.Sprintf("%s\n\nUnit %s of workstream %s meets:\n%s\n\nOsmia-Workstream: %s\nOsmia-Unit: %s\nOsmia-Candidate: %s\nOsmia-Base: %s\nOsmia-Approval: %s\n%s: %s\n",
		string(subject), title, stream, strings.Join(lines, "\n"), stream, unit.ID, in.Candidate, in.Base, approval, landingTrailer, operation)
}

// record records, in one commit, the landing document, the unit's move to
// merged with a notice for the chief of staff, the landing's outcome and
// every planned unit whose dependencies have now all merged moving to ready.
// A landing already recorded returns its result and records nothing more.
func (f *foreman) record(ctx context.Context, stream config.WorkstreamID, in landInput, operation string, review trace.Document, result UnitReviewResult, commit string) (coreadapter.OperationResult, error) {
	b, found, err := f.read(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return coreadapter.OperationResult{}, fmt.Errorf("workstream %s is not building or assembled", stream)
	}
	unit, ok := b.plan.Unit(in.Unit)
	if !ok {
		return coreadapter.OperationResult{}, fmt.Errorf("the sealed plan and follow-ups of workstream %s have no unit %s", stream, in.Unit)
	}
	g, err := featureWorkspaces(f.cfg, f.repository).of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	landed, err := g.Commit(ctx, commit)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	identity := result.Identity
	approval := fmt.Sprintf("%s revision %d", review.Path, review.Revision)
	criteria := landingCriteria(unit)
	branch := featureBranch(stream)
	landing := UnitLanding{Unit: in.Unit, Operation: operation, Approval: approval, Turn: result.Turn, Candidate: in.Candidate, Base: in.Base,
		Spec: identity.Candidate.SpecRevision, Plan: identity.Candidate.PlanRevision, Seal: identity.Seal, Criteria: criteria, Branch: branch, Commit: commit, Message: landed.Message}
	content, err := json.MarshalIndent(landing, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs, err := trace.Read[trace.Document](f.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	revision := 1
	for _, d := range docs {
		if d.ID == landingDocument(in.Unit) {
			revision = d.Revision + 1
		}
	}
	at := f.s.now()
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: landingDocument(in.Unit), Revision: revision, Project: f.repository.Project(), Workstream: stream, Unit: in.Unit, At: at, Actor: foremanActor, Cause: operation},
		Path: fmt.Sprintf("units/%s/landing.json", in.Unit), Content: string(content) + "\n"}

	subject := trace.UnitSubject(in.Unit)
	merged := subject + "-" + UnitMerged
	reason := fmt.Sprintf("unit %s landed as %s on %s: %s approved candidate %s from %s, spec %s, plan %s, seal %d; criteria %s",
		in.Unit, commit, branch, approval, in.Candidate, in.Base, identity.Candidate.SpecRevision, identity.Candidate.PlanRevision, identity.Seal, strings.Join(criteria, ", "))
	txs := []trace.Transaction{{ExpectedVersion: b.states[subject].Version,
		Transition: trace.Transition{Header: f.header(merged, stream, in.Unit, operation, at), Subject: subject, From: UnitApproved, To: UnitMerged, Reason: reason},
		Events:     []trace.Event{trace.Notice(merged, "unit", fmt.Sprintf("Unit %s merged: it landed as %s on %s.", in.Unit, commit, branch))}}}
	transition, _ := landIDs(in.Unit, in.Review)
	landingState := b.states[landingSubject(in.Unit)]
	txs = append(txs, trace.Transaction{ExpectedVersion: landingState.Version,
		Transition: trace.Transition{Header: f.header(transition+"-landed", stream, in.Unit, operation, at), Subject: landingSubject(in.Unit), From: landingState.Value, To: fmt.Sprintf("landed-%d", in.Review), Reason: reason}})
	for _, u := range newlyReady(b.plan, b.states, in.Unit) {
		state := b.states[trace.UnitSubject(u.ID)]
		txs = append(txs, trace.Transaction{ExpectedVersion: state.Version,
			Transition: trace.Transition{Header: f.header(trace.UnitSubject(u.ID)+"-"+UnitReady, stream, u.ID, operation, at), Subject: trace.UnitSubject(u.ID), From: UnitPlanned, To: UnitReady,
				Reason: fmt.Sprintf("unit %s is ready: every unit it depends on has merged: %s", u.ID, strings.Join(u.DependsOn, ", "))}})
	}
	if _, err := f.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := f.outcome(stream, in); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	if err := f.s.step("land-recorded"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// newlyReady returns the planned units of the plan, in plan order, that
// depend on the unit that merges and whose every other dependency has
// merged.
func newlyReady(p plan.Plan, states map[string]trace.WorkflowState, merging string) []plan.Unit {
	merged := func(id string) bool { return id == merging || states[trace.UnitSubject(id)].Value == UnitMerged }
	var out []plan.Unit
	for _, u := range p.Units {
		if states[trace.UnitSubject(u.ID)].Value == UnitPlanned && slices.Contains(u.DependsOn, merging) && !slices.ContainsFunc(u.DependsOn, func(d string) bool { return !merged(d) }) {
			out = append(out, u)
		}
	}
	return out
}

// refuse records why the landing was refused, tells the chief of staff, and
// returns the refusal as the operation's result. The unit stays approved and
// the feature branch is unchanged.
func (f *foreman) refuse(ctx context.Context, stream config.WorkstreamID, in landInput, reason string) (coreadapter.OperationResult, error) {
	transition, _ := landIDs(in.Unit, in.Review)
	id := transition + "-refused"
	subject := landingSubject(in.Unit)
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded := fmt.Sprintf("unit %s did not land: %s", in.Unit, reason)
	body := fmt.Sprintf("Unit %s did not land: %s. Nothing was committed to the feature branch; the unit lands once an approval of its current candidate, base, spec and plan is recorded.", in.Unit, reason)
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: f.header(id, stream, in.Unit, transition, f.s.now()), Subject: subject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Review), Reason: recorded},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := f.repository.Transact(ctx, tx); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}
