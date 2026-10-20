package trace

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// ErrMigrationIncomplete reports that a trace could not finish moving from
// its Git repository to its database. The caller must not open or schedule
// the project; the Git trace stays authoritative and unchanged apart from a
// finished publication, and a later call to Migrate resumes.
var ErrMigrationIncomplete = errors.New("trace migration is incomplete")

// migrationInterrupt lets tests simulate a crash at a named step of a
// trace's migration. It is nil in production.
var migrationInterrupt func(string) error

// importSuffix names the database a migration builds beside the trace
// before it switches to it.
const importSuffix = ".import"

// Migrate moves every Git trace of project to its own database: the
// project's trace and each workstream trace of the project. A trace that
// already has a database is only cleaned of its Git files. A workstream
// already being deleted is left as it is.
//
// For each trace it takes the Git trace's lock, finishes an interrupted
// publication, records the owner's pending hand edits of the charter or of a
// workstream's spec and plan, imports HEAD, verifies the import, and only
// then switches to the database and removes the Git files. check validates
// a workstream's spec and plan, by record ID, before a hand edit of them is
// recorded; an invalid draft stops that workstream's migration with an
// error naming the files and the problems, and the project must not open.
// Bytes in the work tree that HEAD does not hold are dropped, and each
// dropped path is logged. A crash at any step resumes on the next call.
//
// logf receives each step for each trace, naming only the project and
// workstream; it never receives trace content. A nil logf discards them.
func Migrate(ctx context.Context, root config.Root, project config.ProjectID, check func(map[string]string) error, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := config.CheckProjectIDs(project); err != nil {
		return err
	}
	directory, err := root.ProjectTrace(project)
	if err != nil {
		return err
	}
	if err := migrateTrace(ctx, migration{project: project, directory: directory, check: check, logf: logf}); err != nil {
		return err
	}
	return migrateWorkstreams(ctx, root, project, check, logf)
}

// MigrateWorkgroups moves every cross-repo workgroup's Git trace, and the
// Git trace of its host workstream, to their databases, as Migrate does for
// a project.
func MigrateWorkgroups(ctx context.Context, root config.Root, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	entries, err := os.ReadDir(filepath.Join(root.String(), "workgroups"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		id := config.WorkgroupID(entry.Name())
		if config.CheckWorkgroupIDs(id) != nil || !entry.IsDir() {
			continue
		}
		directory, err := root.WorkgroupTrace(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		project := crossRepoProject(id)
		if err := migrateTrace(ctx, migration{project: project, directory: directory, logf: logf}); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, migrateWorkstreams(ctx, root, project, nil, logf))
	}
	return errors.Join(errs...)
}

func migrateWorkstreams(ctx context.Context, root config.Root, project config.ProjectID, check func(map[string]string) error, logf func(string, ...any)) error {
	base, err := root.WorkstreamTraces(project)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		stream, err := config.ParseWorkstreamID(entry.Name())
		if err != nil || !entry.IsDir() {
			return fmt.Errorf("%s: not a workstream trace directory", entry.Name())
		}
		directory, err := root.WorkstreamTrace(project, stream)
		if err != nil {
			return err
		}
		if err := migrateTrace(ctx, migration{project: project, stream: stream, directory: directory, check: check, logf: logf}); err != nil {
			return err
		}
	}
	return nil
}

// migration is the migration of one trace directory.
type migration struct {
	project   config.ProjectID
	stream    config.WorkstreamID
	directory string
	check     func(map[string]string) error
	logf      func(string, ...any)
}

func (m migration) name() string {
	if m.stream == "" {
		return "project " + string(m.project)
	}
	return "project " + string(m.project) + ": workstream " + string(m.stream)
}

func (m migration) log(format string, args ...any) {
	m.logf("migration: "+m.name()+": "+format, args...)
}

func (m migration) boundary(step string) error {
	if migrationInterrupt != nil {
		return migrationInterrupt(step + ":" + m.name())
	}
	return nil
}

// failed reports a failed step of m.
func (m migration) failed(step string, err error) error {
	m.log("%s failed: %v", step, err)
	return fmt.Errorf("%w: %s: %s: %w", ErrMigrationIncomplete, m.name(), step, err)
}

