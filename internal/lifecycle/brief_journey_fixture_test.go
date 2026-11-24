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
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const briefDirective = "BRIEFCALL "

var briefNoticed = regexp.MustCompile(`naming review (\d+) and review_revision (\d+)`)

func ownPrompt(prompt string) string {
	if !strings.HasPrefix(prompt, `{"source":"osmia-owned-log"`) {
		return prompt
	}
	if _, own, ok := strings.Cut(prompt, "\n\n"); ok {
		return own
	}
	return ""
}

type briefCall struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

type briefPlan struct {
	Text        string
	Items       []string
	Screenshots []string
	Early       []map[string]any
	Calls       []briefCall
}

type briefChief struct {
	mu      sync.Mutex
	plans   map[int]briefPlan
	gate    chan struct{}
	starts  int
	results map[string][]string
	early   map[int][]string
	refused []string
	called  map[int][]string
	heard   []string
}

func newBriefChief() *briefChief {
	return &briefChief{plans: map[int]briefPlan{}, results: map[string][]string{}, early: map[int][]string{}, called: map[int][]string{}}
}

func (c *briefChief) plan(review int, p briefPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[review] = p
}

func (c *briefChief) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

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

func (c *briefChief) calls(review int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.called[review]...)
}

func (c *briefChief) heardOf(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, prompt := range c.heard {
		if strings.Contains(prompt, text) {
			n++
		}
	}
	return n
}

func (c *briefChief) earlyResults(review int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.early[review]
}

func briefToolCall(ctx context.Context, tools *mcp.ClientSession, call briefCall) string {
	out, err := intakefixture.CallTool(ctx, tools, call.Tool, call.Input)
	if err != nil {
		return call.Tool + " failed: " + err.Error()
	}
	return out
}

func (c *briefChief) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	prompt := ownPrompt(req.Prompt)
	c.mu.Lock()
	c.heard = append(c.heard, prompt)
	c.mu.Unlock()
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
		for _, call := range plan.Calls {
			out := briefToolCall(ctx, tools, call)
			c.mu.Lock()
			c.called[review] = append(c.called[review], out)
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

func (c *briefChief) problems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.refused...)
}

type briefJourney struct {
	*publicationFixture
	chief   *briefChief
	running bool
}

func newBriefJourney(t *testing.T, extra string) *briefJourney {
	t.Helper()
	j := newBriefFixture(t, extra)
	stream, repository := j.buildFixture().SeedGit(t, "brief")
	j.stream, j.repository = stream, repository
	j.host(t)
	j.s.setSole(&activeProject{repository: repository})
	j.assemble(t, stream, "internal/trace", "resume", "dedupe")
	return j
}

func (j *briefJourney) host(t *testing.T) {
	t.Helper()
	j.fork, j.pulls = hostBranches(t, j.shedFixture)
}

func newBriefFixture(t *testing.T, extra string) *briefJourney {
	t.Helper()
	return newBriefFixtureAt(t, extra, nil)
}

func newBriefFixtureAt(t *testing.T, extra string, now func() time.Time) *briefJourney {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, intakefixture.ChiefRole+extra, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		if now != nil {
			opts.Reconciliation.Now = now
		}
		enforced := Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
	})
	chief := newBriefChief()
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = chief.turn
	f.engine.Mu.Unlock()
	return &briefJourney{publicationFixture: &publicationFixture{shedFixture: f}, chief: chief}
}

func (j *briefJourney) assemble(t *testing.T, stream config.WorkstreamID, dir, resume, dedupe string) api.FinalReport {
	t.Helper()
	a := &finalReviewer{s: j.s, repository: j.repository}
	_, op := assembleBoth(t, j.shedFixture, j.repository, a, stream,
		map[string]string{dir + "/" + resume + ".go": "package trace\n"},
		map[string]string{dir + "/" + dedupe + ".go": "package trace\n"})
	return j.review(t, stream, op, "Resumable uploads")
}

func (j *briefJourney) review(t *testing.T, stream config.WorkstreamID, op coreadapter.Operation, summary string) api.FinalReport {
	t.Helper()
	in, err := delivery.DecodeFinalReview(op)
	intakefixture.Must(t, err)
	j.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": summary, "findings": []any{}})
		return err
	})
	a := &finalReviewer{s: j.s, repository: j.repository}
	if result := settleOperation(t, j.s, j.repository, stream, op, a); result.Outcome != "succeeded" {
		t.Fatalf("final review %d: %+v", in.Review, result)
	}
	report, found, err := delivery.LatestFinalReport(j.repository, stream)
	intakefixture.Must(t, err)
	if !found || report.Review != in.Review || report.Outcome != delivery.FinalReviewed {
		t.Fatalf("final review %d recorded %+v", in.Review, report)
	}
	if stream == j.stream {
		j.report = report
	}
	return report
}

