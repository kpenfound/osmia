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

func TestLoggerWithoutOneInContextDiscardsRatherThanWritingToAStreamNobodyConfigured(t *testing.T) {
	// No WithLogger call: Logger must return a silent logger, not nil and
	// not slog.Default(), so Log never writes to an unconfigured stream.
	Log(context.Background(), time.Now(), Entry{Action: "a"})
}

func TestScopeOfIsEmptyWithoutWithScope(t *testing.T) {
	if scope := ScopeOf(context.Background()); scope != (Scope{}) {
		t.Fatalf("scope %+v", scope)
	}
}
