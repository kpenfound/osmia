package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestApprovedChangeSurvivesProjectRemovalGateAndReaddition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	entered, release := f.block(project, "first")
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	s, client := start(t, f.opts)
	await(t, "admitted unit turn", entered)
	wait, closeWait := serviceChanges(t, s)
	defer closeWait()
	requested, api := s.requestChange(ctx, string(stream), ChangeRequest{Key: "boundary-cancel", Kind: "cancel", Scope: "workgroup", Change: "Cancel after this whole unit boundary."})
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
	_, err = client.Reload(ctx)
	assertCode(t, err, Validation)
	if approved.Workgroup == "" || !strings.Contains(err.Error(), string(approved.Workgroup)) || !s.current().Active(project) {
		t.Fatalf("reload did not refuse removal and name the unresolved workgroup: %v", err)
	}
	repo := f.repository(project)
	changes, err := readChanges(repo, stream)
	must(t, err)
	if len(changes) != 1 || changes[0].State == "applied" || len(changes[0].Applied) != 0 || !equalJSONTest(changes[0].Decision, approved.Decision) || !equalJSONTest(changes[0].Members, approved.Members) || !equalJSONTest(changes[0].Before, approved.Before) || !equalJSONTest(changes[0].After, approved.After) {
		t.Fatalf("refused removal lost or applied the approved journal: %+v", changes)
	}
	unblock()
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
			t.Fatalf("completed turn did not apply the original approval: %+v", changes)
		}
		wait()
	}
	finished := changes[0]
	if len(finished.Applied) != 1 || finished.Applied[0] != stream || !equalJSONTest(finished.Decision, approved.Decision) || !equalJSONTest(finished.Members, approved.Members) || !equalJSONTest(finished.Before, approved.Before) || !equalJSONTest(finished.After, approved.After) {
		t.Fatalf("cancellation changed original pins or duplicated effects: %+v", finished)
	}
	// The cancellation resolves the member, so removing and re-adding its
	// project can now reload the journal without repeating its effects.
	_, err = client.Reload(ctx)
	must(t, err)
	if s.current().Active(project) {
		t.Fatal("resolved project stayed active after removal")
	}
	must(t, os.WriteFile(path, original, 0600))
	_, err = client.Reload(ctx)
	must(t, err)
	if !s.current().Active(project) {
		t.Fatal("resolved project was not re-added")
	}
	repo, err = s.repository(project)
	must(t, err)
	changes, err = readChanges(repo, stream)
	must(t, err)
	if len(changes) != 1 || !equalJSONTest(changes[0], finished) {
		t.Fatalf("readdition changed the applied journal: %+v", changes)
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
