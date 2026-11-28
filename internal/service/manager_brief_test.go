package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type briefResult struct {
	Recorded       bool   `json:"recorded"`
	Reason         string `json:"reason"`
	Review         int    `json:"review"`
	ReviewRevision int    `json:"review_revision"`
	Commit         string `json:"commit"`
}

func briefCall(t *testing.T, tool coreadapter.Tool, in map[string]any) briefResult {
	t.Helper()
	raw, err := json.Marshal(in)
	fixturekit.Must(t, err)
	out, err := tool.Handle(context.Background(), raw)
	fixturekit.Must(t, err)
	var result briefResult
	fixturekit.Must(t, json.Unmarshal(out, &result))
	return result
}

func briefFixture(t *testing.T) (*shedFixture, config.WorkstreamID, *trace.Repository, FinalReport, coreadapter.Tool) {
	t.Helper()
	f, stream, repository, report := deliveryFixture(t)
	scope := previewScope(t, repository, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return f, stream, repository, report, controls.deliveryBrief(repository, scope)
}

func recordFinalReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, report FinalReport, k int) int {
	t.Helper()
	next := report
	next.Review, next.Operation, next.Outcome = k, fmt.Sprintf("final-operation-%d", k), finalReviewed
	_, err := (&delivery.FinalReviewer{S: f.s.delivery(), Repository: repository}).Record(context.Background(), stream, next)
	fixturekit.Must(t, err)
	review, _, _, err := deliveryDocuments(repository, stream)
	fixturekit.Must(t, err)
	return review.Revision
}

func previewCapture(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, commit, name string) string {
	t.Helper()
	ctx := context.Background()
	image, err := repository.RecordImage(ctx, trace.Header{Project: repository.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "preview"}, name, "image/png", attachmentPNG(t), "turn")
	fixturekit.Must(t, err)
	content, err := json.Marshal(previewOperation{Workstream: stream, Commit: commit, Result: &previewResult{ImageID: image.ID, Workstream: stream, Commit: commit}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "preview-" + name, Revision: 1, Project: repository.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "preview"}, Path: "tools/preview-" + name + ".json", Content: string(content)}}))
	return image.ID
}

func briefDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, review int) int {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	n := 0
	for _, d := range docs {
		if d.ID == delivery.BriefDocument(review) {
			n++
		}
	}
	return n
}

// Only an ordinary manager turn is granted record_delivery_brief.
func TestDeliveryBriefToolReachesOnlyOrdinaryManagerTurns(t *testing.T) {
	t.Parallel()
	if !slices.Contains(managerOrdinaryNarrow.Tools, briefToolName) || !slices.Contains(managerGrant.Tools, briefToolName) {
		t.Fatalf("an ordinary chief-of-staff turn is not granted %s: %v", briefToolName, managerOrdinaryNarrow.Tools)
	}
	for name, tools := range map[string][]string{"design session": designNarrow.Tools, "mason": masonGrant.Tools, "unit mason": unitMasonGrant.Tools, "reviewer": reviewerGrant.Tools} {
		if slices.Contains(tools, briefToolName) {
			t.Errorf("a %s turn is granted %s: %v", name, briefToolName, tools)
		}
	}
}

