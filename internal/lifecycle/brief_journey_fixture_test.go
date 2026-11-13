package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// briefDirective prefixes an owner message that has the fake chief of staff
// make the tool calls the message carries, as one JSON object.
const briefDirective = "BRIEFCALL "

// briefNoticed finds the review and review revision a final review notice
// tells the chief of staff to name.
var briefNoticed = regexp.MustCompile(`naming review (\d+) and review_revision (\d+)`)

// ownPrompt returns the part of a chief-of-staff prompt that belongs to the
// turn itself. A turn that is replayed rather than resumed carries the
// thread's earlier exchanges, encoded on one line, ahead of its own prompt.
func ownPrompt(prompt string) string {
	if !strings.HasPrefix(prompt, `{"source":"osmia-owned-log"`) {
		return prompt
	}
	if _, own, ok := strings.Cut(prompt, "\n\n"); ok {
		return own
	}
	return ""
}

// briefCall is one tool call of the fake chief of staff.
type briefCall struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

// briefPlan is what the fake chief of staff does on the notice of one final
// review: it makes the early calls, waits at the gate, then records Text with
// the items and screenshots, or records nothing when Text is empty.
type briefPlan struct {
	Text        string
	Items       []string
	Screenshots []string
	// Early are record_delivery_brief inputs made before the gate, as a
	// delayed or replayed call of an earlier review would be.
	Early []map[string]any
}

// briefChief is a fake chief of staff that handles the delivery brief
// notices of final reviews as its plans say and makes the calls an owner
// message asks for. It matches only the part of a prompt that is its own.
type briefChief struct {
	mu      sync.Mutex
	plans   map[int]briefPlan
	gate    chan struct{}
	starts  int
	results map[string][]string
	early   map[int][]string
	refused []string
}

func newBriefChief() *briefChief {
	return &briefChief{plans: map[int]briefPlan{}, results: map[string][]string{}, early: map[int][]string{}}
}

// plan sets what the chief does on the notice of final review review.
func (c *briefChief) plan(review int, p briefPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[review] = p
}

// hold makes the chief wait, in the turn that handles a final review notice,
// until release is called.
func (c *briefChief) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

// release lets the turns that wait at the gate go on, and later ones too.
func (c *briefChief) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
}

func (c *briefChief) started() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

func (c *briefChief) result(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.results[id]
}

func (c *briefChief) earlyResults(review int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.early[review]
}

// call makes one tool call and returns its result as text; a failed call is
// a result too, so the turn completes and the test names it.
func briefToolCall(ctx context.Context, tools *mcp.ClientSession, call briefCall) string {
	out, err := callTool(ctx, tools, call.Tool, call.Input)
	if err != nil {
		return call.Tool + " failed: " + err.Error()
	}
	return out
}

func (c *briefChief) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	prompt := ownPrompt(req.Prompt)
	finish := func(text string) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	if _, rest, found := strings.Cut(prompt, briefDirective); found {
		var directive struct {
			ID    string      `json:"id"`
			Calls []briefCall `json:"calls"`
		}
		if err := json.NewDecoder(strings.NewReader(rest)).Decode(&directive); err != nil {
			return nil, err
		}
		results := []string{}
		for _, call := range directive.Calls {
			results = append(results, briefToolCall(ctx, tools, call))
		}
		c.mu.Lock()
		c.results[directive.ID] = results
		c.mu.Unlock()
		return finish("Made the calls.")
	}
	noticed := briefNoticed.FindAllStringSubmatch(prompt, -1)
	if len(noticed) == 0 {
		return finish("Nothing to do.")
	}
	for _, n := range noticed {
		review, _ := strconv.Atoi(n[1])
		revision, _ := strconv.Atoi(n[2])
		c.mu.Lock()
		plan := c.plans[review]
		gate := c.gate
		c.starts++
		c.mu.Unlock()
		for _, input := range plan.Early {
			out := briefToolCall(ctx, tools, briefCall{Tool: "record_delivery_brief", Input: input})
			c.mu.Lock()
			c.early[review] = append(c.early[review], out)
			c.mu.Unlock()
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if plan.Text == "" {
			continue
		}
		input := map[string]any{"review": review, "review_revision": revision, "text": plan.Text}
		if len(plan.Items) > 0 {
			input["before_you_ship"] = plan.Items
		}
		if len(plan.Screenshots) > 0 {
			input["screenshots"] = plan.Screenshots
		}
		if out := briefToolCall(ctx, tools, briefCall{Tool: "record_delivery_brief", Input: input}); !strings.Contains(out, `"recorded":true`) {
			c.mu.Lock()
			c.refused = append(c.refused, out)
			c.mu.Unlock()
		}
	}
	return finish("Handled the final review notice.")
}

// problems returns the brief calls of the notices the service refused.
func (c *briefChief) problems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.refused...)
}

