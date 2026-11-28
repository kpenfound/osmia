package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/workspace"
)

// DeliveryPresentation is the final review, its recorded check result and
// the title and description presented to the owner for marking the
// workstream's draft pull request ready for review. Criteria with a gap
// precede criteria with evidence. A delivered workstream presents the
// report, approval and publication it was delivered with, and no draft.
type DeliveryPresentation struct {
	Project        config.ProjectID            `json:"project"`
	Workstream     config.WorkstreamID         `json:"workstream"`
	Report         FinalReport                 `json:"report"`
	ReviewRevision int                         `json:"review_revision"`
	Title          string                      `json:"title,omitempty"`
	Draft          string                      `json:"draft"`
	DraftHash      string                      `json:"draft_hash"`
	Messages       []workspace.DeliveryMessage `json:"messages"`
	Style          string                      `json:"style"`
	Approval       *DeliveryApproval           `json:"approval,omitempty"`
	// Publication is the latest record of the push that put the branch on
	// the pull request, Ready the record of the pull request being marked
	// ready for review, Merge the owner's own merge of the branch into
	// upstream that delivered it instead, Landing the service's own landing of
	// the signed commit on the base branch that delivered it instead of a pull
	// request merge (Merge is then not set), and Delivered whether the
	// workstream is delivered.
	Publication *DeliveryPublication `json:"publication,omitempty"`
	Ready       *DeliveryReady       `json:"ready,omitempty"`
	Merge       *DeliveryMerge       `json:"merge,omitempty"`
	Landing     *DeliveryLanding     `json:"landing,omitempty"`
	Delivered   bool                 `json:"delivered,omitempty"`
	Maintenance bool                 `json:"maintenance,omitempty"`
	// DraftPullRequest is true when approving leaves the pull request a
	// draft because an upstream merge-order dependency has not merged: the
	// drafted description states the dependency. It informs the owner and
	// changes no approval rule.
	DraftPullRequest bool `json:"draft_pull_request,omitempty"`
	// Checks is the recorded check result of the presented final review's
	// commit.
	Checks *DeliveryChecks `json:"checks,omitempty"`
	// Brief is the delivery brief of the presented final review, and only of
	// that review: a brief recorded for an earlier review is never shown. It
	// is omitted for a delivered workstream with no brief recorded.
	Brief *DeliveryBrief `json:"brief,omitempty"`
	// CanLand is true when the owner may approve and land the workstream: its
	// project's delivery setting is "jev", so the service can fast-forward the
	// base branch to the pushed commit. It is omitted for a project that
	// delivers by pull request and for a delivered workstream.
	CanLand bool `json:"can_land,omitempty" description:"True when the owner may approve and land the workstream: its project's delivery is jev and the workstream is assembled. Landing is still refused when checks did not pass on the pushed commit. Absent for a project that delivers by pull request or a delivered workstream."`
	// Recommendation is the route Jev recommends when it picked one without
	// the authority to apply it: DeliveryRouteLand or "pull-request".
	// It is empty when Jev made no pick.
	Recommendation string `json:"recommendation,omitempty" description:"The route land or pull-request that Jev picked below the decision threshold or without applying it, from its delivery-route decision. Empty when Jev made no pick."`
}

// DeliveryRouteLand is the route of an approval that lands the workstream on
// the project's base branch. An approval with no route, or the route
// "pull-request", marks the draft pull request ready for review.
const DeliveryRouteLand = "land"

// The states of the check result a delivery presentation shows.
const (
	ChecksStatePassed     = "passed"
	ChecksStateFailed     = "failed"
	ChecksStateIncomplete = "incomplete"
	ChecksStateNone       = "none"
)

