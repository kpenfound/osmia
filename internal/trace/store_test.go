package trace

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// reportRevision is a revision of a unit's report document.
func reportRevision(unit string, revision int, content string) Document {
	h := header("document", "report-"+unit)
	h.Revision = revision
	return Document{Header: h, Path: "units/" + unit + "/report.json", Content: content}
}

// directorySize sums the sizes of the regular files in directory.
func directorySize(t *testing.T, directory string) int64 {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

func TestWriteCostDependsOnTheWriteNotTheTrace(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	const units, writes = 20, 2000
	content := strings.Repeat("a", 64*1024)
	var recorded int64
	record := func(d Document) uint64 {
		t.Helper()
		before := s.decoded.Load()
		if err := r.RecordDocuments(ctx, []Document{d}); err != nil {
			t.Fatal(err)
		}
		recorded += int64(len(d.Content))
		return s.decoded.Load() - before
	}
	for u := range units {
		record(reportRevision(fmt.Sprint("u", u), 1, content))
	}
	var first, last uint64
	for i := range writes {
		decoded := record(reportRevision(fmt.Sprint("u", i%units), i/units+2, content))
		if i == 0 {
			first = decoded
		}
		last = decoded
	}
	if last > first {
		t.Fatalf("the last write decoded %d existing records, the first %d", last, first)
	}
	if size := directorySize(t, s.directory); size >= 2*recorded {
		t.Fatalf("the trace holds %d bytes for %d recorded", size, recorded)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != units+writes {
		t.Fatalf("recorded %d revisions: %v", len(docs), err)
	}
}

func TestRecordDocumentsWithIsAtomic(t *testing.T) {
	for _, step := range []string{boundaryBeforeCommit, boundaryCommitted} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			s := mustScoped(t, r, streamID)
			ctx := context.Background()
			injected := errors.New("injected commit failure")
			s.failPublication = func(got string) error {
				if got == step {
					return injected
				}
				return nil
			}
			docs := []Document{reportRevision("u1", 1, "{}\n")}
			tx := transaction("first", 0, "", "active", 2)
			if _, err := r.RecordDocumentsWith(ctx, docs, tx); !errors.Is(err, injected) {
				t.Fatalf("injection not reached: %v", err)
			}
			s.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			committed := step == boundaryCommitted
			recorded, err := Read[Document](r, streamID)
			if err != nil {
				t.Fatal(err)
			}
			state, err := r.Workflow(streamID, "feature")
			if err != nil {
				t.Fatal(err)
			}
			entries := ready(t, r, at)
			if committed != (len(recorded) == 1) || committed != (state.Value == "active") || committed != (len(entries) == 2) {
				t.Fatalf("committed=%t: documents %d, state %+v, outbox %d", committed, len(recorded), state, len(entries))
			}
		})
	}
}

