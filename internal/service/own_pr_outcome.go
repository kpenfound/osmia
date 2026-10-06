package service

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownPullRequestOutcomeDocument is the own-PR-outcome document's record ID,
// stored like the base observation: one document, read backward for its
// latest revision, keyed to the workstream and to the pull request number it
// observed.
const ownPullRequestOutcomeDocument = "own-pull-request-outcome"

// ownPullRequestOutcomePath is the workstream document path of the own-PR
// outcome.
const ownPullRequestOutcomePath = "own-pull-request-outcome.json"

// ownPullRequestOutcomeMerged and ownPullRequestOutcomeClosed are the two
// recordable outcomes of a workstream's own pull request. It does not add a
// value to DeliveryPublication.Status, whose meaning stays unchanged.
const (
	ownPullRequestOutcomeMerged = "merged"
	ownPullRequestOutcomeClosed = "closed"
)

// ownPullRequestOutcome records that pull request PullRequest of a
// workstream's own publication was observed Outcome, merged or closed.
type ownPullRequestOutcome struct {
	PullRequest int    `json:"pull_request"`
	Outcome     string `json:"outcome"`
}

// ownPullRequestOutcomeAt reads the latest recorded own-PR outcome of
// stream, and the document revision it holds, 0 when none is recorded yet.
func ownPullRequestOutcomeAt(repo *trace.Repository, stream config.WorkstreamID) (ownPullRequestOutcome, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return ownPullRequestOutcome{}, 0, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == ownPullRequestOutcomeDocument {
			var o ownPullRequestOutcome
			err := json.Unmarshal([]byte(d.Content), &o)
			return o, d.Revision, err
		}
	}
	return ownPullRequestOutcome{}, 0, nil
}

// currentOwnPublication returns stream's latest publication record that has
// an open pull request of its own, and whether one exists. A workstream
// delivered by the owner merging its branch into upstream outside the
// factory has no such record.
func currentOwnPublication(repo *trace.Repository, stream config.WorkstreamID) (DeliveryPublication, bool, error) {
	records, err := publications(repo, stream)
	if err != nil {
		return DeliveryPublication{}, false, err
	}
	var current DeliveryPublication
	found := false
	for _, p := range records {
		if p.Status == publicationOpened && p.PullRequest > 0 {
			current, found = p, true
		}
	}
	return current, found, nil
}

// ownPullRequestFinished reports whether stream's current publication's
// pull request is durably recorded merged or closed. It is false when the
// recorded outcome names an earlier pull request than the current
// publication's, or the workstream has no pull request of its own.
func ownPullRequestFinished(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	current, found, err := currentOwnPublication(repo, stream)
	if err != nil || !found {
		return false, err
	}
	outcome, revision, err := ownPullRequestOutcomeAt(repo, stream)
	if err != nil || revision == 0 {
		return false, err
	}
	return outcome.PullRequest == current.PullRequest, nil
}

// ownPullRequestOutcomeDocumentFor builds the next revision of the own-PR
// outcome document, written exactly like the base observation: one JSON
// revision, read backward by record ID. It returns ok=false when the
// recorded outcome already matches pr and outcome, so recording stays
// idempotent across a restart between the lookup and the record (charter#2).
func ownPullRequestOutcomeDocumentFor(repo *trace.Repository, stream config.WorkstreamID, at time.Time, cause string, pr int, outcome string) (trace.Document, bool, error) {
	prior, revision, err := ownPullRequestOutcomeAt(repo, stream)
	if err != nil {
		return trace.Document{}, false, err
	}
	next := ownPullRequestOutcome{PullRequest: pr, Outcome: outcome}
	if revision > 0 && prior == next {
		return trace.Document{}, false, nil
	}
	data, err := json.Marshal(next)
	if err != nil {
		return trace.Document{}, false, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ownPullRequestOutcomeDocument, Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: cause}
	return trace.Document{Header: h, Path: ownPullRequestOutcomePath, Content: string(data)}, true, nil
}

// pullRequestOutcomeOf reports the outcome a pull request's current state
// records, and whether it is finished: merged or closed. An open pull
// request reports ok=false.
func pullRequestOutcomeOf(pr pulls.PullRequest) (string, bool) {
	switch {
	case pr.Merged:
		return ownPullRequestOutcomeMerged, true
	case pr.State == "closed":
		return ownPullRequestOutcomeClosed, true
	default:
		return "", false
	}
}

// recordOwnPullRequestOutcome durably records, idempotently, that pull
// request number of stream was observed outcome. It is used where an
// existing operation already looked the pull request up and found it
// merged or closed, so it adds no PullRequests call of its own.
func recordOwnPullRequestOutcome(ctx context.Context, repo *trace.Repository, at time.Time, stream config.WorkstreamID, cause string, number int, outcome string) error {
	doc, ok, err := ownPullRequestOutcomeDocumentFor(repo, stream, at, cause, number, outcome)
	if err != nil || !ok {
		return err
	}
	return repo.RecordDocuments(ctx, []trace.Document{doc})
}

// ownPullRequestNumber returns the pull request number a publish or
// retarget operation is maintaining as the workstream's own, or 0 when the
// operation is opening the workstream's first pull request.
func ownPullRequestNumber(in publishInput, prior []DeliveryPublication) int {
	if in.Retarget != nil {
		return in.Retarget.Number
	}
	number := 0
	for _, d := range prior {
		if d.Status == publicationOpened && d.PullRequest > 0 {
			number = d.PullRequest
		}
	}
	return number
}

// recordOwnPullRequestOutcomeFromFind records the own-PR outcome when
// found, already fetched by an existing lookup of the workstream's own pull
// request, shows pull request number merged or closed. It makes no
// PullRequests call of its own.
func recordOwnPullRequestOutcomeFromFind(ctx context.Context, repo *trace.Repository, at time.Time, stream config.WorkstreamID, cause string, number int, found []pulls.PullRequest) error {
	if number <= 0 {
		return nil
	}
	for _, pr := range found {
		if pr.Number != number {
			continue
		}
		if outcome, finished := pullRequestOutcomeOf(pr); finished {
			return recordOwnPullRequestOutcome(ctx, repo, at, stream, cause, number, outcome)
		}
		return nil
	}
	return nil
}