// briefJourney is a project whose chief of staff is a fake model behind the
// service's real chief-of-staff tools, with one workstream that has been
// assembled and given its first final review. The service runs only while
// the journey says so: the final reviews and the publication run on the
// stopped service's controllers, as they do in the other delivery tests, and
// the chief's turns, the inbox, the delivery presentation and the owner's
// approval run in the service.
type briefJourney struct {
	*publicationFixture
	chief   *briefChief
	running bool
}

// newBriefJourney assembles one workstream, records its first final review
// and leaves the service stopped with the trace open. extra is appended to
// the configuration.
func newBriefJourney(t *testing.T, extra string) *briefJourney {
	t.Helper()
	j := newBriefFixture(t, extra)
	stream, repository := j.buildFixture().SeedGit(t, "brief")
	j.stream, j.repository = stream, repository
	j.s.setSole(&activeProject{repository: repository})
	j.assemble(t, stream, "internal/trace", "resume", "dedupe")
	return j
}

// newBriefFixture starts the project with a chief of staff behind the real
// tools and the fake chief of this journey.
func newBriefFixture(t *testing.T, extra string) *briefJourney {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, chiefRole+extra, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
	})
	chief := newBriefChief()
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = chief.turn
	f.engine.Mu.Unlock()
	return &briefJourney{publicationFixture: &publicationFixture{shedFixture: f}, chief: chief}
}

// assemble merges both units of the stream's plan onto its feature branch,
// asks for its final review and runs the review, which finds nothing. dir is
// the directory the units' files go in, and the names say which files, so
// that members of one project do not share a commit.
func (j *briefJourney) assemble(t *testing.T, stream config.WorkstreamID, dir, resume, dedupe string) FinalReport {
	t.Helper()
	a := &finalReviewer{s: j.s, repository: j.repository}
	_, op := assembleBoth(t, j.shedFixture, j.repository, a, stream,
		map[string]string{dir + "/" + resume + ".go": "package trace\n"},
		map[string]string{dir + "/" + dedupe + ".go": "package trace\n"})
	return j.review(t, stream, op, "Resumable uploads")
}

// review runs final review op as a reader that finds nothing, and returns the
// report it recorded.
func (j *briefJourney) review(t *testing.T, stream config.WorkstreamID, op coreadapter.Operation, summary string) FinalReport {
	t.Helper()
	in, err := decodeFinalReview(op)
	must(t, err)
	j.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": summary, "findings": []any{}})
		return err
	})
	a := &finalReviewer{s: j.s, repository: j.repository}
	if result := settleOperation(t, j.s, j.repository, stream, op, a); result.Outcome != "succeeded" {
		t.Fatalf("final review %d: %+v", in.Review, result)
	}
	report, found, err := latestFinalReport(j.repository, stream)
	must(t, err)
	if !found || report.Review != in.Review || report.Outcome != finalReviewed {
		t.Fatalf("final review %d recorded %+v", in.Review, report)
	}
	if stream == j.stream {
		j.report = report
	}
	return report
}

// reassemble has the owner send the workstream back to building, repairs its
// feature branch with files, moves it to assembled and runs the fresh final
// review. The service must be stopped.
func (j *briefJourney) reassemble(t *testing.T, files map[string]string) FinalReport {
	t.Helper()
	ctx := context.Background()
	move := func(to, note string) {
		t.Helper()
		if _, api := j.s.ownerMoveWorkstream(ctx, string(j.stream), WorkstreamMoveRequest{To: to, Note: note}); api != nil {
			t.Fatalf("move to %s: %s", to, api.Message)
		}
	}
	move(BuildingState, "The owner asked for a change to the brief's page.")
	moveFeature(t, j.shedFixture, j.stream, files)
	move(AssembledState, "The change landed; review the branch.")
	must(t, (&finalReviewer{s: j.s, repository: j.repository}).Pass(ctx))
	ops := finalOperations(t, j.repository, j.stream)
	return j.review(t, j.stream, ops[len(ops)-1].Operation, "Resumable uploads, changed")
}

// capture records what the preview tool records for a capture of the
// workstream at commit, and returns the image's ID. The trace must be open.
func (j *briefJourney) capture(t *testing.T, name, commit string) string {
	t.Helper()
	ctx := context.Background()
	var data bytes.Buffer
	must(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	img, err := j.repository.RecordImage(ctx, trace.Header{Project: j.repository.Project(), Workstream: j.stream, At: demoStart, Actor: serviceActor, Cause: "preview"}, name, "image/png", data.Bytes(), "turn")
	must(t, err)
	content, err := json.Marshal(map[string]any{"workstream": j.stream, "commit": commit, "result": map[string]any{"image_id": img.ID, "workstream": j.stream, "commit": commit}})
	must(t, err)
	must(t, j.repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "preview-" + name, Revision: 1, Project: j.repository.Project(), Workstream: j.stream, At: demoStart, Actor: serviceActor, Cause: "preview"}, Path: "tools/preview-" + name + ".json", Content: string(content)}}))
	return img.ID
}

