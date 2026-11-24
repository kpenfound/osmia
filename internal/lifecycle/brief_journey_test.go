package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	briefText      = "Owners can resume a dropped upload from the last chunk the server acknowledged."
	briefShipFirst = "Set UPLOAD_KEY in the server environment"
	briefShipNext  = "Restart the upload worker after the update"
	briefBlock     = "\nBefore you ship:\n- " + briefShipFirst + "\n- " + briefShipNext + "\n"
)

func (j *briefJourney) requireNoProblems(t *testing.T) {
	t.Helper()
	if problems := j.chief.problems(); len(problems) > 0 {
		t.Fatalf("the service refused the chief of staff's brief: %q", problems)
	}
}

func TestDeliveryBriefHoldsTheEntryAndItsItemsReachThePublishedBody(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, "")
	stream := j.Stream
	screenshot := j.capture(t, "uploads", j.Report.Commit)
	j.chief.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst, briefShipNext}, Screenshots: []string{screenshot}})
	j.chief.hold()
	j.run(t)
	j.awaitStarts(t, 1)

	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the delivery entry is listed while the brief is pending: %+v", entries)
	}
	waiting := j.present(t, stream)
	if waiting.Brief == nil || waiting.Brief.State != api.BriefWaiting || waiting.Brief.Review != 1 || waiting.Brief.ReviewRevision != waiting.ReviewRevision || waiting.Brief.Commit != waiting.Report.Commit || waiting.Brief.Text != "" {
		t.Fatalf("the brief of the waiting presentation: %+v", waiting.Brief)
	}
	if strings.Contains(waiting.Draft, "Before you ship") {
		t.Fatalf("the draft carries a block before any brief:\n%s", waiting.Draft)
	}

	j.chief.release()
	entry := j.awaitEntry(t, stream)
	if strings.Contains(entry.Question, api.NoBriefRecorded) {
		t.Fatalf("an entry with a recorded brief says none was recorded: %q", entry.Question)
	}
	j.requireNoProblems(t)
	p := j.present(t, stream)
	b := p.Brief
	if b == nil || b.State != api.BriefRecorded || b.Text != briefText || !slices.Equal(b.BeforeYouShip, []string{briefShipFirst, briefShipNext}) || !slices.Equal(b.Screenshots, []string{screenshot}) ||
		b.Review != 1 || b.ReviewRevision != p.ReviewRevision || b.Commit != p.Report.Commit {
		t.Fatalf("the presented brief: %+v", b)
	}
	if !strings.Contains(p.Draft, briefBlock) {
		t.Fatalf("the draft lacks the Before you ship block:\n%s", p.Draft)
	}
	if assumptions := strings.Index(p.Draft, "Assumptions:"); assumptions >= 0 && assumptions < strings.Index(p.Draft, "Before you ship:") {
		t.Fatalf("the block follows the assumptions:\n%s", p.Draft)
	}
	for _, leaked := range []string{briefText, screenshot} {
		if strings.Contains(p.Draft, leaked) || strings.Contains(p.Title, leaked) {
			t.Fatalf("%q reached the title or the draft:\n%s", leaked, p.Draft)
		}
	}

	approved, err := j.approvePresented(p)
	fixturekit.Must(t, err)
	if approved.Description != p.Draft || !strings.Contains(approved.Description, briefBlock) {
		t.Fatalf("the approved description:\n%s", approved.Description)
	}

	refused := j.ask(t, stream, "after-approval", recordBrief(p, "", "Changed my mind.", "Run a migration"))
	if len(refused) != 1 || !strings.Contains(refused[0], `"recorded":false`) || !strings.Contains(refused[0], "already approved") {
		t.Fatalf("a brief after the approval: %q", refused)
	}
	after := j.present(t, stream)
	if after.Approval == nil || after.Approval.Description != approved.Description || after.Brief == nil || after.Brief.Text != briefText || !slices.Equal(after.Brief.BeforeYouShip, []string{briefShipFirst, briefShipNext}) {
		t.Fatalf("the approval or the brief changed after the approval: %+v %+v", after.Approval, after.Brief)
	}

	j.halt(t)
	body := j.publish(t)
	if body != approved.Description || !strings.Contains(body, briefBlock) {
		t.Fatalf("the published body is not the approved description:\n%s\napproved:\n%s", body, approved.Description)
	}
}

