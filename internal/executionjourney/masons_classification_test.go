package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	systemone "github.com/kpenfound/agentcore/jev/systemone"
	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMasonContestedRuling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, response, reason string
	}{
		{"gave_up", "I cannot complete this work.", "gave_up"},
		{"bound", "Work is ongoing.", "bound exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
			defer f.stop(t)
			fake.Response = map[string]string{service.TestMasonTurnID("resume"): tc.response}
			ownerTurn := intake.MasonAgent("resume") + "-owner-revise-"
			f.engine.Mu.Lock()
			if tc.name == "bound" {
				for i := 1; i < f.s.cfg.Mason.MaxCleanTurns; i++ {
					delete(fake.Play, fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i))
				}
			}
			f.engine.Turns[ownerTurn+"1"] = fake.Turn
			f.engine.Turns[ownerTurn+"2"] = fake.Turn
			f.engine.Turns[ownerTurn+"3"] = fake.Turn
			f.engine.Mu.Unlock()
			stream := f.seedBuilding(t, tc.name+"-ruling", fixturekit.ValidPlan)
			f.awaitUnit(t, stream, "resume", intake.UnitContested)
			status, err := f.c.Status(context.Background(), stream)
			if err != nil || len(status.Gates) != 0 {
				t.Fatalf("mason contest gate: %+v %v", status.Gates, err)
			}
			if len(status.Units) == 0 || !strings.Contains(status.Units[0].Reason, tc.reason) {
				t.Fatalf("mason contest status: %+v", status.Units)
			}
			raiseFixtureContest(t, f.repository(), stream, "resume")
			if entries := f.raisedContests(t, stream); len(entries) != 1 || !slices.Equal(entries[0].Options, []string{"revise"}) || !strings.Contains(entries[0].Question, tc.reason) {
				t.Fatalf("inbox entries of the mason contest %+v", entries)
			}
			if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Try a reviewer"); err == nil || !strings.Contains(err.Error(), "no candidate under review; use revise") {
				t.Fatalf("review refusal: %v", err)
			}
			th := f.thread(t, stream, intake.MasonAgent("resume"))
			if tc.name == "bound" {
				if len(th.Turns) != f.s.cfg.Mason.MaxCleanTurns {
					t.Fatalf("bounded turns: %d", len(th.Turns))
				}
				f.awaitEventTurns(t, stream, "classified unclear")
				f.awaitEventTurns(t, stream, "bound exhausted")
			} else {
				f.awaitEventTurns(t, stream, "classified gave_up")
			}
			reset := th.Turns[len(th.Turns)-1].Sequence
			turnID := fmt.Sprintf("%s%d", ownerTurn, reset)
			if tc.name == "bound" {
				continuation := fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), reset+1)
				fake.Play[continuation] = intakefixture.ReportDone("Owner revision")
				f.engine.Mu.Lock()
				f.engine.Turns[continuation] = fake.Turn
				f.engine.Mu.Unlock()
			} else {
				fake.Play[turnID] = intakefixture.ReportDone("Owner revision")
			}
			ruling, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "Use the existing workspace")
			if err != nil || ruling.Ruling.ResetTurn != reset || ruling.Ruling.Decision != "revise" {
				t.Fatalf("mason ruling: %+v %v", ruling, err)
			}
			if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "Again"); err == nil {
				t.Fatal("accepted duplicate mason ruling")
			}
			f.stop(t)
			f.start(t)
			intakefixture.Soon(t, "the owner turn to be queued", func() bool {
				th = f.thread(t, stream, intake.MasonAgent("resume"))
				return len(th.Turns) > int(reset) && th.Turns[reset].Request.TurnID == turnID
			})
			if !strings.Contains(th.Turns[reset].Request.Prompt, "Use the existing workspace") || th.Turns[reset].Request.ThreadID != th.Identity.ThreadID {
				t.Fatalf("owner turn: %+v", th.Turns[reset])
			}
			f.awaitUnit(t, stream, "resume", intake.UnitReviewing)
			th = f.thread(t, stream, intake.MasonAgent("resume"))
			wantTurns := int(reset) + 1
			if tc.name == "bound" {
				wantTurns++
			}
			if len(th.Turns) != wantTurns {
				t.Fatalf("duplicate mason turn: %+v", th.Turns)
			}
			transitions, err := trace.Read[trace.Transition](f.repository(), stream)
			if err != nil {
				t.Fatal(err)
			}
			rulings := 0
			for _, transition := range transitions {
				if transition.Cause == fmt.Sprintf("%s-mason-ruling-%d", trace.UnitSubject("resume"), reset) {
					rulings++
					if transition.From != intake.UnitContested || transition.To != intake.UnitImplementing {
						t.Fatalf("ruling transition: %+v", transition)
					}
				}
			}
			if rulings != 1 {
				t.Fatalf("ruling transitions: %d", rulings)
			}
			fake.Check(t)
		})
	}
}

func TestMasonCleanTurnPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, response, class, tool string
	}{
		{"question", "Could you clarify which store to use?", "asked_in_prose", "ask"},
		{"completion", "I have completed the unit.", "claims_done", "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, fake := newMasonFixture(t, 1, fixturekit.ValidPlan)
			defer f.stop(t)
			fake.Response = map[string]string{service.TestMasonTurnID("resume"): tc.response}
			fake.Play[service.TestMasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
				_, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": fixturekit.MasonWrote})
				return err
			}
			delete(fake.Play, intake.MasonAgent("resume")+"-clarify-1")
			f.engine.Mu.Lock()
			f.engine.Turns[intake.MasonAgent("resume")+"-clarify-1"] = fake.Turn
			f.engine.Mu.Unlock()
			stream := f.seedBuilding(t, "classified", fixturekit.ValidPlan)
			intakefixture.Soon(t, "the classification policy to settle", func() bool {
				th, err := f.repository().Thread(stream, intake.MasonAgent("resume"))
				if err != nil || len(th.Turns) == 0 || th.Turns[0].Response == nil || th.Turns[0].Response.Classification == nil {
					return false
				}
				if got := th.Turns[0].Response.Classification.Class; got != tc.class {
					t.Fatalf("class %s, want %s", got, tc.class)
				}
				if got := th.Turns[0].Response.Classification.ToolCounts["file_read"]; got != 1 {
					t.Fatalf("tool count %d, want 1", got)
				}
				return len(th.Turns) > 1 && strings.Contains(th.Turns[1].Request.Prompt, "Call "+tc.tool) && th.Turns[1].Request.ThreadID == th.Identity.ThreadID
			})
			f.awaitEventTurns(t, stream, "classified "+tc.class)
		})
	}
}

func TestMasonJevClassification(t *testing.T) {

	t.Setenv("OSMIA_TEST_JEV_KEY", "key")
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-1.13.0", Answers: map[string]systemone.Answer{
		"class":                   jevtest.Choice("claims_done", 0.94, 0.9, "asked_in_prose", "gave_up", "unclear"),
		"evidence-claims_done":    jevtest.Choice("span-2", 0.9, 0.9, "span-1", "none"),
		"evidence-asked_in_prose": jevtest.Choice("none", 0.9, 0.9, "span-1", "span-2"),
		"evidence-gave_up":        jevtest.Choice("none", 0.9, 0.9, "span-1", "span-2"),
	}}}}}
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", fixturekit.ValidPlan, func(opts *Options) {
		opts.JevProvider = p.Factory()
		configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		fixturekit.Must(t, err)
		_, err = configFile.WriteString("\n[jev]\nmodel = \"jev-test\"\napi_key_env = \"OSMIA_TEST_JEV_KEY\"\n")
		fixturekit.Must(t, errors.Join(err, configFile.Close()))
	})
	defer f.stop(t)
	fake.Response = map[string]string{service.TestMasonTurnID("resume"): "Wrote the store. The unit is ready for review."}
	delete(fake.Play, intake.MasonAgent("resume")+"-clarify-1")
	f.engine.Mu.Lock()
	f.engine.Turns[intake.MasonAgent("resume")+"-clarify-1"] = fake.Turn
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "jev-classified", fixturekit.ValidPlan)
	intakefixture.Soon(t, "Jev's classification to settle", func() bool {
		th, err := f.repository().Thread(stream, intake.MasonAgent("resume"))
		if err != nil || len(th.Turns) <= 1 || th.Turns[0].Response == nil {
			return false
		}
		c := th.Turns[0].Response.Classification
		if c == nil || c.Class != "claims_done" || c.By != trace.ClassifiedByJev || c.Evidence != "The unit is ready for review." || c.Judgment == "" {
			t.Fatalf("classification %+v", c)
		}
		if !strings.Contains(th.Turns[1].Request.Prompt, "Call done") {
			t.Fatalf("continuation prompt %q", th.Turns[1].Request.Prompt)
		}
		f.awaitEventTurns(t, stream, "Jev judgment "+c.Judgment)
		return true
	})
	if requests := p.Requests(); len(requests) == 0 || requests[0].State.(map[string]any)["message"] != "Wrote the store. The unit is ready for review." {
		t.Fatalf("Jev requests %+v", requests)
	}
}

func TestMasonJevFallbackCountsTowardCleanTurnBound(t *testing.T) {
	t.Setenv("OSMIA_TEST_JEV_KEY", "key")
	outage := jevtest.Result{Err: &systemone.Error{Kind: systemone.KindTimeout, Message: "deadline"}}
	p := &jevtest.Provider{Results: slices.Repeat([]jevtest.Result{outage}, 16)}
	f, fake := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", fixturekit.ValidPlan, func(opts *Options) {
		opts.JevProvider = p.Factory()
		configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		fixturekit.Must(t, err)
		_, err = configFile.WriteString("\n[jev]\nmodel = \"jev-test\"\napi_key_env = \"OSMIA_TEST_JEV_KEY\"\n")
		fixturekit.Must(t, errors.Join(err, configFile.Close()))
	})
	defer f.stop(t)
	bound := f.s.cfg.Mason.MaxCleanTurns
	fake.Response = map[string]string{service.TestMasonTurnID("resume"): "Work is ongoing."}
	f.engine.Mu.Lock()
	for i := 1; i < bound; i++ {
		delete(fake.Play, fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i))
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "jev-fallback-bound", fixturekit.ValidPlan)
	f.awaitUnit(t, stream, "resume", intake.UnitContested)
	f.awaitEventTurns(t, stream, "bound exhausted")
	th := f.thread(t, stream, intake.MasonAgent("resume"))
	if len(th.Turns) != bound {
		t.Fatalf("%d mason turns, want the %d of mason.max_clean_turns", len(th.Turns), bound)
	}
	for i, turn := range th.Turns {
		c := turn.Response.Classification
		if c == nil || c.Class != "unclear" || c.By != trace.ClassifiedByRule || c.Judgment != "" {
			t.Fatalf("turn %d classification %+v", i+1, c)
		}
		if i > 0 && (turn.Request.TurnID != fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i) || !strings.Contains(turn.Request.Prompt, "Call ask")) {
			t.Fatalf("turn %d is not the clarify continuation of turn %d: %+v", i+1, i, turn.Request)
		}
	}
	if len(p.Requests()) == 0 {
		t.Fatal("Jev was never asked to classify an ending")
	}
}
