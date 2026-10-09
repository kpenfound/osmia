package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// answerChief captures final as the response of the workstream's claimed
// chief-of-staff turn and completes it, as a finished turn of the chief of
// staff would.
func (f *pageFixture) answerChief(t *testing.T, ws config.WorkstreamID, turn, final string) {
	t.Helper()
	ctx := context.Background()
	repo := f.s.sole().repository
	thread, err := repo.ChiefOfStaffThread(ws)
	must(t, err)
	i := slices.IndexFunc(thread.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if i < 0 || thread.Turns[i].Claim == nil {
		t.Fatalf("no claimed chief-of-staff turn %s", turn)
	}
	q := thread.Turns[i]
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), time.Now().UTC()
	response := trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: q.Request.ThreadID, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: q.Claim.Token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: final}}
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repo.CompleteTurn(ctx, ws, trace.ChiefOfStaff, turn, q.Claim.Token, time.Now().UTC()))
}

// The page sends the owner's message to the workstream's chief of staff
// through the conversation endpoint, refuses an empty one, and shows the
// message and the chief of staff's answer as the event stream announces
// them, without a reload.
func TestBrowserPageSendsMessagesAndFollowsTheConversation(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	card := `[data-workstream="` + string(quiet) + `"] `
	send := card + "[data-field=send] "

	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(quiet)
	p.awaitText(card+"[data-field=feed]", "Nothing has happened yet.")
	p.eval(`window.notReloaded = true`, nil)

	// An empty message is refused on the page and never sent.
	p.click(send + "button")
	p.awaitText(send+".result", "Write a message first.")
	list, err := f.c.Conversation(ctx, quiet)
	must(t, err)
	if len(list.Entries) != 0 {
		t.Fatalf("an empty message reached the service: %+v", list.Entries)
	}

	p.typeInto(send+"textarea", "Pick the uploads back up on Friday.")
	p.click(send + "button")
	p.awaitText(send+".result", "Sent")
	p.await("an emptied message field", `document.querySelector(`+quote(send+"textarea")+`).value === ''`)
	// The message is queued on the chief of staff's thread in the trace, as
	// osmia send queues it.
	list, err = f.c.Conversation(ctx, quiet)
	must(t, err)
	if len(list.Entries) != 1 || list.Entries[0].Kind != "message" || list.Entries[0].Text != "Pick the uploads back up on Friday." || list.Entries[0].State != TurnQueued {
		t.Fatalf("conversation after sending: %+v", list.Entries)
	}
	turn := list.Entries[0].Turn
	message := card + `[data-kind=message][data-turn="` + turn + `"] `
	p.awaitText(message+"[data-field=text]", "Pick the uploads back up on Friday.")
	p.awaitText(message+"[data-field=state]", "queued")

	// The chief of staff takes the message and answers it; the page follows
	// each step without a reload.
	q, err := f.s.sole().repository.ClaimTurn(ctx, quiet, trace.ChiefOfStaff, "token_message", t.TempDir(), time.Now().UTC())
	must(t, err)
	if q.Request.TurnID != turn {
		t.Fatalf("claimed %s, not the message's turn %s", q.Request.TurnID, turn)
	}
	p.awaitText(message+"[data-field=state]", "running")
	f.answerChief(t, quiet, turn, "Noted; the uploads resume on Friday.")
	p.awaitText(card+`[data-kind=response][data-turn="`+turn+`"] [data-field=text]`, "Noted; the uploads resume on Friday.")
	p.awaitText(message+"[data-field=state]", "done")
	p.await("the same document", `window.notReloaded === true`)

	// The other workstream's conversation stays its own.
	p.selectWorkstream(stream)
	var other string
	p.eval(textOf(`[data-workstream="`+string(stream)+`"] [data-field=feed]`), &other)
	if strings.Contains(other, "Friday") {
		t.Fatalf("the message shows in the other workstream's conversation: %q", other)
	}
}

// pauseOf returns the effective pause of target, if any.
func pauseOf(rt RuntimeResponse, target runtime.Target) (runtime.Pause, bool) {
	i := slices.IndexFunc(rt.Effective.Pauses, func(p runtime.Pause) bool { return p.Target == target })
	if i < 0 {
		return runtime.Pause{}, false
	}
	return rt.Effective.Pauses[i], true
}

