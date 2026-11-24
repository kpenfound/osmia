package intakejourney

import (
	context "context"
	fs "io/fs"
	http "net/http"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	syscall "syscall"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	issues "github.com/kpenfound/osmia/internal/issues"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestHandInFileURLAndStdin(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	design := filepath.Join(f.Home, "design.md")
	content := "# Design\n\nNo trailing newline, a tab\tand unicode: é"
	fixturekit.Must(t, os.WriteFile(design, []byte(content), 0600))

	out := f.HandIn(t, api.HandInRequest{Key: "file", Path: design})
	f.CheckHanded(t, out, "file", "design.md", "file:"+design, content)

	fixturekit.Must(t, os.WriteFile(design, []byte("changed"), 0600))
	if data, _ := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, out.Workstream, "handed/design.md"); string(data) != content {
		t.Fatalf("handed copy follows the original: %q", data)
	}

	out = f.HandIn(t, api.HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/12"})
	f.CheckHanded(t, out, "url", "issue-12.md", "https://github.com/owner/repo/issues/12", "# Issue title\n\nIssue body\n")

	stdin := "piped\r\ndesign\n\n"
	out = f.HandIn(t, api.HandInRequest{Key: "stdin", Stdin: &stdin})
	f.CheckHanded(t, out, "stdin", "stdin", "stdin", stdin)

	hidden := filepath.Join(f.Home, ".hidden:design")
	fixturekit.Must(t, os.WriteFile(hidden, []byte("hidden"), 0600))
	out = f.HandIn(t, api.HandInRequest{Key: "hidden", Path: hidden})
	f.CheckHanded(t, out, "hidden", "input", "file:"+hidden, "hidden")

	if streams := f.Streams(t); len(streams) != 4 {
		t.Fatalf("workstreams %v", streams)
	}
	st, err := f.C.Status(context.Background(), intake.HandInWorkstream(f.Project, "stdin"))
	fixturekit.Must(t, err)
	if st.State == nil || *st.State != "handed" {
		t.Fatalf("status %+v", st)
	}
}

func TestHandInRetryReturnsTheSameWorkstream(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	design := filepath.Join(f.Home, "design.md")
	fixturekit.Must(t, os.WriteFile(design, []byte("design"), 0600))
	first := f.HandIn(t, api.HandInRequest{Key: "retry", Path: design})
	head := f.Head(t)

	fixturekit.Must(t, os.WriteFile(design, []byte("edited"), 0600))
	if again := f.HandIn(t, api.HandInRequest{Key: "retry", Path: design}); again != first {
		t.Fatalf("retry %+v, first %+v", again, first)
	}
	if !f.Unchanged(t, head) {
		t.Fatal("retry wrote to the trace")
	}
	f.CheckHanded(t, first, "retry", "design.md", "file:"+design, "design")

	url := "https://github.com/owner/repo/issues/12"
	first = f.HandIn(t, api.HandInRequest{Key: "issue", URL: url})
	if again := f.HandIn(t, api.HandInRequest{Key: "issue", URL: url}); again != first || f.Issues.Fetches != 1 {
		t.Fatalf("retry %+v fetched %d times", again, f.Issues.Fetches)
	}
	text := "text"
	first = f.HandIn(t, api.HandInRequest{Key: "text", Stdin: &text})
	if again := f.HandIn(t, api.HandInRequest{Key: "text", Stdin: &text}); again != first {
		t.Fatalf("stdin retry %+v", again)
	}
	if streams := f.Streams(t); len(streams) != 3 {
		t.Fatalf("workstreams %v", streams)
	}

	head = f.Head(t)
	other := "other"
	for _, req := range []api.HandInRequest{
		{Key: "text", Stdin: &other},
		{Key: "text", Path: design},
		{Key: "retry", URL: url},
		{Key: "retry", Path: filepath.Join(f.Home, "other.md")},
	} {
		req.Project = f.Project
		if apiErr := handInError(t, f.C, req); apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "key "+req.Key) {
			t.Fatalf("%+v: %+v", req, apiErr)
		}
	}
	if !f.Unchanged(t, head) || len(f.Streams(t)) != 3 {
		t.Fatal("refused retry wrote to the trace")
	}
}

