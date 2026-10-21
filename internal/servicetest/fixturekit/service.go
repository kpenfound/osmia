package fixturekit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const Project config.ProjectID = "p_0123456789abcdef0123456789abcdef"

const Stream config.WorkstreamID = "w_0123456789abcdef0123456789abcdef"

func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TraceQuery runs query against the trace database in directory through a
// read-only connection of its own, which needs no lock and so reads beside
// any handle the service holds.
func TraceQuery(t *testing.T, directory, query string, args ...any) [][]byte {
	t.Helper()
	out, err := QueryTrace(directory, query, args...)
	Must(t, err)
	return out
}

// QueryTrace is TraceQuery returning its error.
func QueryTrace(directory, query string, args ...any) ([][]byte, error) {
	db, err := sql.Open("sqlite", "file:"+trace.TraceDatabase(directory)+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// TraceExec runs statement against the trace database in directory, which no
// handle may hold open, to stage damage a test then reads back.
func TraceExec(t *testing.T, directory, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+trace.TraceDatabase(directory)+"?_pragma=busy_timeout(5000)")
	Must(t, err)
	defer db.Close()
	_, err = db.Exec(statement, args...)
	Must(t, err)
}

// WorkstreamTraceDirectory is Stream's own trace directory.
func WorkstreamTraceDirectory(t *testing.T, root config.Root, Project config.ProjectID, Stream config.WorkstreamID) string {
	t.Helper()
	dir, err := root.WorkstreamTrace(Project, Stream)
	Must(t, err)
	return dir
}

// WorkstreamRecords returns the bodies of the records Stream's trace holds at
// its logical path name, in the order they were recorded.
func WorkstreamRecords(t *testing.T, root config.Root, Project config.ProjectID, Stream config.WorkstreamID, name string) [][]byte {
	t.Helper()
	return TraceQuery(t, WorkstreamTraceDirectory(t, root, Project, Stream), "SELECT body FROM records WHERE path = ? ORDER BY seq", "workstreams/"+string(Stream)+"/"+name)
}

// LatestDocumentIn returns the content of the latest revision of the
// document at path in the trace in directory, whose documents are recorded
// at log. A document with no revision is fs.ErrNotExist.
func LatestDocumentIn(directory, log, path string) ([]byte, error) {
	bodies, err := QueryTrace(directory, "SELECT body FROM records WHERE path = ? ORDER BY seq", log)
	if err != nil {
		return nil, err
	}
	var latest *trace.Document
	for _, body := range bodies {
		var d trace.Document
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		if d.Path == path {
			latest = &d
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return []byte(latest.Content), nil
}

// WorkstreamDocument returns the content of the latest revision of the
// document at path in Stream's trace.
func WorkstreamDocument(t *testing.T, root config.Root, Project config.ProjectID, Stream config.WorkstreamID, path string) ([]byte, error) {
	t.Helper()
	return LatestDocumentIn(WorkstreamTraceDirectory(t, root, Project, Stream), "workstreams/"+string(Stream)+"/documents.jsonl", path)
}

// ProjectDocument returns the content of the latest revision of the Project
// document at path.
func ProjectDocument(t *testing.T, root config.Root, Project config.ProjectID, path string) ([]byte, error) {
	t.Helper()
	dir, err := root.ProjectTrace(Project)
	Must(t, err)
	return LatestDocumentIn(dir, "documents.jsonl", path)
}

// TraceCommits returns how many commits the trace in directory holds.
func TraceCommits(t *testing.T, directory string) int {
	t.Helper()
	rows := TraceQuery(t, directory, "SELECT CAST(count(*) AS TEXT) FROM commits")
	n, err := strconv.Atoi(string(rows[0]))
	Must(t, err)
	return n
}

// TraceFile returns the trace file name of the trace in directory, such as
// a role's notes, and whether it exists.
func TraceFile(t *testing.T, directory, name string) ([]byte, bool) {
	t.Helper()
	rows := TraceQuery(t, directory, "SELECT content FROM files WHERE path = ?", name)
	if len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
}

// RecordCharter records content as the owner's next charter revision through
// repo, a trace the test holds itself.
func RecordCharter(t *testing.T, repo *trace.Repository, content string) {
	t.Helper()
	ctx := context.Background()
	current, err := repo.Charter(ctx)
	Must(t, err)
	_, err = repo.EditCharter(ctx, current.Revision, content, current.At.Add(time.Second))
	Must(t, err)
}