// A call naming the current reviewed final review stores a brief with that
// review's number, revision and commit, untruncated at the limits, and a
// second call for the same review replaces it.
func TestDeliveryBriefIsRecordedWithTheReviewItNamesAndReplaced(t *testing.T) {
	t.Parallel()
	f, stream, repository, report, tool := briefFixture(t)
	p, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil {
		t.Fatal(api)
	}
	text := strings.Repeat("é", delivery.MaxBriefText)
	items := []string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100), strings.Repeat("ü", 100)}
	result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": text, "before_you_ship": items})
	if !result.Recorded || result.Review != 1 || result.ReviewRevision != p.ReviewRevision || result.Commit != report.Commit {
		t.Fatalf("call: %+v", result)
	}
	brief, found, err := delivery.ReadBrief(repository, stream, 1, p.ReviewRevision)
	fixturekit.Must(t, err)
	if !found || brief.Text != text || !slices.Equal(brief.BeforeYouShip, items) || brief.Review != 1 || brief.ReviewRevision != p.ReviewRevision || brief.Commit != report.Commit {
		t.Fatalf("stored %+v, %v", brief, found)
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": "Replaced."}); !result.Recorded {
		t.Fatalf("replacement: %+v", result)
	}
	brief, found, err = delivery.ReadBrief(repository, stream, 1, p.ReviewRevision)
	fixturekit.Must(t, err)
	if !found || brief.Text != "Replaced." || len(brief.BeforeYouShip) != 0 || briefDocuments(t, repository, stream, 1) != 2 {
		t.Fatalf("replaced %+v, %v", brief, found)
	}
	presented, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil || presented.Brief == nil || presented.Brief.State != delivery.BriefRecorded || presented.Brief.Text != "Replaced." || presented.Brief.Review != 1 || presented.Brief.ReviewRevision != p.ReviewRevision || presented.Brief.Commit != report.Commit {
		t.Fatalf("presentation brief %+v, %v", presented.Brief, api)
	}
	if presented.DraftHash != p.DraftHash || presented.Draft != p.Draft {
		t.Fatal("the brief changed the drafted description")
	}
}

// A refused call stores nothing and replaces nothing.
func TestDeliveryBriefRefusals(t *testing.T) {
	t.Parallel()
	f, stream, repository, report, tool := briefFixture(t)
	p, api := f.s.deliveryPresentation(context.Background(), string(stream))
	if api != nil {
		t.Fatal(api)
	}
	revision := p.ReviewRevision
	good := briefCall(t, tool, map[string]any{"review": 1, "review_revision": revision, "text": "Kept."})
	if !good.Recorded {
		t.Fatalf("baseline: %+v", good)
	}
	current := previewCapture(t, repository, stream, report.Commit, "current")
	older := previewCapture(t, repository, stream, strings.Repeat("a", 40), "older")
	for name, tc := range map[string]struct {
		in     map[string]any
		reason string
	}{
		"text over the limit":     {map[string]any{"review": 1, "review_revision": revision, "text": strings.Repeat("x", delivery.MaxBriefText+1)}, "800"},
		"empty text":              {map[string]any{"review": 1, "review_revision": revision, "text": "  "}, "empty"},
		"items over the limit":    {map[string]any{"review": 1, "review_revision": revision, "text": "t", "before_you_ship": []string{strings.Repeat("a", 200), strings.Repeat("b", 201)}}, "400"},
		"multi-line item":         {map[string]any{"review": 1, "review_revision": revision, "text": "t", "before_you_ship": []string{"one\ntwo"}}, "one line"},
		"too many screenshots":    {map[string]any{"review": 1, "review_revision": revision, "text": "t", "screenshots": []string{current, current, current, current}}, "3"},
		"unknown screenshot":      {map[string]any{"review": 1, "review_revision": revision, "text": "t", "screenshots": []string{"image-unknown"}}, "not an image preview"},
		"screenshot of a commit":  {map[string]any{"review": 1, "review_revision": revision, "text": "t", "screenshots": []string{older}}, "reviewed commit"},
		"another review revision": {map[string]any{"review": 1, "review_revision": revision + 1, "text": "t"}, "not the current"},
		"another review":          {map[string]any{"review": 2, "review_revision": revision, "text": "t"}, "not the current"},
	} {
		result := briefCall(t, tool, tc.in)
		if result.Recorded || !strings.Contains(result.Reason, tc.reason) {
			t.Errorf("%s: %+v, want a refusal naming %q", name, result, tc.reason)
		}
	}
	brief, found, err := delivery.ReadBrief(repository, stream, 1, revision)
	fixturekit.Must(t, err)
	if !found || brief.Text != "Kept." || len(brief.Screenshots) != 0 || briefDocuments(t, repository, stream, 1) != 1 {
		t.Fatalf("a refused call changed the brief: %+v, %v", brief, found)
	}
	result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": revision, "text": strings.Repeat("x", delivery.MaxBriefText), "screenshots": []string{current}})
	if !result.Recorded {
		t.Fatalf("screenshot of the reviewed commit: %+v", result)
	}
	if brief, _, err := delivery.ReadBrief(repository, stream, 1, revision); err != nil || !slices.Equal(brief.Screenshots, []string{current}) {
		t.Fatalf("stored screenshots %+v, %v", brief, err)
	}
}