func TestDeliveryEntryFallsBackWhenTheChiefRecordsNoBrief(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, "")
	stream := j.Stream
	j.chief.hold()
	j.run(t)
	j.awaitStarts(t, 1)
	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the delivery entry is listed while the chief's turn runs: %+v", entries)
	}

	j.chief.release()
	entry := j.awaitEntry(t, stream)
	if !strings.Contains(entry.Question, api.NoBriefRecorded) {
		t.Fatalf("the fallback entry: %q", entry.Question)
	}
	p := j.present(t, stream)
	if p.Brief == nil || p.Brief.State != api.BriefNoneRecorded || p.Brief.Text != "" || p.Brief.Review != 1 {
		t.Fatalf("the fallback brief: %+v", p.Brief)
	}
	if strings.Contains(p.Draft, "Before you ship") {
		t.Fatalf("the fallback draft carries a block:\n%s", p.Draft)
	}

	recorded := j.ask(t, stream, "late", recordBrief(p, "", briefText, briefShipFirst))
	if len(recorded) != 1 || !strings.Contains(recorded[0], `"recorded":true`) {
		t.Fatalf("a late brief: %q", recorded)
	}
	late := j.present(t, stream)
	if late.Brief == nil || late.Brief.State != api.BriefRecorded || late.Brief.Text != briefText || !strings.Contains(late.Draft, "\nBefore you ship:\n- "+briefShipFirst+"\n") {
		t.Fatalf("the late brief: %+v\n%s", late.Brief, late.Draft)
	}
	entries := j.entries(t, stream)
	if len(entries) != 1 || strings.Contains(entries[0].Question, api.NoBriefRecorded) {
		t.Fatalf("the entry still says no brief was recorded: %+v", entries)
	}
}

