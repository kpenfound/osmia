package service

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

type handInFixture struct {
	s       *Service
	c       *Client
	issues  *fakeIssues
	project config.ProjectID
	trace   string
	home    string
}

// newHandInFixture starts a service with an active project whose charter has
// rules.
func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	fake := &fakeIssues{Token: "ghp_handin_secret", Text: map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: "# Issue title\n\nIssue body\n"}}
	opts.Issues = fake
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return &handInFixture{s: s, c: c, issues: fake, project: added.Project.ID, trace: added.Project.Trace, home: filepath.Dir(clone)}
}

func (f *handInFixture) handIn(t *testing.T, req HandInRequest) HandInResponse {
	t.Helper()
	req.Project = f.project
	out, err := f.c.HandIn(context.Background(), req)
	must(t, err)
	return out
}

func (f *handInFixture) repository() *trace.Repository { return f.s.sole().repository }

// head returns how many commits the project's trace and each of its
// workstreams' traces hold, apart from the librarian's workstream, which the
// project's extraction writes to.
func (f *handInFixture) head(t *testing.T) map[string]int {
	t.Helper()
	commits := map[string]int{"project": traceCommits(t, f.trace)}
	for _, stream := range f.streams(t) {
		commits[string(stream)] = traceCommits(t, workstreamTraceDirectory(t, f.s.cfg.Root, f.project, stream))
	}
	return commits
}

// unchanged reports whether nothing was committed since head outside the
// librarian's workstream, and no workstream was created.
func (f *handInFixture) unchanged(t *testing.T, head map[string]int) bool {
	t.Helper()
	now := f.head(t)
	if !maps.Equal(now, head) {
		t.Logf("commits went from %v to %v", head, now)
		return false
	}
	return true
}

// streams lists the project's workstreams other than the librarian's.
func (f *handInFixture) streams(t *testing.T) []config.WorkstreamID {
	t.Helper()
	base, err := f.s.cfg.Root.WorkstreamTraces(f.project)
	must(t, err)
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	must(t, err)
	var out []config.WorkstreamID
	for _, entry := range entries {
		if id := config.WorkstreamID(entry.Name()); id != librarianWorkstream(f.project) {
			out = append(out, id)
		}
	}
	return out
}

// checkHanded verifies everything a successful hand-in records.
func (f *handInFixture) checkHanded(t *testing.T, out HandInResponse, key, name, source, content string) {
	t.Helper()
	ctx := context.Background()
	stream := HandInWorkstream(f.project, key)
	if out.Project != f.project || out.Workstream != stream || out.State != HandedState || out.Source != source {
		t.Fatalf("response %+v", out)
	}
	data, err := workstreamDocument(t, f.s.cfg.Root, f.project, stream, "handed/"+name)
	must(t, err)
	if string(data) != content {
		t.Fatalf("handed bytes %q, want %q", data, content)
	}
	r := f.repository()
	docs, err := trace.Read[trace.Document](r, stream)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	if len(docs) != 1 || docs[0].Path != "handed/"+name || docs[0].Content != content || docs[0].Source != source || docs[0].Actor != owner {
		t.Fatalf("handed document %+v", docs)
	}
	transitions, err := trace.Read[trace.Transition](r, stream)
	must(t, err)
	transitions = slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != trace.FeatureSubject })
	if len(transitions) != 1 {
		t.Fatalf("transitions %+v", transitions)
	}
	tr := transitions[0]
	if tr.Subject != trace.FeatureSubject || tr.From != "" || tr.To != "handed" || tr.Actor != owner || !strings.Contains(tr.Reason, "owner handed in handed/"+name+" from "+source) {
		t.Fatalf("transition %+v", tr)
	}
	if events := allTransitions(t, f.s.cfg.Root, f.project, stream); !slices.ContainsFunc(events, func(tr trace.Transition) bool { return tr.To == "handed" }) {
		t.Fatalf("transitions: %+v", events)
	}
	state, err := r.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	if state != (trace.WorkflowState{Version: 1, Value: "handed"}) {
		t.Fatalf("state %+v", state)
	}
	outbox, err := r.Outbox(stream)
	must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if chiefEvent(e.Event) {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || notices[0].TransitionID != tr.ID || !strings.Contains(notices[0].Event.Body, "changed to handed") {
		t.Fatalf("outbox %+v", outbox)
	}
	th, err := r.ChiefOfStaffThread(stream)
	must(t, err)
	if th.Identity.Role != trace.ChiefOfStaff {
		t.Fatalf("chief of staff %+v", th)
	}
	if _, err := r.EnsureChiefOfStaff(ctx, stream, time.Now(), owner); err != nil {
		t.Fatal(err)
	}
}

