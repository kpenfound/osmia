package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// archivedMemberFixture is an approved two-member workgroup whose first
// member delivered, merged and was archived while the second stays open.
func archivedMemberFixture(t *testing.T) (*productionDesignFixture, config.WorkstreamID, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	f := resolutionApprovedFixture(t, productionDesignConfiguration{wait: lifecycleAwait}, "", nil)
	deliverResolutionMembers(t, f)
	archived, survivor := f.stream1, f.stream2
	repo, err := f.s.repository(f.project)
	must(t, err)
	recordResolutionMerge(t, repo, archived)
	if _, err := f.c.Archive(ctx, archived); err != nil {
		if _, found := archiveRecord(t, f.s.store, archived); !found {
			t.Fatal(err)
		}
	}
	lifecycleAwait(t, f, "merged member archived", func() bool {
		a, found := archiveRecord(t, f.s.store, archived)
		return found && a.Resolved && a.ResolutionEvidence == "merged"
	})
	return f, archived, survivor
}

func assertArchivedMemberSnapshot(t *testing.T, c WorkgroupChange, archived, survivor config.WorkstreamID) {
	t.Helper()
	i := slices.IndexFunc(c.Members, func(m amendment.Member) bool { return m.Workstream == archived })
	j := slices.IndexFunc(c.Members, func(m amendment.Member) bool { return m.Workstream == survivor })
	if i < 0 || j < 0 {
		t.Fatalf("snapshot lost a member: %+v", c.Members)
	}
	if m := c.Members[i]; !m.Retired || !m.Merged || m.State != DeliveredState {
		t.Fatalf("archived member was not read from its archive record: %+v", m)
	}
	if m := c.Members[j]; m.Retired || m.State != DeliveredState {
		t.Fatalf("survivor snapshot: %+v", m)
	}
}

func impactRow(c WorkgroupChange, stream config.WorkstreamID) amendment.StreamImpact {
	for _, row := range c.Impact.Streams {
		if row.Workstream == stream {
			return row
		}
	}
	return amendment.StreamImpact{}
}

