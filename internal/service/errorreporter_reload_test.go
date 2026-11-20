package service

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/errorreport"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

const (
	reloadA = "owner/alpha"
	reloadB = "owner/beta"
)

// reconcilingRig starts a reporter on reloadA whose first create is refused
// with a 503 before GitHub applies it, so its intent is unresolved and the
// retry reconciles it. The fake holds that reconciliation's issue list until
// the test releases it. It returns once the list is outstanding.
func reconcilingRig(t *testing.T) (*reporterRig, *fixturekit.Fault) {
	t.Helper()
	r := newReporterRig(t, section(reloadA), func(o *ErrorReporterOptions) { o.Retry = 20 * time.Millisecond })
	r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultCreate, Repo: reloadA, Effect: fixturekit.StatusBeforeApply, Status: 503})
	hold := r.gh.Inject(fixturekit.Fault{Kind: fixturekit.FaultListIssues, Repo: reloadA, Effect: fixturekit.HoldBeforeServe, Applied: make(chan struct{}, 1), Release: make(chan struct{})})
	r.opFail("workspace.draft", "draft failed")
	select {
	case <-hold.Applied:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciliation never read the issue list")
	}
	return r, hold
}

// afterRelease lets the held response go and waits for the reporter to act on
// it.
func afterRelease(hold *fixturekit.Fault) {
	close(hold.Release)
	time.Sleep(150 * time.Millisecond)
}

// A reload to another repository while a reconciliation read is outstanding
// stops the old pass from writing: the empty list that returns leads to no
// create or comment in the old repository, its intent stays saved there, and an
// error captured after the switch is filed in the new repository with the new
// repository's environment.
func TestErrorReporterReloadStopsAReconciliationInFlight(t *testing.T) {
	t.Parallel()
	r, hold := reconcilingRig(t)
	r.reload(section(reloadB))
	afterRelease(hold)

	if got := r.gh.Count("POST", "/repos/"+reloadA); got != 1 {
		t.Fatalf("%d writes reached %s, want only the refused create", got, reloadA)
	}
	if n := len(r.gh.Issues(reloadA)); n != 0 {
		t.Fatalf("%s holds %d issues", reloadA, n)
	}
	st := r.state()
	if len(st.Intents) != 1 || st.Intents[0].Repository != reloadA || st.Intents[0].Kind != errorreport.IntentCreate {
		t.Fatalf("the unresolved intent was not kept under %s: %+v", reloadA, st.Intents)
	}
	if st.Intents[0].Attempts != 1 {
		t.Fatalf("the dropped write was counted: %d attempts", st.Intents[0].Attempts)
	}
	// Only the refused create's own diagnostic shows; the dropped write adds
	// no attempt and no message.
	cfg, status := r.diagnostics()
	for _, m := range append(cfg, status...) {
		if !strings.Contains(m, "attempt 1 of 5") {
			t.Fatalf("a dropped write changed the diagnostic: %q", m)
		}
	}

	r.opFail("workspace.draft", "pull of "+reloadA+" and "+reloadB+" failed")
	issues := r.awaitIssues(reloadB, 1)
	r.quiet(reloadB, 1)
	if !strings.Contains(issues[0].Body, reloadB) || strings.Contains(issues[0].Body, reloadA) {
		t.Fatalf("the issue is not built with %s's environment:\n%s", reloadB, issues[0].Body)
	}
	if got := r.gh.Count("POST", "/repos/"+reloadA); got != 1 {
		t.Fatalf("%d writes reached %s", got, reloadA)
	}
	if n := len(r.gh.Issues(reloadA)); n != 0 {
		t.Fatalf("%s holds %d issues", reloadA, n)
	}
}

// A reload that turns the reporter off while a reconciliation read is
// outstanding leaves no request after the read returns.
func TestErrorReporterReloadThatDisablesStopsAReconciliationInFlight(t *testing.T) {
	t.Parallel()
	r, hold := reconcilingRig(t)
	r.reload("[error_reports]\nenabled = false\nrepository = \"" + reloadA + "\"\n")
	before := len(r.gh.Requests())
	afterRelease(hold)
	time.Sleep(100 * time.Millisecond)

	if after := r.gh.Requests(); len(after) != before {
		t.Fatalf("requests reached GitHub after the reload turned the reporter off: %+v", after[before:])
	}
	if got := r.gh.Count("POST", "/repos/"+reloadA); got != 1 {
		t.Fatalf("%d writes reached %s, want only the refused create", got, reloadA)
	}
	if n := len(r.gh.Issues(reloadA)); n != 0 {
		t.Fatalf("%s holds %d issues", reloadA, n)
	}
	if len(r.state().Intents) != 1 {
		t.Fatalf("the unresolved intent was dropped: %+v", r.state().Intents)
	}
	if cfg, status := r.diagnostics(); len(cfg) != 0 || len(status) != 0 {
		t.Fatalf("a disabled reporter shows a diagnostic: %v %v", cfg, status)
	}
}

// After the switch away from an interrupted reconciliation, switching back
// reconciles the saved intent: it adopts the issue when the server has it and
// files once when it does not, and the other repository is untouched.
func TestErrorReporterReloadBackReconcilesTheInterruptedWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		exists bool
	}{{"issue exists", true}, {"issue does not exist", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, hold := reconcilingRig(t)
			r.reload(section(reloadB))
			afterRelease(hold)
			r.opFail("workspace.draft", "pull of "+reloadB+" failed")
			r.awaitIssues(reloadB, 1)
			r.quiet(reloadA, 0)

			intent := r.state().Intents[0]
			if tc.exists {
				rendered, err := errorreport.RenderIssue(intent.Filing)
				must(t, err)
				r.gh.Seed(reloadA, rendered.Title, rendered.Body, intent.Labels...)
			}
			r.reload(section(reloadA))
			r.awaitResolved()
			r.quiet(reloadA, 1)
			r.quiet(reloadB, 1)

			if rec := r.state().Record(reloadA, intent.Fingerprint); rec == nil || rec.Issue != 1 {
				t.Fatalf("the issue is not saved for the fingerprint: %+v", rec)
			}
			creates := r.gh.Count("POST", "/repos/"+reloadA+"/issues")
			if want := map[bool]int{true: 1, false: 2}[tc.exists]; creates != want {
				t.Fatalf("%d creates in %s, want %d", creates, reloadA, want)
			}
			if n := len(r.gh.Comments(reloadA, 1)); n != 0 {
				t.Fatalf("%d comments on the issue", n)
			}
		})
	}
}
