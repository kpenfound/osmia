package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// ReadyPath is the workstream document that records the decision that
	// delivered the workstream: its pull request marked ready for review, with
	// its trace record ID.
	ReadyPath     = "final/ready.json"
	ReadyDocument = "final-ready"
	// ReadyCause is the cause of the transitions recorded when the owner marks
	// the draft ready on the pull request host directly.
	ReadyCause = "owner-ready"
)

// ReadyAt returns the recorded marking of the workstream's pull request ready
// for review, and whether one is recorded.
func ReadyAt(repository *trace.Repository, stream config.WorkstreamID) (DeliveryReady, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DeliveryReady{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != ReadyDocument {
			continue
		}
		var ready DeliveryReady
		if err := json.Unmarshal([]byte(d.Content), &ready); err != nil {
			return DeliveryReady{}, false, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		return ready, true, nil
	}
	return DeliveryReady{}, false, nil
}

// ReadyDocumentFor builds the document that records ready.
func ReadyDocumentFor(repository *trace.Repository, stream config.WorkstreamID, ready DeliveryReady, cause string) (trace.Document, error) {
	content, err := json.MarshalIndent(ready, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	revision, err := nextRevision(repository, stream, ReadyDocument)
	if err != nil {
		return trace.Document{}, err
	}
	actor := foremanActor
	if ready.Source == api.ReadyOnHost {
		actor = ownerActor
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ReadyDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: ready.At, Actor: actor, Cause: cause},
		Path: ReadyPath, Content: string(content) + "\n"}, nil
}

// deliveredTransitionID is the ID of the transition that moves the workstream
// to delivered: DeliveredState the first time, and alternative when an earlier
// delivery of the same workstream already used it.
func deliveredTransitionID(repository *trace.Repository, stream config.WorkstreamID, alternative string) (string, error) {
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == DeliveredState }) {
		return alternative, nil
	}
	return DeliveredState, nil
}

// MarkedReadyOnHost records the assembled workstream delivered by the owner
// marking its draft pull request ready for review on the pull request host
// directly: pr, as the host reported it, is open and no longer a draft. One
// commit records final/ready.json and the move to delivered with a notice for
// the chief of staff; nothing is written to the host. It records nothing when
// the workstream is no longer assembled, when its pull request is still a
// draft, or when pr is not the pull request of a recorded push at its head.
func MarkedReadyOnHost(ctx context.Context, rt router.Port, repository *trace.Repository, stream config.WorkstreamID, pr pulls.PullRequest, at time.Time) (bool, error) {
	if pr.Draft || pr.State != "open" || pr.Merged {
		return false, nil
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != AssembledState {
		return false, err
	}
	push, found, err := CurrentPush(repository, stream)
	if err != nil || !found || push.PullRequest != pr.Number || push.Commit != pr.HeadCommit {
		return false, err
	}
	ready := DeliveryReady{Source: api.ReadyOnHost, PullRequest: pr.Number, URL: pr.URL, Upstream: push.Upstream, Commit: pr.HeadCommit, At: at}
	doc, err := ReadyDocumentFor(repository, stream, ready, ReadyCause)
	if err != nil {
		return false, err
	}
	reason := fmt.Sprintf("the owner marked pull request #%d (%s) ready for review on the pull request host, at %s", pr.Number, pr.URL, pr.HeadCommit)
	deliveredID, err := deliveredTransitionID(repository, stream, fmt.Sprintf("%s-%s-%d", ReadyCause, DeliveredState, feature.Version))
	if err != nil {
		return false, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: deliveredID, Revision: 1, Project: repository.Project(), Workstream: stream, At: ready.At, Actor: ownerActor, Cause: ReadyCause}
	tx := trace.Transaction{ExpectedVersion: feature.Version,
		Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState, Reason: reason},
		Events:     []trace.Event{trace.Notice(deliveredID, "state", fmt.Sprintf("Workstream delivered: the owner marked pull request #%d ready for review at %s.", pr.Number, pr.URL))}}
	res, err := routeDelivered(ctx, rt, repository, stream, TriggerReadyOnHost, []trace.Document{doc}, tx)
	if err != nil {
		return false, err
	}
	// A workstream that moved since it was read is left to the next observation.
	return res.Committed, nil
}

// PresentChecks is the check result recorded for final review k's commit, for
// the delivery card, inbox and CLI. A run recorded for another commit is not
// shown.
func PresentChecks(repository *trace.Repository, stream config.WorkstreamID, k int, commit string) (*DeliveryChecks, error) {
	_, latest, err := IncompleteFinalChecks(repository, stream, k, commit)
	if err != nil {
		return nil, err
	}
	if latest.Review == 0 || latest.Commit != commit {
		return &DeliveryChecks{State: api.ChecksStateNone, Summary: "No check result is recorded for this review"}, nil
	}
	out := &DeliveryChecks{Commit: latest.Commit, Failed: slices.Clone(latest.Failed), Flaky: slices.Clone(latest.Flaky), Error: latest.Error}
	switch latest.Status {
	case checkrun.ChecksPassed:
		out.State, out.Summary = api.ChecksStatePassed, "Checks passed"
		if len(out.Flaky) > 0 {
			out.Summary += "; flaky, passed on their rerun: " + strings.Join(out.Flaky, ", ")
		}
	case checkrun.ChecksFailed:
		out.State, out.Summary = api.ChecksStateFailed, "Checks failed"
		if len(out.Failed) > 0 {
			out.Summary += ": " + strings.Join(out.Failed, ", ")
		}
	default:
		out.State, out.Summary = api.ChecksStateIncomplete, "Checks did not complete"
		if latest.Error != "" {
			out.Summary += ": " + latest.Error
		}
	}
	return out, nil
}
