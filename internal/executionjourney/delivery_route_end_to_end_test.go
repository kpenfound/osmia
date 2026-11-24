package executionjourney

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type routeWorld struct {
	t        *testing.T
	f        *journey.ShedFixture
	stream   config.WorkstreamID
	down     config.WorkstreamID
	pulls    *fixturekit.FakePulls
	hook     *fixturekit.Webhook
	provider *jevfixture.Provider
	upstream string
	base     string

	pushes string

	cards    atomic.Int64
	passes   *atomic.Int64
	stopCard chan struct{}
	cardDone chan struct{}
}

type routeOptions struct {
	jev *jevfixture.Provider

	delivery string

	boundary func(context.Context, string) error

	later []any
}

const downstreamStream = config.WorkstreamID("w_00000000000000000000000000000c02")

func confident(option string, confidence float64) *jevfixture.Provider {
	return &jevfixture.Provider{Confidence: confidence, Pick: func(options []string) string {
		for _, o := range options {
			if o == option {
				return o
			}
		}
		if slices.Contains(options, "defer") {
			return "defer"
		}
		return options[0]
	}}
}

func routeDecisions(p *jevfixture.Provider) int {
	if p == nil {
		return 0
	}
	n := 0
	for _, r := range p.Decisions() {
		if names := jevfixture.Options(r); len(names) == 3 && names[0] == "land" {
			n++
		}
	}
	return n
}

func newRouteWorld(t *testing.T, o routeOptions) *routeWorld {
	t.Helper()
	ctx := context.Background()
	w := &routeWorld{t: t, provider: o.jev, hook: fixturekit.NewWebhook(t), pulls: &fixturekit.FakePulls{SyncHead: true}, stopCard: make(chan struct{}), cardDone: make(chan struct{})}
	passes, onPass := intakefixture.ScheduleCounter()
	w.passes = passes
	f := journey.NewDebateFixtureWith(t, 1, 1, "", func(opts *journey.Options) {
		opts.SchedulePassed = onPass
		opts.PullRequests = w.pulls
		if o.jev != nil {
			withJev(t, o.jev)(opts)
		}
		w.hook.Records(opts.Config.Root)
		fixturekit.SetWebhook(t, opts.Config.Root, w.hook.Hook())
		opts.NotifyRetry = 10 * time.Millisecond
	})
	w.f = f

	saved, err := f.C.ProjectConfig(ctx, f.Project)
	fixturekit.Must(t, err)
	none := ""
	set := api.ProjectConfigValues{Fork: &none}
	if o.delivery != "" {
		set.Delivery = &o.delivery
	}
	_, err = f.C.SaveProjectConfig(ctx, f.Project, api.ProjectConfigEdit{Pin: saved.Pin, Set: set})
	fixturekit.Must(t, err)
	f.S.Cfg = f.S.Current()

	var repository *trace.Repository
	w.stream, repository = f.BuildFixture().SeedGit(t, "route")
	home := filepath.Dir(f.Clone)
	w.upstream = strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "remote", "get-url", "upstream"))
	w.pulls.Mu.Lock()

	w.pulls.Branches = func(branch string) string {
		tip, _ := fixturekit.PublicationForkTip(w.upstream, branch)
		return tip
	}
	w.pulls.Mu.Unlock()
	w.base = w.rev("refs/heads/main")
	w.pushes = filepath.Join(home, "main-updates")
	hookScript := "#!/bin/sh\nwhile read old new ref; do\n  if [ \"$ref\" = refs/heads/main ]; then\n    echo \"$new\" >> '" + w.pushes + "'\n  fi\ndone\n"
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.upstream, "hooks", "pre-receive"), []byte(hookScript), 0700))

	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	a := &journey.FinalReviewer{S: f.S, Repository: repository}
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": []any{}})
		return err
	})

	for k := 2; k <= 4; k++ {
		later := o.later
		if later == nil {
			later = []any{}
		}
		f.FinalTurn(k, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
			_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume again.", "findings": later})
			return err
		})
	}
	journey.AssembleBoth(t, f, repository, a, w.stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})

	at := f.S.Now()
	fixturekit.Must(t, repository.CreateWorkstream(ctx, downstreamStream, at, fixturekit.ServiceActor))
	g, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{w.stream, downstreamStream}, at, fixturekit.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, repository, g, trace.ProjectSpec{Behavior: "The downstream workstream needs this one.", Graph: []trace.Edge{{From: w.stream, To: downstreamStream, Type: trace.EdgeHardBlock}}}, at, fixturekit.OwnerActor, "fixture"))
	w.down = downstreamStream
	fixturekit.Must(t, repository.Close())

	if o.boundary != nil {
		f.Opts.Boundary = o.boundary
	}
	f.Start(t)
	go w.watchCards()
	t.Cleanup(func() {
		select {
		case <-w.stopCard:
		default:
			close(w.stopCard)
		}
		<-w.cardDone

		w.f.Stop(t)
	})
	return w
}

