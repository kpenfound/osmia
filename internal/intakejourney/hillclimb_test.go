package intakejourney

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	climbSkill       = "https://github.com/kpenfound/skills#hill-climb-evaluator"
	climbSkillConfig = "[kinds.hill-climb]\nskills = [\"" + climbSkill + "\"]\n"
	climbBrief       = "# Faster parsing\n\nMake the parser at least 10% faster on the corpus.\n"

	validClimbSpec = `{
  "version": 1,
  "goal": "Make the parser faster on the corpus.",
  "evaluator": "parse-bench",
  "metric": {"name": "wall_ms", "direction": "lower", "min_improvement": 5},
  "guardrails": {"checks": ["lint"], "limits": [{"metric": "peak_rss_mb", "direction": "lower", "max": 200}]},
  "protected": {"fixtures": ["testdata/corpus"]},
  "stop": {"max_attempts": 20, "plateau": 5}
}`
	incompleteClimbSpec = `{"version": 1, "goal": "Make the parser faster.", "metric": {"name": "wall_ms", "direction": "sideways", "min_improvement": 0}, "stop": {}}`
)

type turnSkills struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (s *turnSkills) record(turn string, req agent.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string][]string{}
	}
	s.seen[turn] = slices.Clone(req.Profile.Skills)
}

func (s *turnSkills) of(turn string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	skills, ok := s.seen[turn]
	return skills, ok
}

func newClimbFixture(t *testing.T, members, rounds int) *journey.ShedFixture {
	t.Helper()
	f := journey.NewDebateFixtureWith(t, members, rounds, climbSkillConfig, nil)
	f.Engine.Mu.Lock()
	f.Engine.Skills = []string{t.TempDir()}
	f.Engine.Mu.Unlock()
	return f
}

func handInKind(t *testing.T, f *journey.ShedFixture, key, content, kind string, skip bool) config.WorkstreamID {
	t.Helper()
	out, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: key, Stdin: &content, Kind: kind, SkipDebate: skip})
	fixturekit.Must(t, err)
	return out.Workstream
}

