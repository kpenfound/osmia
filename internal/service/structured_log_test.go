package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// service's background goroutines, such as the tailnet supervisor, make
// through its logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// decodeStructuredLines fails the test if any line of text is not a JSON
// object with a timestamp, a level and a message.
func decodeStructuredLines(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Time, Level, Msg string
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a JSON object: %s: %v", line, err)
		}
		if record.Time == "" || record.Level == "" || record.Msg == "" {
			t.Fatalf("line is missing a timestamp, level or message: %s", line)
		}
	}
}

// TestServiceWritesOnlyStructuredRecordsToItsLogger shows that a service
// started with a JSON-handler logger writes only JSON-line records through
// it, with a timestamp, a level and a message, whether the service is
// quiet or an embedded component such as the tailnet supervisor logs
// through it.
func TestServiceWritesOnlyStructuredRecordsToItsLogger(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	var buf syncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	opts.TailnetRetry = time.Hour
	opts.JoinTailnet = func(hostname, dir string) (TailnetListener, error) { return nil, errors.New("no tailnet in this test") }
	s, err := Start(context.Background(), opts)
	must(t, err)
	// The tailnet join failure logs through the service's own logger,
	// synchronously before Start returns, giving at least one record to
	// check besides the vacuous case of an otherwise quiet service.
	text := buf.String()
	decodeStructuredLines(t, text)
	if !strings.Contains(text, `"component":"tailnet"`) {
		t.Fatalf("tailnet join failure did not log through the service's logger: %s", text)
	}
	must(t, s.Close())
	decodeStructuredLines(t, buf.String())
}
