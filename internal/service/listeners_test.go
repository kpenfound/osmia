package service

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// firstStreamEvent opens GET /v1/events through client and returns the
// stream's first event, its lines up to the blank line.
func firstStreamEvent(t *testing.T, client *http.Client, url, host string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	must(t, err)
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("event stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var event strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		must(t, err)
		if line == "\n" {
			return event.String()
		}
		event.WriteString(line)
	}
}

// The web page and the API answer alike on the Unix socket, the web listener
// and the tailnet listener of one service, an owner decision made over the
// tailnet shows on all three, and a tailnet outage leaves the socket and the
// web listener serving while diagnostics report it.
func TestEveryListenerServesThePageAndTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := eventTraceOptions(t)
	withListen(t, opts, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, c := start(t, opts)
	node := *joined

	type listener struct {
		name, base, host string
		client           *http.Client
	}
	listeners := []listener{
		{"socket", "http://osmia", "", socketHTTP(s.Socket())},
		{"web", "http://" + s.WebAddr(), "", webHTTP()},
		{"tailnet", "http://" + s.TailnetAddr(), "osmia", webHTTP()},
	}
	// same fetches path on every listener and requires identical answers,
	// except for the elapsed time of a running agent, which the clock moves.
	elapsed := regexp.MustCompile(`"elapsed":\d+`)
	same := func(path string) (int, []byte) {
		t.Helper()
		fetch := func(l listener) (int, []byte) {
			code, body := exchange(t, l.client, "GET", l.base+path, "", "", l.host)
			return code, elapsed.ReplaceAll(body, []byte(`"elapsed":0`))
		}
		code, body := fetch(listeners[0])
		for _, l := range listeners[1:] {
			c, b := fetch(l)
			if c != code || !bytes.Equal(b, body) {
				t.Fatalf("%s: %s answered %d %s, socket %d %s", path, l.name, c, b, code, body)
			}
		}
		return code, body
	}

	for _, path := range []string{"/", "/app.js", "/style.css"} {
		if code, body := same(path); code != 200 || len(body) == 0 {
			t.Fatalf("%s: %d %q", path, code, body)
		}
	}
	if _, page := same("/"); !bytes.Contains(page, []byte("<html")) {
		t.Fatalf("page: %s", page)
	}
	for _, path := range []string{"/health", "/status", "/runtime", "/config", "/inbox", "/nowhere"} {
		same(Prefix + path)
	}

	inbox, err := c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 1 || inbox.Entries[0].Kind != InboxEscalation {
		t.Fatalf("inbox: %+v", inbox)
	}
	events := make([]string, 0, len(listeners))
	for _, l := range listeners {
		event := firstStreamEvent(t, l.client, l.base+Prefix+"/events", l.host)
		if !strings.Contains(event, "event: resync") {
			t.Fatalf("%s stream starts with %q", l.name, event)
		}
		events = append(events, event)
	}
	if events[0] != events[1] || events[0] != events[2] {
		t.Fatalf("streams start differently: %q", events)
	}

	// The owner's ruling over the tailnet closes the decision everywhere.
	tail := listeners[2]
	code, body := exchange(t, tail.client, "POST", tail.base+Prefix+"/inbox/1", "application/json", `{"text":"In files."}`, tail.host)
	if code != 200 {
		t.Fatalf("ruling over the tailnet: %d %s", code, body)
	}
	same(Prefix + "/inbox")
	if inbox, err := c.Inbox(ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the ruling: %+v %v", inbox, err)
	}

	// The tailnet goes away: the socket and web listener keep answering alike.
	node.drop()
	eventually(t, "tailnet never reported down", func() bool {
		h, err := c.Health(ctx)
		return err == nil && h.Tailnet != nil && h.Tailnet.State == TailnetDown
	})
	for _, l := range listeners[:2] {
		if code, body := exchange(t, l.client, "GET", l.base+Prefix+"/health", "", "", l.host); code != 200 {
			t.Fatalf("%s during the outage: %d %s", l.name, code, body)
		}
	}
	if _, err := c.Inbox(ctx); err != nil {
		t.Fatalf("socket client during the outage: %v", err)
	}
	h, err := c.Health(ctx)
	must(t, err)
	st, err := c.Statuses(ctx)
	must(t, err)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	for _, got := range []*TailnetStatus{h.Tailnet, st.Tailnet, cfg.Tailnet} {
		if got == nil || got.State != TailnetDown || got.Reason == "" {
			t.Fatalf("outage diagnostics: %+v", got)
		}
	}
	dial := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := dial.Get(tail.base + Prefix + "/health"); err == nil {
		t.Fatal("the dropped tailnet listener still answers")
	}
}