func TestHillClimbIsDraftedDebatedOnceRatifiedAndSealedWithoutAPlan(t *testing.T) {
	t.Parallel()
	f := newClimbFixture(t, 2, 3)
	defer f.Stop(t)
	ctx := context.Background()
	f.Upstream(t)
	p := &intakefixture.Faults{}
	skills := &turnSkills{}
	metric := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)

	f.Script("draft-1-1", map[string]string{kinds.ClimbSpecPath: incompleteClimbSpec}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		skills.record("draft-1-1", req)
		for _, want := range []string{kinds.ClimbSpecPath, "climb spec", "min_improvement", "protected", "stop"} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("the architect's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if strings.Contains(req.Prompt, "Acceptance criteria") || strings.Contains(req.Prompt, "plan.json") {
			p.Report("the architect is asked for a spec and a plan:\n%s", req.Prompt)
		}
		return nil
	})
	f.Script("draft-2-1", map[string]string{kinds.ClimbSpecPath: validClimbSpec}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		skills.record("draft-2-1", req)
		if !strings.Contains(req.Prompt, "Draft 1 was not accepted") || !strings.Contains(req.Prompt, "metric.direction") || !strings.Contains(req.Prompt, "evaluator is required") || !strings.Contains(req.Prompt, "stop.max_attempts") {
			p.Report("the second draft is not told what was refused:\n%s", req.Prompt)
		}
		return nil
	})
	f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, v *agent.Turn, tools *mcp.ClientSession) error {
		skills.record("round-1-member-1", req)
		for _, want := range []string{"the only round the climb spec gets", "climb#metric", "gaming", "noise", "protected paths"} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("the metric round's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if got, err := intakefixture.ReadTool(ctx, tools, kinds.ClimbSpecPath); err != nil || got != validClimbSpec {
			p.Report("the round debates %q %v", got, err)
		}
		return intakefixture.Objects(p, shed.Metric, "climb#metric", "climb#guardrails")(ctx, req, v, tools)
	})
	f.Member(1, 2, 1, func(ctx context.Context, req agent.Request, v *agent.Turn, tools *mcp.ClientSession) error {
		skills.record("round-1-member-2", req)
		return nil
	})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, v *agent.Turn, tools *mcp.ClientSession) error {
		skills.record("reply-1", req)
		if !strings.Contains(req.Prompt, "the only round") && !strings.Contains(req.Prompt, "one round of the shed is heard") {
			p.Report("the reply prompt: %s", req.Prompt)
		}
		return intakefixture.Answers(p, "I raised the repeats.", metric)(ctx, req, v, tools)
	})

	stream := handInKind(t, f, "climb", climbBrief, "hill-climb", false)
	if recorded, err := f.Repository().RecordedKind(stream); err != nil || recorded != "hill-climb" {
		t.Fatalf("recorded kind: %q %v", recorded, err)
	}
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	docs := f.Documents(t, stream, kinds.ClimbSpecID)
	if len(docs) != 2 || docs[0].Path != kinds.ClimbSpecPath || docs[1].Content != validClimbSpec {
		t.Fatalf("climb spec revisions: %+v", docs)
	}
	for _, id := range []string{"plan"} {
		if got := f.Documents(t, stream, id); len(got) != 0 {
			t.Fatalf("a hill-climb workstream recorded %s: %+v", id, got)
		}
	}
	if len(f.AcknowledgedDraftOperations(t, stream)) != 2 {
		t.Fatalf("draft operations: %+v", f.DraftOperations(t, stream))
	}
	if first := f.Delivered("draft-1-1"); len(first) != 1 || first[0].Verdict != intake.DraftInvalid || len(first[0].Problems) < 4 {
		t.Fatalf("the incomplete climb spec was judged %+v", first)
	}
	if second := f.Delivered("draft-2-1"); len(second) != 1 || second[0].Verdict != intake.DraftValid {
		t.Fatalf("the valid climb spec was judged %+v", second)
	}

	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ops := f.AcknowledgedRoundOperations(t, stream); len(ops) != 1 {
		t.Fatalf("a climb spec got %d round operations, want exactly one", len(ops))
	}
	if record := f.Record(t, stream, intake.CommitteeAgent(1)); record.Revision != (shed.Pin{Spec: 2}) || len(record.Objections) != 1 || record.Objections[0].Kind != shed.Metric {
		t.Fatalf("the committee's record: %+v", record)
	}
	packet := f.AwaitPacket(t, stream, "ratify: nothing blocks, and 1 objection stands as advice on the record")
	if packet.Revision != (shed.Pin{Spec: 2}) {
		t.Fatalf("packet pins %+v", packet.Revision)
	}

	if _, err := f.C.ShedMore(ctx, stream, 1); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "exactly 1 committee round") {
		t.Fatalf("further rounds: %v", err)
	}
	if _, err := f.C.ShedRedraft(ctx, stream, "Rewrite it."); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "amendment") {
		t.Fatalf("a redraft: %v", err)
	}
	if _, err := f.C.Ratify(ctx, stream, 2, 1); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("ratifying a plan revision of a kind without a plan: %v", err)
	}
	if _, err := f.C.Ratify(ctx, stream, 1, 0); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("ratifying a stale revision: %v", err)
	}
	if _, err := f.C.Ratify(ctx, stream, 2, 0); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.BuildOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("build operations %+v", ops)
	}

	sealed, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || sealed.Kind != "hill-climb" || sealed.Revision != (shed.Pin{Spec: 2}) || len(sealed.Footprints) != 0 || sealed.Seal != 1 {
		t.Fatalf("seal %+v", sealed)
	}
	if got := ops[0].Result.Evidence; !strings.Contains(got, "no plan and no units") {
		t.Fatalf("build evidence: %q", got)
	}
	transitions, err := trace.Read[trace.Transition](f.Repository(), stream)
	fixturekit.Must(t, err)
	for _, tr := range transitions {
		if strings.HasPrefix(tr.Subject, "unit-") && tr.Cause == ops[0].Operation.ID {
			t.Fatalf("a hill-climb build recorded the unit transition %+v", tr)
		}
	}
	spec, bySeal, err := intake.SealedClimbSpec(f.Repository(), f.S.Current(), stream)
	fixturekit.Must(t, err)
	if bySeal.Seal != 1 || spec.Evaluator != "parse-bench" || spec.Metric.Name != "wall_ms" || spec.Repeats != 1 || spec.Stop.MaxAttempts != 20 {
		t.Fatalf("sealed climb spec %+v", spec)
	}
	data, err := json.Marshal(spec)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), `"repeats":1`) {
		t.Fatalf("defaults are not applied: %s", data)
	}

	for _, turn := range []string{"draft-1-1", "draft-2-1", "round-1-member-1", "round-1-member-2", "reply-1"} {
		if got, ok := skills.of(turn); !ok || !slices.Equal(got, []string{climbSkill}) {
			t.Errorf("%s carried skills %v (seen %v), want the evaluator skill", turn, got, ok)
		}
	}
}

