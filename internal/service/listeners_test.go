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

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func firstStreamEvent(t *testing.T, client *http.Client, url, host string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	fixturekit.Must(t, err)
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	fixturekit.Must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("event stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var event strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		fixturekit.Must(t, err)
		if line == "\n" {
			return event.String()
		}
		event.WriteString(line)
	}
}

type listenerEndpoint struct {
	name, base, host string
	client           *http.Client
}

type listenersRig struct {
	t         *testing.T
	s         *Service
	c         *Client
	node      *fixturekit.TailnetNode
	listeners []listenerEndpoint
}

func newListenersRig(t *testing.T) *listenersRig {
	t.Helper()
	opts := eventTraceOptions(t)
	fixturekit.WithListen(t, opts.Config.Root, "web = \"127.0.0.1:0\"\ntailnet = \"osmia\"\n")
	joined := withTailnet(t, &opts)
	s, c := start(t, opts)
	return &listenersRig{t: t, s: s, c: c, node: *joined, listeners: []listenerEndpoint{
		{"socket", "http://osmia", "", socketHTTP(s.Socket())},
		{"web", "http://" + s.WebAddr(), "", intakefixture.WebHTTP()},
		{"tailnet", "http://" + s.TailnetAddr(), "osmia", intakefixture.WebHTTP()},
	}}
}

var elapsedField = regexp.MustCompile(`"elapsed":\d+`)

func (r *listenersRig) same(path string) (int, []byte) {
	r.t.Helper()
	fetch := func(l listenerEndpoint) (int, []byte) {
		code, body := intakefixture.Exchange(r.t, l.client, "GET", l.base+path, "", "", l.host)
		return code, elapsedField.ReplaceAll(body, []byte(`"elapsed":0`))
	}
	code, body := fetch(r.listeners[0])
	for _, l := range r.listeners[1:] {
		c, b := fetch(l)
		if c != code || !bytes.Equal(b, body) {
			r.t.Fatalf("%s: %s answered %d %s, socket %d %s", path, l.name, c, b, code, body)
		}
	}
	return code, body
}

func TestEveryListenerServesTheSamePageAndAssets(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		if code, body := r.same(path); code != 200 || len(body) == 0 {
			t.Fatalf("%s: %d %q", path, code, body)
		}
	}
	if _, page := r.same("/"); !bytes.Contains(page, []byte("<html")) {
		t.Fatalf("page: %s", page)
	}
}

func TestEveryListenerAnswersTheAPIIdentically(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	for _, path := range []string{"/health", "/status", "/runtime", "/config", "/inbox", "/nowhere"} {
		r.same(Prefix + path)
	}
}

func TestEveryListenerStreamsEventsFromTheSameResync(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	events := make([]string, 0, len(r.listeners))
	for _, l := range r.listeners {
		event := firstStreamEvent(t, l.client, l.base+Prefix+"/events", l.host)
		if !strings.Contains(event, "event: resync") {
			t.Fatalf("%s stream starts with %q", l.name, event)
		}
		events = append(events, event)
	}
	if events[0] != events[1] || events[0] != events[2] {
		t.Fatalf("streams start differently: %q", events)
	}
}

func TestARulingOverTheTailnetIsSeenByEveryListener(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	ctx := context.Background()
	inbox, err := r.c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 1 || inbox.Entries[0].Kind != InboxEscalation {
		t.Fatalf("inbox: %+v", inbox)
	}
	tail := r.listeners[2]
	code, body := intakefixture.Exchange(t, tail.client, "POST", tail.base+Prefix+"/inbox/1", "application/json", `{"text":"In files."}`, tail.host)
	if code != 200 {
		t.Fatalf("ruling over the tailnet: %d %s", code, body)
	}
	r.same(Prefix + "/inbox")
	if inbox, err := r.c.Inbox(ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the ruling: %+v %v", inbox, err)
	}
}

func TestADroppedTailnetLeavesTheOtherListenersServing(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	ctx := context.Background()
	r.node.Drop()
	intakefixture.Soon(t, "the tailnet to be reported down", func() bool {
		h, err := r.c.Health(ctx)
		return err == nil && h.Tailnet != nil && h.Tailnet.State == TailnetDown
	})
	for _, l := range r.listeners[:2] {
		if code, body := intakefixture.Exchange(t, l.client, "GET", l.base+Prefix+"/health", "", "", l.host); code != 200 {
			t.Fatalf("%s during the outage: %d %s", l.name, code, body)
		}
	}
	if _, err := r.c.Inbox(ctx); err != nil {
		t.Fatalf("socket client during the outage: %v", err)
	}
}

func TestADroppedTailnetIsReportedDownWithItsReasonByEveryRead(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	ctx := context.Background()
	r.node.Drop()
	intakefixture.Soon(t, "the tailnet to be reported down", func() bool {
		h, err := r.c.Health(ctx)
		return err == nil && h.Tailnet != nil && h.Tailnet.State == TailnetDown
	})
	h, err := r.c.Health(ctx)
	fixturekit.Must(t, err)
	st, err := r.c.Statuses(ctx)
	fixturekit.Must(t, err)
	cfg, err := r.c.Configuration(ctx)
	fixturekit.Must(t, err)
	for _, got := range []*TailnetStatus{h.Tailnet, st.Tailnet, cfg.Tailnet} {
		if got == nil || got.State != TailnetDown || got.Reason == "" {
			t.Fatalf("outage diagnostics: %+v", got)
		}
	}
}

func TestADroppedTailnetListenerStopsAnswering(t *testing.T) {
	t.Parallel()
	r := newListenersRig(t)
	ctx := context.Background()
	r.node.Drop()
	intakefixture.Soon(t, "the tailnet to be reported down", func() bool {
		h, err := r.c.Health(ctx)
		return err == nil && h.Tailnet != nil && h.Tailnet.State == TailnetDown
	})
	dial := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := dial.Get(r.listeners[2].base + Prefix + "/health"); err == nil {
		t.Fatal("the dropped tailnet listener still answers")
	}
}