func (w *routeWorld) watchCards() {
	defer close(w.cardDone)
	for {
		select {
		case <-w.stopCard:
			return
		case <-time.After(15 * time.Millisecond):
		}
		inbox, err := w.f.C.Inbox(context.Background())
		if err != nil {
			continue
		}
		for _, e := range inbox.Entries {
			if e.Workstream == w.stream {
				w.cards.Add(1)
			}
		}
	}
}

func (w *routeWorld) repository() *trace.Repository { return w.f.Repository() }

func (w *routeWorld) rev(ref string) string {
	w.t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(w.t, filepath.Dir(w.f.Clone), "-C", w.upstream, "rev-parse", ref))
}

func (w *routeWorld) mainUpdates() int {
	data, err := os.ReadFile(w.pushes)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	fixturekit.Must(w.t, err)
	return len(strings.Fields(string(data)))
}

func (w *routeWorld) feature() string {
	w.t.Helper()
	state, err := w.repository().Workflow(w.stream, trace.FeatureSubject)
	fixturekit.Must(w.t, err)
	return state.Value
}

func (w *routeWorld) edgeWait() string {
	w.t.Helper()
	state, err := w.repository().Workflow(w.down, lifecycle.EdgeWaitSubject)
	fixturekit.Must(w.t, err)
	return state.Value
}

func (w *routeWorld) advance(d time.Duration) {
	w.f.Clock.Mu.Lock()
	w.f.Clock.At = w.f.Clock.At.Add(d)
	w.f.Clock.Mu.Unlock()
}

func (w *routeWorld) await(what string, step time.Duration, cond func() bool) {
	w.t.Helper()
	intakefixture.Soon(w.t, what, func() bool {
		if cond() {
			return true
		}
		if step > 0 {
			w.advance(step)
		}
		return false
	})
}

func (w *routeWorld) advanceThroughPasses() {
	w.t.Helper()
	for range 3 {
		w.advance(11 * time.Minute)
		intakefixture.AwaitFurtherPasses(w.t, w.passes, 1)
	}
}

func (w *routeWorld) awaitDraft() delivery.Push {
	w.t.Helper()
	var push delivery.Push
	w.await("the draft pull request", 0, func() bool {
		pushes, err := delivery.Pushes(w.repository(), w.stream)
		if err != nil || len(pushes) == 0 {
			return false
		}
		push = pushes[len(pushes)-1]
		return true
	})
	return push
}

func (w *routeWorld) awaitCard() api.InboxEntry {
	w.t.Helper()
	var card api.InboxEntry
	w.await("the delivery card", 0, func() bool {
		inbox, err := w.f.C.Inbox(context.Background())
		if err != nil {
			return false
		}
		for _, e := range inbox.Entries {
			if e.Kind == api.InboxDelivery && e.Workstream == w.stream {
				card = e
				return true
			}
		}
		return false
	})
	return card
}

func (w *routeWorld) pull() pulls.PullRequest {
	w.t.Helper()
	w.pulls.Mu.Lock()
	defer w.pulls.Mu.Unlock()
	if len(w.pulls.Prs) != 1 {
		w.t.Fatalf("the host holds %d pull requests", len(w.pulls.Prs))
	}
	return w.pulls.Prs[0]
}

func (w *routeWorld) hostCounts() (creates, readies, closes int) {
	w.pulls.Mu.Lock()
	defer w.pulls.Mu.Unlock()
	return w.pulls.Creates, w.pulls.Readies, w.pulls.Closes
}

