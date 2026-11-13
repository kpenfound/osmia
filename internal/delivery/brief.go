package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/trace"
)

// BriefToolName is the chief-only service tool that records a delivery brief.
const BriefToolName = "record_delivery_brief"

// The limits of a delivery brief. The main text and the items are counted in
// characters (runes); a brief over any of them is refused, never shortened.
const (
	MaxBriefText        = 800
	MaxBriefItems       = 400
	MaxBriefScreenshots = 3
)

// NoBriefRecorded is what the delivery entry and card say when the chief of
// staff's turn for a final review's notice ended without recording a brief.
const NoBriefRecorded = api.NoBriefRecorded

// Brief is the delivery brief recorded for one final review. Review and
// ReviewRevision are the final report's number and its document revision, and
// Commit the commit that review read; the brief covers only that review.
// Screenshots are the IDs of preview images of the member at Commit.
type Brief struct {
	Review         int      `json:"review"`
	ReviewRevision int      `json:"review_revision"`
	Commit         string   `json:"commit"`
	Text           string   `json:"text"`
	BeforeYouShip  []string `json:"before_you_ship,omitempty"`
	Screenshots    []string `json:"screenshots,omitempty"`
}

// BriefDocument is the trace record ID of the brief for final review k. Each
// review has a document of its own, so a call for one review can never
// replace the brief of another.
func BriefDocument(k int) string { return fmt.Sprintf("delivery-brief-%d", k) }

// BriefPath is the workstream document path of the brief for final review k.
func BriefPath(k int) string { return fmt.Sprintf("final/brief-%d.json", k) }

// ReadBrief returns the latest brief recorded for the final review with the
// given number and report revision, and whether there is one. A brief
// recorded for any other review is never returned.
func ReadBrief(repository *trace.Repository, stream config.WorkstreamID, review, reviewRevision int) (Brief, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Brief{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == BriefDocument(review) && d.Revision > latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return Brief{}, false, nil
	}
	var b Brief
	if err := json.Unmarshal([]byte(latest.Content), &b); err != nil {
		return Brief{}, false, fmt.Errorf("%s revision %d: %w", latest.Path, latest.Revision, err)
	}
	if b.Review != review || b.ReviewRevision != reviewRevision {
		return Brief{}, false, nil
	}
	return b, true, nil
}

// RecordedBrief is the latest brief of one final review and when the first
// brief for that review was recorded.
type RecordedBrief struct {
	Brief
	At time.Time
}

// Briefs returns the latest brief of every final review the workstream has one
// for, oldest review first, each with the time its review's first brief was
// recorded.
func Briefs(repository *trace.Repository, stream config.WorkstreamID) ([]RecordedBrief, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	first := map[string]time.Time{}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, "delivery-brief-") {
			continue
		}
		if at, ok := first[d.ID]; !ok || d.At.Before(at) {
			first[d.ID] = d.At
		}
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	out := make([]RecordedBrief, 0, len(latest))
	for id, d := range latest {
		var b Brief
		if err := json.Unmarshal([]byte(d.Content), &b); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		if d.ID != BriefDocument(b.Review) {
			continue
		}
		out = append(out, RecordedBrief{Brief: b, At: first[id]})
	}
	slices.SortFunc(out, func(a, b RecordedBrief) int { return a.Review - b.Review })
	return out, nil
}

// BriefForReport is ReadBrief for the final review report is, whose document
// has revision reviewRevision: the brief for the same review, number,
// revision and commit, or none.
func BriefForReport(repository *trace.Repository, stream config.WorkstreamID, report FinalReport, reviewRevision int) (Brief, bool, error) {
	b, found, err := ReadBrief(repository, stream, report.Review, reviewRevision)
	if err != nil || !found || b.Commit != report.Commit {
		return Brief{}, false, err
	}
	return b, true, nil
}

// BriefInput is one record_delivery_brief call: the review it names, the
// final report's number and revision, and the brief's content.
type BriefInput struct {
	Review         int
	ReviewRevision int
	Text           string
	BeforeYouShip  []string
	Screenshots    []string
	// Turn is the chief-of-staff turn that made the call.
	Turn string
}

