package service

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/trace"
)

// fakeIssues serves issue text from memory and counts fetches. Its token
// stands in for a credential that must never leave the service.
type fakeIssues struct {
	mu      sync.Mutex
	token   string
	text    map[issues.Ref]string
	fetches int
	// block, when set, holds the first fetch until it is closed; started
	// is signalled when that fetch begins.
	block, started chan struct{}
}

func (f *fakeIssues) Fetch(_ context.Context, ref issues.Ref) (string, error) {
	f.mu.Lock()
	block, started := f.block, f.started
	f.block = nil
	f.mu.Unlock()
	if block != nil {
		close(started)
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	text, ok := f.text[ref]
	if !ok {
		return "", errors.New("not found with token " + f.token)
	}
	return text, nil
}

type handInFixture struct {
	s       *Service
	c       *Client
	issues  *fakeIssues
	project config.ProjectID
	trace   string
	charter string
	home    string
}

// newHandInFixture starts a service with an active project whose charter has
// rules.
func newHandInFixture(t *testing.T) *handInFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	fake := &fakeIssues{token: "ghp_handin_secret", text: map[issues.Ref]string{{Owner: "owner", Repo: "repo", Number: 12}: "# Issue title\n\nIssue body\n"}}
	opts.Issues = fake
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	return &handInFixture{s: s, c: c, issues: fake, project: added.Project.ID, trace: added.Project.Trace, charter: added.Project.Charter, home: filepath.Dir(clone)}
}

func (f *handInFixture) handIn(t *testing.T, req HandInRequest) HandInResponse {
	t.Helper()
	req.Project = f.project
	out, err := f.c.HandIn(context.Background(), req)
	must(t, err)
	return out
}

func (f *handInFixture) repository() *trace.Repository { return f.s.active.repository }

func (f *handInFixture) head(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", f.trace, "rev-parse", "HEAD").Output()
	must(t, err)
	return strings.TrimSpace(string(out))
}

// unchanged reports whether no commit since head touched the trace outside
// the librarian's workstream, which the project's extraction writes to.
func (f *handInFixture) unchanged(t *testing.T, head string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", f.trace, "diff", "--name-only", head, "HEAD").Output()
	must(t, err)
	librarian := "workstreams/" + string(librarianWorkstream(f.project)) + "/"
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, librarian) {
			t.Logf("changed: %s", name)
			return false
		}
	}
	return true
}

// streams lists the project's workstreams other than the librarian's.
func (f *handInFixture) streams(t *testing.T) []config.WorkstreamID {
	t.Helper()
	streams, err := f.repository().Workstreams()
	must(t, err)
	var out []config.WorkstreamID
	for _, id := range streams {
		if id != librarianWorkstream(f.project) {
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
	if out.Project != f.project || out.Workstream != stream || out.State != HandedState || out.Source != source ||
		out.Handed != filepath.Join(f.trace, "workstreams", string(stream), "handed", name) {
		t.Fatalf("response %+v", out)
	}
	data, err := os.ReadFile(out.Handed)
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
	if len(transitions) != 1 {
		t.Fatalf("transitions %+v", transitions)
	}
	tr := transitions[0]
	if tr.Subject != trace.FeatureSubject || tr.From != "" || tr.To != "handed" || tr.Actor != owner || !strings.Contains(tr.Reason, "owner handed in handed/"+name+" from "+source) {
		t.Fatalf("transition %+v", tr)
	}
	events, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "events.jsonl"))
	must(t, err)
	if !strings.Contains(string(events), `"to":"handed"`) {
		t.Fatalf("events.jsonl:\n%s", events)
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
		if e.Event.Kind == trace.NoticeKind {
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
	if data, _ := os.ReadFile(out.Handed); string(data) != content {
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
	if again := f.handIn(t, HandInRequest{Key: "issue", URL: url}); again != first || f.issues.fetches != 1 {
		t.Fatalf("retry %+v fetched %d times", again, f.issues.fetches)
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
			if !tr.At.Equal(at) || tr.Actor != owner {
				t.Fatalf("transition %+v", tr)
			}
			out = append(out, tr.ID+":"+tr.Subject+":"+tr.Cause)
		}
		return strings.Join(out, " ")
	}
	want := "shed-owner-skip:shed-owner:handin handin:feature:handin"
	stream = copied("unskipped")
	f.checkHanded(t, f.handIn(t, HandInRequest{Key: "unskipped", Stdin: &text, SkipDebate: true}), "unskipped", "stdin", "stdin", text)
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
	f.checkHanded(t, f.handIn(t, HandInRequest{Key: "skipped", Stdin: &text, SkipDebate: true}), "skipped", "stdin", "stdin", text)
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
		if api.Code != c.code || !strings.Contains(api.Message, c.text) || strings.Contains(api.Message, f.issues.token) {
			t.Errorf("%+.80v: %+v", c.req, api)
		}
	}
	if !f.unchanged(t, head) || len(f.streams(t)) != 1 {
		t.Fatal("a refused hand-in wrote to the trace")
	}

	// The charter gate comes before the input is read or fetched.
	fetches := f.issues.fetches
	must(t, os.WriteFile(f.charter, []byte("# Charter\n"), 0600))
	_, _, err := f.s.loadCharter(context.Background(), f.project)
	must(t, err)
	head = f.head(t)
	api := handInError(t, f.c, HandInRequest{Project: f.project, Key: "k", URL: "https://github.com/owner/repo/issues/12"})
	if api.Code != CharterEmpty || !strings.Contains(api.Message, f.charter) || f.issues.fetches != fetches {
		t.Fatalf("empty charter: %+v, %d fetches", api, f.issues.fetches-fetches)
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
		if err == nil && strings.Contains(string(data), f.issues.token) {
			t.Errorf("%s contains the issue client's token", path)
		}
		return err
	}))
	// A session environment admits no credential variable.
	if err := coreadapter.PublicEnvironment(map[string]string{"GITHUB_TOKEN": f.issues.token}); err == nil {
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
	f.issues.mu.Lock()
	f.issues.block, f.issues.started = release, make(chan struct{})
	started := f.issues.started
	f.issues.mu.Unlock()
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