func TestHandInFileURLAndStdin(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	design := filepath.Join(f.home, "design.md")
	content := "# Design\n\nNo trailing newline, a tab\tand unicode: é"
	must(t, os.WriteFile(design, []byte(content), 0600))

	out := f.handIn(t, HandInRequest{Key: "file", Path: design})
	f.checkHanded(t, out, "file", "design.md", "file:"+design, content)
	// The copy is independent of the original file.
	must(t, os.WriteFile(design, []byte("changed"), 0600))
	if data, _ := workstreamDocument(t, f.s.cfg.Root, f.project, out.Workstream, "handed/design.md"); string(data) != content {
		t.Fatalf("handed copy follows the original: %q", data)
	}

	out = f.handIn(t, HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/12"})
	f.checkHanded(t, out, "url", "issue-12.md", "https://github.com/owner/repo/issues/12", "# Issue title\n\nIssue body\n")

	stdin := "piped\r\ndesign\n\n"
	out = f.handIn(t, HandInRequest{Key: "stdin", Stdin: &stdin})
	f.checkHanded(t, out, "stdin", "stdin", "stdin", stdin)

	// A file name the trace does not accept is stored under a fixed name.
	hidden := filepath.Join(f.home, ".hidden:design")
	must(t, os.WriteFile(hidden, []byte("hidden"), 0600))
	out = f.handIn(t, HandInRequest{Key: "hidden", Path: hidden})
	f.checkHanded(t, out, "hidden", "input", "file:"+hidden, "hidden")

	if streams := f.streams(t); len(streams) != 4 {
		t.Fatalf("workstreams %v", streams)
	}
	st, err := f.c.Status(context.Background(), HandInWorkstream(f.project, "stdin"))
	must(t, err)
	if st.State == nil || *st.State != "handed" {
		t.Fatalf("status %+v", st)
	}
}

func TestHandInRetryReturnsTheSameWorkstream(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	design := filepath.Join(f.home, "design.md")
	must(t, os.WriteFile(design, []byte("design"), 0600))
	first := f.handIn(t, HandInRequest{Key: "retry", Path: design})
	head := f.head(t)
	// A retry reads nothing and writes nothing, even if the file changed.
	must(t, os.WriteFile(design, []byte("edited"), 0600))
	if again := f.handIn(t, HandInRequest{Key: "retry", Path: design}); again != first {
		t.Fatalf("retry %+v, first %+v", again, first)
	}
	if !f.unchanged(t, head) {
		t.Fatal("retry wrote to the trace")
	}
	f.checkHanded(t, first, "retry", "design.md", "file:"+design, "design")

	url := "https://github.com/owner/repo/issues/12"
	first = f.handIn(t, HandInRequest{Key: "issue", URL: url})
	if again := f.handIn(t, HandInRequest{Key: "issue", URL: url}); again != first || f.issues.Fetches != 1 {
		t.Fatalf("retry %+v fetched %d times", again, f.issues.Fetches)
	}
	text := "text"
	first = f.handIn(t, HandInRequest{Key: "text", Stdin: &text})
	if again := f.handIn(t, HandInRequest{Key: "text", Stdin: &text}); again != first {
		t.Fatalf("stdin retry %+v", again)
	}
	if streams := f.streams(t); len(streams) != 3 {
		t.Fatalf("workstreams %v", streams)
	}

	// The same key with other input is refused.
	head = f.head(t)
	other := "other"
	for _, req := range []HandInRequest{
		{Key: "text", Stdin: &other},
		{Key: "text", Path: design},
		{Key: "retry", URL: url},
		{Key: "retry", Path: filepath.Join(f.home, "other.md")},
	} {
		req.Project = f.project
		if api := handInError(t, f.c, req); api.Code != Conflict || !strings.Contains(api.Message, "key "+req.Key) {
			t.Fatalf("%+v: %+v", req, api)
		}
	}
	if !f.unchanged(t, head) || len(f.streams(t)) != 3 {
		t.Fatal("refused retry wrote to the trace")
	}
}

// The workstream a hand-in creates takes a priority and a workstream pause on
// the running service, without a restart. A workstream the trace does not hold
// is still refused.
func TestHandInMakesItsWorkstreamKnownToRuntime(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	ctx := context.Background()
	text := "design"
	out := f.handIn(t, HandInRequest{Key: "known", Stdin: &text})
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: out.Workstream}
	mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{out.Workstream}})
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	rt, err := f.c.Runtime(ctx)
	must(t, err)
	if len(rt.Diagnostics) != 0 {
		t.Fatalf("diagnostics %+v", rt.Diagnostics)
	}
	if len(rt.Effective.Priorities) != 1 || rt.Effective.Priorities[0].Project != f.project ||
		!reflect.DeepEqual(rt.Effective.Priorities[0].Workstreams, []config.WorkstreamID{out.Workstream}) {
		t.Fatalf("effective priorities %+v", rt.Effective.Priorities)
	}
	if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target != target || rt.Effective.Pauses[0].Mode != "soft" {
		t.Fatalf("effective pauses %+v", rt.Effective.Pauses)
	}
	// A retried hand-in on the workstream still succeeds.
	if again := f.handIn(t, HandInRequest{Key: "known", Stdin: &text}); again != out {
		t.Fatalf("retry %+v, first %+v", again, out)
	}
	unknown := config.WorkstreamID("w_00000000000000000000000000000000")
	assertCode(t, f.c.Do(ctx, "PUT", Prefix+"/runtime/priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{unknown}}, nil), Validation)
	assertCode(t, f.c.Do(ctx, "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: unknown}, Mode: "soft", Source: "owner"}, nil), Validation)
}

func TestHandInFinishesAnInterruptedHandIn(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	r := f.repository()
	text := "design"

	// Interrupted after the workstream was created, before the copy.
	stream := HandInWorkstream(f.project, "created")
	must(t, r.CreateWorkstream(ctx, stream, time.Now().UTC(), owner))
	f.checkHanded(t, f.handIn(t, HandInRequest{Key: "created", Stdin: &text}), "created", "stdin", "stdin", text)

	// Interrupted after the copy, before the transition: the transition takes
	// the copy's timestamp.
	stream = HandInWorkstream(f.project, "copied")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, r.CreateWorkstream(ctx, stream, at, owner))
	must(t, r.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "handed", Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: owner, Cause: "handin"},
		Path: "handed/stdin", Content: text, Source: "stdin"}))
	f.checkHanded(t, f.handIn(t, HandInRequest{Key: "copied", Stdin: &text}), "copied", "stdin", "stdin", text)
	transitions, err := trace.Read[trace.Transition](r, stream)
	must(t, err)
	if !transitions[0].At.Equal(at) {
		t.Fatalf("transition at %v", transitions[0].At)
	}

	// Interrupted after the copy, before the skip of debate: the retry records
	// the skip, then the handed state, both at the copy's timestamp.
	copied := func(key string) config.WorkstreamID {
		stream := HandInWorkstream(f.project, key)
		must(t, r.CreateWorkstream(ctx, stream, at, owner))
		must(t, r.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "handed", Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: owner, Cause: "handin"},
			Path: "handed/stdin", Content: text, Source: "stdin"}))
		return stream
	}
	moves := func(stream config.WorkstreamID) string {
		transitions, err := trace.Read[trace.Transition](r, stream)
		must(t, err)
		var out []string
		for _, tr := range transitions {
			if tr.Subject == factoryPolicySubject {
				continue
			}
			if !tr.At.Equal(at) || tr.Actor != owner {
				t.Fatalf("transition %+v", tr)
			}
			out = append(out, tr.ID+":"+tr.Subject+":"+tr.Cause)
		}
		return strings.Join(out, " ")
	}
	// checkHanded expects the handed transition alone, so these check the
	// skip's transitions in order instead.
	want := "shed-owner-skip:shed-owner:handin handin:feature:handin"
	stream = copied("unskipped")
	if out := f.handIn(t, HandInRequest{Key: "unskipped", Stdin: &text, SkipDebate: true}); out.Workstream != stream || out.State != HandedState || !out.SkipDebate {
		t.Fatalf("response %+v", out)
	}
	if got := moves(stream); got != want {
		t.Fatalf("transitions %s, want %s", got, want)
	}

	// Interrupted after the skip, before the handed state: the retry must ask
	// for the skip, and then finishes the hand-in without skipping again.
	stream = copied("skipped")
	_, err = r.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: ownerHeader(skipTransition, f.project, stream, handInTransition, at), Subject: ownerSubject, To: skippedValue, Reason: handInSkipReason}})
	must(t, err)
	_, err = f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "skipped", Stdin: &text})
	if !failed(err, Conflict) || err.Error() != "conflict: key skipped already handed in workstream "+string(stream)+" skipping debate; use a new key" {
		t.Fatalf("retry without the skip: %v", err)
	}
	if out := f.handIn(t, HandInRequest{Key: "skipped", Stdin: &text, SkipDebate: true}); out.Workstream != stream || out.State != HandedState || !out.SkipDebate {
		t.Fatalf("response %+v", out)
	}
	if got := moves(stream); got != want {
		t.Fatalf("transitions %s, want %s", got, want)
	}
	if len(f.streams(t)) != 4 {
		t.Fatal(f.streams(t))
	}
}

func TestHandInRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	design := filepath.Join(f.home, "design.md")
	must(t, os.WriteFile(design, []byte("design"), 0600))
	// Record the charter edit before taking the baseline.
	f.handIn(t, HandInRequest{Key: "baseline", Path: design})
	head := f.head(t)
	empty, blank := "", " \n\t"
	must(t, os.WriteFile(filepath.Join(f.home, "latin1.md"), []byte("bad \xff byte"), 0600))
	large := strings.Repeat("x", MaxHandedBytes+1)
	unknown := config.ProjectID("p_0123456789abcdef0123456789abcdef")
	must(t, os.WriteFile(filepath.Join(f.home, "large.md"), []byte(large), 0600))
	must(t, os.WriteFile(filepath.Join(f.home, "empty.md"), nil, 0600))
	for _, c := range []struct {
		req  HandInRequest
		code Code
		text string
	}{
		{HandInRequest{Project: unknown, Key: "k", Path: design}, NotFound, "project " + string(unknown) + " is not an active project"},
		{HandInRequest{Project: f.project, Path: design}, Validation, "key must be"},
		{HandInRequest{Project: f.project, Key: "-bad", Path: design}, Validation, "key must be"},
		{HandInRequest{Project: f.project, Key: strings.Repeat("k", 129), Path: design}, Validation, "key must be"},
		{HandInRequest{Project: f.project, Key: "k"}, Validation, "exactly one input"},
		{HandInRequest{Project: f.project, Key: "k", Path: design, URL: "https://github.com/owner/repo/issues/12"}, Validation, "exactly one input"},
		{HandInRequest{Project: f.project, Key: "k", Path: design, Stdin: &empty}, Validation, "exactly one input"},
		{HandInRequest{Project: f.project, Key: "k", Path: "design.md"}, Validation, "absolute"},
		{HandInRequest{Project: f.project, Key: "k", Path: f.home + "/../design.md"}, Validation, "absolute"},
		{HandInRequest{Project: f.project, Key: "k", Path: filepath.Join(f.home, "missing.md")}, Validation, "cannot read " + filepath.Join(f.home, "missing.md")},
		{HandInRequest{Project: f.project, Key: "k", Path: f.home}, Validation, "not a regular file"},
		{HandInRequest{Project: f.project, Key: "k", Path: filepath.Join(f.home, "large.md")}, Validation, "larger than"},
		{HandInRequest{Project: f.project, Key: "k", Path: filepath.Join(f.home, "empty.md")}, Validation, "empty"},
		{HandInRequest{Project: f.project, Key: "k", Stdin: &empty}, Validation, "empty"},
		{HandInRequest{Project: f.project, Key: "k", Stdin: &blank}, Validation, "empty"},
		{HandInRequest{Project: f.project, Key: "k", Path: filepath.Join(f.home, "latin1.md")}, Validation, "UTF-8"},
		{HandInRequest{Project: f.project, Key: "k", URL: "https://example.com/owner/repo/issues/12"}, Validation, "issue URL"},
		{HandInRequest{Project: f.project, Key: "k", URL: "https://github.com/owner/repo/issues/13"}, Internal, "cannot fetch https://github.com/owner/repo/issues/13"},
	} {
		api := handInError(t, f.c, c.req)
		if api.Code != c.code || !strings.Contains(api.Message, c.text) || strings.Contains(api.Message, f.issues.Token) {
			t.Errorf("%+.80v: %+v", c.req, api)
		}
	}
	if !f.unchanged(t, head) || len(f.streams(t)) != 1 {
		t.Fatal("a refused hand-in wrote to the trace")
	}

	// The charter gate comes before the input is read or fetched.
	fetches := f.issues.Fetches
	writeCharter(t, f.c, f.project, "# Charter\n")
	_, _, err := f.s.loadCharter(context.Background(), f.project)
	must(t, err)
	head = f.head(t)
	api := handInError(t, f.c, HandInRequest{Project: f.project, Key: "k", URL: "https://github.com/owner/repo/issues/12"})
	if api.Code != CharterEmpty || !strings.Contains(api.Message, "osmia edit charter "+string(f.project)) || f.issues.Fetches != fetches {
		t.Fatalf("empty charter: %+v, %d fetches", api, f.issues.Fetches-fetches)
	}
	if !f.unchanged(t, head) || len(f.streams(t)) != 1 {
		t.Fatal("an empty charter hand-in wrote to the trace")
	}

	// An inactive project is refused the same way as an unknown one.
	_, err = f.c.RemoveProject(context.Background(), f.project)
	must(t, err)
	api = handInError(t, f.c, HandInRequest{Project: f.project, Key: "k", Path: design})
	if api.Code != NotFound || !strings.Contains(api.Message, "project "+string(f.project)+" is not an active project") {
		t.Fatalf("inactive project: %+v", api)
	}
	if !f.unchanged(t, head) {
		t.Fatal("an inactive project hand-in wrote to the trace")
	}
}

