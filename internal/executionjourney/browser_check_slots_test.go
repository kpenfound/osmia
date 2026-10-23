package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// capacityChecks holds actual runner calls while tracking their concurrency.
// The service still owns admission, selection, records and notifications.
type capacityChecks struct {
	*heldChecks
	mu         sync.Mutex
	used, peak int
}

func (c *capacityChecks) Check(ctx context.Context, dir string, identity api.CheckIdentity, links []string) (CheckResult, error) {
	c.mu.Lock()
	c.used++
	c.peak = max(c.peak, c.used)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.used--; c.mu.Unlock() }()
	return c.heldChecks.Check(ctx, dir, identity, links)
}

func TestBrowserCheckSlotsAcrossProjectsDrainWithSmartFinalReview(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, _ := newMasonFixture(t, 2, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	finalStream, repo := seedBuild(t, f, "browser-check-slots", validPlan, config.WorkspacesGit, base)
	reviewer := &finalReviewer{s: f.s, repository: repo}
	assembleBoth(t, f, repo, reviewer, finalStream,
		map[string]string{"one.txt": "one\n"}, map[string]string{"two.txt": "two\n"})
	// The final check selection must judge the rebased branch, just as a
	// normal final review does, rather than a synthetic check record.
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	var unitStreams []config.WorkstreamID
	for i := range 2 {
		stream := recordBuild(t, f, repo, fmt.Sprintf("slot-unit-%d", i), independentPlan, config.WorkspacesGit, base)
		seedChecking(t, f, repo, stream, "resume", resumeReport)
		unitStreams = append(unitStreams, stream)
	}
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The selected checks passed.", "findings": []any{}})
		return err
	})
	must(t, repo.Close())
	file, err := os.OpenFile(filepath.Join(f.opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	must(t, err)
	_, err = file.WriteString("\n[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, file.Close())
	checks := &capacityChecks{heldChecks: newHeldChecks()}
	checks.links = []string{"dag://go/check", "dag://release/check"}
	f.opts.reviewChecks = checks
	var unitsAllowed atomic.Int32
	var finalAllowed atomic.Bool
	f.opts.Reconciliation.Ticks = nil
	f.opts.Reconciliation.Interval = 20 * time.Millisecond
	f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		if op.Action == CheckAction {
			i := slices.Index(unitStreams, stream)
			return i < 0 || i >= int(unitsAllowed.Load())
		}
		return op.Action != FinalReviewAction || stream != finalStream || !finalAllowed.Load()
	}
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	if f.s.CheckSlots().Limit != 2 {
		t.Fatal("fixture must use the default two check slots")
	}
	provider := &jevtest.Provider{}
	for range 5 {
		provider.Results = append(provider.Results, jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{
			"check-0": jevtest.Noul(0.9), "check-1": jevtest.Noul(0.1),
		}}})
	}
	boostJev(f.s, provider)

	// Owner sessions in a second local project share the factory's slots
	// with the first project's real unit and final-review controllers.
	home := filepath.Dir(f.clone)
	clone := filepath.Join(home, "other-clone")
	bare := filepath.Join(home, "remotes", "acme", "other.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", f.clone, bare)
	demoGit(t, home, "clone", "--quiet", "--no-hardlinks", bare, clone)
	added, err := f.c.AddProject(ctx, ProjectAddRequest{Name: "other", Upstream: "acme/other", Clone: clone})
	must(t, err)
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	var sessions []config.WorkstreamID
	for range 2 {
		opened, err := f.c.OpenSession(ctx, SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
		must(t, err)
		sessions = append(sessions, opened.Session.Workstream)
	}

	p := openBrowser(t)
	// Capacity must update from the service's EventSource, even when no
	// configuration poll or page reload can refresh the status incidentally.
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`window.setInterval = () => 0;`).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.sameSlotDocument = true`, nil)
	p.Click("#capacity-button")
	const row = `#capacity [data-role=checks]`
	observe := func(used int, waiting []SlotWait) {
		t.Helper()
		soon(t, "check capacity from the status API", func() bool {
			status, err := f.c.Statuses(ctx)
			must(t, err)
			if status.Capacity == nil || len(status.Capacity.Roles) != 6 {
				return false
			}
			got := roleCapacity(t, status.Capacity, checksRole)
			return got.Limit == 2 && got.Used == used && reflect.DeepEqual(got.Waiting, waiting)
		})
		p.AwaitText(row+` [data-field=slots]`, fmt.Sprintf("%d / 2 slots", used))
		p.Await("check wait count", `document.querySelectorAll('`+row+` [data-field=waiting]').length === `+fmt.Sprint(len(waiting)))
		for i, w := range waiting {
			label := "unit " + w.Unit
			if w.Review != 0 {
				label = fmt.Sprintf("final review %d", w.Review)
			} else if w.Session {
				label = "session"
			}
			p.Await("waiting run identity", `document.querySelectorAll('`+row+` [data-field=waiting]')[`+fmt.Sprint(i)+`]?.textContent === `+quote(string(w.Workstream)+" "+label+": every slot is taken"))
		}
		header := fmt.Sprintf("checks: %d of 2 slots", used)
		if len(waiting) > 0 {
			header += fmt.Sprintf(", %d waiting", len(waiting))
		}
		p.Await("the header check meter", `document.querySelector('[data-meter=checks]')?.title === `+quote(header))
		p.Await("the same page document", `window.sameSlotDocument === true`)
	}
	noWait := []SlotWait{}
	observe(0, noWait)
	awaitRunner := func() {
		t.Helper()
		if got := awaitCheck(t, checks.entered); !slices.Equal(got, checks.links[:1]) {
			t.Fatalf("runner links %v, want smart selection %v", got, checks.links[:1])
		}
	}
	sessionDone := make(chan SessionCheckRun, 2)
	sessionErrors := make(chan error, 2)
	startSession := func(stream config.WorkstreamID) {
		go func() {
			run, err := f.c.SessionCheck(ctx, stream)
			sessionDone <- run
			sessionErrors <- err
		}()
	}
	startSession(sessions[0])
	awaitRunner()
	observe(1, noWait)
	unitsAllowed.Store(1)
	awaitRunner()
	observe(2, noWait)
	unitsAllowed.Store(2)
	unitWait := SlotWait{Project: f.project, Workstream: unitStreams[1], Unit: "resume", Reason: DeferCapacity}
	observe(2, []SlotWait{unitWait})
	finalAllowed.Store(true)
	finalWait := SlotWait{Project: f.project, Workstream: finalStream, Review: 1, Reason: DeferCapacity}
	observe(2, []SlotWait{unitWait, finalWait})
	startSession(sessions[1])
	sessionWait := SlotWait{Project: added.Project.ID, Workstream: sessions[1], Session: true, Reason: DeferCapacity}
	observe(2, []SlotWait{unitWait, finalWait, sessionWait})

	for _, waiting := range [][]SlotWait{{finalWait, sessionWait}, {sessionWait}, noWait} {
		checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
		awaitRunner()
		observe(2, waiting)
	}
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	observe(1, noWait)
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	observe(0, noWait)
	for range sessions {
		must(t, awaitCheck(t, sessionErrors))
		if run := awaitCheck(t, sessionDone); run.Status != ChecksPassed {
			t.Fatalf("session run %+v", run)
		}
	}
	repo, err = f.s.repository(f.project)
	must(t, err)
	for _, stream := range unitStreams {
		soon(t, "unit check result", func() bool {
			runs, err := checkRuns(repo, stream)
			must(t, err)
			return len(runs) == 1 && runs[0].Status == ChecksPassed
		})
	}
	soon(t, "final-review result", func() bool {
		docs := streamDocuments(t, repo, finalStream, finalChecksDocument(1))
		if len(docs) == 0 {
			return false
		}
		var run FinalCheckRun
		must(t, json.Unmarshal([]byte(docs[len(docs)-1].Content), &run))
		return run.Status == ChecksPassed && run.Selection != nil && run.Selection.Mode == checkrun.SelectionSelected &&
			slices.Equal(run.Selection.Links, checks.links[:1]) && run.Selection.Judgment != "" &&
			strings.HasSuffix(strings.Join(run.Command, " "), checks.links[0])
	})
	soon(t, "the final reader completes", func() bool {
		state, err := repo.Workflow(finalStream, finalReviewSubject)
		must(t, err)
		return state.Value == "reviewed-1"
	})
	checks.mu.Lock()
	defer checks.mu.Unlock()
	if checks.peak != 2 || checks.used != 0 {
		t.Fatalf("runner concurrency peak %d, still running %d", checks.peak, checks.used)
	}
	if got := len(provider.Requests()); got != 5 {
		t.Fatalf("got %d Jev requests for five check runs", got)
	}
}
