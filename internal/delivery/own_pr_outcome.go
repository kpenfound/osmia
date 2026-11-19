package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// OwnPullRequestOutcomeDocument is the own-PR-outcome document's record ID,
// stored like the base observation: one document, read backward for its
// latest revision, keyed to the workstream and to the pull request number it
// observed.
const OwnPullRequestOutcomeDocument = "own-pull-request-outcome"

// OwnPullRequestOutcomePath is the workstream document path of the own-PR
// outcome.
const OwnPullRequestOutcomePath = "own-pull-request-outcome.json"

// OwnPullRequestOutcomeMerged and OwnPullRequestOutcomeClosed are the two
// recordable outcomes of a workstream's own pull request. It does not add a
// value to DeliveryPublication.Status, whose meaning stays unchanged.
const (
	OwnPullRequestOutcomeMerged = "merged"
	OwnPullRequestOutcomeClosed = "closed"
)

// OwnPullRequestOutcome records that pull request PullRequest of a
// workstream's own publication was observed Outcome, merged or closed.
type OwnPullRequestOutcome struct {
	PullRequest int    `json:"pull_request"`
	Outcome     string `json:"outcome"`
	MergeCommit string `json:"merge_commit,omitempty"`
}

// OwnPullRequestOutcomeAt reads the latest recorded own-PR outcome of
// stream, and the document revision it holds, 0 when none is recorded yet.
func OwnPullRequestOutcomeAt(repo *trace.Repository, stream config.WorkstreamID) (OwnPullRequestOutcome, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return OwnPullRequestOutcome{}, 0, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == OwnPullRequestOutcomeDocument {
			var o OwnPullRequestOutcome
			err := json.Unmarshal([]byte(d.Content), &o)
			return o, d.Revision, err
		}
	}
	return OwnPullRequestOutcome{}, 0, nil
}

// CurrentOwnPublication returns stream's latest publication record that has
// an open pull request of its own, and whether one exists. A workstream
// delivered by the owner merging its branch into upstream outside the
// factory has no such record.
func CurrentOwnPublication(repo *trace.Repository, stream config.WorkstreamID) (DeliveryPublication, bool, error) {
	records, err := Publications(repo, stream)
	if err != nil {
		return DeliveryPublication{}, false, err
	}
	var current DeliveryPublication
	found := false
	for _, p := range records {
		if p.Status == PublicationOpened && p.PullRequest > 0 {
			current, found = p, true
		}
	}
	return current, found, nil
}

// OwnPullRequestFinished reports whether stream's current publication's
// pull request is durably recorded merged or closed, or the workstream landed
// on its base branch, which leaves the pull request nothing to deliver. It is
// false when the recorded outcome names an earlier pull request than the
// current publication's, or the workstream has no pull request of its own.
func OwnPullRequestFinished(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	current, found, err := CurrentOwnPublication(repo, stream)
	if err != nil || !found {
		return false, err
	}
	if _, landed, err := ReadDeliveryLanding(repo, stream); err != nil || landed {
		return landed, err
	}
	outcome, revision, err := OwnPullRequestOutcomeAt(repo, stream)
	if err != nil || revision == 0 {
		return false, err
	}
	return outcome.PullRequest == current.PullRequest && (outcome.Outcome == OwnPullRequestOutcomeMerged || outcome.Outcome == OwnPullRequestOutcomeClosed), nil
}

// OwnPullRequestOutcomeDocumentFor builds the next revision of the own-PR
// outcome document, written exactly like the base observation: one JSON
// revision, read backward by record ID. It returns ok=false when the
// recorded outcome already matches pr and outcome, so recording stays
// idempotent across a restart between the lookup and the record (charter#2).
func OwnPullRequestOutcomeDocumentFor(repo *trace.Repository, stream config.WorkstreamID, at time.Time, cause string, pr int, outcome string, mergeCommit ...string) (trace.Document, bool, error) {
	prior, revision, err := OwnPullRequestOutcomeAt(repo, stream)
	if err != nil {
		return trace.Document{}, false, err
	}
	next := OwnPullRequestOutcome{PullRequest: pr, Outcome: outcome}
	if len(mergeCommit) > 0 {
		next.MergeCommit = mergeCommit[0]
	} else if prior.PullRequest == pr && prior.Outcome == outcome {
		next.MergeCommit = prior.MergeCommit
	}
	if revision > 0 && prior == next {
		return trace.Document{}, false, nil
	}
	data, err := json.Marshal(next)
	if err != nil {
		return trace.Document{}, false, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: OwnPullRequestOutcomeDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: cause}
	return trace.Document{Header: h, Path: OwnPullRequestOutcomePath, Content: string(data)}, true, nil
}

// PullRequestOutcomeOf reports the outcome a pull request's current state
// records, and whether it is finished: merged or closed. An open pull
// request reports ok=false.
func PullRequestOutcomeOf(pr pulls.PullRequest) (string, bool) {
	switch {
	case pr.Merged:
		return OwnPullRequestOutcomeMerged, true
	case pr.State == "closed":
		return OwnPullRequestOutcomeClosed, true
	default:
		return "", false
	}
}

// RecordOwnPullRequestOutcome durably records, idempotently, that pull
// request number of stream was observed outcome. It is used where an
// existing operation already looked the pull request up and found it
// merged or closed, so it adds no PullRequests call of its own.
func RecordOwnPullRequestOutcome(ctx context.Context, repo *trace.Repository, at time.Time, stream config.WorkstreamID, cause string, number int, outcome string) error {
	doc, ok, err := OwnPullRequestOutcomeDocumentFor(repo, stream, at, cause, number, outcome)
	if err != nil || !ok {
		return err
	}
	return repo.RecordDocuments(ctx, []trace.Document{doc})
}