func TestHandInStatusCodes(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	body := `{"project":"` + string(f.project) + `","key":"k","stdin":"text"}`
	if code := handInStatus(t, f.s, body); code != http.StatusOK {
		t.Fatalf("hand-in status %d", code)
	}
	if code := handInStatus(t, f.s, `{"project":"`+string(f.project)+`","key":"k","stdin":"other"}`); code != http.StatusConflict {
		t.Fatalf("key conflict status %d", code)
	}
	if code := handInStatus(t, f.s, `{"project":"`+string(f.project)+`","key":"k","paths":["/tmp/a"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field status %d", code)
	}
	if code := handInStatus(t, f.s, `{"project":"`+string(f.project)+`","key":"fetch","url":"https://github.com/owner/repo/issues/99"}`); code != http.StatusInternalServerError {
		t.Fatalf("fetch failure status %d", code)
	}
}

// serial: sets process environment variable GITHUB_TOKEN via t.Setenv
func TestHandInCredentialsStayInTheService(t *testing.T) {
	f := newHandInFixture(t)
	f.handIn(t, HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/12"})
	handInError(t, f.c, HandInRequest{Project: f.project, Key: "missing", URL: "https://github.com/owner/repo/issues/99"})
	// Session prompts and bundles are built from the trace.
	must(t, filepath.WalkDir(f.trace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), f.issues.Token) {
			t.Errorf("%s contains the issue client's token", path)
		}
		return err
	}))
	// A session environment admits no credential variable.
	if err := coreadapter.PublicEnvironment(map[string]string{"GITHUB_TOKEN": f.issues.Token}); err == nil {
		t.Fatal("session environment admits GITHUB_TOKEN")
	}

	// Without an injected client the service reads GitHub with its own token.
	t.Setenv("GITHUB_TOKEN", "from-environment")
	s, _ := start(t, fixture(t))
	gh, ok := s.options.Issues.(issues.GitHub)
	if !ok || gh.Token != "from-environment" || gh.BaseURL != "" || gh.HTTP == nil || gh.HTTP.Timeout == 0 {
		t.Fatalf("default issue client %#v", s.options.Issues)
	}
}

func TestHandInRefusesAFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	fifo := filepath.Join(f.home, "design.fifo")
	must(t, syscall.Mkfifo(fifo, 0600))
	done := make(chan *APIError, 1)
	go func() {
		done <- handInError(t, f.c, HandInRequest{Project: f.project, Key: "fifo", Path: fifo})
	}()
	select {
	case api := <-done:
		if api.Code != Validation || !strings.Contains(api.Message, fifo+" is not a regular file") {
			t.Fatalf("fifo: %+v", api)
		}
	case <-time.After(time.Minute):
		t.Fatal("hand-in of a FIFO blocked")
	}
	text := "design"
	f.handIn(t, HandInRequest{Key: "after", Stdin: &text})
	if len(f.streams(t)) != 1 {
		t.Fatal(f.streams(t))
	}
}

func TestHandInFetchDoesNotHoldOtherHandIns(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	url := "https://github.com/owner/repo/issues/12"
	release := make(chan struct{})
	f.issues.Mu.Lock()
	f.issues.Block, f.issues.Started = release, make(chan struct{})
	started := f.issues.Started
	f.issues.Mu.Unlock()
	slow := make(chan HandInResponse, 1)
	go func() {
		out, err := f.c.HandIn(context.Background(), HandInRequest{Project: f.project, Key: "issue", URL: url})
		if err != nil {
			t.Error(err)
		}
		slow <- out
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("fetch did not start")
	}
	// Another hand-in, and the same key handed in again, finish while the
	// first fetch is held.
	text := "design"
	f.handIn(t, HandInRequest{Key: "other", Stdin: &text})
	fast := f.handIn(t, HandInRequest{Key: "issue", URL: url})
	close(release)
	select {
	case out := <-slow:
		// The held request finds the copy made meanwhile and returns it.
		if out != fast {
			t.Fatalf("held request %+v, retry %+v", out, fast)
		}
	case <-time.After(time.Minute):
		t.Fatal("held hand-in did not finish")
	}
	f.checkHanded(t, fast, "issue", "issue-12.md", url, "# Issue title\n\nIssue body\n")
	if len(f.streams(t)) != 2 {
		t.Fatal(f.streams(t))
	}
}
