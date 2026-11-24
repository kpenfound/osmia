package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

func publishThrough(t *testing.T, s *Service, repo *trace.Repository) []hooksPost {
	t.Helper()
	t.Setenv("PUBLISH_TEST_TOKEN", publishTestToken)
	hooks := newHooksFake(t)
	cfg := *s.current()
	cfg.Hearsay = config.Hearsay{URL: "https://memory.example", Principal: "owner", TokenEnv: "PUBLISH_TEST_OWNER",
		Publish: &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: hooks.URL, Agent: "osmia-publisher", TokenEnv: "PUBLISH_TEST_TOKEN"}}
	res, err := s.publishPass(context.Background(), repo, &cfg)
	intakefixture.Must(t, err)
	if len(res.Failures) != 0 || res.Unreachable {
		t.Fatalf("pass: %+v", res)
	}
	return hooks.take()
}

// A ruling Jev made through the gate worker on an escalated question, and the
// chief of staff's relay of it, reach the work source as one decision of Jev's
// with no author. Nothing of it reaches the owner source.
func TestPublishPostsTheRulingTheGateWorkerRecordedForJev(t *testing.T) {
	ctx := context.Background()
	p := &gateJev{confidence: 0.95, pick: func(_ string, options []string) string { return options[1] }}
	f, repo, stream := newEscalationFixture(t, p)
	chief := escalate(t, f, masonRole, "Resume from the last chunk.", "Restart the upload.")
	gates := f.gates(t)
	if len(gates) != 1 {
		t.Fatalf("gates %+v", gates)
	}
	f.w.handle(ctx, gates[0])
	judgment, _, ok := decisionJudgment(t, repo, stream, jev.KindEscalatedQuestion, jev.QuestionKey("1"))
	if !ok {
		t.Fatal("the gate recorded no judgment")
	}
	if _, err := repo.RelayRuling(ctx, "chief", chief, "1", "Restart the upload.", trace.ScopeLocal, f.now()); err != nil {
		t.Fatal(err)
	}

	posts := publishThrough(t, f.s, repo)
	var rulings []hooksPost
	for _, d := range decisions(posts) {
		if d.str("question") == string(stream)+":1" {
			rulings = append(rulings, d)
		}
	}
	if len(rulings) != 1 {
		t.Fatalf("%d decisions about the question: %v", len(rulings), kinds(posts))
	}
	requireWorkDecision(t, rulings[0], judgment)
	if rulings[0].str("outcome") != "Restart the upload." || rulings[0].str("decision") != string(stream)+"-jev-ruling-1" || !slices.Contains(contractDeciders, rulings[0].str("decider")) {
		t.Fatalf("Jev's ruling: %s", rulings[0].Raw)
	}
	for _, post := range posts {
		if post.Source == "osmia-owner" && post.str("kind") == "decision" {
			t.Fatalf("a decision reached the owner source: %s", post.Raw)
		}
		if strings.Contains(post.Raw, `"author":"owner"`) && post.str("kind") == "decision" {
			t.Fatalf("a decision names the owner as author: %s", post.Raw)
		}
	}
}