// DeliveryChecks is the check result recorded for the commit of the presented
// final review. State is ChecksStateNone when no result is recorded for that
// commit, and Summary says it in one line. Failed lists the failed checks and
// Flaky those that failed and then passed on their rerun; Error says why an
// incomplete run reported no result.
type DeliveryChecks struct {
	State  string `json:"state"`
	Commit string `json:"commit,omitempty"`
	// Summary is the result in one sentence.
	Summary string   `json:"summary"`
	Failed  []string `json:"failed,omitempty"`
	Flaky   []string `json:"flaky,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// The sources of a DeliveryReady record: the owner's approval of the
// delivery, or the owner marking the draft ready on the pull request host
// directly.
const (
	ReadyByApproval = "approval"
	ReadyOnHost     = "host"
)

// DeliveryReady records that the workstream was delivered by the decision to
// mark its pull request ready for review. Source says whether that followed
// the owner's approval (Approval is then the revision of final/delivery.json)
// or the owner's own action on the pull request host, which was noticed
// afterwards. Draft is true when the approval left the pull request a draft
// because an upstream merge-order dependency has not merged.
type DeliveryReady struct {
	Source      string    `json:"source"`
	Approval    int       `json:"approval,omitempty"`
	Draft       bool      `json:"draft,omitempty"`
	PullRequest int       `json:"pull_request"`
	URL         string    `json:"url,omitempty"`
	Upstream    string    `json:"upstream"`
	Commit      string    `json:"commit"`
	At          time.Time `json:"at"`
}

// The states of a delivery brief: BriefWaiting while the manager's turn
// for the final review's notice has not ended and no brief is recorded,
// BriefRecorded once one is, and BriefNoneRecorded when that turn ended
// without one, no manager turn can run, or delivery was approved without one.
const (
	BriefWaiting      = "waiting"
	BriefRecorded     = "recorded"
	BriefNoneRecorded = "none_recorded"
)

// NoBriefRecorded is what the delivery card and the CLI say for a final review
// whose brief state is BriefNoneRecorded.
const NoBriefRecorded = "No brief was recorded for this review"

// DeliveryBrief is the short brief the manager recorded for one final
// review. Review, ReviewRevision and Commit identify the review it covers.
// Screenshots are the IDs of images the preview tool captured at that commit,
// readable at the images endpoint of the workstream they were captured for.
// Text, BeforeYouShip and Screenshots are empty unless State is BriefRecorded.
type DeliveryBrief struct {
	State          string   `json:"state"`
	Review         int      `json:"review"`
	ReviewRevision int      `json:"review_revision"`
	Commit         string   `json:"commit"`
	Text           string   `json:"text,omitempty"`
	BeforeYouShip  []string `json:"before_you_ship,omitempty"`
	Screenshots    []string `json:"screenshots,omitempty"`
}

// DeliveryApproval preserves the exact title and text and reviewed identity
// the owner approved when marking the pull request ready for review.
// DescriptionHash allows publication to check the text it will send.
type DeliveryApproval struct {
	Review          int                         `json:"review"`
	ReviewRevision  int                         `json:"review_revision"`
	Commit          string                      `json:"commit"`
	Seal            int                         `json:"seal"`
	SpecHash        string                      `json:"spec_hash"`
	Spec            int                         `json:"spec"`
	Plan            int                         `json:"plan"`
	Charter         int                         `json:"charter"`
	DraftHash       string                      `json:"draft_hash"`
	Title           string                      `json:"title,omitempty"`
	Description     string                      `json:"description"`
	DescriptionHash string                      `json:"description_hash"`
	At              time.Time                   `json:"at"`
	Messages        []workspace.DeliveryMessage `json:"messages"`
	Style           string                      `json:"style"`
	// AcceptedBlocking is what the final report found blocking that the
	// owner accepted as it is by approving.
	AcceptedBlocking []string `json:"accepted_blocking,omitempty"`
	// DecidedBy names who the approval's authority is when it is not the owner
	// who marked the pull request ready: "jev", with Judgment naming the
	// judgment it rests on, when Jev decided the delivery at the owner
	// threshold or by a gate decision. The service records such an approval of
	// the drafted title and description, and the publication it asks for reads
	// the authority here. Empty is the owner.
	DecidedBy string `json:"decided_by,omitempty"`
	Judgment  string `json:"judgment,omitempty"`
	// Route is what the approval authorises: DeliveryRouteLand when the owner
	// approved landing the workstream on the base branch, and empty or
	// "pull-request" when it marks the draft pull request ready.
	Route string `json:"route,omitempty"`
}

// DeliveryDecision marks the draft pull request ready as shown, or supplies the
// owner's edit of its description. The review identity and draft hash are
// required even when the description is edited. Messages, when supplied, must
// be the presented messages: the commits were signed and pushed with them
// before the decision.
type DeliveryDecision struct {
	Review         int                         `json:"review"`
	ReviewRevision int                         `json:"review_revision"`
	Commit         string                      `json:"commit"`
	DraftHash      string                      `json:"draft_hash"`
	Description    *string                     `json:"description,omitempty"`
	Messages       []workspace.DeliveryMessage `json:"messages,omitempty"`
	// AcceptBlocking accepts what the final report found blocking as it is;
	// a report with blocking findings is approved only with it.
	AcceptBlocking bool `json:"accept_blocking,omitempty"`
}

// DeliveryLandDecision approves landing the presented workstream on its base
// branch. The review identity and draft hash pin what the owner read, as a
// DeliveryDecision does.
type DeliveryLandDecision struct {
	Review         int    `json:"review"`
	ReviewRevision int    `json:"review_revision"`
	Commit         string `json:"commit"`
	DraftHash      string `json:"draft_hash"`
	// AcceptBlocking accepts what the final report found blocking as it is;
	// a report with blocking findings lands only with it.
	AcceptBlocking bool `json:"accept_blocking,omitempty"`
}

// The outcomes DeliveryLandResponse reports.
const (
	// LandOutcomeLanded: the base branch is at the pushed commit and the
	// workstream is delivered.
	LandOutcomeLanded = "landed"
	// LandOutcomeHeld: the router holds the landing for now. Nothing was
	// pushed, and the landing is applied once the hold clears.
	LandOutcomeHeld = "held"
	// LandOutcomeFellBack: the upstream refused the push, so the draft pull
	// request was marked ready and the workstream delivered.
	LandOutcomeFellBack = "fell-back"
	// LandOutcomeBaseMoved: the upstream base branch moved, so nothing landed.
	// The workstream is rebased, checked and reviewed again, and the service
	// lands the new commit under the same decision.
	LandOutcomeBaseMoved = "base-moved"
	// LandOutcomePending: the outcome of the push is not known yet. The service
	// settles it against the fetched base branch.
	LandOutcomePending = "pending"
)

// DeliveryLandResponse reports what approving the landing did. Approval is
// the owner approval recorded for it, Outcome one of the LandOutcome values and
// Reason says why when the workstream did not land. Commit is the signed
// commit the landing moves the base branch to.
type DeliveryLandResponse struct {
	Approval DeliveryApproval `json:"approval"`
	Outcome  string           `json:"outcome" enum:"landed,held,fell-back,base-moved,pending" description:"landed: the base branch is at the pushed commit and the workstream is delivered. held: the router holds the landing and nothing was pushed; it is applied once the hold clears. fell-back: the upstream refused the push, so the draft pull request was marked ready and the workstream delivered. base-moved: the base branch moved, so nothing landed; the workstream is rebased, checked and reviewed again and the service lands the new commit on its own under the same decision, with no second approval. pending: the push outcome is unknown and is settled against the fetched base branch."`
	Reason   string           `json:"reason,omitempty" description:"Why the workstream did not land, when it did not."`
	Commit   string           `json:"commit,omitempty" description:"The signed commit the landing moves the base branch to."`
}

// DeliveryDecline is the owner's decision to decline the pull request that
// holds only the evaluator of a Hill Climb workstream that kept no attempt. The
// review identity pins what the owner read, as a DeliveryDecision does.
type DeliveryDecline struct {
	Review         int    `json:"review"`
	ReviewRevision int    `json:"review_revision"`
	Commit         string `json:"commit"`
	Note           string `json:"note,omitempty" description:"Why the pull request is declined."`
}

// DeliveryDeclined reports a declined evaluator-only pull request: the
// workstream ended as concluded, no improvement, which is terminal and is not
// an abandonment. PullRequest is the draft pull request the climb opened, and
// PullRequestClosed says whether the service closed it with a comment.
type DeliveryDeclined struct {
	Project           config.ProjectID    `json:"project"`
	Workstream        config.WorkstreamID `json:"workstream"`
	State             string              `json:"state" enum:"concluded"`
	Review            int                 `json:"review"`
	Commit            string              `json:"commit"`
	PullRequest       int                 `json:"pull_request,omitempty"`
	PullRequestClosed bool                `json:"pull_request_closed"`
}