// OwnPullRequestNumber returns the pull request number a publish or
// retarget operation is maintaining as the workstream's own, or 0 when the
// operation is opening the workstream's first pull request.
func OwnPullRequestNumber(in PublishInput, prior []DeliveryPublication) int {
	if in.Retarget != nil {
		return in.Retarget.Number
	}
	number := 0
	for _, d := range prior {
		if d.Status == PublicationOpened && d.PullRequest > 0 {
			number = d.PullRequest
		}
	}
	return number
}

// RecordOwnPullRequestOutcomeFromFind records the own-PR outcome when
// found, already fetched by an existing lookup of the workstream's own pull
// request, shows pull request number merged or closed. It makes no
// PullRequests call of its own.
func RecordOwnPullRequestOutcomeFromFind(ctx context.Context, repo *trace.Repository, at time.Time, stream config.WorkstreamID, cause string, number int, found []pulls.PullRequest) error {
	if number <= 0 {
		return nil
	}
	for _, pr := range found {
		if pr.Number != number {
			continue
		}
		if outcome, finished := PullRequestOutcomeOf(pr); finished {
			return RecordOwnPullRequestOutcome(ctx, repo, at, stream, cause, number, outcome)
		}
		return nil
	}
	return nil
}

// LandedPullRequestCause is the cause of the own-PR outcome recorded for a
// landed workstream's pull request.
const LandedPullRequestCause = "landed-pull-request"

// LandedPullRequest returns the own pull request of a landed workstream whose
// outcome is not recorded yet, with the landing it is left over from, and
// whether there is one.
func LandedPullRequest(repo *trace.Repository, stream config.WorkstreamID) (DeliveryPublication, DeliveryLanding, bool, error) {
	landing, landed, err := ReadDeliveryLanding(repo, stream)
	if err != nil || !landed {
		return DeliveryPublication{}, DeliveryLanding{}, false, err
	}
	publication, found, err := CurrentOwnPublication(repo, stream)
	if err != nil || !found {
		return DeliveryPublication{}, DeliveryLanding{}, false, err
	}
	outcome, revision, err := OwnPullRequestOutcomeAt(repo, stream)
	if err != nil {
		return DeliveryPublication{}, DeliveryLanding{}, false, err
	}
	if revision > 0 && outcome.PullRequest == publication.PullRequest && (outcome.Outcome == OwnPullRequestOutcomeMerged || outcome.Outcome == OwnPullRequestOutcomeClosed) {
		return DeliveryPublication{}, DeliveryLanding{}, false, nil
	}
	return publication, landing, true, nil
}

// LandedPullRequestComment is the comment that closes a landed workstream's
// pull request: it names the landed commit and the base branch it landed on.
func LandedPullRequestComment(landing DeliveryLanding) string {
	return fmt.Sprintf("Osmia landed this change directly on the base branch: commit %s is now at %s of %s by a fast-forward, so there is nothing left to merge here. Closing this pull request; the branch %s is kept.", landing.Commit, landing.BaseBranch, landing.Upstream, landing.Branch)
}

// SettleLandedPullRequest looks up the own pull request of a landed workstream
// and closes it with a comment naming the landed commit and base branch while it
// is still open, then records the outcome so that no later poll or restart
// looks at it or comments again. A pull request the host already shows merged
// or closed is left alone and only its outcome is recorded: the landing's
// delivered and resolved state stands either way. It reports whether it closed
// the pull request. A lookup that fails supplies no evidence and is retried on
// the next poll. step is a fault-injection point called before and after the
// close.
func SettleLandedPullRequest(ctx context.Context, client pulls.Client, repo *trace.Repository, at time.Time, stream config.WorkstreamID, step func(string) error) (bool, error) {
	publication, landing, pending, err := LandedPullRequest(repo, stream)
	if err != nil || !pending {
		return false, err
	}
	if step == nil {
		step = func(string) error { return nil }
	}
	found, err := client.Find(ctx, publication.Upstream, publication.Fork, publication.Branch)
	if err != nil {
		return false, ctx.Err()
	}
	for _, pr := range found {
		if pr.Number != publication.PullRequest {
			continue
		}
		if outcome, finished := PullRequestOutcomeOf(pr); finished {
			return false, recordLandedOutcome(ctx, repo, at, stream, pr.Number, outcome, pr.MergeCommit)
		}
		closer, ok := client.(pulls.LandedClient)
		if !ok {
			return false, nil
		}
		if err := step("landed-pr-found"); err != nil {
			return false, err
		}
		if _, err := closer.CloseWithComment(ctx, publication.Upstream, pr.Number, LandedPullRequestComment(landing)); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// The close may have reached the host before the response was lost: the
			// next poll finds it closed and records that.
			return false, nil
		}
		if err := step("landed-pr-closed"); err != nil {
			return false, err
		}
		return true, recordLandedOutcome(ctx, repo, at, stream, pr.Number, OwnPullRequestOutcomeClosed, "")
	}
	return false, nil
}

func recordLandedOutcome(ctx context.Context, repo *trace.Repository, at time.Time, stream config.WorkstreamID, number int, outcome, mergeCommit string) error {
	var doc trace.Document
	var ok bool
	var err error
	if mergeCommit != "" {
		doc, ok, err = OwnPullRequestOutcomeDocumentFor(repo, stream, at, LandedPullRequestCause, number, outcome, mergeCommit)
	} else {
		doc, ok, err = OwnPullRequestOutcomeDocumentFor(repo, stream, at, LandedPullRequestCause, number, outcome)
	}
	if err != nil || !ok {
		return err
	}
	return repo.RecordDocuments(ctx, []trace.Document{doc})
}