func TestArchivedMemberLeavesSurvivorAmendmentToApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, archived, survivor := archivedMemberFixture(t)
	defer f.stop(t)
	var changeArchived atomic.Bool
	var drafts atomic.Int32
	installRetirementDelta(t, f, &drafts, func(after *trace.ProjectSpec) {
		for i := range after.Slices {
			sl := &after.Slices[i]
			if sl.Workstream == archived && !changeArchived.Load() {
				continue
			}
			p, err := plan.Parse([]byte(sl.Plan))
			if err != nil || len(p.Units) == 0 {
				continue
			}
			number := len(plan.ParseSpec(sl.Spec).Criteria) + 1
			sl.Spec += fmt.Sprintf("%d. Invalid diagnostic requests are refused.\n", number)
			p.Units = append(p.Units, plan.Unit{ID: "diagnostics", Task: "Refuse invalid diagnostic requests", Acceptance: []string{"Invalid diagnostic requests are refused"}, Criteria: []string{plan.Cite(number)}, DependsOn: []string{}, Footprint: slices.Clone(p.Units[0].Footprint)})
			data, err := plan.Encode(p)
			if err != nil {
				continue
			}
			sl.Plan = string(data)
		}
	})

	// A draft that changes the archived member's frozen slice is refused
	// with a reason naming that member; the change fails and holds nothing.
	changeArchived.Store(true)
	refused, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "frozen-slice", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests in every member."})
	if api != nil {
		t.Fatal(api)
	}
	assertArchivedMemberSnapshot(t, refused, archived, survivor)
	if refused.State != "drafting" || refused.Held() {
		t.Fatalf("request with an archived member was held: %+v", refused)
	}
	lifecycleAwait(t, f, "frozen-slice draft refused", func() bool {
		return currentChange(t, f.s, f.origin, refused.ID).State == "failed"
	})
	assertFrozenSliceRefusal(t, f, survivor, refused.ID, archived)

	// The same request limited to the survivor is presented, approved and
	// applied; the archived member receives no effect.
	changeArchived.Store(false)
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "survivor-repair", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests."})
	if api != nil {
		t.Fatal(api)
	}
	assertArchivedMemberSnapshot(t, c, archived, survivor)
	lifecycleAwait(t, f, "survivor amendment presented", func() bool {
		c = currentChange(t, f.s, f.origin, c.ID)
		if c.State == "failed" {
			t.Fatalf("survivor amendment failed: %s", c.Failure)
		}
		return c.State == "presented"
	})
	if row := impactRow(c, archived); row.Action != amendment.Leave || row.Pause || slices.ContainsFunc(row.Units, func(u amendment.UnitImpact) bool { return u.Action != amendment.Leave }) {
		t.Fatalf("archived member received an effect: %+v", row)
	}
	if row := impactRow(c, survivor); row.Action != amendment.Rework || !row.Pause {
		t.Fatalf("survivor impact: %+v", row)
	}
	before, _ := trace.SliceFor(c.Before, archived)
	if after, _ := trace.SliceFor(c.After, archived); !equalJSONTest(before, after) {
		t.Fatal("presented proposal changed the archived member's slice")
	}
	if _, api := f.s.decideChange(ctx, string(f.origin), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve"}); api != nil {
		t.Fatal(api)
	}
	lifecycleAwait(t, f, "survivor amendment applied", func() bool {
		c = currentChange(t, f.s, f.origin, c.ID)
		return c.State == "applied"
	})
	if !slices.Equal(c.Applied, []config.WorkstreamID{survivor}) || len(c.Retired) != 0 || c.Failure != "" {
		t.Fatalf("applied change: applied=%v retired=%v failure=%q", c.Applied, c.Retired, c.Failure)
	}
	if kept, _ := trace.SliceFor(*c.Result, archived); !equalJSONTest(before, kept) {
		t.Fatal("published project spec changed the archived member's slice")
	}
	repo, err := f.s.repository(f.project2)
	must(t, err)
	docs, err := trace.Read[trace.Document](repo, survivor)
	must(t, err)
	revised := false
	for _, doc := range docs {
		if doc.ID != amendment.DocumentID("group-"+c.ID) {
			continue
		}
		var app amendment.Application
		must(t, json.Unmarshal([]byte(doc.Content), &app))
		revised = slices.Equal(app.Added, []string{"diagnostics"}) && app.To.Seal == app.From.Seal+1
	}
	if !revised {
		t.Fatal("survivor did not receive its revision")
	}
	feature, err := repo.Workflow(survivor, trace.FeatureSubject)
	must(t, err)
	if feature.Value != BuildingState {
		t.Fatalf("survivor did not return to building for its open pull request: %s", feature.Value)
	}
}

func TestArchivedMemberLeavesSurvivorCancellationToApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, archived, survivor := archivedMemberFixture(t)
	defer f.stop(t)
	if _, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "cancel-archived", Kind: "cancel", Scope: "workstream", Workstream: archived, Change: "Cancel the archived member."}); api == nil || !strings.Contains(api.Message, "archived and already terminal") {
		t.Fatalf("cancelling an archived member: %v", api)
	}
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "cancel-survivor", Kind: "cancel", Scope: "workstream", Workstream: survivor, Change: "Cancel the open member."})
	if api != nil {
		t.Fatal(api)
	}
	assertArchivedMemberSnapshot(t, c, archived, survivor)
	if row := impactRow(c, archived); row.Action != amendment.Leave {
		t.Fatalf("archived member impact: %+v", row)
	}
	if row := impactRow(c, survivor); row.Action != amendment.Cancel {
		t.Fatalf("survivor impact: %+v", row)
	}
	lifecycleAwait(t, f, "cancellation presented", func() bool {
		c = currentChange(t, f.s, f.origin, c.ID)
		return c.State == "presented"
	})
	if _, api := f.s.decideChange(ctx, string(f.origin), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "draft"}); api != nil {
		t.Fatal(api)
	}
	lifecycleAwait(t, f, "survivor cancellation applied", func() bool {
		c = currentChange(t, f.s, f.origin, c.ID)
		return c.State == "applied"
	})
	if !slices.Equal(c.Applied, []config.WorkstreamID{survivor}) || len(c.Retired) != 0 {
		t.Fatalf("applied cancellation: applied=%v retired=%v", c.Applied, c.Retired)
	}
	repo, err := f.s.repository(f.project2)
	must(t, err)
	feature, err := repo.Workflow(survivor, trace.FeatureSubject)
	must(t, err)
	if feature.Value != AbandonedState {
		t.Fatalf("survivor was not cancelled: %s", feature.Value)
	}
}

