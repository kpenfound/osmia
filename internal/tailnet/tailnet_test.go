package tailnet

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

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

func TestCertificateLogfForwardsOnlyCertificateLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logf := certificateLogf(slog.New(slog.NewJSONHandler(&buf, nil)))
	logf(`cert("osmia.tail0.ts.net"): did SetDNS for %s`, "_acme-challenge.osmia.tail0.ts.net")
	logf("magicsock: disco: node %s now using %s", "[abc]", "192.0.2.1:41641")
	logf(`cert("osmia.tail0.ts.net"): got cert`)

	var messages []string
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var r tailscaleLogRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		if r.Component != "tailnet" {
			t.Fatalf("record %+v", r)
		}
		messages = append(messages, r.Message)
	}
	want := []string{`cert("osmia.tail0.ts.net"): did SetDNS for _acme-challenge.osmia.tail0.ts.net`, `cert("osmia.tail0.ts.net"): got cert`}
	if strings.Join(messages, "\n") != strings.Join(want, "\n") {
		t.Fatalf("messages %q, want %q", messages, want)
	}
}

type tailscaleLogRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Component string `json:"component"`
	Message   string `json:"message"`
	LoginURL  string `json:"login_url"`
}
