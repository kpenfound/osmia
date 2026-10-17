package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// setCharter records text as the project's next charter revision through
// osmia edit.
func setCharter(t *testing.T, root string, project config.ProjectID, text string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "charter.md")
	must(t, os.WriteFile(file, []byte(text), 0600))
	successful(t, root, "edit", "charter", string(project), "--from", file)
}

// projectDatabase is the project's trace database under root.
func projectDatabase(t *testing.T, root string, project config.ProjectID) string {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	must(t, err)
	dir, err := resolved.ProjectTrace(project)
	must(t, err)
	return trace.TraceDatabase(dir)
}

// workstreamDatabase is the workstream's trace database under root.
func workstreamDatabase(t *testing.T, root string, project config.ProjectID, stream string) string {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	must(t, err)
	dir, err := resolved.WorkstreamTrace(project, config.WorkstreamID(stream))
	must(t, err)
	return trace.TraceDatabase(dir)
}

// traceBodies reads, without taking the trace's lock, the bodies of the
// records stored at the logical path name of the trace database db, in
// order.
func traceBodies(t *testing.T, db, name string) []string {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db+"?mode=ro&_pragma=busy_timeout(5000)")
	must(t, err)
	defer conn.Close()
	rows, err := conn.Query("SELECT body FROM records WHERE path = ? ORDER BY seq", name)
	must(t, err)
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		must(t, rows.Scan(&body))
		bodies = append(bodies, body)
	}
	must(t, rows.Err())
	return bodies
}

// traceDocuments reads the document revisions of the trace database db at
// the logical documents log name, by record ID, in order.
func traceDocuments(t *testing.T, db, name string) map[string][]trace.Document {
	t.Helper()
	docs := map[string][]trace.Document{}
	for _, body := range traceBodies(t, db, name) {
		var d trace.Document
		must(t, json.Unmarshal([]byte(body), &d))
		docs[d.ID] = append(docs[d.ID], d)
	}
	return docs
}

// latestDocument returns the latest revision of the document at path among
// the documents log name of db.
func latestDocument(t *testing.T, db, name, path string) trace.Document {
	t.Helper()
	var latest trace.Document
	found := false
	for _, revisions := range traceDocuments(t, db, name) {
		for _, d := range revisions {
			if d.Path == path {
				latest, found = d, true
			}
		}
	}
	if !found {
		t.Fatalf("%s holds no document at %s", db, path)
	}
	return latest
}

// commitCount reads how many commits the trace database db holds.
func commitCount(t *testing.T, db string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db+"?mode=ro&_pragma=busy_timeout(5000)")
	must(t, err)
	defer conn.Close()
	var n int
	must(t, conn.QueryRow("SELECT count(*) FROM commits").Scan(&n))
	return n
}