func TestHeldChangeReportsHeldToTheChief(t *testing.T) {
	t.Parallel()
	held := WorkgroupChange{ID: "held", State: "drafting", Failure: "the requester's ruling is unavailable"}.ToolResult()
	if held.State != "held" || held.Reason != "the requester's ruling is unavailable" {
		t.Fatalf("held change reported %+v", held)
	}
	data, err := json.Marshal(held)
	must(t, err)
	if !strings.Contains(string(data), `"state":"held"`) || strings.Contains(string(data), `"state":"drafting"`) {
		t.Fatalf("held change JSON: %s", data)
	}
	if open := (WorkgroupChange{ID: "open", State: "drafting"}).ToolResult(); open.State != "drafting" || open.Reason != "" {
		t.Fatalf("open change reported %+v", open)
	}
	if failed := (WorkgroupChange{ID: "failed", State: "failed", Failure: "no draft"}).ToolResult(); failed.State != "failed" {
		t.Fatalf("failed change reported %+v", failed)
	}
}

// A change whose draft failed before it was presented is retried: it returns to
// drafting under a fresh attempt, the architect drafts it again, and it is
// presented; a change that was already presented is not retried.
func TestAFailedChangeIsRetriedAndDraftsAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, archived, _ := archivedMemberFixture(t)
	defer f.stop(t)
	var changeArchived atomic.Bool
	var drafts atomic.Int32
	installRetirementDelta(t, f, &drafts, func(after *trace.ProjectSpec) {
		for i := range after.Slices {
			sl := &after.Slices[i]
			if sl.Workstream == archived && !changeArchived.Load() {
				continue
			}
			p, err := plan.Parse([]byte(sl.Plan))
			if err != nil || len(p.Units) == 0 {
				continue
			}
			number := len(plan.ParseSpec(sl.Spec).Criteria) + 1
			sl.Spec += fmt.Sprintf("%d. Invalid diagnostic requests are refused.\n", number)
			p.Units = append(p.Units, plan.Unit{ID: "diagnostics", Task: "Refuse invalid diagnostic requests", Acceptance: []string{"Invalid diagnostic requests are refused"}, Criteria: []string{plan.Cite(number)}, DependsOn: []string{}, Footprint: slices.Clone(p.Units[0].Footprint)})
			data, err := plan.Encode(p)
			if err != nil {
				continue
			}
			sl.Plan = string(data)
		}
	})
	changeArchived.Store(true)
	c, api := f.s.requestChange(ctx, string(f.origin), ChangeRequest{Key: "retried", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests."})
	if api != nil {
		t.Fatal(api)
	}
	lifecycleAwait(t, f, "first draft failed", func() bool {
		return currentChange(t, f.s, f.origin, c.ID).State == "failed"
	})
	first := drafts.Load()

	changeArchived.Store(false)
	retried, api := f.s.retryChange(ctx, f.origin, c.ID)
	if api != nil {
		t.Fatal(api.Message)
	}
	if retried.State != "drafting" || retried.Attempt != 1 || retried.Failure != "" {
		t.Fatalf("retried change: %+v", retried)
	}
	lifecycleAwait(t, f, "retried change presented", func() bool {
		c = currentChange(t, f.s, f.origin, c.ID)
		return c.State == "presented"
	})
	if drafts.Load() <= first {
		t.Fatalf("the retry drafted nothing: %d drafts before, %d after", first, drafts.Load())
	}
	if _, api := f.s.retryChange(ctx, f.origin, c.ID); api == nil || !strings.Contains(api.Message, "only a change still drafting or debating") {
		t.Fatalf("a presented change was retried: %v", api)
	}
}