// A shared manager names the member, and only a member of its workgroup.
func TestDeliveryBriefOfASharedManagerNamesTheMember(t *testing.T) {
	t.Parallel()
	s, manager, group := workgroupFeedFixture(t, false, 2)
	scope := previewScope(t, manager, group.Members[0], func() time.Time { return fixturekit.DemoStart })
	controls := &runtimeControls{}
	controls.service.Store(s)
	tool := controls.deliveryBrief(manager, scope)
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": 1, "text": "t"}); result.Recorded || !strings.Contains(result.Reason, "must name") {
		t.Fatalf("shared chief without a member: %+v", result)
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": 1, "text": "t", "workstream": "w_" + strings.Repeat("9", 32)}); result.Recorded || !strings.Contains(result.Reason, "does not share") {
		t.Fatalf("a workstream outside the workgroup: %+v", result)
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": 1, "text": "t", "workstream": fixturekit.Quiet}); result.Recorded || !strings.Contains(result.Reason, "no final review") {
		t.Fatalf("a member with no final review: %+v", result)
	}
	if docs, err := trace.Read[trace.Document](manager, fixturekit.Quiet); err != nil || slices.ContainsFunc(docs, func(d trace.Document) bool { return d.ID == delivery.BriefDocument(1) }) {
		t.Fatalf("a refused call stored a brief: %v", err)
	}
}

// Once the owner has approved delivery for a review, its brief is closed.
func TestDeliveryBriefIsClosedByApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, report, tool := briefFixture(t)
	ctx := context.Background()
	p, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": "Before approval."}); !result.Recorded {
		t.Fatalf("before approval: %+v", result)
	}
	p, api = f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	approved, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: 1, ReviewRevision: p.ReviewRevision, Commit: report.Commit, DraftHash: p.DraftHash})
	if api != nil {
		t.Fatal(api)
	}
	result := briefCall(t, tool, map[string]any{"review": 1, "review_revision": p.ReviewRevision, "text": "After approval."})
	if result.Recorded || !strings.Contains(result.Reason, "already approved") {
		t.Fatalf("after approval: %+v", result)
	}
	brief, _, err := delivery.ReadBrief(repository, stream, 1, p.ReviewRevision)
	fixturekit.Must(t, err)
	after, api := f.s.deliveryPresentation(ctx, string(stream))
	if brief.Text != "Before approval." || briefDocuments(t, repository, stream, 1) != 1 || api != nil || after.Approval == nil || after.Approval.Description != approved.Description || after.Brief == nil || after.Brief.State != delivery.BriefRecorded {
		t.Fatalf("approval did not close the brief: %+v %v", after.Brief, api)
	}
}

// The final review notice names the review and revision to give and carries
// the brief's instructions.
func TestFinalReviewNoticeAsksForTheBrief(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	revision := recordFinalReview(t, f, repository, stream, report, 2)
	entries, err := repository.Outbox(stream)
	fixturekit.Must(t, err)
	var body string
	for _, e := range entries {
		if e.TransitionID == "final-review-2-reviewed" {
			body = e.Event.Body
		}
	}
	for _, want := range []string{
		briefToolName + ", naming review 2 and review_revision " + fmt.Sprint(revision),
		"how the approved intent shows up in the product", "at most one line on how it was implemented", "departs from the intent",
		"Never list tests, test names, files or commits", "at most 800 characters",
		"image_id", "at most 3", "exact commit this review read, " + report.Commit, "say so in one line",
		"on every delivery", "secrets and environment variables", "migrations", "breaking changes for API or CLI callers", "omit before_you_ship when you find nothing", "at most 400 characters in total",
		"owner pushes back", "move_workstream", "route_amendment", "usual amendment approval", "new screenshots", "what changed since the last brief",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the notice lacks %q:\n%s", want, body)
		}
	}
}

