package trace

import (
	"context"
	"database/sql"
	"testing"
)

// storedBodies returns the bodies of the records stored at the logical path
// name, in seq order.
func storedBodies(t *testing.T, r *Repository, name string) []string {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(), "SELECT body FROM records WHERE path = ? ORDER BY seq", name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bodies
}

// storeExec runs a statement against r's database directly, bypassing every
// check, to stage damage a test then reads back.
func storeExec(t *testing.T, r *Repository, query string, args ...any) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
	r.cache.mu.Lock()
	r.resetCache()
	r.cache.mu.Unlock()
	r.workflows = nil
}

// commits returns how many commits r's own trace holds.
func commits(t *testing.T, r *Repository) int {
	t.Helper()
	n, err := r.CommitCount(string(r.stream))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fileOf reads the trace file name of r: a stored file, or the latest
// revision of the document at that logical path.
func fileOf(t *testing.T, r *Repository, name string) string {
	t.Helper()
	data, err := r.file(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// kbFiles returns the project's knowledge base by file name: each
// subsystem's latest prose, and the latest entity map when one is recorded.
func kbFiles(t *testing.T, r *Repository) map[string]string {
	t.Helper()
	names, err := r.Subsystems()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range names {
		prose, err := r.Prose(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name+".md"] = prose
	}
	if data, err := r.file(EntitiesPath); err == nil {
		out["entities.json"] = string(data)
	}
	return out
}

// closedStoreExec runs a statement against the database of the closed trace
// in directory.
func closedStoreExec(t *testing.T, directory, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", databaseDSN(TraceDatabase(directory)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
