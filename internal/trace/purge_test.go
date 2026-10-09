package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestPurgeWorkstreamErasesOutputAndPreservesOtherHistory(t *testing.T) {
	for _, boundary := range []string{"", "purge-journal-written", "purge-ref-published", "purge-files-removed", "purge-objects-pruned"} {
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
	for _, boundary := range []string{"", "purge-journal-written", "purge-ref-published", "purge-files-removed", "purge-objects-pruned"} {
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
			if err := r.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second, streamID}); err == nil {
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
			err := r.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second, streamID})
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
			if err := reopened.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, second}); err != nil {
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