func TestHillClimbSkippedDebateGoesStraightToRatification(t *testing.T) {
	t.Parallel()
	f := newClimbFixture(t, 2, 3)
	defer f.Stop(t)
	ctx := context.Background()
	f.Upstream(t)
	f.Script("draft-1-1", map[string]string{kinds.ClimbSpecPath: validClimbSpec}, nil)
	stream := handInKind(t, f, "climb-skip", climbBrief, "hill-climb", true)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	f.AwaitFeature(t, stream, intake.InShedState)
	f.DebatedNothing(t, stream)
	if _, err := f.C.Ratify(ctx, stream, 1, 0); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	sealed, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || sealed.Kind != "hill-climb" || sealed.Revision != (shed.Pin{Spec: 1}) {
		t.Fatalf("seal %+v", sealed)
	}
}

func TestFeatureWorkstreamsLoadNoKindSkillsAndKeepTheirSealRecord(t *testing.T) {
	t.Parallel()
	f := newClimbFixture(t, 1, 1)
	defer f.Stop(t)
	f.Upstream(t)
	p := &intakefixture.Faults{}
	skills := &turnSkills{}
	f.Script("draft-1-1", map[string]string{"spec.md": fixturekit.ValidSpec, "plan.json": fixturekit.ValidPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		skills.record("draft-1-1", req)
		return nil
	})
	f.Member(1, 1, 1, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		skills.record("round-1", req)
		return nil
	})
	stream := f.HandIn(t, "feature", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	for _, turn := range []string{"draft-1-1", "round-1"} {
		if got, ok := skills.of(turn); !ok || len(got) != 0 {
			t.Errorf("%s of a feature workstream carried skills %v (seen %v)", turn, got, ok)
		}
	}
	if _, err := f.C.Ratify(context.Background(), stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	sealed, _, found, err := seal.Latest(f.Repository(), stream)
	fixturekit.Must(t, err)
	if !found || sealed.Kind != "" || sealed.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(sealed.Footprints) == 0 {
		t.Fatalf("a feature seal: %+v", sealed)
	}
	if _, err := f.C.Ratify(context.Background(), stream, 1, 0); err == nil {
		t.Fatal("a feature workstream was ratified without its plan revision")
	}
}

func TestHillClimbCannotHaveABaseOrBeOne(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	input := climbBrief
	feature := f.HandIn(t, api.HandInRequest{Key: "feature", Stdin: &input})
	climb := f.HandIn(t, api.HandInRequest{Key: "climb", Kind: "hill-climb", Stdin: &input})
	if recorded, err := f.Repository().RecordedKind(climb.Workstream); err != nil || recorded != "hill-climb" {
		t.Fatalf("recorded kind: %q %v", recorded, err)
	}
	head := f.Head(t)
	for name, req := range map[string]api.HandInRequest{
		"a base on a hill-climb workstream":       {Project: f.Project, Key: "climb-based", Kind: "hill-climb", Base: feature.Workstream, Stdin: &input},
		"a hill-climb workstream as a base":       {Project: f.Project, Key: "feature-based", Base: climb.Workstream, Stdin: &input},
		"a hill-climb workstream on a hill-climb": {Project: f.Project, Key: "climb-on-climb", Kind: "hill-climb", Base: climb.Workstream, Stdin: &input},
	} {
		_, err := f.C.HandIn(context.Background(), req)
		if !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "base") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !f.Unchanged(t, head) {
		t.Fatal("a refused hand-in wrote to the trace")
	}
}

func TestDraftBriefRefusesHillClimbBriefsTheKindCannotRun(t *testing.T) {
	t.Parallel()
	d := newDesignProjects(t)
	d.script(func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for _, want := range []string{"- hill-climb: use when", "exactly one project", "no new_project"} {
			if !strings.Contains(req.Prompt, want) {
				return nil, fmt.Errorf("the design agent's turn context lacks %q", want)
			}
		}
		for name, input := range map[string]map[string]any{
			"two projects":  {"content": intakefixture.ProductionBrief, "kind": "hill-climb", "projects": []config.ProjectID{d.home, d.other}},
			"a new project": {"content": intakefixture.ProductionBrief, "kind": "hill-climb", "new_project": map[string]any{"name": "Gadget", "repo": "gadget", "description": "A gadget.", "visibility": "private", "charter": "1. Keep it small.\n"}},
		} {
			out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, input)
			if err != nil || !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "hill-climb") {
				return nil, fmt.Errorf("%s: %s %v", name, out, err)
			}
		}
		if _, revision, err := intake.LatestDesignBrief(d.repo, d.stream); err != nil || revision != 1 {
			return nil, fmt.Errorf("a refusal recorded revision %d: %v", revision, err)
		}
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief, "kind": "hill-climb"})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("a hill-climb brief of one project: %s %v", out, err)
		}
		out, err = fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief, "projects": []config.ProjectID{d.home, d.other}})
		if err != nil || !strings.Contains(out, `"recorded":false`) {
			return nil, fmt.Errorf("a second project added to a recorded hill-climb brief: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-climb", ResultText: "I chose the hill-climb kind."}, nil
	})
	d.say(t, "Make the parser faster.", 2)
	view, err := d.c.Design(context.Background(), d.stream)
	fixturekit.Must(t, err)
	if view.Design.Kind != "hill-climb" || view.Design.BriefRevision != 2 {
		t.Fatalf("design view: %+v", view.Design)
	}

	_, current, err := intake.LatestDesignBrief(d.repo, d.stream)
	fixturekit.Must(t, err)
	data, err := json.Marshal(intake.DesignBriefRecord{Content: intakefixture.ProductionBrief, Projects: []config.ProjectID{d.home, d.other}, Kind: "hill-climb"})
	fixturekit.Must(t, err)
	h := intake.OwnerHeader(intake.DesignDraftDocument, d.repo.Project(), d.stream, intake.DraftBriefToolName, time.Now().UTC())
	h.Schema, h.Revision = "osmia.trace.document", current+1
	fixturekit.Must(t, d.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: intake.DesignDraftPath, Content: string(data)}}))
	_, err = d.c.DesignStartWithBriefRevision(context.Background(), d.stream, current+1)
	if !intakefixture.Failed(err, api.Validation) || !strings.Contains(err.Error(), "exactly one project") {
		t.Fatalf("starting a hill-climb brief of two projects: %v", err)
	}
}

