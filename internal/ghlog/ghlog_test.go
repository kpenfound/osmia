package ghlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogWritesOneRecordWithTheMarkerAccessAndScope(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx = WithScope(ctx, Scope{Project: "p_1", Workstream: "w_1", Unit: "u_1"})
	Log(ctx, time.Now().Add(-time.Millisecond), Entry{Action: "pulls.find", Access: Read, Repository: "acme/widgets", Method: "GET", Path: "/repos/acme/widgets/pulls", Status: 200})

	var record struct {
		Time, Level, Msg                        string
		Activity, Access, Action, Repository    string
		Method, Path, Project, Workstream, Unit string
		Status                                  int
		Duration                                float64
	}
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &record); err != nil {
		t.Fatalf("not valid JSON: %s: %v", buf.String(), err)
	}
	if record.Time == "" || record.Level != "INFO" || record.Msg == "" {
		t.Fatalf("missing a timestamp, level or message: %+v", record)
	}
	if record.Activity != MarkerValue || record.Access != Read || record.Action != "pulls.find" || record.Repository != "acme/widgets" ||
		record.Method != "GET" || record.Path != "/repos/acme/widgets/pulls" || record.Status != 200 {
		t.Fatalf("record %+v", record)
	}
	if record.Project != "p_1" || record.Workstream != "w_1" || record.Unit != "u_1" {
		t.Fatalf("record did not carry its scope: %+v", record)
	}
}

func TestLogLogsFailuresAtAHigherLevelThanSuccesses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
		want  string
	}{
		{"success", Entry{Action: "a", Status: 200}, "INFO"},
		{"transport failure", Entry{Action: "a", Err: errors.New("boom")}, "ERROR"},
		{"unexpected status", Entry{Action: "a", Status: 500}, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			Log(ctx, time.Now(), tc.entry)
			var record struct{ Level string }
			if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &record); err != nil {
				t.Fatalf("not valid JSON: %s: %v", buf.String(), err)
			}
			if record.Level != tc.want {
				t.Fatalf("level %s, want %s", record.Level, tc.want)
			}
		})
	}
}

func TestLogRedactsKnownSecretsFromTheError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	Log(ctx, time.Now(), Entry{Action: "a", Err: errors.New("request with token ghp_secret failed at https://x-access-token:hunter2@github.com/acme/widgets.git")})
	if strings.Contains(buf.String(), "ghp_secret") || strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("record leaked a secret: %s", buf.String())
	}
}

func TestScopeOfIsEmptyWithoutWithScope(t *testing.T) {
	if scope := ScopeOf(context.Background()); scope != (Scope{}) {
		t.Fatalf("scope %+v", scope)
	}
}

func TestLogMarksTheErrorReportersRequestsAndNoOthers(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if IsReporter(ctx) {
		t.Fatal("a fresh context is not the reporter's")
	}
	Log(ctx, time.Now(), Entry{Action: "pulls.find", Access: Read, Method: "GET", Path: "/repos/a/b/pulls", Status: 200})
	Log(WithReporter(ctx), time.Now(), Entry{Action: "issues.create", Access: Write, Method: "POST", Path: "/repos/a/b/issues", Status: 500})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("log %s", buf.String())
	}
	var plain, own map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &own); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain[ReporterField]; ok {
		t.Fatalf("an ordinary record carries the reporter marker: %v", plain)
	}
	if own[ReporterField] != true || own[Marker] != MarkerValue || own["action"] != "issues.create" {
		t.Fatalf("record %v", own)
	}
}
