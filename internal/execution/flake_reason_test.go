package execution

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	reasonLink     = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestFlaky"
	reasonSibling  = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestOther"
	reasonFlakeFix = config.WorkstreamID("w_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func openFlakeFix(t *testing.T, repository *trace.Repository) string {
	t.Helper()
	ctx := context.Background()
	group := flakes.GroupFlaky(nil, []string{reasonLink})[0].Group
	store := &flakes.Store{Repository: repository}
	const run = "w_cccccccccccccccccccccccccccccccc/units/earlier/checks-1.json"
	_, _, err := store.Record(ctx, flakes.OccurrenceInput{Group: group, Run: run, Source: flakes.Source{Workstream: "w_cccccccccccccccccccccccccccccccc", Unit: "earlier"}, Links: []string{reasonLink}})
	must(t, err)
	_, err = store.Reserve(ctx, group, run)
	must(t, err)
	_, err = store.Opened(ctx, group, run, reasonFlakeFix)
	must(t, err)
	return group
}

func flakyPassingChecks(links ...string) *fakeChecks {
	output := failedReport(links[0]) + "CHECKS  ✔ 1 passed\n"
	for _, link := range links {
		output += "✔ " + link + " 1.0s OK\n"
	}
	return &fakeChecks{Outcomes: []checkOutcome{{Result: checkrun.CheckResult{Output: output, Flaky: links}}}}
}

func reviewingReason(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	if got := unitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	i := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.To == UnitReviewing })
	if i < 0 {
		t.Fatalf("no move to reviewing: %+v", transitions)
	}
	return transitions[i].Reason
}

func TestPassReasonNamesTheOpenFlakeFixWorkstreamOfAFlakyCheck(t *testing.T) {
	t.Parallel()
	f, stream, repository := newChecksFixture(t, "checks-flaky-open", flakyPassingChecks(reasonSibling))
	f.cfg.Project.FlakeFixes = true
	group := openFlakeFix(t, repository)
	runChecks(t, f.s, repository, stream)
	if reason := reviewingReason(t, repository, stream); !strings.Contains(reason, "flake-fix workstream "+string(reasonFlakeFix)+" is open for flake group "+group) || !strings.Contains(reason, checkrun.DescribeFlaky([]string{reasonSibling})) {
		t.Fatalf("the pass reason is %q", reason)
	}
}

func TestPassReasonNamesNoFlakeFixWorkstreamWithTheSettingOffOrAnotherGroup(t *testing.T) {
	t.Parallel()
	const elsewhere = "dag://go/packages/tests/test?go-package=internal/web&go-test=TestFlaky"
	for name, setup := range map[string]struct {
		on   bool
		link string
	}{"setting off": {false, reasonLink}, "another group": {true, elsewhere}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newChecksFixture(t, "checks-flaky-unnamed", flakyPassingChecks(setup.link))
			f.cfg.Project.FlakeFixes = setup.on
			openFlakeFix(t, repository)
			runChecks(t, f.s, repository, stream)
			if reason := reviewingReason(t, repository, stream); strings.Contains(reason, "flake-fix") {
				t.Fatalf("the pass reason is %q", reason)
			}
		})
	}
}

func TestPassReasonFallsBackSilentlyWhenTheFlakeRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	f, stream, repository := newChecksFixture(t, "checks-flaky-unreadable", flakyPassingChecks(reasonLink))
	f.cfg.Project.FlakeFixes = true
	group := flakes.GroupFlaky(nil, []string{reasonLink})[0].Group
	id := flakes.GroupID(group)
	must(t, repository.Append(context.Background(), trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.FlakeDocumentID(id), Revision: 1, Project: repository.Project(), At: time.Now(), Actor: trace.Actor{Kind: "service", ID: "fixture"}, Cause: "unreadable-record"},
		Path:   trace.FlakePath(id), Content: "this is not a flake record\n"}))
	runChecks(t, f.s, repository, stream)
	if reason := reviewingReason(t, repository, stream); strings.Contains(reason, "flake-fix") || !strings.Contains(reason, checkrun.DescribeFlaky([]string{reasonLink})) {
		t.Fatalf("the pass reason is %q", reason)
	}
}
