package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func archivedMemberFixture(t *testing.T) (*journey.ProductionDesignFixture, config.WorkstreamID, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	f := resolutionApprovedFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait}, "", nil)
	deliverResolutionMembers(t, f)
	archived, survivor := f.Stream1, f.Stream2
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, repo, archived)
	if _, err := f.C.Archive(ctx, archived); err != nil {
		if _, found := intakefixture.ArchiveRecord(t, f.S.Store, archived); !found {
			t.Fatal(err)
		}
	}
	journey.LifecycleAwait(t, f, "merged member archived", func() bool {
		a, found := intakefixture.ArchiveRecord(t, f.S.Store, archived)
		return found && a.Resolved && a.ResolutionEvidence == "merged"
	})
	return f, archived, survivor
}

func assertArchivedMemberSnapshot(t *testing.T, c lifecycle.WorkgroupChange, archived, survivor config.WorkstreamID) {
	t.Helper()
	i := slices.IndexFunc(c.Members, func(m amendment.Member) bool { return m.Workstream == archived })
	j := slices.IndexFunc(c.Members, func(m amendment.Member) bool { return m.Workstream == survivor })
	if i < 0 || j < 0 {
		t.Fatalf("snapshot lost a member: %+v", c.Members)
	}
	if m := c.Members[i]; !m.Retired || !m.Merged || m.State != intake.DeliveredState {
		t.Fatalf("archived member was not read from its archive record: %+v", m)
	}
	if m := c.Members[j]; m.Retired || m.State != intake.DeliveredState {
		t.Fatalf("survivor snapshot: %+v", m)
	}
}

func impactRow(c lifecycle.WorkgroupChange, stream config.WorkstreamID) amendment.StreamImpact {
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
	defer f.Stop(t)
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
	refused, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "frozen-slice", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests in every member."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	assertArchivedMemberSnapshot(t, refused, archived, survivor)
	if refused.State != "drafting" || refused.Held() {
		t.Fatalf("request with an archived member was held: %+v", refused)
	}
	journey.LifecycleAwait(t, f, "frozen-slice draft refused", func() bool {
		return journey.CurrentChange(t, f.S, f.Origin, refused.ID).State == "failed"
	})
	assertFrozenSliceRefusal(t, f, survivor, refused.ID, archived)

	changeArchived.Store(false)
	c, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "survivor-repair", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	assertArchivedMemberSnapshot(t, c, archived, survivor)
	journey.LifecycleAwait(t, f, "survivor amendment presented", func() bool {
		c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
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
	if after, _ := trace.SliceFor(c.After, archived); !intakefixture.EqualJSONTest(before, after) {
		t.Fatal("presented proposal changed the archived member's slice")
	}
	if _, apiFault := f.S.Lifecycle().DecideChange(ctx, string(f.Origin), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"}); apiFault != nil {
		t.Fatal(apiFault)
	}
	journey.LifecycleAwait(t, f, "survivor amendment applied", func() bool {
		c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
		return c.State == "applied"
	})
	if !slices.Equal(c.Applied, []config.WorkstreamID{survivor}) || len(c.Retired) != 0 || c.Failure != "" {
		t.Fatalf("applied change: applied=%v retired=%v failure=%q", c.Applied, c.Retired, c.Failure)
	}
	if kept, _ := trace.SliceFor(*c.Result, archived); !intakefixture.EqualJSONTest(before, kept) {
		t.Fatal("published project spec changed the archived member's slice")
	}
	repo, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](repo, survivor)
	fixturekit.Must(t, err)
	revised := false
	for _, doc := range docs {
		if doc.ID != amendment.DocumentID("group-"+c.ID) {
			continue
		}
		var app amendment.Application
		fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &app))
		revised = slices.Equal(app.Added, []string{"diagnostics"}) && app.To.Seal == app.From.Seal+1
	}
	if !revised {
		t.Fatal("survivor did not receive its revision")
	}
	feature, err := repo.Workflow(survivor, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.BuildingState {
		t.Fatalf("survivor did not return to building for its open pull request: %s", feature.Value)
	}
}