func TestReassembledWorkstreamNeedsANewBriefAndKeepsNoEarlierItems(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, "")
	stream := j.Stream
	const earlier = "Run the one-way migration first"
	j.chief.plan(1, briefPlan{Text: "First brief.", Items: []string{earlier}})
	j.run(t)
	j.awaitEntry(t, stream)
	first := j.present(t, stream)
	if first.Brief == nil || first.Brief.State != api.BriefRecorded || !strings.Contains(first.Draft, "- "+earlier+"\n") {
		t.Fatalf("the first review's presentation: %+v\n%s", first.Brief, first.Draft)
	}
	j.requireNoProblems(t)
	j.halt(t)

	second := j.reassemble(t, map[string]string{"internal/trace/resume.go": "package trace\n// Resume from the last acknowledged chunk.\n"})
	if second.Review != 2 || second.Commit == first.Report.Commit {
		t.Fatalf("the new final review %+v after %+v", second, first.Report)
	}
	delayed := recordBrief(first, "", "Delayed.", "Old item that must not ship")
	j.chief.plan(2, briefPlan{Early: []map[string]any{delayed.Input}})
	j.chief.hold()
	j.run(t)
	j.awaitStarts(t, 2)
	intakefixture.Eventually(t, "the chief of staff never repeated its call for review 1", func() bool { return len(j.chief.earlyResults(2)) == 1 })

	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the entry of the new review is listed with no brief for it: %+v", entries)
	}
	waiting := j.present(t, stream)
	if waiting.Report.Review != 2 || waiting.Brief == nil || waiting.Brief.State != api.BriefWaiting || waiting.Brief.Review != 2 || waiting.Brief.Text != "" || len(waiting.Brief.BeforeYouShip) != 0 {
		t.Fatalf("the earlier brief is current for the new review: %+v", waiting.Brief)
	}
	if strings.Contains(waiting.Draft, earlier) || strings.Contains(waiting.Draft, "Before you ship") {
		t.Fatalf("the earlier items reached the new draft:\n%s", waiting.Draft)
	}
	early := j.chief.earlyResults(2)
	if len(early) != 1 || !strings.Contains(early[0], `"recorded":false`) || !strings.Contains(early[0], "not the current final review") {
		t.Fatalf("the delayed call naming review 1: %q", early)
	}
	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the delayed call released the entry: %+v", entries)
	}

	j.chief.release()
	entry := j.awaitEntry(t, stream)
	if !strings.Contains(entry.Question, api.NoBriefRecorded) {
		t.Fatalf("the entry of the new review: %q", entry.Question)
	}
	fallback := j.present(t, stream)
	if fallback.Brief == nil || fallback.Brief.State != api.BriefNoneRecorded || strings.Contains(fallback.Draft, earlier) || strings.Contains(fallback.Draft, "Old item") {
		t.Fatalf("the fallback of the new review: %+v\n%s", fallback.Brief, fallback.Draft)
	}

	const current = "Confirm the new chunk size is deployed"
	recorded := j.ask(t, stream, "new-brief", recordBrief(fallback, "", "Second brief.", current))
	if len(recorded) != 1 || !strings.Contains(recorded[0], `"recorded":true`) {
		t.Fatalf("the new brief: %q", recorded)
	}
	p := j.present(t, stream)
	if p.Brief == nil || p.Brief.State != api.BriefRecorded || p.Brief.Review != 2 || p.Brief.Text != "Second brief." || !strings.Contains(p.Draft, "\nBefore you ship:\n- "+current+"\n") || strings.Contains(p.Draft, earlier) {
		t.Fatalf("the new presentation: %+v\n%s", p.Brief, p.Draft)
	}
	approved, err := j.approvePresented(p)
	fixturekit.Must(t, err)
	j.halt(t)
	body := j.publish(t)
	if body != approved.Description || !strings.Contains(body, "- "+current+"\n") || strings.Contains(body, earlier) || strings.Contains(body, "Old item") {
		t.Fatalf("the published body of the new review:\n%s", body)
	}
	if brief, found, err := delivery.ReadBrief(j.Repository, stream, 1, first.ReviewRevision); err != nil || !found || brief.Text != "First brief." {
		t.Fatalf("review 1's brief after the new one: %+v %v %v", brief, found, err)
	}
}

func TestApprovalOfAnEarlierReadIsRefusedAndTheBodyWithoutItemsIsUnchanged(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, "")
	stream := j.Stream
	j.run(t)
	j.awaitEntry(t, stream)
	read := j.present(t, stream)
	if read.Brief == nil || read.Brief.State != api.BriefNoneRecorded || strings.Contains(read.Draft, "Before you ship") {
		t.Fatalf("the presentation the owner read: %+v\n%s", read.Brief, read.Draft)
	}

	recorded := j.ask(t, stream, "with-items", recordBrief(read, "", briefText, briefShipFirst))
	if len(recorded) != 1 || !strings.Contains(recorded[0], `"recorded":true`) {
		t.Fatalf("the brief with items: %q", recorded)
	}
	changed := j.present(t, stream)
	if changed.DraftHash == read.DraftHash || !strings.Contains(changed.Draft, "- "+briefShipFirst+"\n") {
		t.Fatalf("the items did not change the draft:\n%s", changed.Draft)
	}
	_, err := j.approvePresented(read)
	var refusal *api.APIError
	if !errors.As(err, &refusal) || refusal.Code != api.Conflict || !strings.Contains(refusal.Message, "the final report or draft description changed") {
		t.Fatalf("an approval of the earlier read: %v", err)
	}
	if after := j.present(t, stream); after.Approval != nil {
		t.Fatalf("the refused approval was recorded: %+v", after.Approval)
	}

	replaced := j.ask(t, stream, "without-items", recordBrief(read, "", briefText))
	if len(replaced) != 1 || !strings.Contains(replaced[0], `"recorded":true`) {
		t.Fatalf("the brief without items: %q", replaced)
	}
	unchanged := j.present(t, stream)
	if unchanged.Brief == nil || unchanged.Brief.State != api.BriefRecorded || unchanged.Brief.Text != briefText || unchanged.Draft != read.Draft || unchanged.DraftHash != read.DraftHash || unchanged.Title != read.Title {
		t.Fatalf("a brief without items changed the presentation: %+v\n%s\nwas:\n%s", unchanged.Brief, unchanged.Draft, read.Draft)
	}
	approved, err := j.approvePresented(read)
	fixturekit.Must(t, err)
	if approved.Description != read.Draft {
		t.Fatalf("the approved description:\n%s", approved.Description)
	}
	j.halt(t)
	body := j.publish(t)
	if body != read.Draft || strings.Contains(body, "Before you ship") || strings.Contains(body, briefText) {
		t.Fatalf("the published body of a workstream with no items:\n%s\nread:\n%s", body, read.Draft)
	}
}