func TestHandInMakesItsWorkstreamKnownToRuntime(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	ctx := context.Background()
	text := "design"
	out := f.HandIn(t, api.HandInRequest{Key: "known", Stdin: &text})
	target := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: out.Workstream}
	intakefixture.Mutation(t, f.C, "PUT", "priority", api.PriorityRequest{Project: f.Project, Workstreams: []config.WorkstreamID{out.Workstream}})
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	rt, err := f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	if len(rt.Diagnostics) != 0 {
		t.Fatalf("diagnostics %+v", rt.Diagnostics)
	}
	if len(rt.Effective.Priorities) != 1 || rt.Effective.Priorities[0].Project != f.Project ||
		!reflect.DeepEqual(rt.Effective.Priorities[0].Workstreams, []config.WorkstreamID{out.Workstream}) {
		t.Fatalf("effective priorities %+v", rt.Effective.Priorities)
	}
	if len(rt.Effective.Pauses) != 1 || rt.Effective.Pauses[0].Target != target || rt.Effective.Pauses[0].Mode != "soft" {
		t.Fatalf("effective pauses %+v", rt.Effective.Pauses)
	}

	if again := f.HandIn(t, api.HandInRequest{Key: "known", Stdin: &text}); again != out {
		t.Fatalf("retry %+v, first %+v", again, out)
	}
	unknown := config.WorkstreamID("w_00000000000000000000000000000000")
	fixturekit.AssertCode(t, f.C.Do(ctx, "PUT", api.Prefix+"/runtime/priority", api.PriorityRequest{Project: f.Project, Workstreams: []config.WorkstreamID{unknown}}, nil), api.Validation)
	fixturekit.AssertCode(t, f.C.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: unknown}, Mode: "soft", Source: "owner"}, nil), api.Validation)
}

func TestHandInFinishesAnInterruptedHandIn(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	r := f.Repository()
	text := "design"

	stream := intake.HandInWorkstream(f.Project, "created")
	fixturekit.Must(t, r.CreateWorkstream(ctx, stream, time.Now().UTC(), owner))
	f.CheckHanded(t, f.HandIn(t, api.HandInRequest{Key: "created", Stdin: &text}), "created", "stdin", "stdin", text)

	stream = intake.HandInWorkstream(f.Project, "copied")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fixturekit.Must(t, r.CreateWorkstream(ctx, stream, at, owner))
	fixturekit.Must(t, r.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "handed", Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: owner, Cause: "handin"},
		Path: "handed/stdin", Content: text, Source: "stdin"}))
	f.CheckHanded(t, f.HandIn(t, api.HandInRequest{Key: "copied", Stdin: &text}), "copied", "stdin", "stdin", text)
	transitions, err := trace.Read[trace.Transition](r, stream)
	fixturekit.Must(t, err)
	if !transitions[0].At.Equal(at) {
		t.Fatalf("transition at %v", transitions[0].At)
	}

	copied := func(key string) config.WorkstreamID {
		stream := intake.HandInWorkstream(f.Project, key)
		fixturekit.Must(t, r.CreateWorkstream(ctx, stream, at, owner))
		fixturekit.Must(t, r.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "handed", Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: owner, Cause: "handin"},
			Path: "handed/stdin", Content: text, Source: "stdin"}))
		return stream
	}
	moves := func(stream config.WorkstreamID) string {
		transitions, err := trace.Read[trace.Transition](r, stream)
		fixturekit.Must(t, err)
		var out []string
		for _, tr := range transitions {
			if tr.Subject == intake.FactoryPolicySubject {
				continue
			}
			if !tr.At.Equal(at) || tr.Actor != owner {
				t.Fatalf("transition %+v", tr)
			}
			out = append(out, tr.ID+":"+tr.Subject+":"+tr.Cause)
		}
		return strings.Join(out, " ")
	}

	want := "shed-owner-skip:shed-owner:handin handin:feature:handin"
	stream = copied("unskipped")
	if out := f.HandIn(t, api.HandInRequest{Key: "unskipped", Stdin: &text, SkipDebate: true}); out.Workstream != stream || out.State != intake.HandedState || !out.SkipDebate {
		t.Fatalf("response %+v", out)
	}
	if got := moves(stream); got != want {
		t.Fatalf("transitions %s, want %s", got, want)
	}

	stream = copied("skipped")
	_, err = r.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: intake.OwnerHeader(intake.SkipTransition, f.Project, stream, intake.HandInTransition, at), Subject: intake.OwnerSubject, To: intake.SkippedValue, Reason: intake.HandInSkipReason}})
	fixturekit.Must(t, err)
	_, err = f.C.HandIn(ctx, api.HandInRequest{Project: f.Project, Key: "skipped", Stdin: &text})
	if !intakefixture.Failed(err, api.Conflict) || err.Error() != "conflict: key skipped already handed in workstream "+string(stream)+" skipping debate; use a new key" {
		t.Fatalf("retry without the skip: %v", err)
	}
	if out := f.HandIn(t, api.HandInRequest{Key: "skipped", Stdin: &text, SkipDebate: true}); out.Workstream != stream || out.State != intake.HandedState || !out.SkipDebate {
		t.Fatalf("response %+v", out)
	}
	if got := moves(stream); got != want {
		t.Fatalf("transitions %s, want %s", got, want)
	}
	if len(f.Streams(t)) != 4 {
		t.Fatal(f.Streams(t))
	}
}

func TestHandInRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	design := filepath.Join(f.Home, "design.md")
	fixturekit.Must(t, os.WriteFile(design, []byte("design"), 0600))

	f.HandIn(t, api.HandInRequest{Key: "baseline", Path: design})
	head := f.Head(t)
	empty, blank := "", " \n\t"
	fixturekit.Must(t, os.WriteFile(filepath.Join(f.Home, "latin1.md"), []byte("bad \xff byte"), 0600))
	large := strings.Repeat("x", intake.MaxHandedBytes+1)
	unknown := config.ProjectID("p_0123456789abcdef0123456789abcdef")
	fixturekit.Must(t, os.WriteFile(filepath.Join(f.Home, "large.md"), []byte(large), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(f.Home, "empty.md"), nil, 0600))
	for _, c := range []struct {
		req  api.HandInRequest
		code api.Code
		text string
	}{
		{api.HandInRequest{Project: unknown, Key: "k", Path: design}, api.NotFound, "project " + string(unknown) + " is not an active project"},
		{api.HandInRequest{Project: f.Project, Path: design}, api.Validation, "key must be"},
		{api.HandInRequest{Project: f.Project, Key: "-bad", Path: design}, api.Validation, "key must be"},
		{api.HandInRequest{Project: f.Project, Key: strings.Repeat("k", 129), Path: design}, api.Validation, "key must be"},
		{api.HandInRequest{Project: f.Project, Key: "k"}, api.Validation, "exactly one input"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: design, URL: "https://github.com/owner/repo/issues/12"}, api.Validation, "exactly one input"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: design, Stdin: &empty}, api.Validation, "exactly one input"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: "design.md"}, api.Validation, "absolute"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: f.Home + "/../design.md"}, api.Validation, "absolute"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: filepath.Join(f.Home, "missing.md")}, api.Validation, "cannot read " + filepath.Join(f.Home, "missing.md")},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: f.Home}, api.Validation, "not a regular file"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: filepath.Join(f.Home, "large.md")}, api.Validation, "larger than"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: filepath.Join(f.Home, "empty.md")}, api.Validation, "empty"},
		{api.HandInRequest{Project: f.Project, Key: "k", Stdin: &empty}, api.Validation, "empty"},
		{api.HandInRequest{Project: f.Project, Key: "k", Stdin: &blank}, api.Validation, "empty"},
		{api.HandInRequest{Project: f.Project, Key: "k", Path: filepath.Join(f.Home, "latin1.md")}, api.Validation, "UTF-8"},
		{api.HandInRequest{Project: f.Project, Key: "k", URL: "https://example.com/owner/repo/issues/12"}, api.Validation, "issue URL"},
		{api.HandInRequest{Project: f.Project, Key: "k", URL: "https://github.com/owner/repo/issues/13"}, api.Internal, "cannot fetch https://github.com/owner/repo/issues/13"},
	} {
		apiErr := handInError(t, f.C, c.req)
		if apiErr.Code != c.code || !strings.Contains(apiErr.Message, c.text) || strings.Contains(apiErr.Message, f.Issues.Token) {
			t.Errorf("%+.80v: %+v", c.req, apiErr)
		}
	}
	if !f.Unchanged(t, head) || len(f.Streams(t)) != 1 {
		t.Fatal("a refused hand-in wrote to the trace")
	}
}

func TestHandInToARemovedProjectIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	design := filepath.Join(f.Home, "design.md")
	fixturekit.Must(t, os.WriteFile(design, []byte("design"), 0600))
	f.HandIn(t, api.HandInRequest{Key: "baseline", Path: design})
	_, err := f.C.Abandon(context.Background(), f.Streams(t)[0], "Owner finished checking hand-in refusals")
	fixturekit.Must(t, err)
	_, err = f.C.RemoveProject(context.Background(), f.Project)
	fixturekit.Must(t, err)
	head := f.Head(t)
	apiErr := handInError(t, f.C, api.HandInRequest{Project: f.Project, Key: "k", Path: design})
	if apiErr.Code != api.NotFound || !strings.Contains(apiErr.Message, "project "+string(f.Project)+" is not an active project") {
		t.Fatalf("inactive project: %+v", apiErr)
	}
	if !f.Unchanged(t, head) {
		t.Fatal("an inactive project hand-in wrote to the trace")
	}
}

