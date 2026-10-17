package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// HasTrace reports whether directory holds a trace: a trace database, or a
// Git trace that Migrate moves to one.
func HasTrace(directory string) (bool, error) {
	if ok, err := HasTraceDatabase(directory); err != nil || ok {
		return ok, err
	}
	return hasLegacyTrace(directory)
}

// Published reports whether directory holds a project trace database whose
// manifest is committed. It reads the database without taking the trace's
// lock.
func Published(directory string) (bool, error) {
	ok, err := HasTraceDatabase(directory)
	if err != nil || !ok {
		return false, err
	}
	db, err := openReadOnly(directory)
	if err != nil {
		return false, err
	}
	defer db.Close()
	// A creation interrupted before the schema was written leaves no tables.
	var one int
	err = db.QueryRowContext(context.Background(), "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'files'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", TraceDatabase(directory), err)
	}
	err = db.QueryRowContext(context.Background(), "SELECT 1 FROM files WHERE path = 'project.json'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", TraceDatabase(directory), err)
	}
	return true, nil
}

// ProjectDocument returns the content of the latest revision of the project
// document at path in the trace database in directory, without taking the
// trace's lock. A document with no revision is fs.ErrNotExist.
func ProjectDocument(directory, path string) (string, error) {
	if ok, err := HasTraceDatabase(directory); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%s: %w", TraceDatabase(directory), fs.ErrNotExist)
		}
		return "", err
	}
	db, err := openReadOnly(directory)
	if err != nil {
		return "", err
	}
	defer db.Close()
	d, err := (&Repository{}).latestDocument(context.Background(), db, "", path)
	if err != nil {
		return "", err
	}
	return d.Content, nil
}

// openReadOnly opens the trace database in directory for reading alone.
func openReadOnly(directory string) (*sql.DB, error) {
	path := filepath.Join(directory, databaseFile)
	if err := checkStoreFiles(directory); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", databaseDSN(path, "mode=ro", "_pragma=busy_timeout(5000)", "_pragma=query_only(1)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
