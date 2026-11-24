package trace

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

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

func commits(t *testing.T, r *Repository) int {
	t.Helper()
	n, err := r.CommitCount(string(r.stream))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func fileOf(t *testing.T, r *Repository, name string) string {
	t.Helper()
	data, err := r.file(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func kbFiles(t *testing.T, r *Repository) map[string]string {
	t.Helper()
	documents, err := r.ProjectDocuments()
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]Document{}
	for _, d := range documents {
		if (prose(d.Path) || d.Path == EntitiesPath) && d.Revision > latest[d.Path].Revision {
			latest[d.Path] = d
		}
	}
	out := map[string]string{}
	for path, d := range latest {
		if d.Content != "" {
			out[strings.TrimPrefix(path, "kb/")] = d.Content
		}
	}
	return out
}

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