func (j *briefJourney) reassemble(t *testing.T, files map[string]string) api.FinalReport {
	t.Helper()
	ctx := context.Background()
	move := func(to, note string) {
		t.Helper()
		if _, apiFault := j.s.ownerMoveWorkstream(ctx, string(j.stream), api.WorkstreamMoveRequest{To: to, Note: note}); apiFault != nil {
			t.Fatalf("move to %s: %s", to, apiFault.Message)
		}
	}
	move(intakefixture.BuildingState, "The owner asked for a change to the brief's page.")
	moveFeature(t, j.shedFixture, j.stream, files)
	move(intake.AssembledState, "The change landed; review the branch.")
	intakefixture.Must(t, (&finalReviewer{s: j.s, repository: j.repository}).Pass(ctx))
	ops := finalOperations(t, j.repository, j.stream)
	return j.review(t, j.stream, ops[len(ops)-1].Operation, "Resumable uploads, changed")
}

func (j *briefJourney) capture(t *testing.T, name, commit string) string {
	t.Helper()
	var data bytes.Buffer
	intakefixture.Must(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	return j.captureImage(t, name, commit, data.Bytes())
}

func (j *briefJourney) captureImage(t *testing.T, name, commit string, data []byte) string {
	t.Helper()
	ctx := context.Background()
	img, err := j.repository.RecordImage(ctx, trace.Header{Project: j.repository.Project(), Workstream: j.stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "preview"}, name, "image/png", data, "turn")
	intakefixture.Must(t, err)
	content, err := json.Marshal(map[string]any{"workstream": j.stream, "commit": commit, "result": map[string]any{"image_id": img.ID, "workstream": j.stream, "commit": commit}})
	intakefixture.Must(t, err)
	intakefixture.Must(t, j.repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "preview-" + name, Revision: 1, Project: j.repository.Project(), Workstream: j.stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "preview"}, Path: "tools/preview-" + name + ".json", Content: string(content)}}))
	return img.ID
}

func (j *briefJourney) run(t *testing.T) {
	t.Helper()
	intakefixture.Must(t, j.repository.Close())
	j.start(t)
	if !j.running {
		t.Cleanup(func() { j.stop(t) })
		t.Cleanup(j.chief.release)
	}
	j.running = true
}

func (j *briefJourney) halt(t *testing.T) {
	t.Helper()
	j.chief.release()
	j.stop(t)
	repository, err := trace.Open(j.s.cfg.Root, j.s.cfg.Project)
	intakefixture.Must(t, err)
	t.Cleanup(func() { repository.Close() })
	j.repository = repository
	j.s.setSole(&activeProject{repository: repository})
}

func (j *briefJourney) awaitStarts(t *testing.T, n int) {
	t.Helper()
	intakefixture.Eventually(t, "the chief of staff never began the turn for the final review notice", func() bool { return j.chief.started() >= n })
}

func (j *briefJourney) entries(t *testing.T, stream config.WorkstreamID) []api.InboxEntry {
	t.Helper()
	var out []api.InboxEntry
	for _, e := range intakefixture.EntriesOf(t, j.c, api.InboxDelivery) {
		if e.Workstream == stream {
			out = append(out, e)
		}
	}
	return out
}

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

func (j *briefJourney) present(t *testing.T, stream config.WorkstreamID) api.DeliveryPresentation {
	t.Helper()
	p, err := j.c.Delivery(context.Background(), stream)
	intakefixture.Must(t, err)
	return p
}

func (j *briefJourney) approvePresented(p api.DeliveryPresentation) (api.DeliveryApproval, error) {
	return j.c.ApproveDelivery(context.Background(), p.Workstream, api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash})
}

func (j *briefJourney) ask(t *testing.T, stream config.WorkstreamID, id string, calls ...briefCall) []string {
	t.Helper()
	data, err := json.Marshal(struct {
		ID    string      `json:"id"`
		Calls []briefCall `json:"calls"`
	}{id, calls})
	intakefixture.Must(t, err)
	_, err = j.c.Send(context.Background(), stream, briefDirective+string(data))
	intakefixture.Must(t, err)
	var out []string
	intakefixture.Eventually(t, "the chief of staff never made the calls of "+id, func() bool {
		out = j.chief.result(id)
		return out != nil
	})
	return out
}

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

func (j *briefJourney) publish(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var op coreadapter.Operation
	if ops := j.operations(t); len(ops) > 0 {
		op = ops[len(ops)-1]
	} else {
		op = j.request(t)
	}
	result, err := j.publisher().Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "succeeded" || len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pulls.Readies != 1 || j.pulls.Prs[0].Draft {
		t.Fatalf("marking ready %+v: %+v", result, j.pulls)
	}
	return j.pulls.Prs[0].Body
}