// ScreenshotCheck returns why screenshots are not all previews of the member
// workstream at commit, or "" when they are.
type ScreenshotCheck func(commit string, screenshots []string) (string, error)

// BriefLimits returns why text, items and screenshots exceed the brief's
// limits or break its shape, or "" when they do not.
func BriefLimits(text string, items, screenshots []string) string {
	if strings.TrimSpace(text) == "" {
		return "the brief's text is empty; write how the approved intent shows up in the product"
	}
	if n := utf8.RuneCountInString(text); n > MaxBriefText {
		return fmt.Sprintf("the brief's text is %d characters, over the limit of %d; rewrite it shorter, nothing is cut off", n, MaxBriefText)
	}
	total := 0
	for i, item := range items {
		if strings.TrimSpace(item) == "" {
			return fmt.Sprintf("before_you_ship item %d is empty", i+1)
		}
		if strings.ContainsAny(item, "\r\n") {
			return fmt.Sprintf("before_you_ship item %d spans more than one line; each item is one short line", i+1)
		}
		total += utf8.RuneCountInString(item)
	}
	if total > MaxBriefItems {
		return fmt.Sprintf("the before_you_ship items total %d characters, over the limit of %d; shorten them, nothing is cut off", total, MaxBriefItems)
	}
	if len(screenshots) > MaxBriefScreenshots {
		return fmt.Sprintf("the brief names %d screenshots, over the limit of %d", len(screenshots), MaxBriefScreenshots)
	}
	return ""
}

// briefTarget is the final review a brief can be recorded for.
type briefTarget struct {
	report   FinalReport
	revision int
}

// briefTarget returns the member workstream's current reviewed final review
// and its report revision when it awaits the owner's delivery approval, or why
// no review does. A workstream awaits it while it is assembled, or delivered
// with a pending update of its open pull request, its latest final review is
// reviewed and still reads the branch, and the owner has not approved
// delivery for that review.
func (s *Controller) briefTarget(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, named *BriefInput) (briefTarget, string, error) {
	review, _, approval, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return briefTarget{}, "", err
	}
	report, found, err := LatestFinalReport(repository, stream)
	if err != nil {
		return briefTarget{}, "", err
	}
	if !found || review.Revision == 0 {
		return briefTarget{}, "no final review is recorded for this workstream", nil
	}
	if named != nil && (named.Review != report.Review || named.ReviewRevision != review.Revision) {
		return briefTarget{}, fmt.Sprintf("final review %d revision %d is not the current final review: the current one is review %d revision %d; the brief for a review is recorded only while that review is current", named.Review, named.ReviewRevision, report.Review, review.Revision), nil
	}
	if approval != nil && approval.Review == report.Review && approval.ReviewRevision == review.Revision {
		return briefTarget{}, fmt.Sprintf("the owner has already approved delivery for final review %d, so its brief is closed", report.Review), nil
	}
	if report.Outcome != FinalReviewed {
		return briefTarget{}, fmt.Sprintf("final review %d did not read the branch, so no reviewed final review awaits delivery approval", report.Review), nil
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return briefTarget{}, "", err
	}
	if feature.Value != AssembledState {
		pending := false
		if feature.Value == DeliveredState {
			if pending, err = s.UpstreamDeliveryPending(repository, s.About(repository), stream); err != nil {
				return briefTarget{}, "", err
			}
		}
		if !pending {
			return briefTarget{}, "no reviewed final review awaits delivery approval: the workstream is " + feature.Value, nil
		}
	}
	if _, reason, err := (&FinalReviewer{S: s, Repository: repository}).FinalGate(ctx, stream); err != nil || reason != "" {
		if reason != "" {
			reason = "no reviewed final review awaits delivery approval: " + reason
		}
		return briefTarget{}, reason, err
	}
	return briefTarget{report: report, revision: review.Revision}, "", nil
}