func migrateTrace(ctx context.Context, m migration) error {
	switched, err := HasTraceDatabase(m.directory)
	if err != nil {
		return err
	}
	legacy, err := hasLegacyTrace(m.directory)
	if err != nil {
		return err
	}
	if switched {
		if !legacy {
			return nil
		}
		return m.cleanup()
	}
	if !legacy {
		return nil
	}
	if m.stream == "" {
		if _, err := os.Lstat(filepath.Join(m.directory, "workstreams")); err == nil {
			return fmt.Errorf("%w: %s: %s keeps every workstream inside the project trace; that layout is no longer supported", ErrMigrationIncomplete, m.name(), m.directory)
		}
		if _, err := os.Lstat(filepath.Join(m.directory, legacyReclaimFile)); err == nil {
			return fmt.Errorf("%w: %s: %s holds an unfinished shared-layout migration; that layout is no longer supported", ErrMigrationIncomplete, m.name(), m.directory)
		}
	}
	l, err := openLegacyTrace(m.directory)
	if err != nil {
		return m.failed("lock", err)
	}
	defer l.Close()
	// Another migration may have switched while this one waited.
	if switched, err := HasTraceDatabase(m.directory); err != nil || switched {
		if err != nil {
			return err
		}
		return m.cleanup()
	}
	m.log("starting migration from Git to the trace database")
	importPath := filepath.Join(m.directory, databaseFile+importSuffix)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(importPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return m.failed("discard an interrupted import", err)
		}
	}
	if err := l.finishPublication(ctx); err != nil {
		return m.failed("finish the publication journal", err)
	}
	if err := m.boundary("migration-journal-finished"); err != nil {
		return err
	}
	head, err := l.head()
	if err != nil {
		return m.failed("read HEAD", err)
	}
	tree, err := l.tree(ctx, head)
	if err != nil {
		return m.failed("read HEAD", err)
	}
	files, err := l.blobs(ctx, tree)
	if err != nil {
		return m.failed("read HEAD", err)
	}
	plan, err := m.plan(files)
	if err != nil {
		return m.failed("plan the import", err)
	}
	work, err := l.workTree()
	if err != nil {
		return m.failed("read the work tree", err)
	}
	edits, edited, err := m.handEdits(plan, work)
	if err != nil {
		return m.failed("record owner edits", err)
	}
	for _, name := range slices.Sorted(maps.Keys(work)) {
		if m.stream == "" && name == "config.toml" {
			continue
		}
		if data, ok := files[name]; (!ok || !bytes.Equal(data, work[name])) && !edited[name] {
			m.log("dropping %s, which HEAD does not hold", name)
		}
	}
	m.log("importing HEAD")
	imported, err := m.importHead(ctx, importPath, plan)
	if err != nil {
		return m.failed("import", err)
	}
	ok := false
	defer func() {
		if !ok {
			imported.closeDatabases()
		}
	}()
	if err := m.boundary("migration-imported"); err != nil {
		return err
	}
	m.log("verifying the import")
	if err := m.verify(ctx, imported, plan, importPath); err != nil {
		return m.failed("verify", err)
	}
	if err := m.boundary("migration-verified"); err != nil {
		return err
	}
	if len(edits) > 0 {
		m.log("recording %d owner edits", len(edits))
		imported.mu.Lock()
		for _, d := range edits {
			if err = imported.append(ctx, d); err != nil {
				break
			}
		}
		imported.mu.Unlock()
		if err != nil {
			return m.failed("record owner edits", err)
		}
	}
	m.log("switching to the trace database")
	ok = true
	if err := m.switchTo(ctx, imported, importPath); err != nil {
		return m.failed("switch", err)
	}
	if err := m.boundary("migration-switched"); err != nil {
		return err
	}
	if err := m.cleanup(); err != nil {
		return err
	}
	m.log("migration complete")
	return nil
}

// importPlan is a legacy trace's HEAD sorted into what the database holds.
type importPlan struct {
	// order lists the record logs in the order they are imported, and
	// lines holds each one's lines.
	order []string
	lines map[string][][]byte
	// records holds every record that decodes and validates, in import order.
	records []Record
	files   map[string][]byte
	// workflow is the workstream's workflow.json, when HEAD holds one.
	workflow []byte
}

