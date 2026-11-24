package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

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