func (w *routeWorld) landings() int {
	w.t.Helper()
	records, err := w.repository().Operations(w.stream)
	fixturekit.Must(w.t, err)
	n := 0
	for _, r := range records {
		if delivery.LandingAction(r.Operation) {
			n++
		}
	}
	return n
}

func (w *routeWorld) landed() (delivery.DeliveryLanding, bool) {
	w.t.Helper()
	landing, found, err := delivery.ReadDeliveryLanding(w.repository(), w.stream)
	fixturekit.Must(w.t, err)
	return landing, found
}

func (w *routeWorld) delivered() trace.Transition {
	w.t.Helper()
	var found []trace.Transition
	for _, tr := range w.f.Transitions(w.t, w.stream) {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.DeliveredState {
			found = append(found, tr)
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("the workstream was delivered %d times: %+v", len(found), found)
	}
	return found[0]
}

func (w *routeWorld) noPullRequestWasMerged() {
	w.t.Helper()
	w.pulls.Mu.Lock()
	defer w.pulls.Mu.Unlock()
	for _, pr := range w.pulls.Prs {
		if pr.Merged {
			w.t.Fatalf("the host shows a merged pull request: %+v", pr)
		}
	}
}

func (w *routeWorld) posts(contains ...string) int {
	n := 0
	for _, body := range w.hook.Received() {
		match := true
		for _, want := range contains {
			match = match && strings.Contains(body, want)
		}
		if match {
			n++
		}
	}
	return n
}

func (w *routeWorld) requireLanded(push delivery.Push, decidedBy string) {
	w.t.Helper()
	t := w.t
	w.await("the workstream to be delivered", 0, func() bool { return w.feature() == intake.DeliveredState })
	if got := w.rev("refs/heads/main"); got != push.Commit {
		t.Fatalf("the base branch is at %s, want the signed pushed commit %s", got, push.Commit)
	}
	if merges := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(w.f.Clone), "-C", w.upstream, "rev-list", "--merges", w.base+".."+push.Commit)); merges != "" {
		t.Fatalf("landing created merge commits: %s", merges)
	}
	if got := w.rev("refs/heads/" + intake.FeatureBranch(w.stream)); got != push.Commit {
		t.Fatalf("the feature branch is at %s, want it kept at %s", got, push.Commit)
	}
	if n := w.mainUpdates(); n != 1 {
		t.Fatalf("the base branch was updated %d times", n)
	}
	landing, found := w.landed()
	if !found || landing.Commit != push.Commit || landing.Confirmed != push.Commit || landing.BaseBefore != w.base || landing.DecidedBy != decidedBy || landing.BaseBranch != "main" {
		t.Fatalf("the landing record is %+v (found %v)", landing, found)
	}
	if creates, readies, _ := w.hostCounts(); creates != 1 || readies != 0 {
		t.Fatalf("landing opened %d pull requests and marked %d ready", creates, readies)
	}

	w.await("the downstream workstream to be released", 11*time.Minute, func() bool { return w.edgeWait() == "available" })
	w.noPullRequestWasMerged()

	w.await("the leftover draft to be closed", 11*time.Minute, func() bool { _, _, closes := w.hostCounts(); return closes == 1 })
	w.pulls.Mu.Lock()
	comments := append([]fixturekit.PullComment(nil), w.pulls.Comments...)
	w.pulls.Mu.Unlock()
	if len(comments) != 1 || !strings.Contains(comments[0].Text, push.Commit) || !strings.Contains(comments[0].Text, "main") {
		t.Fatalf("the comment on the closed draft: %+v", comments)
	}
	if got := w.pull().State; got != "closed" {
		t.Fatalf("the draft is %s", got)
	}

	w.await("the owner's notification", 0, func() bool { return w.posts("Landed commit: "+push.Commit) > 0 })
	if n := w.posts("Landed commit: "+push.Commit, "main of dagger/dagger"); n != 1 {
		t.Fatalf("%d notifications name the landed commit and the base branch: %q", n, w.hook.Received())
	}

	w.advanceThroughPasses()
	if _, _, closes := w.hostCounts(); closes != 1 || w.feature() != intake.DeliveredState || w.landings() != 1 || w.mainUpdates() != 1 {
		t.Fatalf("later polls changed the outcome: %d closes, %s, %d landings, %d base updates", closes, w.feature(), w.landings(), w.mainUpdates())
	}
	summary, err := w.f.C.Trace(context.Background(), w.stream)
	fixturekit.Must(t, err)
	if summary.Delivery == nil || summary.Delivery.Merged != push.Commit {
		t.Fatalf("the trace shows the delivery as %+v", summary.Delivery)
	}
	if inbox, err := w.f.C.Inbox(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range inbox.Entries {
			if e.Workstream == w.stream {
				t.Fatalf("a landed workstream still has an owner card: %+v", e)
			}
		}
	}
}