// plan sorts HEAD's files. Every file must be a record log, a kept file,
// the workflow, or the materialized file of a document's latest revision.
func (m migration) plan(head map[string][]byte) (importPlan, error) {
	p := importPlan{lines: map[string][][]byte{}, files: map[string][]byte{}}
	names := slices.Collect(maps.Keys(head))
	walkOrder(names)
	var derived []string
	for _, name := range names {
		if m.stream != "" && !strings.HasPrefix(name, workstreamPrefix(string(m.stream))) {
			return p, fmt.Errorf("%s: HEAD holds a file of another trace", name)
		}
		switch {
		case legacyRecordFile(name):
			p.order = append(p.order, name)
			p.lines[name] = legacyLines(head[name])
			for _, line := range p.lines[name] {
				if v, err := decodeRecord(line); err == nil && validate(v) == nil {
					p.records = append(p.records, v)
				}
			}
		case legacyFile(name):
			p.files[name] = head[name]
		case m.stream != "" && name == workstreamPrefix(string(m.stream))+"workflow.json":
			p.workflow = head[name]
		default:
			derived = append(derived, name)
		}
	}
	latest := map[string]Document{}
	for _, v := range p.records {
		if d, ok := v.(Document); ok {
			latest[scopePath(d.Header)+d.Path] = d
		}
	}
	for _, name := range derived {
		d, ok := latest[name]
		switch {
		case ok && d.Content == string(head[name]):
		case !ok && m.stream == "" && name == EntitiesPath && string(head[name]) == "{}\n":
		default:
			return p, fmt.Errorf("%s: HEAD holds a file that is neither a record log, a trace file nor a document's latest revision", name)
		}
	}
	return p, nil
}

// handEdits returns the owner's hand edits of the charter or of the
// workstream's spec and plan as their next revisions, and the paths they
// record. An invalid spec or plan is refused with the problems.
func (m migration) handEdits(p importPlan, work map[string][]byte) ([]Document, map[string]bool, error) {
	latest := map[string]Document{}
	for _, v := range p.records {
		if d, ok := v.(Document); ok && d.Workstream == m.stream {
			latest[d.Path] = d
		}
	}
	at := time.Now().UTC()
	edited := map[string]bool{}
	if m.stream == "" {
		data, ok := work["charter.md"]
		if !ok || data == nil {
			return nil, edited, nil
		}
		next := charterRevision(m.project, at, ownerActor, "owner-edit", 1, string(data))
		if d, ok := latest["charter.md"]; ok {
			if d.Content == string(data) {
				return nil, edited, nil
			}
			next.Revision = d.Revision + 1
		}
		edited["charter.md"] = true
		m.log("recording the owner's edit of charter.md")
		return []Document{next}, edited, nil
	}
	prefix := workstreamPrefix(string(m.stream))
	content := map[string]string{}
	var changed []string
	for _, id := range slices.Sorted(maps.Keys(ownerEditable)) {
		d, ok := latest[ownerEditable[id]]
		if !ok || d.ID != id {
			continue
		}
		content[id] = d.Content
		data, ok := work[prefix+d.Path]
		if ok && data != nil && string(data) != d.Content {
			content[id] = string(data)
			changed = append(changed, id)
		}
	}
	if len(changed) == 0 {
		return nil, edited, nil
	}
	var paths []string
	for _, id := range changed {
		paths = append(paths, prefix+ownerEditable[id])
	}
	if len(content) == len(ownerEditable) && m.check != nil {
		if err := m.check(content); err != nil {
			return nil, nil, fmt.Errorf("the owner's edit of %s is invalid; fix or revert it and restart: %w", strings.Join(paths, " and "), err)
		}
	}
	var docs []Document
	for _, id := range changed {
		d := latest[ownerEditable[id]]
		d.Revision++
		d.Content, d.At, d.Actor, d.Cause, d.Depth = content[id], at, ownerActor, "owner-edit", 0
		docs = append(docs, d)
		edited[prefix+d.Path] = true
	}
	m.log("recording the owner's edit of %s", strings.Join(paths, " and "))
	return docs, edited, nil
}

// importHead writes p to a new database at path, in one transaction.
func (m migration) importHead(ctx context.Context, path string, p importPlan) (*Repository, error) {
	write, read, err := openDatabases(path)
	if err != nil {
		return nil, err
	}
	r := &Repository{project: m.project, stream: m.stream, directory: m.directory, db: write, reads: read, session: rand.Text(), wake: coreadapter.NewWakeups()}
	if err := initialize(ctx, write); err != nil {
		r.closeDatabases()
		return nil, err
	}
	if err := m.write(ctx, r, p); err != nil {
		r.closeDatabases()
		return nil, err
	}
	return r, nil
}

