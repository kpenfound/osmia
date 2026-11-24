package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	databaseFile = "trace.db"
	lockFile     = "osmia.lock"
)

// ErrIntegrity reports a trace database that failed its integrity check. The
// trace refuses to open until it is repaired or restored.
var ErrIntegrity = errors.New("trace database failed its integrity check")

const schemaVersion = 1

const schema = `
CREATE TABLE commits (
  seq INTEGER PRIMARY KEY,
  at TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE TABLE records (
  seq INTEGER PRIMARY KEY,
  commit_seq INTEGER NOT NULL REFERENCES commits(seq),
  path TEXT NOT NULL,
  schema TEXT NOT NULL,
  id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  body TEXT NOT NULL,
  UNIQUE (path, id, revision)
);
CREATE INDEX records_schema ON records (schema, seq);
CREATE INDEX records_path ON records (path, seq);
CREATE INDEX records_document ON records (path, CASE WHEN json_valid(body) THEN json_extract(body, '$.path') END, seq) WHERE schema = 'osmia.trace.document';
CREATE TABLE files (
  path TEXT PRIMARY KEY,
  content BLOB NOT NULL
);
CREATE TABLE workflow_log (
  seq INTEGER PRIMARY KEY,
  list TEXT NOT NULL CHECK (list IN ('transactions', 'deliveries', 'operations')),
  body TEXT NOT NULL
);
CREATE INDEX workflow_log_list ON workflow_log (list, seq);
CREATE TABLE workflow_threads (
  agent_id TEXT PRIMARY KEY,
  body TEXT NOT NULL
);
`

var storeFiles = []string{databaseFile, databaseFile + "-wal", databaseFile + "-shm", lockFile}

func checkStoreFiles(directory string) error {
	for _, name := range storeFiles {
		info, err := os.Lstat(filepath.Join(directory, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlink aliases are forbidden", filepath.Join(directory, name))
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: expected a regular file", filepath.Join(directory, name))
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			return fmt.Errorf("%s: hardlink aliases are forbidden", filepath.Join(directory, name))
		}
	}
	return nil
}

func databaseDSN(path string, params ...string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + strings.Join(params, "&")
}

func openDatabases(path string) (*sql.DB, *sql.DB, error) {
	write, err := sql.Open("sqlite", databaseDSN(path, "_pragma=busy_timeout(5000)", "_pragma=journal_mode(WAL)", "_pragma=synchronous(FULL)", "_pragma=foreign_keys(ON)", "_txlock=immediate"))
	if err != nil {
		return nil, nil, err
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	write.SetConnMaxIdleTime(0)
	if err := write.Ping(); err != nil {
		write.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	read, err := sql.Open("sqlite", databaseDSN(path, "_pragma=busy_timeout(5000)", "_pragma=foreign_keys(ON)", "_pragma=query_only(1)"))
	if err != nil {
		write.Close()
		return nil, nil, err
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)
	return write, read, nil
}

func schemaOf(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

func initialize(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func quickCheck(ctx context.Context, db *sql.DB, path string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("%s: %w: %v", path, ErrIntegrity, err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%s: %w: %v", path, ErrIntegrity, err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w: %v", path, ErrIntegrity, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s: %w: %s", path, ErrIntegrity, strings.Join(problems, "; "))
	}
	return nil
}

func (r *Repository) openDatabase(create bool) error {
	if err := checkStoreFiles(r.directory); err != nil {
		return err
	}
	if err := r.acquire(); err != nil {
		return err
	}
	return r.openLocked(create)
}

func (r *Repository) openLocked(create bool) error {
	path := filepath.Join(r.directory, databaseFile)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) && !create {
		return fmt.Errorf("%s: trace database missing: %w", path, fs.ErrNotExist)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	write, read, err := openDatabases(path)
	if damaged(err) {
		return fmt.Errorf("%s: %w: %v", path, ErrIntegrity, err)
	}
	if err != nil {
		return err
	}
	r.db, r.reads = write, read
	ctx := context.Background()
	version, err := schemaOf(ctx, write)
	if damaged(err) {
		return fmt.Errorf("%s: %w: %v", path, ErrIntegrity, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case version > schemaVersion:
		return fmt.Errorf("%s: trace database schema version %d is newer than this Osmia supports (%d)", path, version, schemaVersion)
	case version == 0 && create:
		if err := initialize(ctx, write); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case version == 0:
		return fmt.Errorf("%s: trace database has no schema", path)
	}
	if err := checkStoreFiles(r.directory); err != nil {
		return err
	}
	return quickCheck(ctx, write, path)
}

func (r *Repository) acquire() error {
	path := filepath.Join(r.directory, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err := checkStoreFiles(r.directory); err != nil {
		f.Close()
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	r.lock = f
	return nil
}

func (r *Repository) closeDatabases() error {
	var errs []error
	if r.reads != nil {
		errs = append(errs, r.reads.Close())
		r.reads = nil
	}
	if r.db != nil {
		errs = append(errs, r.db.Close())
		r.db = nil
	}
	return errors.Join(errs...)
}

// TraceDatabase is the database file of the trace kept in directory.
func TraceDatabase(directory string) string { return filepath.Join(directory, databaseFile) }

// HasTraceDatabase reports whether directory holds a trace database.
func HasTraceDatabase(directory string) (bool, error) {
	_, err := os.Lstat(TraceDatabase(directory))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func sqliteCode(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code() & 0xff
	}
	return 0
}

func damaged(err error) bool {
	code := sqliteCode(err)
	return code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
}