func TestApprovedClimbSpecAmendmentIsResealedAndAppliedWithoutUnits(t *testing.T) {
	t.Parallel()
	f := newClimbFixture(t, 2, 3)
	ctx := context.Background()
	f.Upstream(t)
	f.Script("draft-1-1", map[string]string{kinds.ClimbSpecPath: validClimbSpec}, nil)
	stream := handInKind(t, f, "climb-amend", climbBrief, "hill-climb", true)
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	if _, err := f.C.Ratify(ctx, stream, 1, 0); err != nil {
		t.Fatal(err)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.BuildOperations(t, stream) })
	f.Stop(t)
	repository, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repository.Close()

	unitMoves := func() int {
		transitions, err := trace.Read[trace.Transition](repository, stream)
		fixturekit.Must(t, err)
		var n int
		for _, tr := range transitions {
			if strings.HasPrefix(tr.Subject, "unit-") {
				n++
			}
		}
		return n
	}
	unitMovesBefore := unitMoves()

	amended := strings.Replace(validClimbSpec, `"min_improvement": 5`, `"min_improvement": 8`, 1)
	sealed, sealDoc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the workstream has no seal")
	}
	at := f.Clock.Now()
	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	header := func(schema, id string, by trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: by, Cause: "fixture"}
	}
	request := trace.Amendment{Header: header("osmia.trace.amendment", "1", chief), Requester: chief, Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1",
		Citations: []string{"climb#metric"}, Change: "Raise the minimum improvement", Reason: "The noise is larger than first thought", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}
	fixturekit.Must(t, repository.Append(ctx, request))
	document := func(id, path, content string) trace.Document {
		return trace.Document{Header: header("osmia.trace.document", id, intake.ArchitectActor), Path: path, Content: content}
	}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{
		document("amendment_1_spec", "amendments/1/"+kinds.ClimbSpecPath, amended),
		document("amendment_1_affected", "amendments/1/affected.json", `{"criteria":[],"units":[]}`),
		document("amendment-1-presented-packet", intake.AmendmentPacketPath("1"), `{"round":1}`+"\n"),
	}, trace.Transaction{Transition: trace.Transition{Header: header("osmia.trace.transition", "amendment-1-presented", intake.ShedActor), Subject: intake.AmendmentSubject("1"), To: intake.AmendmentPresented, Reason: "presented"}})
	fixturekit.Must(t, err)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	if out, apiErr := f.S.DecideAmendment(ctx, string(stream), "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentApprove, Note: "Yes.", Packet: 1}); apiErr != nil || out.State != intake.AmendmentApproved {
		t.Fatalf("approval %+v %v", out, apiErr)
	}
	a := intake.AmendmentDebate{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repository}}
	req := request

	intakefixture.Step(t, a, stream, req, intake.AmendmentResealed)
	current, doc, found, err := seal.Latest(repository, stream)
	fixturekit.Must(t, err)
	if !found || doc.Revision != sealDoc.Revision+1 || current.Seal != sealed.Seal+1 || current.Kind != "hill-climb" || current.Revision != (shed.Pin{Spec: 2}) ||
		current.SpecHash != seal.SpecHash(amended) || current.Base != sealed.Base || current.Branch != sealed.Branch || len(current.Footprints) != 0 {
		t.Fatalf("resealed %+v at revision %d", current, doc.Revision)
	}
	spec, _, err := intake.SealedClimbSpec(repository, f.S.Current(), stream)
	fixturekit.Must(t, err)
	if spec.Metric.MinImprovement != 8 {
		t.Fatalf("the amended climb spec: %+v", spec)
	}
	applications, err := amendment.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].From != (amendment.Pin{Seal: 1, SealRevision: sealDoc.Revision, Spec: 1}) || applications[0].To != (amendment.Pin{Seal: 2, SealRevision: sealDoc.Revision + 1, Spec: 2}) ||
		len(applications[0].Rework) != 0 || len(applications[0].Notify) != 0 || applications[0].Applied != nil {
		t.Fatalf("application %+v", applications)
	}

	intakefixture.Step(t, a, stream, req, intake.AmendmentApplied)
	applications, err = amendment.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(applications) != 1 || applications[0].Applied == nil || len(applications[0].Applied.Held) != 0 {
		t.Fatalf("applied %+v", applications)
	}
	if got := unitMoves(); got != unitMovesBefore {
		t.Fatalf("an amendment of a climb spec moved the unit state: %d unit transitions before it, %d after", unitMovesBefore, got)
	}
}