func (w *routeWorld) requireNoCards() {
	w.t.Helper()
	if n := w.cards.Load(); n != 0 {
		w.t.Fatalf("the owner's inbox showed a card for the workstream %d times", n)
	}
	entries, err := w.repository().Inbox()
	fixturekit.Must(w.t, err)
	if len(entries) != 0 {
		w.t.Fatalf("%d entries were escalated to the owner: %+v", len(entries), entries)
	}
}

func TestJevLandsACleanWorkstreamThroughTheRunningService(t *testing.T) {
	t.Parallel()
	p := confident("land", 0.97)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryJev})
	push := w.awaitDraft()
	w.await("the downstream workstream to wait", 0, func() bool { return w.edgeWait() == "waiting" || w.feature() == intake.DeliveredState })
	w.requireLanded(push, trace.DecidedByJev)
	if d := w.delivered(); d.DecidedBy != trace.DecidedByJevGate || d.Judgment == "" {
		t.Fatalf("the workstream was delivered by %+v", d)
	}
	if n := routeDecisions(p); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	w.requireNoCards()
}

func TestTheOwnerApprovesAndLandsWithJevUnconfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newRouteWorld(t, routeOptions{delivery: config.DeliveryJev})
	push := w.awaitDraft()
	card := w.awaitCard()
	if strings.Join(card.Options, ",") != "approve,land" || card.Land == nil || card.Jev != nil {
		t.Fatalf("the delivery card is %+v", card)
	}
	w.await("the downstream workstream to wait", 0, func() bool { return w.edgeWait() == "waiting" })
	presented, err := w.f.C.Delivery(ctx, w.stream)
	fixturekit.Must(t, err)
	if !presented.CanLand || presented.Recommendation != "" {
		t.Fatalf("the presentation %+v", presented)
	}
	if w.rev("refs/heads/main") != w.base || w.feature() != intake.AssembledState {
		t.Fatal("the workstream landed before the owner approved")
	}

	response, err := w.f.C.ApproveAndLand(ctx, w.stream, api.DeliveryLandDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	fixturekit.Must(t, err)
	if response.Outcome != api.LandOutcomeLanded || response.Commit != push.Commit || response.Approval.Route != api.DeliveryRouteLand {
		t.Fatalf("the response is %+v", response)
	}
	w.requireLanded(push, trace.DecidedByOwner)
	if d := w.delivered(); d.DecidedBy != trace.DecidedByOwner {
		t.Fatalf("the workstream was delivered by %+v", d)
	}
}

