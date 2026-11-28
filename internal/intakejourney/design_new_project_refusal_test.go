package intakejourney

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/trace"
)

type takenAfterStart struct {
	pulls.Repositories
	armed, free atomic.Bool
	reads       atomic.Int32
}

func (r *takenAfterStart) Repository(ctx context.Context, owner, name string) (pulls.Repository, bool, error) {
	repo, found, err := r.Repositories.Repository(ctx, owner, name)
	if name != "gadget" || !r.armed.Load() {
		return repo, found, err
	}
	if r.reads.Add(1) > 1 && !r.free.Load() {
		return pulls.Repository{}, true, err
	}
	return repo, found, err
}

func TestNewProjectStartRefusedAfterStartReachesTheInboxAndTheManager(t *testing.T) {
	f := newNewProjectStartFixture(t, true)
	ctx := context.Background()
	repos := &takenAfterStart{Repositories: f.github.Client()}
	f.opts.Repositories = repos
	f.restart(t)
	stream, revision := f.draft(t, map[string]any{"new_project": f.gadget(nil)})
	repos.armed.Store(true)
	_, err := f.c.DesignStartWithBriefRevision(ctx, stream, revision)
	fixturekit.Must(t, err)

	entry := f.creationEntry(t)
	if entry.Workstream != stream || entry.Project != f.home || !strings.Contains(entry.Question, "already exists on GitHub") || !strings.Contains(entry.Question, "tries again on every pass") {
		t.Fatalf("the entry: %+v", entry)
	}
	if entry.Answer.Path != "/v1/projects/create" || entry.Answer.Body["id"] != string(f.intent(t, stream).Creation) || entry.Answer.Body["repo"] != "gadget" || len(entry.Options) != 1 || entry.Options[0] != "retry" {
		t.Fatalf("the entry's retry: %+v", entry)
	}
	refusalTurns := func() []trace.QueuedTurn {
		th, err := f.homeRepository(t).Thread(stream, trace.Manager)
		fixturekit.Must(t, err)
		return slices.DeleteFunc(slices.Clone(th.Turns), func(q trace.QueuedTurn) bool {
			return !strings.HasPrefix(q.Request.TurnID, "design-project-refused-")
		})
	}
	f.await(t, "the manager's turn about the refusal", func() bool {
		turns := refusalTurns()
		return len(turns) == 1 && turns[0].Response != nil
	})
	if turn := refusalTurns()[0]; !strings.Contains(turn.Request.Prompt, "already exists on GitHub") || !strings.Contains(turn.Request.Prompt, "tries the creation again on every pass") {
		t.Fatalf("the manager's turn: %s", turn.Request.Prompt)
	}
	seen := repos.reads.Load()
	f.await(t, "more passes over the refused creation", func() bool { return repos.reads.Load() >= seen+3 })
	transitions, err := trace.Read[trace.Transition](f.homeRepository(t), stream)
	fixturekit.Must(t, err)
	refusals := 0
	for _, tr := range transitions {
		if tr.Subject == intake.DesignCreationRefusalSubject {
			refusals++
		}
	}
	if refusals != 1 || len(refusalTurns()) != 1 {
		t.Fatalf("the same refusal was recorded %d times and told %d times", refusals, len(refusalTurns()))
	}
	if state := f.startState(t, stream); state == "complete" {
		t.Fatal("the start completed without its project")
	}

	repos.free.Store(true)
	f.awaitComplete(t, stream)
	f.assertGadgetCreated(t, stream)
	if inbox, err := f.c.Inbox(ctx); err != nil || slices.ContainsFunc(inbox.Entries, func(e api.InboxEntry) bool { return e.Kind == api.InboxProjectCreation }) {
		t.Fatalf("the entry stayed after the creation: %+v %v", inbox.Entries, err)
	}
}
