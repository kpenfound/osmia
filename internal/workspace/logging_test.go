package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// record is one decoded GitHub activity log line.
type record struct {
	Level    string `json:"level"`
	Msg      string `json:"msg"`
	Activity string `json:"activity"`
	Access   string `json:"access"`
	Action   string `json:"action"`
	Remote   string `json:"remote"`
	Branch   string `json:"branch"`
	Commits  []string
	Outcome  string
	Error    string
}

// decodeRecords parses every line of buf as one JSON object, failing the
// test if any line does not parse: every line a logger writes must be valid
// JSON.
func decodeRecords(t *testing.T, buf *bytes.Buffer) []record {
	t.Helper()
	var records []record
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		records = append(records, r)
	}
	return records
}

func TestGitRecordsOneEntryPerRemoteOperation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: "p_1", Workstream: "w_1"})
	provider := f.provider

	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	first := f.advance(t, "first.txt")

	remote, err := provider.Remote(ctx, "acme/widgets")
	if err != nil || remote != "upstream" {
		t.Fatalf("remote %q, %v", remote, err)
	}
	if _, err := provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.RemoteBranch(ctx, "fork", "osmia/w"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Push(ctx, "fork", first, "osmia/w", ""); err != nil {
		t.Fatal(err)
	}
	// A rejected push: the branch is no longer at the expected commit.
	if err := provider.Push(ctx, "fork", base, "osmia/w", "not-"+first); err == nil {
		t.Fatal("expected a rejected push")
	}

	records := decodeRecords(t, &buf)
	if len(records) != 5 {
		t.Fatalf("records %+v", records)
	}
	for _, r := range records {
		if r.Activity != ghlog.MarkerValue {
			t.Fatalf("record missing the github marker: %+v", r)
		}
	}
	wantActions := []string{"workspace.remote", "workspace.fetch", "workspace.remote_branch", "workspace.push", "workspace.push"}
	wantAccess := []string{ghlog.Read, ghlog.Read, ghlog.Read, ghlog.Write, ghlog.Write}
	for i, r := range records {
		if r.Action != wantActions[i] || r.Access != wantAccess[i] {
			t.Fatalf("record %d: action=%q access=%q", i, r.Action, r.Access)
		}
	}
	if records[3].Outcome != "ok" || records[3].Error != "" {
		t.Fatalf("successful push: %+v", records[3])
	}
	if records[4].Outcome != "failed" || records[4].Error == "" {
		t.Fatalf("rejected push did not record its failure: %+v", records[4])
	}
	if records[4].Branch != "osmia/w" || records[4].Remote != "fork" {
		t.Fatalf("rejected push record: %+v", records[4])
	}
	if len(records[4].Commits) != 2 {
		t.Fatalf("push does not record the commits involved: %+v", records[4])
	}
}

func TestJujutsuRecordsOneEntryPerRemoteOperation(t *testing.T) {
	t.Parallel()
	_, j := newJujutsuFixture(t)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	provider := j

	if _, err := provider.Remote(ctx, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}

	records := decodeRecords(t, &buf)
	if len(records) != 2 {
		t.Fatalf("records %+v", records)
	}
	for _, r := range records {
		if r.Activity != ghlog.MarkerValue || r.Access != ghlog.Read {
			t.Fatalf("record %+v", r)
		}
	}
}
