package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestApprovedChangeSurvivesProjectRemovalAndReaddition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	s, client := start(t, f.opts)
	await(t, "admitted unit turn", entered)
	wait, closeWait := serviceChanges(t, s)
	defer closeWait()
	requested, api := s.requestChange(ctx, string(stream), ChangeRequest{Key: "draining-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel after this whole unit boundary."})
	if api != nil {
		t.Fatal(api)
	}
	deadline := time.Now().Add(demoTimeout)
	for requested.State != "presented" {
		if time.Now().After(deadline) {
			t.Fatalf("controller did not present the impact summary: %+v", requested)
		}
		wait()
		requested = currentChange(t, s, stream, requested.ID)
	}
	approved, api := s.decideChange(ctx, string(stream), requested.ID, ChangeDecision{Revision: requested.Revision, Decision: "approve", PullRequests: "draft"})
	if api != nil {
		t.Fatal(api)
	}
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, []byte(strings.Replace(string(original), `"`+string(project)+`", `, "", 1)), 0600))
	drained := make(chan error, 1)
	go func() { _, err := client.Reload(ctx); drained <- err }()
	deadline = time.Now().Add(demoTimeout)
	for s.current().Active(project) {
		if time.Now().After(deadline) {
			t.Fatal("removed project's turn did not enter draining")
		}
		wait()
	}
	select {
	case err := <-drained:
		t.Fatalf("reload did not retain the admitted boundary: %v", err)
	default:
	}
	repo := f.repository(project)
	changes, err := readChanges(repo, stream)
	must(t, err)
	if len(changes) != 1 || changes[0].State == "applied" || len(changes[0].Applied) != 0 || !equalJSONTest(changes[0].Decision, approved.Decision) || !equalJSONTest(changes[0].Members, approved.Members) || !equalJSONTest(changes[0].Before, approved.Before) {
		t.Fatalf("draining lost or applied the approved journal: %+v", changes)
	}
	close(release)
	select {
	case err := <-drained:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("reload did not finish after the admitted turn completed")
	}
	must(t, os.WriteFile(path, original, 0600))
	_, err = client.Reload(ctx)
	must(t, err)
	deadline = time.Now().Add(demoTimeout)
	for {
		repo, err = s.repository(project)
		must(t, err)
		changes, err = readChanges(repo, stream)
		must(t, err)
		if len(changes) == 1 && changes[0].State == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readded project did not resume its original approval: %+v", changes)
		}
		wait()
	}
	finished := changes[0]
	if len(finished.Applied) != 1 || finished.Applied[0] != stream || !equalJSONTest(finished.Decision, approved.Decision) || !equalJSONTest(finished.Members, approved.Members) || !equalJSONTest(finished.Before, approved.Before) || !equalJSONTest(finished.After, approved.After) {
		t.Fatalf("readdition changed original pins or duplicated effects: %+v", finished)
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	count := 0
	for _, tr := range transitions {
		if tr.ID == "cancel-"+requested.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("approved cancellation landed %d times", count)
	}
}
