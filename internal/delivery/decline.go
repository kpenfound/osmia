package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// DeclinePath and DeclineDocument are the workstream document that records the
// owner declining the evaluator-only pull request of a climb that kept no
// attempt.
const (
	DeclinePath     = "final/decline.json"
	DeclineDocument = "final-decline"
	declineID       = "decline"
)

// Decline is final/decline.json.
type Decline struct {
	Review         int    `json:"review"`
	ReviewRevision int    `json:"review_revision"`
	Commit         string `json:"commit"`
	Note           string `json:"note,omitempty"`
	PullRequest    int    `json:"pull_request,omitempty"`
}

// DeclinedPullRequestComment is the comment the service closes the declined
// evaluator-only pull request with.
func DeclinedPullRequestComment() string {
	return "The climb kept no attempt, so this pull request held only the evaluator, and the owner declined it. The workstream ended as concluded, no improvement. The branch is kept."
}

// ReadDecline returns the recorded decline of the evaluator-only pull request,
// and whether the owner declined it.
func ReadDecline(repository *trace.Repository, stream config.WorkstreamID) (Decline, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Decline{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == DeclineDocument && d.Revision >= latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return Decline{}, false, nil
	}
	var d Decline
	if err := json.Unmarshal([]byte(latest.Content), &d); err != nil {
		return Decline{}, false, fmt.Errorf("%s revision %d: %w", latest.Path, latest.Revision, err)
	}
	return d, true, nil
}

// OffersDecline reports whether the workstream's delivery offers the owner the
// choice to decline: a Hill Climb workstream whose conclusion kept no attempt,
// so that its pull request holds only the evaluator.
func OffersDecline(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	planned, err := repository.Planned(stream)
	if err != nil || planned {
		return false, err
	}
	rec, found, err := climb.ReadConclusion(repository, stream)
	return found && rec.Concluded() && rec.KeptNone(), err
}

// DeclineDelivery records the owner's decision to decline the pull request that
// holds only the evaluator of a Hill Climb workstream that kept no attempt. The
// workstream ends as concluded, no improvement: a terminal state that is not
// an abandonment and that archives like a delivered workstream. The decision
// pins the final review the owner read. Declining twice is the same decision.
func (s *Controller) DeclineDelivery(ctx context.Context, raw string, req api.DeliveryDecline) (api.DeliveryDeclined, *APIError) {
	project, stream, repository, apiErr := s.ConversationTrace(raw)
	if apiErr != nil {
		return api.DeliveryDeclined{}, apiErr
	}
	offered, err := OffersDecline(repository, stream)
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read the climb's conclusion"}
	}
	if !offered {
		return api.DeliveryDeclined{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no evaluator-only pull request to decline: only a Hill Climb workstream that kept no attempt offers one, and a workstream the owner does not want is abandoned", stream)}
	}
	state, err := repository.RawWorkflow(stream, trace.FeatureSubject)
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read feature state"}
	}
	if state.Value == intake.ConcludedState {
		recorded, found, err := ReadDecline(repository, stream)
		if err != nil || !found {
			return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read the recorded decline"}
		}
		return api.DeliveryDeclined{Project: project, Workstream: stream, State: intake.ConcludedState, Review: recorded.Review, Commit: recorded.Commit, PullRequest: recorded.PullRequest, PullRequestClosed: s.pullRequestClosed(ctx, repository, stream, recorded.PullRequest)}, nil
	}
	if state.Value != AssembledState {
		return api.DeliveryDeclined{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s, not assembled, so there is no pull request to decline yet", stream, state.Value)}
	}
	report, reason, err := s.finalGate(ctx, repository, stream)
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read final review"}
	}
	if reason != "" {
		return api.DeliveryDeclined{}, &APIError{Code: Conflict, Message: reason}
	}
	review, _, _, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
	}
	if req.Review != report.Review || req.ReviewRevision != review.Revision || req.Commit != report.Commit {
		return api.DeliveryDeclined{}, &APIError{Code: Conflict, Message: "the final report changed; read the current presentation"}
	}
	number := 0
	if publication, found, err := CurrentOwnPublication(repository, stream); err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read publication records"}
	} else if found {
		number = publication.PullRequest
	}
	at := s.Now()
	recorded := Decline{Review: report.Review, ReviewRevision: review.Revision, Commit: report.Commit, Note: strings.TrimSpace(req.Note), PullRequest: number}
	content, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot encode the decline"}
	}
	revision, err := intake.NextRevision(repository, stream, DeclineDocument)
	if err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DeclineDocument, Revision: revision, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: declineID},
		Path: DeclinePath, Content: string(content) + "\n"}
	why := fmt.Sprintf("the climb kept no attempt, so its pull request holds only the evaluator, and the owner declined it after final review %d of %s: concluded, no improvement", report.Review, report.Commit)
	body := "Workstream state changed from " + state.Value + " to " + intake.ConcludedState + ": " + why
	res, err := s.Router.Route(ctx, intake.PinnedMove{
		Repo: repository, Stream: stream, Subject: trace.FeatureSubject, State: state,
		Trigger: intake.TriggerDeclined, Cause: declineID, Target: intake.ConcludedState, Description: "Conclude the workstream with no improvement: the owner declined the evaluator-only pull request.", Gate: true,
		Ref: declineID, ID: declineID, HeaderCause: DeclineDocument, Reason: why, Actor: ownerActor, At: at,
		Events:    []trace.Event{trace.Progress(declineID, "state", body)},
		Documents: []trace.Document{doc},
	}.Exit())
	if apiErr := intake.RouteError(res, err, "the decline of the evaluator-only pull request"); apiErr != nil {
		return api.DeliveryDeclined{}, apiErr
	}
	if err := s.Step("decline-recorded"); err != nil {
		return api.DeliveryDeclined{}, &APIError{Code: Internal, Message: err.Error()}
	}
	return api.DeliveryDeclined{Project: project, Workstream: stream, State: intake.ConcludedState, Review: report.Review, Commit: report.Commit, PullRequest: number, PullRequestClosed: s.pullRequestClosed(ctx, repository, stream, number)}, nil
}

// pullRequestClosed closes the declined evaluator-only pull request with a
// comment, once, and reports whether it is closed. A host that cannot close it
// leaves the draft open for the owner, and the decision stands.
func (s *Controller) pullRequestClosed(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, number int) bool {
	if number == 0 || s.PullRequests == nil || !pulls.Credentialed(s.PullRequests) {
		return false
	}
	publication, found, err := CurrentOwnPublication(repository, stream)
	if err != nil || !found || publication.PullRequest != number {
		return false
	}
	closer, ok := s.PullRequests.(pulls.LandedClient)
	if !ok {
		return false
	}
	if outcome, revision, err := OwnPullRequestOutcomeAt(repository, stream); err == nil && revision > 0 && outcome.PullRequest == number {
		return outcome.Outcome == OwnPullRequestOutcomeClosed
	}
	if _, err := closer.CloseWithComment(ctx, publication.Upstream, number, DeclinedPullRequestComment()); err != nil {
		return false
	}
	if err := s.Step("decline-pr-closed"); err != nil {
		return false
	}
	return RecordOwnPullRequestOutcome(ctx, repository, s.Now(), stream, declineID, number, OwnPullRequestOutcomeClosed) == nil
}