// RecordBrief records the brief in for the member workstream stream in the
// member's own trace as the next revision of the document of the review it
// names, and returns the stored brief and that revision. It refuses, storing
// nothing and replacing nothing, with a reason when the named review is not
// the workstream's current reviewed final review awaiting the owner's
// delivery approval, when the brief is over a limit, or when check finds a
// screenshot that is not a preview of the member at the reviewed commit.
func (s *Controller) RecordBrief(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, in BriefInput, check ScreenshotCheck) (Brief, int, string, error) {
	if in.Review < 1 || in.ReviewRevision < 1 {
		return Brief{}, 0, "name the final review the brief covers: its review number and review revision, as the notice gives them", nil
	}
	target, reason, err := s.briefTarget(ctx, repository, stream, &in)
	if err != nil || reason != "" {
		return Brief{}, 0, reason, err
	}
	if reason := BriefLimits(in.Text, in.BeforeYouShip, in.Screenshots); reason != "" {
		return Brief{}, 0, reason, nil
	}
	if len(in.Screenshots) > 0 {
		if check == nil {
			return Brief{}, 0, "screenshots cannot be checked here", nil
		}
		if reason, err := check(target.report.Commit, in.Screenshots); err != nil || reason != "" {
			return Brief{}, 0, reason, err
		}
	}
	brief := Brief{Review: target.report.Review, ReviewRevision: target.revision, Commit: target.report.Commit, Text: in.Text, BeforeYouShip: in.BeforeYouShip, Screenshots: in.Screenshots}
	content, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		return Brief{}, 0, "", err
	}
	for attempt := 0; ; attempt++ {
		// The review is read again as late as the write allows: a review
		// recorded or an approval given since the first read ends the call.
		if _, reason, err := s.briefTarget(ctx, repository, stream, &in); err != nil || reason != "" {
			return Brief{}, 0, reason, err
		}
		revision, err := nextRevision(repository, stream, BriefDocument(brief.Review))
		if err != nil {
			return Brief{}, 0, "", err
		}
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: BriefDocument(brief.Review), Revision: revision, Project: repository.Project(), Workstream: stream, At: s.Now(), Actor: chiefActor, Cause: in.Turn}, Path: BriefPath(brief.Review), Content: string(content) + "\n"}
		err = repository.RecordDocuments(ctx, []trace.Document{doc})
		if errors.Is(err, trace.ErrConflict) && attempt < 3 {
			continue
		}
		return brief, revision, "", err
	}
}

// briefWaiting reports whether the delivery inbox entry for final review k
// waits for its brief: the chief-of-staff turn that handles the review's
// notice is queued or running, or has not been started, and a chief turn can
// still run. A turn that ended in any status, or no chief turn that can run,
// ends the wait.
func (s *Controller) briefWaiting(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, k int) (bool, error) {
	if s.ChiefThread == nil {
		return false, nil
	}
	transition, _ := FinalReviewIDs(k)
	id := trace.EventID(transition+"-"+FinalReviewed, "chief")
	entries, err := repository.Outbox(stream)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Event.ID != id {
			continue
		}
		chief, runnable, err := s.ChiefThread(ctx, repository, stream)
		if err != nil || !runnable {
			return false, err
		}
		return !events.Handled(e, chief), nil
	}
	return false, nil
}

// presentBrief is the brief field of the presentation of report, the final
// review whose report document has revision reviewRevision: the brief
// recorded for it, or its waiting or none-recorded state. approved says the
// owner approved delivery for it, which closes the brief.
func (s *Controller) presentBrief(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, report FinalReport, reviewRevision int, approved bool) (*DeliveryBrief, error) {
	b, found, err := BriefForReport(repository, stream, report, reviewRevision)
	if err != nil {
		return nil, err
	}
	out := &DeliveryBrief{Review: report.Review, ReviewRevision: reviewRevision, Commit: report.Commit}
	if found {
		out.State, out.Text, out.BeforeYouShip, out.Screenshots = BriefRecorded, b.Text, b.BeforeYouShip, b.Screenshots
		return out, nil
	}
	out.State = BriefNoneRecorded
	if !approved {
		waiting, err := s.briefWaiting(ctx, repository, stream, report.Review)
		if err != nil {
			return nil, err
		}
		if waiting {
			out.State = BriefWaiting
		}
	}
	return out, nil
}