type briefReview struct {
	t          *testing.T
	f          *shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	report     FinalReport
	c          *delivery.Controller
	revision   int
}

type managerThreadFunc = func(context.Context, *trace.Repository, config.WorkstreamID) (trace.Thread, bool, error)

func newBriefReview(t *testing.T) *briefReview {
	t.Helper()
	f, stream, repository, report := deliveryFixture(t)
	c := f.s.delivery()
	return &briefReview{t: t, f: f, stream: stream, repository: repository, report: report, c: c, revision: recordFinalReview(t, f, repository, stream, report, 2)}
}

func managerThreadOf(turns ...trace.QueuedTurn) managerThreadFunc {
	return func(context.Context, *trace.Repository, config.WorkstreamID) (trace.Thread, bool, error) {
		return trace.Thread{Turns: turns}, true, nil
	}
}

func (r *briefReview) state(want string, listed bool, fallback bool) {
	r.t.Helper()
	ctx := context.Background()
	p, api := r.c.PresentDelivery(ctx, r.repository.Project(), r.stream, r.repository)
	if api != nil {
		r.t.Fatal(api)
	}
	if p.Brief == nil || p.Brief.State != want {
		r.t.Fatalf("brief %+v, want state %s", p.Brief, want)
	}
	e, open, err := r.c.DeliveryEntry(ctx, r.repository, r.stream)
	fixturekit.Must(r.t, err)
	if open != listed {
		r.t.Fatalf("entry open %v, want %v (brief state %s)", open, listed, want)
	}
	if open && strings.Contains(e.Question, delivery.NoBriefRecorded) != fallback {
		r.t.Fatalf("entry %q, fallback %v", e.Question, fallback)
	}
}

func (r *briefReview) claimReviewNotice() trace.QueuedTurn {
	r.t.Helper()
	ctx := context.Background()
	entries, err := r.repository.Outbox(r.stream)
	fixturekit.Must(r.t, err)
	var notice trace.OutboxEntry
	for _, e := range entries {
		if e.TransitionID == "final-review-2-reviewed" {
			notice = e
		}
	}
	token := "delivery_brief"
	_, err = r.repository.Claim(ctx, r.stream, notice.Event.ID, token, events.Actor.ID, time.Now().UTC(), time.Minute)
	fixturekit.Must(r.t, err)
	return trace.QueuedTurn{Request: trace.TurnRequest{TurnID: events.TurnID(token)}}
}

func (r *briefReview) record(review, revision int, text string) {
	r.t.Helper()
	recorded, reason, err := briefOf(context.Background(), r.c, r.repository, r.stream, review, revision, text)
	fixturekit.Must(r.t, err)
	if !recorded {
		r.t.Fatalf("brief refused: %s", reason)
	}
}

func TestDeliveryEntryOpensWithTheNoBriefTextWhenNoManagerTurnCanWriteOne(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	r.c.ManagerThread = nil
	r.state(delivery.BriefNoneRecorded, true, true)
	r.c.ManagerThread = func(context.Context, *trace.Repository, config.WorkstreamID) (trace.Thread, bool, error) {
		return trace.Thread{}, false, nil
	}
	r.state(delivery.BriefNoneRecorded, true, true)
}

func TestDeliveryEntryWaitsWhileTheManagersBriefTurnIsQueuedOrRunning(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	r.c.ManagerThread = managerThreadOf()
	r.state(delivery.BriefWaiting, false, false)
	r.c.ManagerThread = managerThreadOf(r.claimReviewNotice())
	r.state(delivery.BriefWaiting, false, false)
}

func TestABriefOfAnEarlierReviewIsRefusedAndTheEntryKeepsWaiting(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	r.c.ManagerThread = managerThreadOf(r.claimReviewNotice())
	stale, reason, err := briefOf(context.Background(), r.c, r.repository, r.stream, 1, r.revision, "Stale.")
	fixturekit.Must(t, err)
	if stale || reason == "" {
		t.Fatalf("a brief for review 1 was accepted after review 2: %q", reason)
	}
	r.state(delivery.BriefWaiting, false, false)
}