// The page pauses the factory, the project and a workstream, soft or hard,
// with a reason, and resumes each, through /v1/runtime/pause; it sets and
// clears the priority order and a role's profile override, with the
// provider's usage beside the profile. It refuses a pause without a scope or
// a reason and a profile override without a profile, offers no resume for a
// provider limit's role pause, shows the API's refusal, and keeps a message
// and a profile being chosen through the reads events cause.
func TestBrowserPageControlsPausesPriorityAndProfiles(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	runtimeView := func() RuntimeResponse {
		t.Helper()
		rt, err := f.c.Runtime(ctx)
		must(t, err)
		return rt
	}
	form := "#pause-form "

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.awaitText(`[data-pause="workstream:`+string(quiet)+`"] [data-field=reason]`, "The owner is travelling")
	p.eval(`window.notReloaded = true`, nil)
	row := `[data-profile-role="mason"] `
	draft := `[data-workstream="` + string(quiet) + `"] [data-field=send] textarea`

	// A profile being chosen and a message being written, still focused,
	// survive the reads a runtime change causes.
	p.openView("profiles")
	p.awaitText(row+"[data-field=source]", "configured")
	var profile string
	p.eval(`document.querySelector(`+quote(row+"select")+`).value`, &profile)
	if profile != "" {
		t.Fatalf("the mason's profile select starts with %q chosen", profile)
	}
	p.choose(row+"select", "other")
	p.click(`.view[data-view="profiles"] [data-close]`)
	p.selectWorkstream(quiet)
	p.typeInto(draft, "Half a thought")
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "Lunch", Source: runtime.PauseOwner})
	p.awaitText(`[data-pause="factory:"] [data-field=reason]`, "Lunch")
	var kept struct {
		Draft, Profile string
		Focused        bool
	}
	p.eval(`({draft: document.querySelector(`+quote(draft)+`).value, profile: document.querySelector(`+quote(row+"select")+`).value, focused: document.activeElement === document.querySelector(`+quote(draft)+`)})`, &kept)
	if kept.Draft != "Half a thought" || kept.Profile != "other" || !kept.Focused {
		t.Fatalf("after a runtime change the page holds %+v", kept)
	}
	mutation(t, f.c, "DELETE", "pause", ClearPauseRequest{Scope: "factory"})
	p.await("the factory resumed", `document.querySelector('[data-pause="factory:"]') === null`)

	// A pause needs a scope and a reason; the page refuses one without and
	// sends nothing.
	p.openPauses()
	p.await("the workstream as a pause target", `[...document.querySelectorAll('#pause-form select[name=target] option')].some((o) => o.value === `+quote("workstream:"+string(stream))+`)`)
	var chosen string
	p.eval(`document.querySelector('#pause-form select[name=target]').value`, &chosen)
	if chosen != "" {
		t.Fatalf("the pause form starts with %q chosen", chosen)
	}
	p.typeInto(form+"input[name=reason]", "Overnight")
	p.click(form + "button[type=submit]")
	p.awaitText("#pause-result", "Choose what to pause.")
	if pauses := runtimeView().Effective.Pauses; len(pauses) != 1 {
		t.Fatalf("a pause without a scope reached the service: %+v", pauses)
	}
	p.eval(`document.querySelector('#pause-form input[name=reason]').value = ''`, nil)
	p.choose(form+"select[name=target]", "workstream:"+string(stream))
	p.choose(form+"select[name=mode]", "hard")
	p.click(form + "button[type=submit]")
	p.awaitText("#pause-result", "Give a reason for the pause.")
	workstream := runtime.Target{Scope: "workstream", Project: project, Workstream: stream}
	if _, ok := pauseOf(runtimeView(), workstream); ok {
		t.Fatal("a pause without a reason reached the service")
	}

	// Each scope pauses with its mode and reason, attributed to the owner.
	for _, step := range []struct {
		value, mode, reason string
		target              runtime.Target
	}{
		{"workstream:" + string(stream), "hard", "Hold the uploads", workstream},
		{"factory", "soft", "Overnight", runtime.Target{Scope: "factory"}},
		{"project:" + string(project), "hard", "Budget review", runtime.Target{Scope: "project", Project: project}},
	} {
		// The page names a pause by its scope and ID, as the form does.
		shown := `[data-pause="` + step.value + `"] `
		if step.value == "factory" {
			shown = `[data-pause="factory:"] `
		}
		p.choose(form+"select[name=target]", step.value)
		p.choose(form+"select[name=mode]", step.mode)
		p.typeInto(form+"input[name=reason]", step.reason)
		p.click(form + "button[type=submit]")
		p.awaitText("#pause-result", "paused ("+step.mode+")")
		p.awaitText(shown+"[data-field=reason]", step.reason)
		p.awaitText(shown+"[data-field=mode]", step.mode)
		p.awaitText(shown+"[data-field=source]", "the owner")
		got, ok := pauseOf(runtimeView(), step.target)
		if !ok || got.Mode != step.mode || got.Reason != step.reason || got.Source != runtime.PauseOwner || got.SetAt.IsZero() {
			t.Fatalf("pause of %+v: %+v, %v", step.target, got, ok)
		}
		// Resuming clears exactly that pause.
		p.click(shown + "[data-field=resume]")
		p.await("the resumed "+step.value, `document.querySelector(`+quote(shown)+`) === null`)
		p.awaitText("#pause-result", "resumed")
		if _, ok := pauseOf(runtimeView(), step.target); ok {
			t.Fatalf("%+v is still paused", step.target)
		}
	}
	if _, ok := pauseOf(runtimeView(), runtime.Target{Scope: "workstream", Project: project, Workstream: quiet}); !ok {
		t.Fatal("resuming the others cleared the quiet workstream's pause")
	}

	// The priority order: nothing chosen is refused; quiet then stream is set
	// and cleared.
	p.openView("priority")
	p.click("#priority-form button[type=submit]")
	p.awaitText("#priority-result", "Choose the workstreams that go first")
	p.click(`[data-priority="` + string(stream) + `"] [data-field=chosen]`)
	p.click(`[data-priority="` + string(quiet) + `"] [data-field=chosen]`)
	var first string
	p.eval(`document.querySelector('#priority-list li').dataset.priority`, &first)
	if first != string(quiet) {
		p.click(`[data-priority="` + string(quiet) + `"] [data-field=up]`)
	}
	p.await("quiet first", `document.querySelector('#priority-list li').dataset.priority === `+quote(string(quiet)))
	p.click("#priority-form button[type=submit]")
	p.awaitText("#priority-result", "Priority order set.")
	p.awaitText("#priority-current", "In force")
	if got := runtimeView().Effective.Priorities; len(got) != 1 || got[0].Project != project || !slices.Equal(got[0].Workstreams, []config.WorkstreamID{quiet, stream}) {
		t.Fatalf("priority after setting it: %+v", got)
	}
	p.click("#priority-clear")
	p.awaitText("#priority-result", "Priority order cleared.")
	p.awaitText("#priority-current", "No order is set")
	if got := runtimeView().Effective.Priorities; len(got) != 0 {
		t.Fatalf("priority after clearing it: %+v", got)
	}

	// The mason's profile is overridden and cleared, with the usage of the
	// profile's provider beside it; an override needs a profile.
	p.openView("profiles")
	p.awaitText(row+"[data-field=usage]", "claude: USD")
	p.await("clear disabled without an override", `document.querySelector(`+quote(row+"[data-field=clear]")+`).disabled`)
	p.eval(`document.querySelector(`+quote(row+"select")+`).value = ''`, nil)
	p.click(row + "[data-field=set]")
	p.awaitText("#profile-result", "Choose a profile for mason.")
	if got := runtimeView().Profiles["mason"]; got.Source != "configuration" {
		t.Fatalf("an override without a profile reached the service: %+v", got)
	}
	p.choose(row+"select", "other")
	p.click(row + "[data-field=set]")
	p.awaitText("#profile-result", "mason runs other")
	p.awaitText(row+"[data-field=profile]", "other · codex")
	p.awaitText(row+"[data-field=source]", "owner override")
	p.awaitText(row+"[data-field=usage]", "codex: USD")
	p.awaitText(`#providers [data-provider=codex] [data-field=spend]`, "USD")
	if got := runtimeView().Profiles["mason"]; got.Name != "other" || got.Source != "owner_override" {
		t.Fatalf("mason's profile after the override: %+v", got)
	}
	p.click(row + "[data-field=clear]")
	p.awaitText(row+"[data-field=source]", "configured")
	p.awaitText("#profile-result", "mason runs its configured profile")
	p.await("clear disabled once the override is gone", `document.querySelector(`+quote(row+"[data-field=clear]")+`).disabled`)
	if got := runtimeView().Profiles["mason"]; got.Source != "configuration" {
		t.Fatalf("mason's profile after clearing the override: %+v", got)
	}

	// A provider limit pauses the roles its provider serves without a
	// fallback; that pause has no resume, an owner pause keeps its own.
	must(t, f.s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "claude", Status: "blocked", SetAt: time.Now().UTC()}))
	p.awaitText(`[data-pause="role:mason"] [data-field=source]`, "a provider usage limit")
	p.await("no resume on the role pause", `document.querySelector('[data-pause="role:mason"] [data-field=resume]') === null`)
	p.await("resume on the owner's pause", `document.querySelector(`+quote(`[data-pause="workstream:`+string(quiet)+`"] [data-field=resume]`)+`) !== null`)
	p.await("the same document", `window.notReloaded === true`)

	// A refusal of the API is shown as it came.
	file := filepath.Join(f.s.current().Root.String(), "runtime.json")
	data, err := os.ReadFile(file)
	must(t, err)
	must(t, os.WriteFile(file, append(data, '\n'), 0600))
	p.openPauses()
	p.choose(form+"select[name=target]", "factory")
	p.typeInto(form+"input[name=reason]", "Overnight")
	p.click(form + "button[type=submit]")
	p.awaitText("#pause-result", "runtime file changed externally")
	p.await("an error outcome", `document.getElementById('pause-result').dataset.outcome === 'error'`)
}