func TestArchivedMemberLeavesSurvivorCancellationToApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, archived, survivor := archivedMemberFixture(t)
	defer f.Stop(t)
	if _, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "cancel-archived", Kind: "cancel", Scope: "workstream", Workstream: archived, Change: "Cancel the archived member."}); apiFault == nil || !strings.Contains(apiFault.Message, "archived and already terminal") {
		t.Fatalf("cancelling an archived member: %v", apiFault)
	}
	c, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "cancel-survivor", Kind: "cancel", Scope: "workstream", Workstream: survivor, Change: "Cancel the open member."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	assertArchivedMemberSnapshot(t, c, archived, survivor)
	if row := impactRow(c, archived); row.Action != amendment.Leave {
		t.Fatalf("archived member impact: %+v", row)
	}
	if row := impactRow(c, survivor); row.Action != amendment.Cancel {
		t.Fatalf("survivor impact: %+v", row)
	}
	journey.LifecycleAwait(t, f, "cancellation presented", func() bool {
		c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
		return c.State == "presented"
	})
	if _, apiFault := f.S.Lifecycle().DecideChange(ctx, string(f.Origin), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "draft"}); apiFault != nil {
		t.Fatal(apiFault)
	}
	journey.LifecycleAwait(t, f, "survivor cancellation applied", func() bool {
		c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
		return c.State == "applied"
	})
	if !slices.Equal(c.Applied, []config.WorkstreamID{survivor}) || len(c.Retired) != 0 {
		t.Fatalf("applied cancellation: applied=%v retired=%v", c.Applied, c.Retired)
	}
	repo, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	feature, err := repo.Workflow(survivor, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.AbandonedState {
		t.Fatalf("survivor was not cancelled: %s", feature.Value)
	}
}

func TestHeldChangeReportsHeldToTheChief(t *testing.T) {
	t.Parallel()
	held := lifecycle.WorkgroupChange{ID: "held", State: "drafting", Failure: "the requester's ruling is unavailable"}.ToolResult()
	if held.State != "held" || held.Reason != "the requester's ruling is unavailable" {
		t.Fatalf("held change reported %+v", held)
	}
	data, err := json.Marshal(held)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), `"state":"held"`) || strings.Contains(string(data), `"state":"drafting"`) {
		t.Fatalf("held change JSON: %s", data)
	}
	if open := (lifecycle.WorkgroupChange{ID: "open", State: "drafting"}).ToolResult(); open.State != "drafting" || open.Reason != "" {
		t.Fatalf("open change reported %+v", open)
	}
	if failed := (lifecycle.WorkgroupChange{ID: "failed", State: "failed", Failure: "no draft"}).ToolResult(); failed.State != "failed" {
		t.Fatalf("failed change reported %+v", failed)
	}
}

func TestAFailedChangeIsRetriedAndDraftsAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, archived, _ := archivedMemberFixture(t)
	defer f.Stop(t)
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
	c, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Origin), lifecycle.ChangeRequest{Key: "retried", Kind: "amend", Scope: "workgroup", Change: "Refuse invalid diagnostic requests."})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	journey.LifecycleAwait(t, f, "first draft failed", func() bool {
		return journey.CurrentChange(t, f.S, f.Origin, c.ID).State == "failed"
	})
	first := drafts.Load()

	changeArchived.Store(false)
	retried, apiFault := f.S.Lifecycle().RetryChange(ctx, f.Origin, c.ID)
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if retried.State != "drafting" || retried.Attempt != 1 || retried.Failure != "" {
		t.Fatalf("retried change: %+v", retried)
	}
	journey.LifecycleAwait(t, f, "retried change presented", func() bool {
		c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
		return c.State == "presented"
	})
	if drafts.Load() <= first {
		t.Fatalf("the retry drafted nothing: %d drafts before, %d after", first, drafts.Load())
	}
	if _, apiFault := f.S.Lifecycle().RetryChange(ctx, f.Origin, c.ID); apiFault == nil || !strings.Contains(apiFault.Message, "only a change still drafting or debating") {
		t.Fatalf("a presented change was retried: %v", apiFault)
	}
}
