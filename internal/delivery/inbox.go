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

// PublicationEntry is an owner approval to mark the pull request ready that
// the service holds because it has no credential to update the pull request,
// opened when it was asked for, or one whose last publicationEscalation or
// more attempts failed in a row, opened at the first of them. It takes no
// decision: it leaves the inbox once an attempt succeeds or the request is
// refused.
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
			e.Question = fmt.Sprintf("Marking the pull request ready for review (owner approval %d) waits for a GitHub token. The service was started without GITHUB_TOKEN, so it changes no pull request.", in.Approval)
			e.Blocked = "Marking the pull request ready for review. Restart osmia serve with GITHUB_TOKEN set to a token that can edit pull requests on " + in.Upstream + ", mark the pull request ready on GitHub yourself, or merge " + featureBranch(stream) + " into upstream yourself and record it with osmia merged " + string(stream) + "."
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
		e.Question = fmt.Sprintf("Marking the pull request ready for review (owner approval %d) has failed %d times in a row. Last failure: %s", in.Approval, failures, last.Failure)
		e.Blocked = fmt.Sprintf("Marking the pull request ready for review. The service tries again at %s, waiting longer after each failure, and this entry closes once an attempt succeeds.", record.RetryAt.UTC().Format(time.RFC3339))
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
	e.Question = fmt.Sprintf("The project's checks on the pull request did not complete %d times in a row, so final review %d cannot read the assembled branch. Last failure: %s", n, k, latest.Error)
	e.Blocked = fmt.Sprintf("The final review. The service reads the pull request's check statuses again at %s, waiting longer after each run, and this entry closes once a run completes.", latest.FinishedAt.Add(FinalCheckDelay(n)).UTC().Format(time.RFC3339))
	return e, true, nil
}

// DeliveryEntry is the "Mark ready for review" decision: an assembled
// workstream's final report, its recorded check result and its brief while
// the draft pull request can be marked ready and no approval of it stands:
// none is recorded, it is stale, or marking it ready was refused.
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
	if p.Brief != nil && p.Brief.State == BriefWaiting {
		return InboxEntry{}, false, nil
	}
	if _, retrying, err := PendingLandingRoute(repository, stream); err != nil || retrying {
		return InboxEntry{}, false, err
	}
	blocking := ReportBlocking(p.Report)
	review, prior, _, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if p.Approval != nil && p.Approval.Route == DeliveryRouteLand {
		if pending, err := landingPending(repository, stream); err != nil || pending {
			return InboxEntry{}, false, err
		}
	} else if pending, err := landingPending(repository, stream); err != nil || pending {
		return InboxEntry{}, false, err
	} else if p.Approval != nil {
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
	e.Question = fmt.Sprintf("Mark ready for review: %s? Final review %d of commit %s found nothing blocking.", title, p.Report.Review, p.Report.Commit)
	if len(blocking) > 0 {
		e.Question = fmt.Sprintf("Mark ready for review: %s as it is? Final review %d of commit %s has blocking findings: %s. Approving accepts them as they are; otherwise the work that resolves them goes through a plan revision, and a new final review follows.", title, p.Report.Review, p.Report.Commit, strings.Join(blocking, "; "))
	}
	if p.Checks != nil {
		e.Question += " " + p.Checks.Summary + "."
	}
	if p.Brief != nil && p.Brief.State == BriefNoneRecorded {
		e.Question += " " + NoBriefRecorded + "."
	}
	if len(blocking) > 0 {
		notes, err := TriageNotes(repository, stream, p.Report.Review)
		if err != nil {
			return InboxEntry{}, false, err
		}
		e.Question += notes
	}
	e.Blocked = "Marking the pull request ready for review."
	e.Options = []string{"approve"}
	if cfg := s.About(repository); p.CanLand {
		e.Options = append(e.Options, "land")
		e.Land = &InboxAnswer{Method: http.MethodPost, Path: e.Answer.Path + "/land", Body: body}
		e.Question += fmt.Sprintf(" Approve and land moves %s of %s to the checked commit instead.", cfg.Project.BaseBranch, cfg.Project.Upstream)
		e.Blocked = "Marking the pull request ready for review, or landing it on " + cfg.Project.BaseBranch + "."
	}
	if p.Recommendation != "" {
		e.Recommendation = fmt.Sprintf("Jev recommends %s.", routeLabel(p.Recommendation))
	}
	return e, true, nil
}
