package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

// publicationEntry is a pending publication the service holds because it has
// no credential to open the pull request, opened when it was asked for, or
// one whose last publicationEscalation or more attempts failed in a row,
// opened at the first of them. It takes no decision: it leaves the inbox
// once an attempt succeeds or the publication is refused.
func (s *Controller) PublicationEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	if !strings.HasPrefix(w.Subjects[PublicationSubject].Value, "requested-") {
		return InboxEntry{}, false, nil
	}
	stream := w.Workstream
	records, err := repository.Operations(stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	for _, record := range records {
		if record.Acknowledged || !PublicationAction(record.Operation) {
			continue
		}
		credentialed := pulls.Credentialed(s.PullRequests)
		failures := reconcile.ConsecutiveFailures(record)
		if credentialed && failures < publicationEscalation {
			continue
		}
		in, err := DecodePublish(record.Operation)
		if err != nil {
			return InboxEntry{}, false, err
		}
		if !credentialed {
			e := decision(InboxPublication, stream, record.Transition.At, "/delivery/"+string(stream), map[string]any{})
			e.Answer.Method = http.MethodGet
			e.Revision = in.Approval
			e.Question = fmt.Sprintf("Publishing owner approval %d waits for a GitHub token. The service was started without GITHUB_TOKEN, so it pushes nothing and opens no pull request.", in.Approval)
			e.Blocked = "Publishing the pull request. Restart osmia serve with GITHUB_TOKEN set to a token that can open pull requests on " + in.Upstream + ", or merge " + featureBranch(stream) + " into upstream yourself and record it with osmia merged " + string(stream) + "."
			return e, true, nil
		}
		var first, last trace.OperationAction
		for _, action := range record.History {
			if action.Kind == reconcile.RetryAction {
				if first.At.IsZero() {
					first = action
				}
				last = action
			}
		}
		e := decision(InboxPublication, stream, first.At, "/delivery/"+string(stream), map[string]any{})
		e.Answer.Method = http.MethodGet
		e.Revision = in.Approval
		e.Question = fmt.Sprintf("Publishing owner approval %d has failed %d times in a row. Last failure: %s", in.Approval, failures, last.Failure)
		e.Blocked = fmt.Sprintf("Publishing the pull request. The service tries again at %s, waiting longer after each failure, and this entry closes once an attempt succeeds.", record.RetryAt.UTC().Format(time.RFC3339))
		return e, true, nil
	}
	return InboxEntry{}, false, nil
}

// FinalChecksEntry reports the requested final review whose checks have not
// completed maxIncompleteChecks times in a row, opened when the first of
// those runs finished. It takes no decision: the owner fixes the cause, and
// it closes once a run completes.
func FinalChecksEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	k, err := strconv.Atoi(strings.TrimPrefix(w.Subjects[FinalReviewSubject].Value, "requested-"))
	if err != nil || !strings.HasPrefix(w.Subjects[FinalReviewSubject].Value, "requested-") {
		return InboxEntry{}, false, nil
	}
	stream := w.Workstream
	rebase, rebased, err := (&FinalReviewer{Repository: repository}).Rebase(stream, k)
	if err != nil || !rebased {
		return InboxEntry{}, false, err
	}
	n, latest, err := IncompleteFinalChecks(repository, stream, k, rebase.Commit)
	if err != nil || n < maxIncompleteChecks {
		return InboxEntry{}, false, err
	}
	e := decision(InboxFinalChecks, stream, latest.StartedAt, "/status/"+string(stream), map[string]any{})
	e.Answer.Method = http.MethodGet
	e.Revision = k
	e.Question = fmt.Sprintf("The project's checks on the assembled branch did not complete %d times in a row, so final review %d cannot read it. Last failure: %s", n, k, latest.Error)
	e.Blocked = fmt.Sprintf("The final review. The service runs the checks again at %s, waiting longer after each run, and this entry closes once a run completes.", latest.FinishedAt.Add(FinalCheckDelay(n)).UTC().Format(time.RFC3339))
	return e, true, nil
}

// deliveryEntry is an assembled workstream's final report while it can be
// approved and no approval of it stands: none is recorded, it is stale, or
// its publication was refused.
func (s *Controller) DeliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	p, api := s.PresentDelivery(ctx, repository.Project(), stream, repository)
	switch {
	case api != nil && api.Code == Conflict:
		return InboxEntry{}, false, nil
	case api != nil:
		return InboxEntry{}, false, errors.New(api.Message)
	case p.Delivered && !p.Maintenance:
		return InboxEntry{}, false, nil
	}
	blocking := ReportBlocking(p.Report)
	review, prior, _, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if p.Approval != nil {
		if refused, err := PublicationRefused(repository, stream, prior.Revision); err != nil || !refused {
			return InboxEntry{}, false, err
		}
	}
	body := map[string]any{"review": p.Report.Review, "review_revision": p.ReviewRevision, "commit": p.Report.Commit, "draft_hash": p.DraftHash}
	if len(blocking) > 0 {
		body["accept_blocking"] = true
	}
	e := decision(InboxDelivery, stream, review.At, "/delivery/"+string(stream), body)
	e.Revision = p.ReviewRevision
	title := strings.TrimSpace(p.Report.Summary)
	if title == "" {
		title = "workstream " + string(stream)
	}
	e.Question = fmt.Sprintf("Deliver %s? Final review %d of commit %s found nothing blocking.", title, p.Report.Review, p.Report.Commit)
	if len(blocking) > 0 {
		e.Question = fmt.Sprintf("Deliver %s as it is? Final review %d of commit %s has blocking findings: %s. Approving accepts them as they are; otherwise the work that resolves them goes through a plan revision, and a new final review follows.", title, p.Report.Review, p.Report.Commit, strings.Join(blocking, "; "))
	}
	e.Blocked = "Publishing the pull request."
	e.Options = []string{"approve"}
	return e, true, nil
}