func (m migration) write(ctx context.Context, r *Repository, p importPlan) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO commits (at, message) VALUES (?, ?)", time.Now().UTC().Format(time.RFC3339Nano), "Migrate trace from Git")
	if err != nil {
		return err
	}
	commit, err := result.LastInsertId()
	if err != nil {
		return err
	}
	for _, name := range p.order {
		for i, line := range p.lines[name] {
			id, err := identityOf(line)
			if err != nil {
				return fmt.Errorf("%s:%d: not a record: %w", name, i+1, err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (?, ?, ?, ?, ?, ?)", commit, name, id.Schema, id.ID, id.Revision, string(line)); err != nil {
				return fmt.Errorf("%s:%d: %w", name, i+1, err)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.files)) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO files (path, content) VALUES (?, ?)", name, p.files[name]); err != nil {
			return err
		}
	}
	if p.workflow != nil {
		log, err := m.legacyWorkflow(r, p)
		if err != nil {
			return err
		}
		r.workflows = map[config.WorkstreamID]*cachedWorkflow{m.stream: {log: workflowLog{}, threads: map[string]string{}}}
		if _, _, err := r.writeWorkflow(ctx, tx, m.stream, log, nil); err != nil {
			return err
		}
		r.workflows = nil
	}
	return tx.Commit()
}

// legacyWorkflow decodes and checks the workflow.json of p against its
// records.
func (m migration) legacyWorkflow(r *Repository, p importPlan) (workflowLog, error) {
	var file workflowFile
	if err := decode(p.workflow, &file); err != nil {
		return workflowLog{}, fmt.Errorf("workflow.json: %w", err)
	}
	log, _, err := r.checkWorkflow(m.stream, file, p.records)
	if err != nil {
		return workflowLog{}, fmt.Errorf("workflow.json: %w", err)
	}
	return log, nil
}

// verify checks the import against HEAD: every record line, in order, the
// files, the decoded workflow, and the database's integrity.
func (m migration) verify(ctx context.Context, r *Repository, p importPlan, path string) error {
	total := 0
	for _, name := range p.order {
		bodies, err := storedRows(ctx, r, name)
		if err != nil {
			return err
		}
		if len(bodies) != len(p.lines[name]) {
			return fmt.Errorf("%s: %d records imported, HEAD holds %d", name, len(bodies), len(p.lines[name]))
		}
		for i, body := range bodies {
			if body != string(p.lines[name][i]) {
				return fmt.Errorf("%s:%d: the imported record differs", name, i+1)
			}
		}
		total += len(bodies)
	}
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM records").Scan(&count); err != nil {
		return err
	}
	if count != total {
		return fmt.Errorf("%d records imported, HEAD holds %d", count, total)
	}
	for name, data := range p.files {
		got, err := r.file(name)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, data) {
			return fmt.Errorf("%s: the imported file differs", name)
		}
	}
	if p.workflow != nil {
		want, err := m.legacyWorkflow(r, p)
		if err != nil {
			return err
		}
		got, _, err := r.loadWorkflow(m.stream)
		if err != nil {
			return err
		}
		if !equalJSON(comparable(got), comparable(want)) {
			return fmt.Errorf("the imported workflow differs from workflow.json")
		}
	}
	return quickCheck(ctx, r.db, path)
}

// comparable returns log with each empty list absent, as the database
// stores it.
func comparable(log workflowLog) workflowLog {
	if len(log.Transactions) == 0 {
		log.Transactions = nil
	}
	if len(log.Deliveries) == 0 {
		log.Deliveries = nil
	}
	if len(log.Operations) == 0 {
		log.Operations = nil
	}
	if len(log.Threads) == 0 {
		log.Threads = nil
	}
	return log
}

// storedRows returns the bodies of the records stored at path, in seq order.
func storedRows(ctx context.Context, r *Repository, path string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT body FROM records WHERE path = ? ORDER BY seq", path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		bodies = append(bodies, body)
	}
	return bodies, rows.Err()
}

// switchTo makes the imported database at path the trace's database: it is
// checkpointed, closed, flushed and renamed into place.
func (m migration) switchTo(ctx context.Context, r *Repository, path string) error {
	_, err := r.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err = errors.Join(err, r.closeDatabases()); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(path, filepath.Join(m.directory, databaseFile)); err != nil {
		return err
	}
	return syncDirectory(m.directory)
}

// cleanup removes every file of a switched trace directory but its
// database, lock and, in a project's trace, its configuration, removing
// .git last.
func (m migration) cleanup() error {
	m.log("removing the Git trace files")
	keep := map[string]bool{lockFile: true, ".git": true}
	for _, name := range storeFiles {
		keep[name] = true
	}
	if m.stream == "" {
		keep["config.toml"] = true
	}
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return m.failed("clean up", err)
	}
	for _, entry := range entries {
		if keep[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.directory, entry.Name())); err != nil {
			return m.failed("clean up", err)
		}
		if err := m.boundary("migration-cleaned:" + entry.Name()); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(m.directory, ".git")); err != nil {
		return m.failed("clean up", err)
	}
	return syncDirectory(m.directory)
}
