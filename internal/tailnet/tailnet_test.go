package tailnet

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

// The embedded node's backend states map to the listener's states.
func TestTailnetStateMapsBackendStates(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		backend, url string
		want         api.TailnetStatus
	}{
		{"Running", "", api.TailnetStatus{State: api.TailnetUp}},
		{"NeedsLogin", "https://login.example/a/1", api.TailnetStatus{State: api.TailnetNeedsLogin, LoginURL: "https://login.example/a/1"}},
		{"NeedsLogin", "", api.TailnetStatus{State: api.TailnetNeedsLogin}},
		{"NeedsMachineAuth", "https://ignored", api.TailnetStatus{State: api.TailnetNeedsLogin, Reason: "the node awaits approval on the tailnet"}},
		{"Stopped", "", api.TailnetStatus{State: api.TailnetDown, Reason: "the node is stopped"}},
		{"Starting", "", api.TailnetStatus{State: api.TailnetConnecting}},
		{"", "", api.TailnetStatus{State: api.TailnetConnecting}},
	} {
		if got := tailnetState(c.backend, c.url); got != c.want {
			t.Errorf("%q %q: %+v, want %+v", c.backend, c.url, got, c.want)
		}
	}
}

// TestTailscaleLogfRecordsAStructuredLineWithTheLoginURLAsAField shows that
// a line embedded Tailscale logs becomes one structured record, with any
// URL it mentions, such as the login URL shown while the node waits for
// approval, as its own field, and known secrets redacted.
//
// serial: calls t.Setenv, which fails if run in parallel with another test.
func TestTailscaleLogfRecordsAStructuredLineWithTheLoginURLAsAField(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-secret")
	var buf bytes.Buffer
	logf := tailscaleLogf(slog.New(slog.NewJSONHandler(&buf, nil)))
	logf("To start this tsnet server, restart with ts.AuthKey set, or visit:\n\t%s", "https://login.tailscale.com/a/0123456789abcdef")
	logf("")
	logf("tskey-secret leaked")

	var records []tailscaleLogRecord
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var r tailscaleLogRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		records = append(records, r)
	}
	// The second logf call logged an empty line, which produces no record.
	if len(records) != 2 {
		t.Fatalf("records %+v", records)
	}
	if records[0].Component != "tailnet" || records[0].LoginURL != "https://login.tailscale.com/a/0123456789abcdef" {
		t.Fatalf("login URL did not travel as its own field: %+v", records[0])
	}
	if strings.Contains(records[1].Message, "tskey-secret") || strings.Contains(buf.String(), "tskey-secret") {
		t.Fatalf("a tailnet message leaked TS_AUTHKEY: %s", buf.String())
	}
}

type tailscaleLogRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Component string `json:"component"`
	Message   string `json:"message"`
	LoginURL  string `json:"login_url"`
}
