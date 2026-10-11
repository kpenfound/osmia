package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestPurgeWorkstreamErasesOutputAndPreservesOtherHistory(t *testing.T) {
	for _, boundary := range []string{"", "purge-journal-written", "purge-ref-published", "purge-files-removed", "purge-pruning", "purge-objects-pruned"} {
		t.Run(boundary, func(t *testing.T) {
			r, root, project := create(t)
			ctx := context.Background()
			other := config.WorkstreamID("w_00000000000000000000000000000002")
			if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
				t.Fatal(err)
			}
			doc := Document{Header: header("document", "check-output"), Path: "units/u/checks-1-output.txt", Content: "private full stdout and stderr\n"}
			kept := Document{Header: header("document", "spec"), Path: "spec.md", Content: "Retained spec revision one\n"}
			kept.Workstream = other
			for _, d := range []Document{doc, kept} {
				if err := r.RecordDocuments(ctx, []Document{d}); err != nil {
					t.Fatal(err)
				}
			}
			blob := gitOutput(t, r, "rev-parse", "HEAD:workstreams/"+string(streamID)+"/"+doc.Path)
			keptBlob := gitOutput(t, r, "rev-parse", "HEAD:workstreams/"+string(other)+"/spec.md")
			kept.Revision, kept.Content = 2, "Retained spec revision two\n"
			if err := r.RecordDocuments(ctx, []Document{kept}); err != nil {
				t.Fatal(err)
			}
			if err := r.PurgeWorkstream(ctx, streamID); err == nil {
				t.Fatal("purged a nonterminal workstream")
			}
			if _, err := r.SetFeatureState(ctx, header("transition", "finish"), "abandoned", "owner ended work"); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("interrupted purge")
			r.failPublication = func(at string) error {
				if at == "purge-journal-written" {
					data, err := r.readFile(purgeFile)
					if err != nil {
						return err
					}
					var journal purgeJournal
					if err := json.Unmarshal(data, &journal); err != nil {
						return err
					}
					journal.Workstream, journal.Workstreams = streamID, nil
					data, err = json.Marshal(journal)
					if err != nil {
						return err
					}
					if err := r.writeFile(purgeFile, data); err != nil {
						return err
					}
				}
				if at == boundary {
					return injected
				}
				return nil
			}
			err := r.PurgeWorkstream(ctx, streamID)
			if boundary == "" && err != nil || boundary != "" && !errors.Is(err, injected) {
				t.Fatalf("purge: %v", err)
			}
			r.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(root, project)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			if err := reopened.PurgeWorkstream(ctx, streamID); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.dir.Stat("workstreams/" + string(streamID)); !os.IsNotExist(err) {
				t.Fatalf("trace still present: %v", err)
			}
			if _, err := reopened.git(ctx, nil, "cat-file", "-e", blob); err == nil {
				t.Fatal("deleted output remains in Git objects")
			}
			if got := gitOutput(t, reopened, "cat-file", "blob", keptBlob); got != "Retained spec revision one" {
				t.Fatalf("other history lost: %q", got)
			}
			if history := gitOutput(t, reopened, "rev-list", "--objects", "--all"); strings.Contains(history, "workstreams/"+string(streamID)) {
				t.Fatal("deleted workstream remains in history")
			}
			docs, err := Read[Document](reopened, other)
			if err != nil || len(docs) != 2 {
				t.Fatalf("other revisions: %+v %v", docs, err)
			}
			kept.Revision, kept.Content = 3, "Still writable\n"
			if err := reopened.RecordDocuments(ctx, []Document{kept}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPurgeWaitsForOperationAcknowledgement(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	tx := operationTransaction()
	tx.Transition.To = "abandoned"
	transact(t, r, tx)
	event := tx.Events[0].ID
	if err := r.PurgeWorkstream(ctx, streamID); err == nil {
		t.Fatal("purged pending operation")
	}
	err := r.WithOperation(ctx, streamID, event, Actor{Kind: "service", ID: "test"}, func() time.Time { return at }, func(a *OperationAttempt, _ OperationRecord) error {
		observe := a.Action("observe", at)
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "already applied", Result: &coreadapter.OperationResult{Outcome: "done", Evidence: "recorded"}}
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		result := a.Action("result", at)
		result.Result = observe.Observation.Result
		return a.Record(ctx, result)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkstream(ctx, streamID); err == nil {
		t.Fatal("purged unacknowledged result")
	}
	err = r.WithOperation(ctx, streamID, event, Actor{Kind: "service", ID: "test"}, func() time.Time { return at }, func(a *OperationAttempt, _ OperationRecord) error {
		if err := a.Record(ctx, a.Action("acknowledge", at)); err != nil {
			return err
		}
		if err := r.PurgeWorkstream(ctx, streamID); err == nil {
			t.Fatal("purged while operation attempt remained in flight")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkstream(ctx, streamID); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeBatchErasesOutputAndPreservesOtherHistory(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"", "purge-journal-written", "purge-ref-published", "purge-files-removed", "purge-pruning", "purge-objects-pruned"} {
		t.Run(boundary, func(t *testing.T) {
			r, root, project := create(t)
			ctx := context.Background()
			second := config.WorkstreamID("w_00000000000000000000000000000003")
			if err := r.CreateWorkstream(ctx, second, at, owner); err != nil {
				t.Fatal(err)
			}
			if _, err := r.SetWorkstreamBase(ctx, second, streamID, 0, at); err != nil {
				t.Fatal(err)
			}
			secondHeader := header("transition", "second-finish")
			secondHeader.Workstream = second
			if _, err := r.SetFeatureState(ctx, secondHeader, "abandoned", "owner ended work"); err != nil {
				t.Fatal(err)
			}
			secret := Document{Header: header("document", "second-output"), Path: "units/u/checks-2-output.txt", Content: "second private output"}
			secret.Workstream = second
			if err := r.RecordDocuments(ctx, []Document{secret}); err != nil {
				t.Fatal(err)
			}
			secondBlob := gitOutput(t, r, "rev-parse", "HEAD:workstreams/"+string(second)+"/units/u/checks-2-output.txt")
			other := config.WorkstreamID("w_00000000000000000000000000000002")
			if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
				t.Fatal(err)
			}
			doc := Document{Header: header("document", "check-output"), Path: "units/u/checks-1-output.txt", Content: "private full stdout and stderr\n"}
			kept := Document{Header: header("document", "spec"), Path: "spec.md", Content: "Retained spec revision one\n"}
			kept.Workstream = other
			for _, d := range []Document{doc, kept} {
				if err := r.RecordDocuments(ctx, []Document{d}); err != nil {
					t.Fatal(err)
				}
			}
			blob := gitOutput(t, r, "rev-parse", "HEAD:workstreams/"+string(streamID)+"/"+doc.Path)
			keptBlob := gitOutput(t, r, "rev-parse", "HEAD:workstreams/"+string(other)+"/spec.md")
			kept.Revision, kept.Content = 2, "Retained spec revision two\n"
			if err := r.RecordDocuments(ctx, []Document{kept}); err != nil {
				t.Fatal(err)
			}
			if err := r.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second, streamID}, nil); err == nil {
				t.Fatal("purged a nonterminal workstream")
			}
			if _, err := r.SetFeatureState(ctx, header("transition", "finish"), "abandoned", "owner ended work"); err != nil {
				t.Fatal(err)
			}
			if err := r.PurgeWorkstream(ctx, streamID); err == nil {
				t.Fatal("purged base of retained dependent")
			}
			journals, prunes := 0, 0
			injected := errors.New("interrupted purge")
			r.failPublication = func(at string) error {
				if at == "purge-journal-written" {
					journals++
				}
				if at == "purge-objects-pruned" {
					prunes++
				}
				if at == boundary {
					return injected
				}
				return nil
			}
			err := r.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second, streamID}, nil)
			if boundary == "" && err != nil || boundary != "" && !errors.Is(err, injected) {
				t.Fatalf("purge: %v", err)
			}
			if boundary == "" && (journals != 1 || prunes != 1) {
				t.Fatalf("batch publications/prunes: %d/%d", journals, prunes)
			}
			r.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(root, project)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			if err := reopened.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.dir.Stat("workstreams/" + string(streamID)); !os.IsNotExist(err) {
				t.Fatalf("trace still present: %v", err)
			}
			if _, err := reopened.dir.Stat("workstreams/" + string(second)); !os.IsNotExist(err) {
				t.Fatalf("second trace remains: %v", err)
			}
			if _, err := reopened.git(ctx, nil, "cat-file", "-e", secondBlob); err == nil {
				t.Fatal("second output remains")
			}
			if _, err := reopened.git(ctx, nil, "cat-file", "-e", blob); err == nil {
				t.Fatal("deleted output remains in Git objects")
			}
			if got := gitOutput(t, reopened, "cat-file", "blob", keptBlob); got != "Retained spec revision one" {
				t.Fatalf("other history lost: %q", got)
			}
			if history := gitOutput(t, reopened, "rev-list", "--objects", "--all"); strings.Contains(history, "workstreams/"+string(streamID)) {
				t.Fatal("deleted workstream remains in history")
			}
			docs, err := Read[Document](reopened, other)
			if err != nil || len(docs) != 2 {
				t.Fatalf("other revisions: %+v %v", docs, err)
			}
			kept.Revision, kept.Content = 3, "Still writable\n"
			if err := reopened.RecordDocuments(ctx, []Document{kept}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestPruningDoesNotFailAReadOrAConcurrentWriteOfRetainedObjects covers
// spec#6 and plan#archive-cleanup acceptance item 3: pruning (prunePurged's
// reflog expiry and destructive gc --prune=now) must never fail a read or a
// write that still needs objects a purge did not touch. It stalls a purge at
// "purge-pruning", which the prune reaches only once pruneMu is already held,
// the same exclusion a concurrent object-creating write relies on. While
// stalled, it shows a read of a retained workstream's committed history
// succeeds immediately, unaffected by the in-progress prune. It then starts a
// concurrent write to that workstream and shows it is fenced (it does not
// race the destructive gc step; it blocks until pruning finishes) rather than
// failing, and that the write is durably recorded once pruning completes.
func TestPruningDoesNotFailAReadOrAConcurrentWriteOfRetainedObjects(t *testing.T) {
	t.Parallel()
	r, _, _ := create(t)
	ctx := context.Background()
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
		t.Fatal(err)
	}
	kept := Document{Header: header("document", "spec"), Path: "spec.md", Content: "Retained spec revision one\n"}
	kept.Workstream = other
	if err := r.RecordDocuments(ctx, []Document{kept}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "finish"), "abandoned", "owner ended work"); err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.failPublication = func(at string) error {
		if at != "purge-pruning" {
			return nil
		}
		once.Do(func() { close(started) })
		<-release
		return nil
	}
	defer func() { r.failPublication = nil }()

	purgeDone := make(chan error, 1)
	go func() { purgeDone <- r.PurgeWorkstream(ctx, streamID) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("purge never reached pruning")
	}

	// Pruning is stalled, holding pruneMu, with nothing else in flight yet:
	// a read of the retained workstream's history must still succeed.
	if docs, err := Read[Document](r, other); err != nil || len(docs) != 1 {
		t.Fatalf("read failed while pruning was in progress: %+v %v", docs, err)
	}

	// A concurrent write that creates Git objects must be fenced against
	// the destructive gc step rather than racing it or failing.
	kept.Revision, kept.Content = 2, "Retained spec revision two\n"
	writeDone := make(chan error, 1)
	go func() { writeDone <- r.RecordDocuments(ctx, []Document{kept}) }()
	select {
	case err := <-writeDone:
		t.Fatalf("write was not fenced against the in-progress prune: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write failed once pruning finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fenced write never completed")
	}
	select {
	case err := <-purgeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("purge never finished")
	}
	r.failPublication = nil

	if _, err := r.dir.Stat("workstreams/" + string(streamID)); !os.IsNotExist(err) {
		t.Fatalf("trace still present: %v", err)
	}
	docs, err := Read[Document](r, other)
	if err != nil || len(docs) != 2 {
		t.Fatalf("write fenced during pruning was lost: %+v %v", docs, err)
	}
}

// TestPruningExcludesAConcurrentRewriteFromCreatingUnreachableObjects covers
// the other half of spec#6: a history rewrite's commits and trees are
// unreachable from any ref until the publish that follows flips main to
// them, so a prune running at the same time could collect them before they
// are ever reachable. purgeMu, held by rewriteAndPublish for a whole attempt
// and by prunePurged for its destructive steps, rules this out structurally
// rather than relying on PurgeWorkstreams's single caller never overlapping
// itself: it stalls a purge's prune at "purge-pruning", holding purgeMu, and
// shows a second, independent purge's rewriteAndPublish call blocks
// acquiring purgeMu rather than proceeding to create objects the stalled
// prune could race, and completes only once the prune finishes.
func TestPruningExcludesAConcurrentRewriteFromCreatingUnreachableObjects(t *testing.T) {
	t.Parallel()
	r, _, _ := create(t)
	ctx := context.Background()
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
		t.Fatal(err)
	}
	otherHeader := header("transition", "other-finish")
	otherHeader.Workstream = other
	if _, err := r.SetFeatureState(ctx, otherHeader, "abandoned", "owner ended work"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetFeatureState(ctx, header("transition", "finish"), "abandoned", "owner ended work"); err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.failPublication = func(at string) error {
		if at != "purge-pruning" {
			return nil
		}
		once.Do(func() { close(started) })
		<-release
		return nil
	}
	defer func() { r.failPublication = nil }()

	purgeDone := make(chan error, 1)
	go func() { purgeDone <- r.PurgeWorkstream(ctx, streamID) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("purge never reached pruning")
	}

	rewriteStarted := make(chan struct{})
	rewriteDone := make(chan error, 1)
	go func() {
		close(rewriteStarted)
		rewriteDone <- r.rewriteAndPublish(ctx, []config.WorkstreamID{other}, nil)
	}()
	<-rewriteStarted
	select {
	case err := <-rewriteDone:
		t.Fatalf("a concurrent rewrite proceeded while a prune held purgeMu: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-purgeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("purge never finished")
	}
	select {
	case err := <-rewriteDone:
		if err != nil {
			t.Fatalf("rewrite failed once the prune released purgeMu: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rewrite excluded by purgeMu never completed")
	}
	r.failPublication = nil
	if err := r.FinishPurge(ctx, nil); err != nil {
		t.Fatal(err)
	}

	remaining, err := r.Workstreams()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(remaining, streamID) || slices.Contains(remaining, other) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}