func TestARefusedLandingFallsBackToTheDraftWithoutAnOwnerCard(t *testing.T) {
	t.Parallel()
	p := confident("land", 0.97)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryJev})
	hook := "#!/bin/sh\nwhile read old new ref; do\n  if [ \"$ref\" = refs/heads/main ]; then\n    echo 'remote: error: GH006: Protected branch update failed for refs/heads/main.' >&2\n    exit 1\n  fi\ndone\n"
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.upstream, "hooks", "pre-receive"), []byte(hook), 0700))
	push := w.awaitDraft()

	w.await("the workstream to be delivered", 0, func() bool { return w.feature() == intake.DeliveredState })
	w.await("the draft to be marked ready", 0, func() bool { _, readies, _ := w.hostCounts(); return readies == 1 })
	if creates, readies, _ := w.hostCounts(); creates != 1 || readies != 1 {
		t.Fatalf("%d pull requests opened and %d marked ready", creates, readies)
	}
	if pr := w.pull(); pr.Draft || pr.State != "open" || pr.HeadCommit != push.Commit {
		t.Fatalf("the pull request is %+v", pr)
	}
	if got := w.rev("refs/heads/main"); got != w.base {
		t.Fatalf("the refused push moved the base branch to %s", got)
	}
	if _, found := w.landed(); found {
		t.Fatal("a landing is recorded though the push was refused")
	}
	notices, err := w.repository().Outbox(w.stream)
	fixturekit.Must(t, err)
	said := false
	for _, n := range notices {
		said = said || strings.Contains(n.Event.Body, "delivered without landing") && strings.Contains(n.Event.Body, "Protected branch")
	}
	if !said {
		t.Fatalf("no notice says why the workstream did not land: %+v", notices)
	}
	key := jev.DeliveryRouteKey(w.stream, push.Review, push.Commit)
	res, err := jev.Recorded(w.repository(), w.stream, jev.KindDeliveryRoute, key)
	fixturekit.Must(t, err)
	if res.Status != jev.Binding || res.Refused != "" {
		t.Fatalf("Jev's pick is %+v: the fallback is a completed pull request delivery, not a refusal", res)
	}

	w.advanceThroughPasses()
	if n := routeDecisions(p); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	if _, _, closes := w.hostCounts(); closes != 0 || w.pull().State != "open" {
		t.Fatalf("the polls closed the delivered pull request: %d closes", closes)
	}
	if w.landings() > 1 || w.feature() != intake.DeliveredState || w.mainUpdates() != 0 {
		t.Fatalf("%d landings, %s, %d base updates", w.landings(), w.feature(), w.mainUpdates())
	}
	w.requireNoCards()
}

func TestAnUnsureJevLeavesTheMarkReadyCardWithItsRecommendation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := confident("land", 0.5)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryJev})
	w.awaitDraft()
	card := w.awaitCard()
	if strings.Join(card.Options, ",") != "approve,land" || card.Land == nil || card.Jev == nil || card.Jev.Pick != "land" || card.Jev.Binding || card.Recommendation == "" {
		t.Fatalf("the delivery card is %+v", card)
	}
	if !strings.Contains(card.Recommendation, "land") {
		t.Fatalf("the recommendation is %q", card.Recommendation)
	}
	presented, err := w.f.C.Delivery(ctx, w.stream)
	fixturekit.Must(t, err)
	if !presented.CanLand || !strings.Contains(presented.Recommendation, "land") {
		t.Fatalf("the presentation offers landing %v with the recommendation %q", presented.CanLand, presented.Recommendation)
	}
	if n := routeDecisions(p); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	if pr := w.pull(); !pr.Draft {
		t.Fatalf("the draft was marked ready: %+v", pr)
	}
	if creates, readies, closes := w.hostCounts(); creates != 1 || readies != 0 || closes != 0 {
		t.Fatalf("%d opened, %d marked ready, %d closed", creates, readies, closes)
	}
	if w.rev("refs/heads/main") != w.base || w.feature() != intake.AssembledState || w.landings() != 0 {
		t.Fatal("the workstream moved without a decision")
	}
}

