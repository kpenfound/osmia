package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// writeFrom writes a file from a session that runs on its own goroutine.
func writeFrom(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Errorf("write %s: %v", path, err)
	}
}

// newCollector is a telemetry collector that counts every request it gets.
func newCollector(t *testing.T, count *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { count.Add(1) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// capture redirects a standard stream into a buffer until the returned function
// is called, which restores the stream and returns what was written.
func capture(t *testing.T, stream **os.File) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := *stream
	*stream = writer
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, reader)
		close(done)
	}()
	return func() string {
		*stream = original
		_ = writer.Close()
		<-done
		_ = reader.Close()
		return buf.String()
	}
}
