package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
)

// beekeeperWebFixture starts a service with a web listener and no owner
// project, whose Beekeeper turns run on run instead of a model session. A
// nil run leaves Options.BeekeeperTurns unset, for tests that only read the
// chat the shadow repository already holds.
func beekeeperWebFixture(t *testing.T, run func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)) (*Service, *Client) {
	t.Helper()
	opts, _ := projectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	if run != nil {
		opts.BeekeeperTurns = turnsFunc(run)
	}
	return start(t, opts)
}

// The Beekeeper sidebar item renders above the workstreams list and is not
// tied to any selected workstream; selecting it opens a chat panel showing
// the recent messages with their authors, including a relayed chief-of-staff
// reply, and a composer.
func TestBrowserBeekeeperSidebarItemOpensChatWithMessagesAuthorsAndComposer(t *testing.T) {
	p := openBrowser(t)
	s, _ := beekeeperWebFixture(t, nil)
	ctx := context.Background()
	repo := s.Beekeeper()
	at := demoStart
	seedBeekeeperTurn(t, repo, "1", at, "Status please.", "All quiet.")
	must(t, beekeeper.RecordRelayedReply(ctx, repo, project, stream, "relay-1", "Shipped the draft.", at.Add(time.Second)))

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	p.await("the beekeeper item above the workstream list", `(() => {
		const b = document.getElementById('beekeeper-button');
		const list = document.getElementById('workstream-list');
		return b !== null && list !== null && !!(b.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING);
	})()`)

	p.click("#beekeeper-button")
	p.await("the beekeeper view", `!document.querySelector('.view[data-view="beekeeper"]').hidden`)
	p.awaitText("#beekeeper-feed", "Status please.")
	p.awaitText("#beekeeper-feed", "You")
	p.awaitText("#beekeeper-feed", "All quiet.")
	p.awaitText("#beekeeper-feed", "Beekeeper")
	p.awaitText("#beekeeper-feed", "Shipped the draft.")
	p.awaitText("#beekeeper-feed", "Chief of staff:")

	var composer bool
	p.eval(`document.querySelector('#beekeeper-form textarea') !== null && document.querySelector('#beekeeper-form [type=submit]') !== null`, &composer)
	if !composer {
		t.Fatal("the beekeeper view has no composer")
	}
}

// Sending a message to the Beekeeper posts it and shows the reply once the
// turn finishes, without a page reload, and a message sent while a previous
// turn is still in flight is refused with a busy error the composer shows.
func TestBrowserBeekeeperSendAndReplyAppearWithoutReloadAndBusyRefusal(t *testing.T) {
	p := openBrowser(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	run := func(_ context.Context, in coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		if in.Prompt == "Hold please" {
			close(entered)
			defer close(finished)
			<-release
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Reply to: " + in.Prompt}, nil
	}
	s, _ := beekeeperWebFixture(t, run)

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click("#beekeeper-button")
	p.await("the beekeeper view", `!document.querySelector('.view[data-view="beekeeper"]').hidden`)

	// A sent message and its reply appear without a page reload.
	p.eval(`window.notReloaded = true`, nil)
	p.typeInto("#beekeeper-form textarea", "Hello there")
	p.click("#beekeeper-form [type=submit]")
	p.awaitText("#beekeeper-feed", "Hello there")
	p.awaitText("#beekeeper-feed", "Reply to: Hello there")
	p.await("the same document", `window.notReloaded === true`)

	// A second owner message sent while the Beekeeper's turn is still in
	// flight is refused as busy; the first is fired as a raw request (not
	// through the composer's own button, which the UI disables for the
	// duration of its own request) so the refusal is genuine, not merely
	// the composer disabling itself.
	p.eval(`(() => { fetch('/v1/beekeeper', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({text: 'Hold please'})}).catch(() => {}); })()`, nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking beekeeper turn never started")
	}
	p.typeInto("#beekeeper-form textarea", "Are you there?")
	p.click("#beekeeper-form [type=submit]")
	p.awaitText("#beekeeper-result", "busy")

	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking beekeeper turn never finished")
	}
}

// The page requests only the Beekeeper chat's recent window, never a larger
// or paginated one, and offers no way to load older history, even though
// more than the default window of history exists.
func TestBrowserBeekeeperRequestsOnlyTheRecentWindow(t *testing.T) {
	p := openBrowser(t)
	s, _ := beekeeperWebFixture(t, nil)
	ctx := context.Background()
	repo := s.Beekeeper()
	at := demoStart
	const total = defaultBeekeeperMessages + 10
	for i := 0; i < total; i++ {
		must(t, beekeeper.RecordRelayedReply(ctx, repo, project, stream, fmt.Sprintf("relay-%d", i), fmt.Sprintf("Update %d", i), at.Add(time.Duration(i)*time.Second)))
	}

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click("#beekeeper-button")
	p.await("the beekeeper view", `!document.querySelector('.view[data-view="beekeeper"]').hidden`)
	p.awaitText("#beekeeper-feed", fmt.Sprintf("Update %d", total-1))

	var count int
	p.eval(`document.querySelectorAll('#beekeeper-feed li').length`, &count)
	if count != defaultBeekeeperMessages {
		t.Fatalf("beekeeper feed entry count: got %d, want %d", count, defaultBeekeeperMessages)
	}

	var oldestShown bool
	p.eval(`document.getElementById('beekeeper-feed').textContent.includes(`+quote("Update 0 ")+`) || document.getElementById('beekeeper-feed').textContent.endsWith(`+quote("Update 0")+`)`, &oldestShown)
	if oldestShown {
		t.Fatal("the beekeeper feed shows history older than the recent window")
	}

	var olderControl bool
	p.eval(`document.querySelector('[data-field="load-older"], [data-action="load-older"], #beekeeper-load-older') !== null`, &olderControl)
	if olderControl {
		t.Fatal("the beekeeper view offers a way to load older history")
	}

	for _, path := range p.paths() {
		if strings.HasPrefix(path, Prefix+"/beekeeper") && path != Prefix+"/beekeeper" {
			t.Fatalf("the page requested the beekeeper chat with extra parameters: %s", path)
		}
	}
}

// The shadow project never appears in the project selector, project list or
// workstream list the page shows, even with an owner project registered.
func TestBrowserBeekeeperNeverShowsTheShadowProject(t *testing.T) {
	p := openBrowser(t)
	opts := fixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	s, _ := start(t, opts)

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	for _, selector := range []string{"#workstream-list [data-select]", "#archived-list [data-select]"} {
		var joined string
		p.eval(`[...document.querySelectorAll(`+quote(selector)+`)].map((e) => e.dataset.select).join(',')`, &joined)
		if strings.Contains(joined, string(config.ShadowProjectID)) || strings.Contains(joined, string(config.BeekeeperWorkstreamID)) {
			t.Fatalf("%s lists the shadow project: %s", selector, joined)
		}
	}

	p.click("#new-workstream")
	p.await("the handin view", `!document.querySelector('.view[data-view="handin"]').hidden`)
	var options string
	p.eval(`[...document.querySelectorAll('#handin-form [data-project-select] option')].map((o) => o.value).join(',')`, &options)
	if strings.Contains(options, string(config.ShadowProjectID)) {
		t.Fatalf("the project selector lists the shadow project: %s", options)
	}
}