func TestHandInStatusCodes(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	body := `{"project":"` + string(f.Project) + `","key":"k","stdin":"text"}`
	if code := handInStatus(t, f.S, body); code != http.StatusOK {
		t.Fatalf("hand-in status %d", code)
	}
	if code := handInStatus(t, f.S, `{"project":"`+string(f.Project)+`","key":"k","stdin":"other"}`); code != http.StatusConflict {
		t.Fatalf("key conflict status %d", code)
	}
	if code := handInStatus(t, f.S, `{"project":"`+string(f.Project)+`","key":"k","paths":["/tmp/a"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field status %d", code)
	}
	if code := handInStatus(t, f.S, `{"project":"`+string(f.Project)+`","key":"fetch","url":"https://github.com/owner/repo/issues/99"}`); code != http.StatusInternalServerError {
		t.Fatalf("fetch failure status %d", code)
	}
}

func TestHandInCredentialsStayInTheService(t *testing.T) {
	f := journey.NewHandInFixture(t)
	f.HandIn(t, api.HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/12"})
	handInError(t, f.C, api.HandInRequest{Project: f.Project, Key: "missing", URL: "https://github.com/owner/repo/issues/99"})

	fixturekit.Must(t, filepath.WalkDir(f.Trace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), f.Issues.Token) {
			t.Errorf("%s contains the issue client's token", path)
		}
		return err
	}))

	if err := coreadapter.PublicEnvironment(map[string]string{"GITHUB_TOKEN": f.Issues.Token}); err == nil {
		t.Fatal("session environment admits GITHUB_TOKEN")
	}

	t.Setenv("GITHUB_TOKEN", "from-environment")
	s, _ := journey.StartT(t, journey.Fixture(t))
	gh, ok := s.Options.Issues.(issues.GitHub)
	if !ok || gh.Token != "from-environment" || gh.BaseURL != "" || gh.HTTP == nil || gh.HTTP.Timeout == 0 {
		t.Fatalf("default issue client %#v", s.Options.Issues)
	}
}

func TestHandInRefusesAFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	fifo := filepath.Join(f.Home, "design.fifo")
	fixturekit.Must(t, syscall.Mkfifo(fifo, 0600))
	done := make(chan *api.APIError, 1)
	go func() {
		done <- handInError(t, f.C, api.HandInRequest{Project: f.Project, Key: "fifo", Path: fifo})
	}()
	select {
	case apiErr := <-done:
		if apiErr.Code != api.Validation || !strings.Contains(apiErr.Message, fifo+" is not a regular file") {
			t.Fatalf("fifo: %+v", apiErr)
		}
	case <-time.After(time.Minute):
		t.Fatal("hand-in of a FIFO blocked")
	}
	text := "design"
	f.HandIn(t, api.HandInRequest{Key: "after", Stdin: &text})
	if len(f.Streams(t)) != 1 {
		t.Fatal(f.Streams(t))
	}
}

func TestHandInFetchDoesNotHoldOtherHandIns(t *testing.T) {
	t.Parallel()
	f := journey.NewHandInFixture(t)
	url := "https://github.com/owner/repo/issues/12"
	release := make(chan struct{})
	f.Issues.Mu.Lock()
	f.Issues.Block, f.Issues.Started = release, make(chan struct{})
	started := f.Issues.Started
	f.Issues.Mu.Unlock()
	slow := make(chan api.HandInResponse, 1)
	go func() {
		out, err := f.C.HandIn(context.Background(), api.HandInRequest{Project: f.Project, Key: "issue", URL: url})
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

	text := "design"
	f.HandIn(t, api.HandInRequest{Key: "other", Stdin: &text})
	fast := f.HandIn(t, api.HandInRequest{Key: "issue", URL: url})
	close(release)
	select {
	case out := <-slow:

		if out != fast {
			t.Fatalf("held request %+v, retry %+v", out, fast)
		}
	case <-time.After(time.Minute):
		t.Fatal("held hand-in did not finish")
	}
	f.CheckHanded(t, fast, "issue", "issue-12.md", url, "# Issue title\n\nIssue body\n")
	if len(f.Streams(t)) != 2 {
		t.Fatal(f.Streams(t))
	}
}