func TestReadsDoNotWaitOnAWrite(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	s.failPublication = func(step string) error {
		if step == boundaryBeforeCommit {
			close(entered)
			<-release
		}
		return nil
	}
	written := make(chan error, 1)
	go func() { written <- r.Append(ctx, specimens()[0]) }()
	<-entered
	read := make(chan error, 1)
	go func() {
		docs, err := Read[Document](r, streamID)
		if err == nil && len(docs) != 0 {
			err = fmt.Errorf("an uncommitted write is visible: %+v", docs)
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read waited on an open write transaction")
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	s.failPublication = nil
	if docs, err := Read[Document](r, streamID); err != nil || len(docs) != 1 {
		t.Fatalf("committed write: %+v %v", docs, err)
	}
}

func TestGenerationChangesOnEveryCommit(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	project := projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1)
	writes := []func() error{
		func() error { return r.Append(ctx, specimens()[0]) },
		func() error { return r.RecordDocuments(ctx, []Document{project}) },
		func() error { _, err := r.Transact(ctx, transaction("first", 0, "", "active", 1)); return err },
		func() error { return r.CreateThread(ctx, threadAgent()) },
	}
	previous := r.Generation()
	for i, write := range writes {
		if err := write(); err != nil {
			t.Fatal(err)
		}
		next := r.Generation()
		if next == previous {
			t.Fatalf("write %d left the generation at %d", i, next)
		}
		previous = next
	}
	if _, err := Read[Document](r, streamID); err != nil {
		t.Fatal(err)
	}
	if r.Generation() != previous {
		t.Fatal("a read changed the generation")
	}
}

func TestStoreFileAliasesAreRefused(t *testing.T) {
	for _, name := range []string{databaseFile, databaseFile + "-wal", databaseFile + "-shm", lockFile} {
		for _, kind := range []string{"symlink", "hardlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				r, root, p := create(t)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(p.Clone, "sentinel")
				if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				full := filepath.Join(r.directory, name)
				_, statErr := os.Lstat(full)
				present := statErr == nil
				switch {
				case kind == "symlink" && present:
					// The symlink names the moved file itself.
					moved := filepath.Join(p.Clone, name)
					if err := os.Rename(full, moved); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(moved, full); err != nil {
						t.Fatal(err)
					}
				case kind == "symlink":
					if err := os.Symlink(sentinel, full); err != nil {
						t.Fatal(err)
					}
				case present:
					if err := os.Link(full, filepath.Join(p.Clone, name)); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.Link(sentinel, full); err != nil {
						t.Fatal(err)
					}
				}
				reopened, err := Open(root, p)
				if err == nil {
					reopened.Close()
					t.Fatal("an aliased trace opened")
				}
				if !strings.Contains(err.Error(), "aliases are forbidden") || !strings.Contains(err.Error(), name) {
					t.Fatalf("alias refusal: %v", err)
				}
				if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
					t.Fatalf("the aliased file changed: %q %v", data, err)
				}
			})
		}
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	r, root, p := create(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	path := TraceDatabase(r.directory)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err == nil {
		reopened.Close()
		t.Fatal("a newer schema opened")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestDamagedDatabaseFailsTheIntegrityCheck(t *testing.T) {
	r, root, p := create(t)
	for i := range 20 {
		if err := r.RecordDocuments(context.Background(), []Document{projectDocument(fmt.Sprint("subsystem-", i), fmt.Sprintf("kb/s%d.md", i), strings.Repeat("x", 2048), 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	path := TraceDatabase(r.directory)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if len(data) < 3*pageSize {
		t.Fatalf("database too small to damage: %d bytes", len(data))
	}
	// Page 1 holds the header and schema; every later page is a table,
	// index or overflow page the check walks.
	for i := pageSize; i < len(data); i++ {
		data[i] = 0xff
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err == nil {
		reopened.Close()
		t.Fatal("a damaged database opened")
	}
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), path) {
		t.Fatalf("damaged database: %v", err)
	}
}

func TestCommitsReportWorkflowChanges(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	var observed []Commit
	r.Observe(func(c Commit) { observed = append(observed, c) })
	workflow := "workstreams/" + string(streamID) + "/workflow.json"

	if err := r.Append(ctx, specimens()[0]); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].Workflow != nil || slices.Contains(observed[0].Paths, workflow) {
		t.Fatalf("a record commit reports a workflow change: %+v", observed)
	}

	observed = nil
	if err := r.RetractChiefOfStaff(ctx, streamID); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].Workflow == nil || !slices.Equal(observed[0].Workflow.Threads, []string{ChiefOfStaff}) || observed[0].Workflow.Deliveries || !slices.Contains(observed[0].Paths, workflow) {
		t.Fatalf("chief-of-staff thread change: %+v", observed)
	}

	tx := transaction("first", 0, "", "active", 1)
	transact(t, r, tx)
	observed = nil
	if _, err := r.Claim(ctx, streamID, tx.Events[0].ID, "token", "worker", at, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].Workflow == nil || !observed[0].Workflow.Deliveries || len(observed[0].Workflow.Threads) != 0 || !slices.Contains(observed[0].Paths, workflow) {
		t.Fatalf("delivery claim: %+v", observed)
	}
}
