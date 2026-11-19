package executionjourney

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type DeliveryLandDecision = api.DeliveryLandDecision

func (s *Service) approveAndLand(ctx context.Context, raw string, req DeliveryLandDecision) (api.DeliveryLandResponse, *APIError) {
	return s.delivery().ApproveAndLand(ctx, raw, req)
}

// newLandJourney is an assembled workstream of a project that opted in to
// landing, and so pushes to its upstream, with its final review recorded: the
// draft pull request is open on a fake host and checks passed on the pushed
// commit. Jev is not configured.
func newLandJourney(t *testing.T, findings ...any) *draftFixture {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, "land", "")
	d := &draftFixture{f: f, stream: stream, repository: repository, a: a}
	d.fork = strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "remote", "get-url", "upstream"))
	d.pulls = &fakePulls{SyncHead: true, Fork: func() string { commit, _ := d.forkBranch(t); return commit }}
	f.s.setSole(&activeProject{repository: repository})
	f.s.cfg.Project.Fork = ""
	f.s.cfg.Project.Delivery = config.DeliveryJev
	f.s.options.PullRequests = d.pulls
	f.s.options.reviewChecks = checkFunc(func(context.Context, string) (CheckResult, error) {
		return CheckResult{Output: "all proofs passed"}, nil
	})
	_, d.first = assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	if findings == nil {
		findings = []any{}
	}
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": findings})
		return err
	})
	if result := d.settle(t, d.first); result.Outcome != "succeeded" {
		t.Fatalf("final review %+v", result)
	}
	return d
}

// upstreamRev is where the upstream's ref is.
func (d *draftFixture) upstreamRev(t *testing.T, ref string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, filepath.Dir(d.f.clone), "-C", d.fork, "rev-parse", ref))
}

func (d *draftFixture) landDecision(p DeliveryPresentation, accept bool) DeliveryLandDecision {
	return DeliveryLandDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash, AcceptBlocking: accept}
}

func (d *draftFixture) approvals(t *testing.T) int {
	t.Helper()
	return len(streamDocuments(t, d.repository, d.stream, deliveryDocument))
}

// On a project that opted in, the delivery presentation and the inbox entry
// offer Approve and land, and approving it with Jev unconfigured moves the
// base branch to the pushed commit by a fast-forward, leaves the feature branch
// in place, marks no pull request ready and delivers the workstream.
func TestApproveAndLandFastForwardsTheBaseToThePushedCommitWithJevUnconfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newLandJourney(t)
	pushed, ok := d.forkBranch(t)
	if !ok {
		t.Fatal("the draft's commit was not pushed")
	}
	base := d.upstreamRev(t, "refs/heads/main")

	presented := d.present(t)
	if !presented.CanLand || presented.Recommendation != "" {
		t.Fatalf("the presentation does not offer landing: can land %v, recommendation %q", presented.CanLand, presented.Recommendation)
	}
	entry, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream)
	must(t, err)
	if !found || strings.Join(entry.Options, ",") != "approve,land" || entry.Land == nil || entry.Land.Method != "POST" || !strings.HasSuffix(entry.Land.Path, "/delivery/"+string(d.stream)+"/land") || entry.Land.Body["draft_hash"] != presented.DraftHash {
		t.Fatalf("the inbox entry does not offer landing: %+v", entry)
	}

	if d.f.s.jev != nil {
		t.Fatal("Jev is configured")
	}
	response, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), d.landDecision(presented, false))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if response.Outcome != api.LandOutcomeLanded || response.Commit != pushed || response.Approval.Route != api.DeliveryRouteLand || response.Approval.Commit != presented.Report.Commit {
		t.Fatalf("the response %+v, want landed at %s", response, pushed)
	}
	if got := d.upstreamRev(t, "refs/heads/main"); got != pushed {
		t.Fatalf("the base branch is at %s, want the pushed commit %s", got, pushed)
	}
	if merges := strings.TrimSpace(demoGit(t, filepath.Dir(d.f.clone), "-C", d.fork, "rev-list", "--merges", base+".."+pushed)); merges != "" {
		t.Fatalf("landing created merge commits: %s", merges)
	}
	if tip, ok := d.forkBranch(t); !ok || tip != pushed {
		t.Fatalf("the feature branch is at %s (kept %v), want %s", tip, ok, pushed)
	}
	if d.pulls.Readies != 0 || d.pulls.Creates != 1 {
		t.Fatalf("landing marked %d pull requests ready and opened %d", d.pulls.Readies, d.pulls.Creates)
	}
	if got := d.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	if _, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream); err != nil || found {
		t.Fatalf("a delivered workstream still has a delivery card: %v %v", found, err)
	}
	if again, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), d.landDecision(presented, false)); apiErr == nil || apiErr.Code != Conflict {
		t.Fatalf("landing again: %+v %v", again, apiErr)
	}
	if got := d.upstreamRev(t, "refs/heads/main"); got != pushed {
		t.Fatalf("the second request moved the base branch to %s", got)
	}
}