// The reload button lights when the configuration on disk differs from the
// loaded one and a reload has something to do. A reload that fails keeps it
// lit and shows the error; one that applies clears it and shows the new
// digest; a change only a restart applies is named and does not light it.
// serial: asserts the config drift indicator, which app.js refreshes only
// through a setInterval poll gated on document.visibilityState === 'visible'
// (internal/web/app.js); openBrowser's shared browser window has one visible
// tab at a time, and a concurrent test's chromedp.BringToFront() races this
// test's visibility.
func TestBrowserPageReloadLightsOnDiskDriftAndClears(t *testing.T) {
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	digest := func() string {
		t.Helper()
		cfg, err := f.c.Configuration(ctx)
		must(t, err)
		return cfg.Digest
	}
	lit := func(want bool) {
		t.Helper()
		value := "false"
		if want {
			value = "true"
		}
		p.await("reload lit "+value, `document.getElementById('reload').dataset.lit === `+quote(value))
	}
	shownDigest := func(want string) {
		t.Helper()
		p.await("digest "+want, `document.getElementById('digest').dataset.digest === `+quote(want))
	}

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.openView("config")
	loaded := digest()
	shownDigest(loaded)
	p.awaitText("#drift", "matches what is loaded")
	lit(false)
	p.eval(`window.notReloaded = true`, nil)

	path := filepath.Join(f.s.current().Root.String(), "config.toml")
	original, err := os.ReadFile(path)
	must(t, err)

	// A file that does not parse lights reload; the reload fails, the loaded
	// configuration stays and the error is shown until a reload succeeds.
	must(t, os.WriteFile(path, append(slices.Clone(original), []byte("masons = \n")...), 0600))
	lit(true)
	p.await("the settings button lit", `document.getElementById('settings-button').dataset.lit === 'true'`)
	p.awaitText(`#drift-files [data-state=invalid]`, "config.toml")
	p.click("#reload")
	p.awaitText("#reload-result", "the loaded configuration is unchanged")
	p.await("an error outcome", `document.getElementById('reload-result').dataset.outcome === 'error'`)
	p.awaitText("#last-error", "The last reload failed")
	lit(true)
	if got := digest(); got != loaded {
		t.Fatalf("a failed reload changed the digest to %s", got)
	}

	// A valid change keeps it lit; the reload applies it and clears it.
	changed := strings.Replace(string(original), "masons = 1", "masons = 2", 1)
	must(t, os.WriteFile(path, []byte(changed), 0600))
	p.awaitText(`#drift-files [data-state=changed]`, "config.toml")
	lit(true)
	p.click("#reload")
	p.await("an ok outcome", `document.getElementById('reload-result').dataset.outcome === 'ok'`)
	reloaded := digest()
	if reloaded == loaded {
		t.Fatal("the reload left the digest as it was")
	}
	p.awaitText("#reload-result", "Reloaded; loaded "+reloaded[:12])
	shownDigest(reloaded)
	lit(false)
	p.awaitText("#drift", "matches what is loaded")
	p.await("no last error", `document.getElementById('last-error').hidden`)
	p.awaitText(`#capacity [data-role=mason] [data-field=slots]`, "/ 2 slots")

	// A change only a restart applies is named and does not light reload; a
	// reload names it too.
	must(t, os.WriteFile(path, []byte(strings.Replace(changed, `web = "127.0.0.1:0"`, `web = "127.0.0.1:9"`, 1)), 0600))
	p.awaitText(`#config-diagnostics [data-code=restart_required]`, "listen.web")
	lit(false)
	p.click("#reload")
	p.awaitText("#reload-result", "Restart the service to apply listen.web.")
	p.await("the same document", `window.notReloaded === true`)
}

