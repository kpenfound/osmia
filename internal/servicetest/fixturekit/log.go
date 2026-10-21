package fixturekit

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Reset discards everything buffered so far, so a test can assert on only
// what is logged after a given point.
func (b *SyncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// DecodeStructuredLines fails the test if any line of text is not a JSON
// object with a timestamp, a level and a message.
func DecodeStructuredLines(t *testing.T, text string) {
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