// Approve and land is refused as a conflict, with nothing recorded or pushed,
// for a stale presentation, blocking findings that are not accepted, checks
// that did not pass on the pushed commit, and a project that delivers by pull
// request, which has no offer of it either.
func TestApproveAndLandIsRefusedWithNothingPushed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blocking := map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}
	for _, c := range []struct {
		name   string
		build  func(t *testing.T) *draftFixture
		change func(t *testing.T, d *draftFixture, req *DeliveryLandDecision)
		accept bool
		reason string
	}{
		{name: "a stale presentation", build: func(t *testing.T) *draftFixture { return newLandJourney(t) },
			change: func(t *testing.T, d *draftFixture, req *DeliveryLandDecision) { req.DraftHash = "stale" },
			reason: "changed; read the current presentation"},
		{name: "a stale review revision", build: func(t *testing.T) *draftFixture { return newLandJourney(t) },
			change: func(t *testing.T, d *draftFixture, req *DeliveryLandDecision) { req.ReviewRevision++ },
			reason: "changed; read the current presentation"},
		{name: "unaccepted blocking findings", build: func(t *testing.T) *draftFixture { return newLandJourney(t, blocking) },
			reason: "has blocking findings"},
		{name: "checks that did not pass", build: func(t *testing.T) *draftFixture { return newLandJourney(t) },
			change: func(t *testing.T, d *draftFixture, req *DeliveryLandDecision) { d.failChecks(t) },
			reason: "did not pass"},
		{name: "a project that delivers by pull request", build: func(t *testing.T) *draftFixture {
			d := newLandJourney(t)
			d.f.s.cfg.Project.Delivery = config.DeliveryPullRequest
			return d
		}, reason: `delivery is "pull-request"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.build(t)
			base, branch := d.upstreamRev(t, "refs/heads/main"), d.upstreamRev(t, "refs/heads/"+featureBranch(d.stream))
			var req DeliveryLandDecision
			if d.f.s.cfg.Project.Lands() {
				p := d.present(t)
				req = d.landDecision(p, c.accept)
				if !p.CanLand {
					t.Fatal("the presentation does not offer landing")
				}
			} else {
				p := d.present(t)
				if p.CanLand {
					t.Fatal("a project that delivers by pull request is offered landing")
				}
				if entry, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream); err != nil || !found || entry.Land != nil || strings.Join(entry.Options, ",") != "approve" {
					t.Fatalf("the inbox entry %+v %v %v", entry, found, err)
				}
				req = d.landDecision(p, false)
			}
			if c.change != nil {
				c.change(t, d, &req)
			}
			approvals := d.approvals(t)
			response, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), req)
			if apiErr == nil || apiErr.Code != Conflict || !strings.Contains(apiErr.Message, c.reason) {
				t.Fatalf("approving to land gave %+v %v, want a conflict naming %q", response, apiErr, c.reason)
			}
			if got := d.upstreamRev(t, "refs/heads/main"); got != base {
				t.Fatalf("the base branch moved to %s", got)
			}
			if got := d.upstreamRev(t, "refs/heads/"+featureBranch(d.stream)); got != branch {
				t.Fatalf("the feature branch moved to %s", got)
			}
			if d.approvals(t) != approvals || d.feature(t) != AssembledState || d.pulls.Readies != 0 {
				t.Fatalf("a refused landing changed the workstream: %d approvals (was %d), %s, %d ready", d.approvals(t), approvals, d.feature(t), d.pulls.Readies)
			}
			if ops := fixturekitLandings(t, d); ops != 0 {
				t.Fatalf("a refused landing recorded %d landing operations", ops)
			}
		})
	}
}

// failChecks records that the checks of the pushed commit failed, as a later
// read of the pull request's statuses would.
func (d *draftFixture) failChecks(t *testing.T) {
	t.Helper()
	docs := streamDocuments(t, d.repository, d.stream, finalChecksDocument(1))
	last := docs[len(docs)-1]
	var run delivery.FinalCheckRun
	must(t, json.Unmarshal([]byte(last.Content), &run))
	run.Status, run.Failed = checkrun.ChecksFailed, []string{"unit"}
	content, err := json.Marshal(run)
	must(t, err)
	last.Revision++
	last.Content = string(content)
	must(t, d.repository.RecordDocuments(context.Background(), []trace.Document{last}))
}

// fixturekitLandings counts the workstream's landing operations.
func fixturekitLandings(t *testing.T, d *draftFixture) int {
	t.Helper()
	records, err := d.repository.Operations(d.stream)
	must(t, err)
	n := 0
	for _, r := range records {
		if delivery.LandingAction(r.Operation) {
			n++
		}
	}
	return n
}

// When the upstream refuses the push after the owner approved landing, the
// draft pull request is marked ready and the workstream delivered with a
// notice, the base branch is untouched, and no second delivery card appears.
func TestApproveAndLandTakesTheFallbackWhenTheUpstreamRefusesThePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newLandJourney(t)
	hook := "#!/bin/sh\nwhile read old new ref; do\n  if [ \"$ref\" = refs/heads/main ]; then\n    echo 'remote: error: GH006: Protected branch update failed for refs/heads/main.' >&2\n    exit 1\n  fi\ndone\n"
	must(t, os.WriteFile(filepath.Join(d.fork, "hooks", "pre-receive"), []byte(hook), 0700))
	base := d.upstreamRev(t, "refs/heads/main")
	presented := d.present(t)

	response, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), d.landDecision(presented, false))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if response.Outcome != api.LandOutcomeFellBack || !strings.Contains(response.Reason, "refused") {
		t.Fatalf("the response %+v, want a fallback after the refusal", response)
	}
	if got := d.upstreamRev(t, "refs/heads/main"); got != base {
		t.Fatalf("the refused push moved the base branch to %s", got)
	}
	if d.pulls.Readies != 1 || d.pulls.Prs[0].Draft || len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 {
		t.Fatalf("pull requests %+v, %d ready, %d created", d.pulls.Prs, d.pulls.Readies, d.pulls.Creates)
	}
	if got := d.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	if _, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream); err != nil || found {
		t.Fatalf("a second delivery card appeared: %v %v", found, err)
	}
	notices, err := d.repository.Outbox(d.stream)
	must(t, err)
	var said bool
	for _, n := range notices {
		said = said || strings.Contains(n.Event.Body, "delivered without landing") && strings.Contains(n.Event.Body, "Protected branch")
	}
	if !said {
		t.Fatal("no notice says why the workstream did not land")
	}
}

// An approval to land is not an approval to mark the pull request ready: the
// publication pass leaves the draft alone and its delivery gate says so, and
// the delivery card is offered again until a landing takes the approval up.
// Approving to land over an approval to mark ready is refused, and marking
// ready replaces an approval to land.
func TestAnApprovalToLandIsNotPublishedAsMarkReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newLandJourney(t)
	presented := d.present(t)
	pushed, _ := d.forkBranch(t)

	approval, apiErr := d.decide(presented)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Route != "" {
		t.Fatalf("marking ready recorded the route %q", approval.Route)
	}
	if _, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), d.landDecision(presented, false)); apiErr == nil || apiErr.Code != Conflict || !strings.Contains(apiErr.Message, "already approved") {
		t.Fatalf("landing over a standing approval to mark ready: %v", apiErr)
	}

	// The same approval, recorded as one to land, as a landing that did not
	// start leaves it.
	docs := streamDocuments(t, d.repository, d.stream, deliveryDocument)
	land := docs[len(docs)-1]
	approval.Route = api.DeliveryRouteLand
	content, err := json.Marshal(approval)
	must(t, err)
	land.Revision++
	land.Content = string(content)
	must(t, d.repository.RecordDocuments(ctx, []trace.Document{land}))
	if _, reason, err := d.f.s.deliveryGate(ctx, d.repository, d.stream, approval.Description); err != nil || !strings.Contains(reason, "approved landing") {
		t.Fatalf("the delivery gate for an approval to land: %q, %v", reason, err)
	}
	p := &publisher{s: d.f.s, repository: d.repository}
	must(t, p.Pass(ctx))
	if ops := fixturekitPublications(t, d); ops != 0 || d.pulls.Readies != 0 {
		t.Fatalf("an approval to land was published: %d operations, %d ready", ops, d.pulls.Readies)
	}
	if entry, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream); err != nil || !found || entry.Land == nil {
		t.Fatalf("the card is not offered while no landing is under way: %+v %v %v", entry, found, err)
	}

	// While a landing is asked for and has no outcome, the card stays away and
	// marking ready is refused.
	push := d.pushes(t)[0]
	_, err = (&delivery.Lander{S: d.f.s.delivery(), Repository: d.repository}).Request(ctx, d.stream, delivery.LandInput{Landing: 1, Push: push.Number, Review: push.Review, Reviewed: push.Reviewed, Commit: push.Commit})
	must(t, err)
	if entry, found, err := d.f.s.delivery().DeliveryEntry(ctx, d.repository, d.stream); err != nil || found {
		t.Fatalf("the card came back while a landing is under way: %+v %v %v", entry, found, err)
	}
	if _, apiErr := d.decide(presented); apiErr == nil || apiErr.Code != Conflict || !strings.Contains(apiErr.Message, "landing") {
		t.Fatalf("marking ready while a landing is under way: %v", apiErr)
	}

	// Approving again lands under the approval that stands and the landing
	// asked for, without a second of either.
	approvals := d.approvals(t)
	response, apiErr := d.f.s.approveAndLand(ctx, string(d.stream), d.landDecision(presented, false))
	if apiErr != nil || response.Outcome != api.LandOutcomeLanded {
		t.Fatalf("landing under the standing approval: %+v %v", response, apiErr)
	}
	if d.approvals(t) != approvals || fixturekitLandings(t, d) != 1 || d.upstreamRev(t, "refs/heads/main") != pushed {
		t.Fatalf("%d approvals, was %d; base at %s, want %s", d.approvals(t), approvals, d.upstreamRev(t, "refs/heads/main"), pushed)
	}
}

// fixturekitPublications counts the workstream's publication operations.
func fixturekitPublications(t *testing.T, d *draftFixture) int {
	t.Helper()
	records, err := d.repository.Operations(d.stream)
	must(t, err)
	n := 0
	for _, r := range records {
		if delivery.PublicationAction(r.Operation) {
			n++
		}
	}
	return n
}
