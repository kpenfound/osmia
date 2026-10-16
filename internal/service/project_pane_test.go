package service

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/runtime"
)

func TestProjectPaneStatesRequireRecordedMerge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, state, wait    string
		paused, open, merged bool
		want                 string
	}{
		{name: "handed", state: HandedState, want: "planning"},
		{name: "sealed", state: RatifiedState, want: "ready"},
		{name: "building", state: BuildingState, want: "building"},
		{name: "pause", state: BuildingState, paused: true, want: "paused"},
		{name: "edge", state: BuildingState, wait: "waiting on owner: repo-a#123 merge", open: true, want: "blocked"},
		{name: "publication", state: DeliveredState, open: true, want: "PR open"},
		{name: "merged", state: DeliveredState, merged: true, want: "delivered (merged)"},
		{name: "cancel", state: AbandonedState, paused: true, want: "cancelled"},
		{name: "no merge evidence", state: DeliveredState, want: "building"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paneState(&c.state, c.wait, c.paused, c.open, c.merged); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestArchivedProjectPaneRequiresPinnedResolution(t *testing.T) {
	for _, tc := range []struct {
		name, state, evidence, link, want string
		resolved                          bool
	}{
		{name: "unresolved PR link", state: DeliveredState, evidence: "unresolved", link: "https://example.invalid/pull/42", want: "blocked"},
		{name: "no publication", state: DeliveredState, evidence: "unresolved", want: "blocked"},
		{name: "legacy ready flag", state: DeliveredState, resolved: true, link: "https://example.invalid/pull/42", want: "blocked"},
		{name: "merged", state: DeliveredState, evidence: "merged", resolved: true, want: "delivered (merged)"},
		{name: "cancelled", state: AbandonedState, evidence: "cancelled", resolved: true, want: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo, _, _ := ownPRFixture(t)
			a := runtime.Archive{State: tc.state, ResolutionEvidence: tc.evidence, Resolved: tc.resolved, DeliveryLink: tc.link}
			w := WorkstreamStatus{Project: repo.Project(), Workstream: stream, State: &tc.state}
			must(t, presentArchivedWorkstream(repo, &w, a))
			if w.DisplayState != tc.want {
				t.Fatalf("label=%s want=%s", w.DisplayState, tc.want)
			}
			if tc.want == "blocked" && !strings.Contains(w.Wait, "no pinned merge or cancel evidence") {
				t.Fatalf("archive hold lacks its diagnostic: %+v", w)
			}
		})
	}
}