// The workstream menu abandons and archives work in progress in one step,
// and archives finished work. An archived workstream leaves the list of work
// for the Archived group and the pause targets, the page shows the next
// workstream in its place, and retains the archived title without history.
func TestBrowserPagePermanentlyArchivesWorkstreams(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	row := `[data-select="` + string(quiet) + `"]`
	menu := func(action string, offered bool) {
		t.Helper()
		p.await("the menu offering "+action, `document.querySelector('[data-workstream-action="`+action+`"]').hidden === `+map[bool]string{true: "false", false: "true"}[offered])
	}
	archived := func(want bool) {
		t.Helper()
		st, err := f.c.Status(ctx, quiet)
		must(t, err)
		if st.Archived != want {
			t.Fatalf("quiet archived %v, want %v: %+v", st.Archived, want, st)
		}
	}

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)
	p.selectWorkstream(quiet)
	p.await("no archived group", `document.getElementById('archived').hidden`)
	menu("archive", false)
	menu("abandon-archive", true)

	// Work in progress is abandoned with a reason and archived.
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="abandon-archive"]`)
	p.await("the abandonment chosen", `document.querySelector('#workstream-action [name=action]').value === 'abandon' && document.querySelector('#workstream-action [name=archive]').checked`)
	p.typeInto("#workstream-action [name=note]", "Superseded by the upload design.")
	p.click("#workstream-action [type=submit]")
	p.awaitText("#workstream-action .result", "Workstream abandoned and archived.")
	p.await("the next workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	p.await("quiet in the archived group", `document.querySelector('#archived-list `+row+`') !== null && document.querySelector('#workstream-list `+row+`') === null`)
	p.awaitText("#archived-count", "1")
	p.await("quiet no longer a pause target", `![...document.querySelectorAll('#pause-form select[name=target] option')].some((o) => o.value === `+quote("workstream:"+string(quiet))+`)`)
	archived(true)
	if st, _ := f.c.Status(ctx, quiet); st.State == nil || *st.State != AbandonedState {
		t.Fatalf("quiet was not abandoned: %+v", st)
	}

	p.click("#archived summary")
	p.await("archived title cannot open deleted history", `document.querySelector('#archived-list `+row+`').disabled`)
	if _, err := f.c.Unarchive(ctx, quiet); err == nil {
		t.Fatal("permanent archive was reversed")
	}
	p.await("the same document", `window.notReloaded === true`)
}

func TestBrowserArchiveWarnsBeforePermanentDeletion(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	_, err := f.c.Abandon(ctx, quiet, "Finished with this work")
	must(t, err)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(quiet)
	p.eval(`window.confirm = message => { window.archiveWarning = message; return false; }`, nil)
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="archive"]`)
	p.await("permanent deletion warning", `window.archiveWarning.includes('check output') && window.archiveWarning.includes('cannot be undone')`)
	status, err := f.c.Status(ctx, quiet)
	must(t, err)
	if status.Archived {
		t.Fatal("cancelled archive took effect")
	}
	p.eval(`window.confirm = () => true`, nil)
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="archive"]`)
	p.awaitText("#inbox-result", "history will be permanently deleted")
	p.await("archived title retained", `document.querySelector('#archived-list [data-select="`+string(quiet)+`"]') !== null`)
}