func TestDeliveryEntryOpensWithTheNoBriefTextOnceTheBriefTurnEndedWithoutOne(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	ended := r.claimReviewNotice()
	ended.Response, ended.CompletedAt = &trace.TurnResponse{}, time.Now().UTC()
	r.c.ManagerThread = managerThreadOf(ended)
	r.state(delivery.BriefNoneRecorded, true, true)
}

func TestABriefRecordedAfterItsTurnEndedOpensTheEntryWithTheBrief(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	running := r.claimReviewNotice()
	ended := running
	ended.Response, ended.CompletedAt = &trace.TurnResponse{}, time.Now().UTC()
	r.c.ManagerThread = managerThreadOf(ended)
	r.record(2, r.revision, "Recorded late.")
	r.state(delivery.BriefRecorded, true, false)
	r.c.ManagerThread = managerThreadOf(running)
	r.state(delivery.BriefRecorded, true, false)
}

func TestANewFinalReviewWaitsForItsOwnBriefWithoutTheEarlierOne(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	running := r.claimReviewNotice()
	r.c.ManagerThread = managerThreadOf(running)
	r.record(2, r.revision, "Recorded late.")
	next := recordFinalReview(t, r.f, r.repository, r.stream, r.report, 3)
	r.state(delivery.BriefWaiting, false, false)
	p, api := r.c.PresentDelivery(context.Background(), r.repository.Project(), r.stream, r.repository)
	if api != nil || p.Brief.Text != "" || p.Brief.Review != 3 || p.Brief.ReviewRevision != next || len(p.Brief.BeforeYouShip) != 0 {
		t.Fatalf("the earlier review's brief is presented for the new one: %+v, %v", p.Brief, api)
	}
	if b, found, err := delivery.ReadBrief(r.repository, r.stream, 2, r.revision); err != nil || !found || b.Text != "Recorded late." {
		t.Fatalf("review 2's brief %+v, %v, %v", b, found, err)
	}
	if _, found, err := delivery.ReadBrief(r.repository, r.stream, 3, next); err != nil || found {
		t.Fatalf("a brief for review 3 exists: %v %v", found, err)
	}
}

func TestADelayedBriefOfTheSupersededReviewIsRefused(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	r.c.ManagerThread = managerThreadOf(r.claimReviewNotice())
	recordFinalReview(t, r.f, r.repository, r.stream, r.report, 3)
	delayed, reason, err := briefOf(context.Background(), r.c, r.repository, r.stream, 2, r.revision, "Delayed.")
	fixturekit.Must(t, err)
	if delayed || !strings.Contains(reason, "not the current") {
		t.Fatalf("delayed call: %v %q", delayed, reason)
	}
	if _, found, err := delivery.ReadBrief(r.repository, r.stream, 2, r.revision); err != nil || found {
		t.Fatalf("a refused delayed call stored a brief for review 2: %v %v", found, err)
	}
	r.state(delivery.BriefWaiting, false, false)
}

func TestABriefOfTheNewFinalReviewOpensTheEntryWithTheBrief(t *testing.T) {
	t.Parallel()
	r := newBriefReview(t)
	r.c.ManagerThread = managerThreadOf(r.claimReviewNotice())
	next := recordFinalReview(t, r.f, r.repository, r.stream, r.report, 3)
	r.record(3, next, "For the new review.")
	r.state(delivery.BriefRecorded, true, false)
}

func briefOf(ctx context.Context, c *delivery.Controller, repository *trace.Repository, stream config.WorkstreamID, review, revision int, text string) (bool, string, error) {
	_, _, reason, err := c.RecordBrief(ctx, repository, stream, delivery.BriefInput{Review: review, ReviewRevision: revision, Text: text, Turn: "turn"}, nil)
	return reason == "" && err == nil, reason, err
}
