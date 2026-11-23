package trace

import (
	"context"
	"strings"
	"testing"
)

// The latest-revision lookups by path and identity run on every workflow read
// of a workgroup member, so they must stay single index lookups however many
// documents a scope holds.
func TestLatestDocumentLookupsUseTheUniqueIdentityIndex(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	for name, query := range map[string]string{"documentSeq": latestDocumentSeqQuery, "latestDocumentByID": latestDocumentByIDQuery} {
		rows, err := s.reads.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, "workstreams/"+string(streamID)+"/documents.jsonl", "workgroup-approval")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := strings.Join(plan, "; ")
		if !strings.Contains(got, "sqlite_autoindex_records_1 (path=? AND id=?)") || strings.Contains(got, "TEMP B-TREE") {
			t.Fatalf("%s plan %q, want one lookup in the unique (path, id, revision) index", name, got)
		}
	}
}