// run starts the service on the trace the journey holds open.
func (j *briefJourney) run(t *testing.T) {
	t.Helper()
	must(t, j.repository.Close())
	j.start(t)
	if !j.running {
		// Cleanups run last-registered first, so the held turns are let go
		// before the service waits for them.
		t.Cleanup(func() { j.stop(t) })
		t.Cleanup(j.chief.release)
	}
	j.running = true
}

// halt stops the service and opens its trace for the stopped service's
// controllers.
func (j *briefJourney) halt(t *testing.T) {
	t.Helper()
	j.chief.release()
	j.stop(t)
	repository, err := trace.Open(j.s.cfg.Root, j.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	j.repository = repository
	j.s.setSole(&activeProject{repository: repository})
}

// awaitStarts waits until the chief has begun handling n final review
// notices.
func (j *briefJourney) awaitStarts(t *testing.T, n int) {
	t.Helper()
	intakefixture.Eventually(t, "the chief of staff never began the turn for the final review notice", func() bool { return j.chief.started() >= n })
}

// entries returns the workstream's delivery entries of the inbox.
func (j *briefJourney) entries(t *testing.T, stream config.WorkstreamID) []api.InboxEntry {
	t.Helper()
	var out []api.InboxEntry
	for _, e := range intakefixture.EntriesOf(t, j.c, InboxDelivery) {
		if e.Workstream == stream {
			out = append(out, e)
		}
	}
	return out
}

// awaitEntry waits until the workstream's delivery entry is listed and
// returns it.
func (j *briefJourney) awaitEntry(t *testing.T, stream config.WorkstreamID) api.InboxEntry {
	t.Helper()
	var found api.InboxEntry
	intakefixture.Eventually(t, "the delivery entry never appeared in the inbox", func() bool {
		entries := j.entries(t, stream)
		if len(entries) == 0 {
			return false
		}
		found = entries[0]
		return true
	})
	return found
}

// present reads the workstream's delivery presentation from the service.
func (j *briefJourney) present(t *testing.T, stream config.WorkstreamID) api.DeliveryPresentation {
	t.Helper()
	p, err := j.c.Delivery(context.Background(), stream)
	must(t, err)
	return p
}

// approvePresented has the owner approve the presentation p as it reads, through the
// service.
func (j *briefJourney) approvePresented(p api.DeliveryPresentation) (DeliveryApproval, error) {
	return j.c.ApproveDelivery(context.Background(), p.Workstream, api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
}

// ask sends the owner message that has the chief of staff make calls, and
// returns their results.
func (j *briefJourney) ask(t *testing.T, stream config.WorkstreamID, id string, calls ...briefCall) []string {
	t.Helper()
	data, err := json.Marshal(struct {
		ID    string      `json:"id"`
		Calls []briefCall `json:"calls"`
	}{id, calls})
	must(t, err)
	_, err = j.c.Send(context.Background(), stream, briefDirective+string(data))
	must(t, err)
	var out []string
	intakefixture.Eventually(t, "the chief of staff never made the calls of "+id, func() bool {
		out = j.chief.result(id)
		return out != nil
	})
	return out
}

// recordBrief is the record_delivery_brief call that records a brief of the
// final review the presentation p shows, for member when it is not empty.
func recordBrief(p api.DeliveryPresentation, member config.WorkstreamID, text string, items ...string) briefCall {
	input := map[string]any{"review": p.Report.Review, "review_revision": p.ReviewRevision, "text": text}
	if len(items) > 0 {
		input["before_you_ship"] = items
	}
	if member != "" {
		input["workstream"] = string(member)
	}
	return briefCall{Tool: "record_delivery_brief", Input: input}
}

// publish publishes the approved workstream through the stopped service and
// returns the body of the pull request it opened. The service must be
// stopped.
func (j *briefJourney) publish(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	j.fork, j.pulls = fixturekit.PublicationFork(t, j.clone, func() string { commit, _ := j.forkBranch(t); return commit })
	j.s.options.PullRequests = j.pulls
	// The running service asked for the publication once the owner approved;
	// it could not carry it out without a pull request host, so the request
	// is still pending. A publication that was not asked for is asked for now.
	var op coreadapter.Operation
	if ops := j.operations(t); len(ops) > 0 {
		op = ops[len(ops)-1]
	} else {
		op = j.request(t)
	}
	result, err := j.publisher().Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" || len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 {
		t.Fatalf("publication %+v: %+v", result, j.pulls)
	}
	return j.pulls.Prs[0].Body
}