func TestAPullRequestProjectDeliversThroughMarkReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := confident("land", 0.99)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryPullRequest})
	w.awaitDraft()
	card := w.awaitCard()
	if strings.Join(card.Options, ",") != "approve" || card.Land != nil || card.Jev == nil || !card.Jev.Deferred || card.Jev.Binding || strings.Contains(card.Recommendation, "land") {
		t.Fatalf("the delivery card is %+v", card)
	}
	presented, err := w.f.C.Delivery(ctx, w.stream)
	fixturekit.Must(t, err)
	if presented.CanLand || strings.Contains(presented.Recommendation, "land") {
		t.Fatalf("the presentation %+v", presented)
	}
	decision := api.DeliveryLandDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash}
	if _, err := w.f.C.ApproveAndLand(ctx, w.stream, decision); err == nil || !strings.Contains(err.Error(), `delivery is "pull-request"`) {
		t.Fatalf("approving to land on a project that delivers by pull request: %v", err)
	}
	if pr := w.pull(); !pr.Draft {
		t.Fatalf("the draft was marked ready without the owner: %+v", pr)
	}

	approved, err := w.f.C.ApproveDelivery(ctx, w.stream, api.DeliveryDecision{Review: decision.Review, ReviewRevision: decision.ReviewRevision, Commit: decision.Commit, DraftHash: decision.DraftHash})
	fixturekit.Must(t, err)
	if approved.Route != "" {
		t.Fatalf("marking ready recorded the route %q", approved.Route)
	}
	w.await("the workstream to be delivered", 0, func() bool { return w.feature() == intake.DeliveredState })
	if pr := w.pull(); pr.Draft || pr.State != "open" {
		t.Fatalf("the pull request is %+v", pr)
	}
	if creates, readies, closes := w.hostCounts(); creates != 1 || readies != 1 || closes != 0 {
		t.Fatalf("%d opened, %d marked ready, %d closed", creates, readies, closes)
	}
	if w.rev("refs/heads/main") != w.base || w.mainUpdates() != 0 {
		t.Fatal("the base branch moved")
	}
	if _, found := w.landed(); found || w.landings() != 0 {
		t.Fatal("a landing was recorded")
	}
	if n := routeDecisions(p); n != 0 {
		t.Fatalf("Jev was asked %d delivery-route decisions", n)
	}
	for _, r := range p.Decisions() {
		if names := jevfixture.Options(r); strings.Join(names, ",") != "approve,defer" {
			t.Fatalf("Jev was asked to choose among %v", names)
		}
	}
	if d := w.delivered(); d.DecidedBy != trace.DecidedByOwner {
		t.Fatalf("the workstream was delivered by %+v", d)
	}
	if w.posts("Landed commit") != 0 {
		t.Fatal("the owner was told of a landing")
	}
}

func TestARestartAfterTheLandingPushCompletesTheLandingOnce(t *testing.T) {
	t.Parallel()
	var pushed sync.Once
	reached := make(chan struct{})
	var restarted atomic.Bool
	boundary := func(ctx context.Context, step string) error {
		if step != "land-pushed" || restarted.Load() {
			return nil
		}
		pushed.Do(func() { close(reached) })
		<-ctx.Done()
		return errors.New("the service stopped")
	}
	p := confident("land", 0.97)
	w := newRouteWorld(t, routeOptions{jev: p, delivery: config.DeliveryJev, boundary: boundary})
	push := w.awaitDraft()
	select {
	case <-reached:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the landing never pushed")
	}
	if got := w.rev("refs/heads/main"); got != push.Commit || w.mainUpdates() != 1 {
		t.Fatalf("the upstream is at %s after %d updates, want the pushed %s", got, w.mainUpdates(), push.Commit)
	}
	if w.feature() == intake.DeliveredState {
		t.Fatal("the landing completed before the service stopped")
	}
	if _, found := w.landed(); found {
		t.Fatal("a landing is recorded before the service stopped")
	}

	restarted.Store(true)
	w.f.Stop(t)
	w.f.Start(t)

	w.await("the workstream to be delivered after the restart", 0, func() bool { return w.feature() == intake.DeliveredState })
	w.requireLanded(push, trace.DecidedByJev)
	if got := w.rev("refs/heads/main"); got != push.Commit || w.mainUpdates() != 1 {
		t.Fatalf("the base branch is at %s after %d updates", got, w.mainUpdates())
	}
	if got := w.rev("refs/heads/" + intake.FeatureBranch(w.stream)); got != push.Commit {
		t.Fatalf("the feature branch moved to %s: the branch was rebased", got)
	}
	if creates, readies, _ := w.hostCounts(); creates != 1 || readies != 0 {
		t.Fatalf("%d pull requests opened and %d marked ready", creates, readies)
	}
	if reviews := len(journey.FinalOperations(t, w.repository(), w.stream)); reviews != 1 {
		t.Fatalf("%d final reviews ran: the branch was rebased and reviewed again", reviews)
	}
	if w.delivered().DecidedBy != trace.DecidedByJevGate {
		t.Fatal("the restarted landing was not decided by Jev's gate")
	}
}