func TestSharedChiefBriefsEachMemberWithItsOwnItems(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := newBriefFixture(t, "")
	first, repository := j.BuildFixture().SeedGit(t, "group-first")
	j.Stream, j.Repository = first, repository
	j.host(t)
	j.S.SetSole(&journey.ActiveProject{Repository: repository})
	second := j.secondMember(t, "group-second")
	_, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{first, second}, fixturekit.DemoStart, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	j.assemble(t, first, "internal/trace", "first-resume", "first-dedupe")
	j.assemble(t, second, "internal/trace", "second-resume", "second-dedupe")
	j.run(t)
	for _, member := range []config.WorkstreamID{first, second} {
		j.awaitEntry(t, member)
	}

	const itemFirst, itemSecond = "Rotate FIRST_KEY", "Run the SECOND migration"
	read := map[config.WorkstreamID]api.DeliveryPresentation{first: j.present(t, first), second: j.present(t, second)}
	results := j.ask(t, first, "group",
		recordBrief(read[first], first, "The first member's brief.", itemFirst),
		recordBrief(read[second], second, "The second member's brief.", itemSecond),
		recordBrief(read[second], "", "No member named.", "Nobody's item"))
	if len(results) != 3 || !strings.Contains(results[0], `"recorded":true`) || !strings.Contains(results[1], `"recorded":true`) || !strings.Contains(results[2], `"recorded":false`) || !strings.Contains(results[2], "must name") {
		t.Fatalf("the shared chief's calls: %q", results)
	}
	want := map[config.WorkstreamID][2]string{first: {itemFirst, itemSecond}, second: {itemSecond, itemFirst}}
	bodies := map[config.WorkstreamID]string{}
	for member, items := range want {
		p := j.present(t, member)
		if p.Brief == nil || p.Brief.State != api.BriefRecorded || !strings.Contains(p.Brief.Text, "member's brief.") || !slices.Equal(p.Brief.BeforeYouShip, []string{items[0]}) {
			t.Fatalf("%s's brief: %+v", member, p.Brief)
		}
		if !strings.Contains(p.Draft, "\nBefore you ship:\n- "+items[0]+"\n") || strings.Contains(p.Draft, items[1]) || strings.Contains(p.Draft, "Nobody's item") {
			t.Fatalf("%s's description does not carry its own items alone:\n%s", member, p.Draft)
		}
		approved, err := j.approvePresented(p)
		fixturekit.Must(t, err)
		bodies[member] = approved.Description
	}
	if bodies[first] == bodies[second] {
		t.Fatal("two members approved the same description")
	}
	for member, items := range want {
		if !strings.Contains(bodies[member], "- "+items[0]+"\n") || strings.Contains(bodies[member], items[1]) {
			t.Fatalf("%s's approved description:\n%s", member, bodies[member])
		}
	}
}

func (j *briefJourney) secondMember(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	base := strings.TrimSpace(fixturekit.DemoGit(t, j.Clone, "-C", j.Clone, "rev-parse", "HEAD"))
	return journey.RecordBuild(t, j.ShedFixture, j.Repository, key, fixturekit.ValidPlan, config.WorkspacesGit, base)
}
