package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/tailnet"
)

type transferRig struct {
	s       *Service
	base    string
	home    string
	clone   string
	started string
	hidden  string
}

func gitIn(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)
	return fixturekit.Git(t, home, full...)
}

func gitClient(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "NO_PROXY=*", "no_proxy=*"}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newTransferRig(t *testing.T) *transferRig {
	t.Helper()
	return newTransferRigWith(t, nil)
}

// newTransferRigWith is newTransferRig with the service's options adjusted
// before it starts.
func newTransferRigWith(t *testing.T, adjust func(*Options)) *transferRig {
	t.Helper()
	opts := fixture(t)
	home := filepath.Dir(opts.Config.Root)
	clone := filepath.Join(home, "clone")
	fixturekit.Must(t, os.MkdirAll(clone, 0o700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "-b", "main", clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("first\n"), 0o600))
	gitIn(t, home, clone, "add", ".")
	gitIn(t, home, clone, "commit", "--quiet", "-m", "first")
	started := gitIn(t, home, clone, "rev-parse", "HEAD")
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "turn.txt"), []byte("the turn's starting point\n"), 0o600))
	gitIn(t, home, clone, "add", ".")
	gitIn(t, home, clone, "commit", "--quiet", "-m", "starting commit")
	hidden := gitIn(t, home, clone, "rev-parse", "HEAD")
	gitIn(t, home, clone, "reset", "--quiet", "--hard", started)
	if refs := gitIn(t, home, clone, "for-each-ref", "--contains", hidden); refs != "" {
		t.Fatalf("a ref points at the starting commit: %s", refs)
	}
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
	withTailnet(t, &opts, "osmia-1")
	if adjust != nil {
		adjust(&opts)
	}
	s, _ := start(t, opts)
	return &transferRig{s: s, base: "http://" + s.TailnetAddr(), home: home, clone: clone, started: started, hidden: hidden}
}

func (r *transferRig) refs(t *testing.T) string {
	t.Helper()
	return gitIn(t, r.home, r.clone, "for-each-ref", "--format=%(refname) %(objectname)")
}

func (r *transferRig) project() string { return api.GitPath(fixturekit.Project) }

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) code(t *testing.T) api.Code {
	t.Helper()
	return intakefixture.ErrorCode(t, r.body)
}

func send(t *testing.T, method, url, host string, header map[string]string, body []byte) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	fixturekit.Must(t, err)
	if host != "" {
		req.Host = host
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	resp, err := intakefixture.WebHTTP().Do(req)
	fixturekit.Must(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	fixturekit.Must(t, err)
	return reply{resp.StatusCode, resp.Header, data}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// A git client clones a registered project and fetches a commit that no
// branch points at through the tailnet listener, over both protocol versions.
func TestGitRouteClonesAndFetchesACommitNoBranchPointsAt(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	url := rig.base + rig.project()
	work := t.TempDir()

	out, err := gitClient(t, work, "clone", "--quiet", url, "cloned")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	cloned := filepath.Join(work, "cloned")
	if head, _ := gitClient(t, cloned, "rev-parse", "HEAD"); head != rig.started {
		t.Fatalf("cloned %s, want %s", head, rig.started)
	}

	for _, version := range []string{"2", "0"} {
		client := filepath.Join(work, "cache-v"+version)
		if out, err := gitClient(t, work, "init", "--quiet", client); err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
		if out, err := gitClient(t, client, "cat-file", "-e", rig.hidden); err == nil {
			t.Fatalf("protocol %s: the cache already holds the starting commit: %s", version, out)
		}
		if out, err := gitClient(t, client, "-c", "protocol.version="+version, "fetch", "--quiet", url, rig.hidden); err != nil {
			t.Fatalf("protocol %s: fetch of the starting commit: %v\n%s", version, err, out)
		}
		if out, err := gitClient(t, client, "cat-file", "-t", rig.hidden); err != nil || out != "commit" {
			t.Fatalf("protocol %s: %q %v", version, out, err)
		}
		if out, err := gitClient(t, client, "show", rig.hidden+":turn.txt"); err != nil || out != "the turn's starting point" {
			t.Fatalf("protocol %s: %q %v", version, out, err)
		}
	}

	if out, err := gitClient(t, cloned, "fetch", "--quiet", url, rig.hidden); err != nil {
		t.Fatalf("fetch into the clone: %v\n%s", err, out)
	}
	if out, err := gitClient(t, cloned, "cat-file", "-t", rig.hidden); err != nil || out != "commit" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestGitRouteRefusesPushAndLeavesTheCloneAlone(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	url := rig.base + rig.project()
	before := rig.refs(t)
	work := t.TempDir()
	if out, err := gitClient(t, work, "clone", "--quiet", url, "cloned"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	cloned := filepath.Join(work, "cloned")
	fixturekit.Must(t, os.WriteFile(filepath.Join(cloned, "pushed.txt"), []byte("x\n"), 0o600))
	for _, args := range [][]string{{"add", "."}, {"commit", "--quiet", "-m", "pushed"}} {
		if out, err := gitClient(t, cloned, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	pushed, _ := gitClient(t, cloned, "rev-parse", "HEAD")
	for _, target := range []string{"HEAD:refs/heads/pushed", "HEAD:refs/heads/main", "--force HEAD:refs/heads/main", "--delete main"} {
		if out, err := gitClient(t, cloned, append([]string{"push", url}, strings.Fields(target)...)...); err == nil {
			t.Fatalf("push %s succeeded: %s", target, out)
		}
	}
	if after := rig.refs(t); after != before {
		t.Fatalf("the clone's refs changed:\n%s\nafter\n%s", before, after)
	}
	if out, err := exec.Command("git", "-C", rig.clone, "cat-file", "-e", pushed).CombinedOutput(); err == nil {
		t.Fatalf("the pushed commit reached the clone: %s", out)
	}

	advert := send(t, "GET", url+"/info/refs?service=git-receive-pack", "", nil, nil)
	if advert.status != 403 || advert.code(t) != Forbidden {
		t.Fatalf("receive-pack advertisement: %d %s", advert.status, advert.body)
	}
	for _, header := range []map[string]string{nil, {"Content-Type": "application/json"}, {"Content-Type": "application/x-git-receive-pack-request"}} {
		post := send(t, "POST", url+"/git-receive-pack", "", header, []byte("0000"))
		if post.status != 403 {
			t.Fatalf("receive-pack post with %v: %d %s", header, post.status, post.body)
		}
	}
	if after := rig.refs(t); after != before {
		t.Fatalf("the clone's refs changed:\n%s\nafter\n%s", before, after)
	}
}

func TestGitRouteServesOnlyRegisteredProjectsClones(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	other := api.GitPath("p_ffffffffffffffffffffffffffffffff")
	fixturekit.Must(t, os.WriteFile(filepath.Join(rig.home, "root", "secret.txt"), []byte("kept\n"), 0o600))
	for _, path := range []string{
		other + "/info/refs?service=git-upload-pack",
		api.GitPath("not-a-project") + "/info/refs?service=git-upload-pack",
		api.GitPath("..") + "/info/refs?service=git-upload-pack",
		rig.project() + "/../info/refs?service=git-upload-pack",
		rig.project() + "/../../root/config.toml",
		rig.project() + "/info/../../root/secret.txt",
		rig.project() + "/HEAD",
		rig.project() + "/config",
		rig.project() + "/.git/config",
		rig.project() + "/objects/info/packs",
		rig.project() + "/info/refs",
		rig.project() + "/info/refs?service=other",
		rig.project() + "/",
		rig.project(),
		api.Prefix + "/git/",
	} {
		got := send(t, "GET", rig.base+path, "", nil, nil)
		if got.status != 404 || strings.Contains(string(got.body), "kept") {
			t.Errorf("GET %s: %d %s", path, got.status, got.body)
		}
	}
	post := send(t, "POST", rig.base+other+"/git-upload-pack", "", map[string]string{"Content-Type": api.GitUploadPackRequestType}, []byte("0000"))
	if post.status != 404 {
		t.Errorf("upload-pack for an unregistered project: %d %s", post.status, post.body)
	}
	if out, err := gitClient(t, t.TempDir(), "clone", "--quiet", rig.base+other, "x"); err == nil {
		t.Fatalf("cloned an unregistered project: %s", out)
	}
}

func TestOnlyTheTailnetListenerServesTheFileRoutes(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "token", Download: t.TempDir()})
	for _, path := range []string{rig.project() + "/info/refs?service=git-upload-pack", api.ViewPath("turn-1")} {
		for _, ctx := range []context.Context{context.Background(), tailnet.WithKind(context.Background(), tailnet.WebListener)} {
			req := httptest.NewRequestWithContext(ctx, "GET", path, nil)
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			rig.s.handle(rec, req)
			if rec.Code != 404 {
				t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
			}
		}
	}
}

func archiveOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		fixturekit.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		fixturekit.Must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	var buf bytes.Buffer
	fixturekit.Must(t, filetransport.Pack(&buf, dir))
	return buf.Bytes()
}

func TestTailnetAcceptsTheTwoRoutesOwnContentTypesAndNothingElse(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "token", Upload: func(context.Context, string, string) (api.ViewAnswer, error) {
		return api.ViewAccepted, nil
	}})
	empty := archiveOf(t, map[string]string{})
	upload := map[string]string{"Content-Type": api.ViewArchiveType, "Authorization": "Bearer token"}
	fetch := map[string]string{"Content-Type": api.GitUploadPackRequestType}
	pause := []byte(`{"target":{"scope":"factory"},"mode":"soft","reason":"travel","source":"owner"}`)

	if got := send(t, "PUT", rig.base+api.ViewPath("turn-1"), "osmia", upload, empty); got.status != 200 {
		t.Fatalf("upload: %d %s", got.status, got.body)
	}
	if got := send(t, "POST", rig.base+rig.project()+"/git-upload-pack", "osmia", fetch, []byte("0000")); got.status == 403 && got.code(t) == Forbidden {
		t.Fatalf("git fetch request: %d %s", got.status, got.body)
	}

	for _, c := range []struct {
		name, method, path, contentType string
	}{
		{"the git type to the pause route", "PUT", api.Prefix + "/runtime/pause", api.GitUploadPackRequestType},
		{"the git type to a POST route", "POST", api.Prefix + "/reload", api.GitUploadPackRequestType},
		{"the view type to the pause route", "PUT", api.Prefix + "/runtime/pause", api.ViewArchiveType},
		{"the view type to a POST route", "POST", api.Prefix + "/reload", api.ViewArchiveType},
		{"the view type to the git fetch route", "POST", rig.project() + "/git-upload-pack", api.ViewArchiveType},
		{"the git type to the upload route", "PUT", api.ViewPath("turn-1"), api.GitUploadPackRequestType},
		{"the view type posted to the upload route", "POST", api.ViewPath("turn-1"), api.ViewArchiveType},
		{"the git type put to the fetch route", "PUT", rig.project() + "/git-upload-pack", api.GitUploadPackRequestType},
		{"the git type to the receive-pack route", "POST", rig.project() + "/git-receive-pack", api.GitUploadPackRequestType},
		{"receive-pack's own type", "POST", rig.project() + "/git-receive-pack", "application/x-git-receive-pack-request"},
		{"the git type to the advertisement", "POST", rig.project() + "/info/refs", api.GitUploadPackRequestType},
		{"the view type below the route", "PUT", api.ViewPath("turn-1") + "/more", api.ViewArchiveType},
		{"no type to the upload route", "PUT", api.ViewPath("turn-1"), ""},
		{"a form type to the upload route", "PUT", api.ViewPath("turn-1"), "application/x-www-form-urlencoded"},
		{"a text type to the fetch route", "POST", rig.project() + "/git-upload-pack", "text/plain"},
		{"a multipart type to the fetch route", "POST", rig.project() + "/git-upload-pack", "multipart/form-data; boundary=x"},
		{"no type to the fetch route", "POST", rig.project() + "/git-upload-pack", ""},
	} {
		header := bearer("token")
		if c.contentType != "" {
			header["Content-Type"] = c.contentType
		}
		got := send(t, c.method, rig.base+c.path, "osmia", header, pause)
		if got.status != 403 || got.code(t) != Forbidden {
			t.Errorf("%s: %d %s", c.name, got.status, got.body)
		}
	}
	c := NewClient(rig.s.Socket())
	defer c.Close()
	state, err := c.Runtime(context.Background())
	fixturekit.Must(t, err)
	if len(state.Effective.Pauses) != 0 {
		t.Fatalf("a refused request changed the runtime: %+v", state.Effective)
	}

	for _, route := range []struct {
		name, method, path string
		header             map[string]string
	}{
		{"fetch", "POST", rig.project() + "/git-upload-pack", fetch},
		{"advertisement", "GET", rig.project() + "/info/refs?service=git-upload-pack", nil},
		{"upload", "PUT", api.ViewPath("turn-1"), upload},
		{"download", "GET", api.ViewPath("turn-1"), bearer("token")},
	} {
		for _, host := range []string{"attacker.example", "osmia.attacker.example", "localhost", "osmia-2", "tail0.ts.net"} {
			got := send(t, route.method, rig.base+route.path, host, route.header, nil)
			if got.status != 403 || got.code(t) != Forbidden {
				t.Errorf("%s with Host %s: %d %s", route.name, host, got.status, got.body)
			}
		}
	}
}

func TestTailnetGrantsNoPreflightToTheFileRoutes(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	for _, path := range []string{rig.project() + "/git-upload-pack", rig.project() + "/info/refs", api.ViewPath("turn-1")} {
		for _, host := range []string{"osmia", "osmia-1", "127.0.0.1", "attacker.example"} {
			got := send(t, "OPTIONS", rig.base+path, host, map[string]string{
				"Origin":                         "https://attacker.example",
				"Access-Control-Request-Method":  "POST",
				"Access-Control-Request-Headers": "content-type",
			}, nil)
			if got.status != 403 {
				t.Errorf("OPTIONS %s with Host %s: %d", path, host, got.status)
			}
			for name := range got.header {
				if strings.HasPrefix(strings.ToLower(name), "access-control-allow") {
					t.Errorf("OPTIONS %s with Host %s granted %s", path, host, name)
				}
			}
		}
	}
}

func TestViewArchiveDownloadNeedsTheTurnsToken(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	view := t.TempDir()
	fixturekit.Must(t, os.MkdirAll(filepath.Join(view, "docs", "empty"), 0o755))
	fixturekit.Must(t, os.WriteFile(filepath.Join(view, "docs", "spec.md"), []byte("the spec\n"), 0o640))
	rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "token-1", Download: view})
	rig.s.Transfers().Register("turn-2", filetransport.Turn{Token: "token-2", Download: view})
	rig.s.Transfers().Register("turn-3", filetransport.Turn{Token: "token-3"})

	got := send(t, "GET", rig.base+api.ViewPath("turn-1"), "", bearer("token-1"), nil)
	if got.status != 200 || got.header.Get("Content-Type") != api.ViewArchiveType {
		t.Fatalf("download: %d %s %s", got.status, got.header.Get("Content-Type"), got.body)
	}
	dest := t.TempDir()
	fixturekit.Must(t, filetransport.Unpack(bytes.NewReader(got.body), dest))
	if data, err := os.ReadFile(filepath.Join(dest, "docs", "spec.md")); err != nil || string(data) != "the spec\n" {
		t.Fatalf("spec: %q %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(dest, "docs", "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory: %v", err)
	}

	for name, header := range map[string]map[string]string{
		"no token":          nil,
		"another turn":      bearer("token-2"),
		"a wrong token":     bearer("nope"),
		"an empty token":    bearer(""),
		"another scheme":    {"Authorization": "Basic token-1"},
		"an unlabelled one": {"Authorization": "token-1"},
	} {
		if got := send(t, "GET", rig.base+api.ViewPath("turn-1"), "", header, nil); got.status != 403 || got.code(t) != Forbidden {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	if got := send(t, "GET", rig.base+api.ViewPath("turn-9"), "", bearer("token-1"), nil); got.status != 403 {
		t.Errorf("an unregistered turn: %d %s", got.status, got.body)
	}
	if got := send(t, "GET", rig.base+api.ViewPath("turn-3"), "", bearer("token-3"), nil); got.status != 404 {
		t.Errorf("a turn without a download: %d %s", got.status, got.body)
	}
}

func TestViewArchiveUploadHandsTheSinkAnUnpackedStagingDirectory(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	var mu sync.Mutex
	var staged []string
	var seen map[string]string
	answer, sinkErr := api.ViewAccepted, error(nil)
	rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "token-1", Upload: func(_ context.Context, turn, staging string) (api.ViewAnswer, error) {
		mu.Lock()
		defer mu.Unlock()
		if sinkErr != nil {
			return "", sinkErr
		}
		staged = append(staged, staging)
		seen = map[string]string{}
		err := filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(p)
			rel, _ := filepath.Rel(staging, p)
			seen[rel] = string(data)
			return err
		})
		if err != nil {
			t.Error(err)
		}
		return answer, nil
	}})
	rig.s.Transfers().Register("turn-2", filetransport.Turn{Token: "token-2", Upload: func(context.Context, string, string) (api.ViewAnswer, error) {
		t.Error("another turn's sink was called")
		return api.ViewAccepted, nil
	}})
	configure := func(a api.ViewAnswer, err error) {
		mu.Lock()
		defer mu.Unlock()
		answer, sinkErr = a, err
	}
	upload := func(token string, body []byte) reply {
		return send(t, "PUT", rig.base+api.ViewPath("turn-1"), "", map[string]string{"Content-Type": api.ViewArchiveType, "Authorization": "Bearer " + token}, body)
	}
	finished := archiveOf(t, map[string]string{"a.txt": "a", "dir/b.txt": "b"})

	got := upload("token-1", finished)
	var out api.ViewUploadResponse
	fixturekit.Must(t, json.Unmarshal(got.body, &out))
	if got.status != 200 || out.Answer != api.ViewAccepted {
		t.Fatalf("upload: %d %s", got.status, got.body)
	}
	mu.Lock()
	directories, files := slices.Clone(staged), maps.Clone(seen)
	mu.Unlock()
	if len(directories) != 1 || files["a.txt"] != "a" || files["dir/b.txt"] != "b" || len(files) != 2 {
		t.Fatalf("the sink saw %v in %v", files, directories)
	}
	if _, err := os.Stat(directories[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staging directory outlived the sink: %v", err)
	}
	if rel, err := filepath.Rel(filepath.Join(rig.home, "root"), directories[0]); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("staging %s is outside the root", directories[0])
	}

	for _, a := range []api.ViewAnswer{api.ViewAlreadyRecorded, api.ViewSuperseded} {
		configure(a, nil)
		got := upload("token-1", finished)
		out = api.ViewUploadResponse{}
		fixturekit.Must(t, json.Unmarshal(got.body, &out))
		if got.status != 200 || out.Answer != a {
			t.Fatalf("%s: %d %s", a, got.status, got.body)
		}
	}

	if got := upload("token-2", finished); got.status != 403 {
		t.Fatalf("another turn's token: %d %s", got.status, got.body)
	}
	if got := upload("", finished); got.status != 403 {
		t.Fatalf("no token: %d %s", got.status, got.body)
	}
	if got := send(t, "PUT", rig.base+api.ViewPath("turn-1"), "", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer token-1"}, finished); got.status != 400 {
		t.Fatalf("a JSON content type: %d %s", got.status, got.body)
	}
	if got := upload("token-1", []byte("not an archive")); got.status != 400 {
		t.Fatalf("garbage: %d %s", got.status, got.body)
	}
	configure(api.ViewAccepted, errors.New("the trace is unavailable"))
	if got := upload("token-1", finished); got.status != 503 {
		t.Fatalf("a failing sink: %d %s", got.status, got.body)
	}
}

func tarEntry(t *testing.T, w *tar.Writer, h tar.Header, body string) {
	t.Helper()
	h.Mode, h.Size = 0o644, int64(len(body))
	fixturekit.Must(t, w.WriteHeader(&h))
	if body != "" {
		_, err := w.Write([]byte(body))
		fixturekit.Must(t, err)
	}
}

func TestViewArchiveUploadRefusesAnEntryThatEscapesAndNeverCallsTheSink(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "token", Upload: func(context.Context, string, string) (api.ViewAnswer, error) {
		t.Error("the sink took a refused archive")
		return api.ViewAccepted, nil
	}})
	transfers := filepath.Join(rig.home, "root", "transfers")
	for name, write := range map[string]func(*tar.Writer){
		"dot-dot":  func(w *tar.Writer) { tarEntry(t, w, tar.Header{Name: "../escaped.txt", Typeflag: tar.TypeReg}, "x") },
		"absolute": func(w *tar.Writer) { tarEntry(t, w, tar.Header{Name: "/tmp/escaped.txt", Typeflag: tar.TypeReg}, "x") },
		"symlink": func(w *tar.Writer) {
			tarEntry(t, w, tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../.."}, "")
			tarEntry(t, w, tar.Header{Name: "link/escaped.txt", Typeflag: tar.TypeReg}, "x")
		},
	} {
		var buf bytes.Buffer
		zip := gzip.NewWriter(&buf)
		w := tar.NewWriter(zip)
		write(w)
		fixturekit.Must(t, w.Close())
		fixturekit.Must(t, zip.Close())
		got := send(t, "PUT", rig.base+api.ViewPath("turn-1"), "", map[string]string{"Content-Type": api.ViewArchiveType, "Authorization": "Bearer token"}, buf.Bytes())
		if got.status != 400 || got.code(t) != Malformed {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
		if left, _ := os.ReadDir(transfers); len(left) != 0 {
			t.Errorf("%s: staging left %v", name, left)
		}
		for _, escaped := range []string{filepath.Join(rig.home, "root", "escaped.txt"), filepath.Join(rig.home, "escaped.txt"), filepath.Join(transfers, "escaped.txt"), "/tmp/escaped.txt"} {
			if _, err := os.Lstat(escaped); err == nil {
				t.Errorf("%s: %s was written", name, escaped)
			}
		}
	}
}

func TestRegisteredTurnEndsWithItsRelease(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	view := t.TempDir()
	release := rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "old", Download: view})
	newer := rig.s.Transfers().Register("turn-1", filetransport.Turn{Token: "new", Download: view})
	release()
	if got := send(t, "GET", rig.base+api.ViewPath("turn-1"), "", bearer("new"), nil); got.status != 200 {
		t.Fatalf("after the replaced registration was released: %d %s", got.status, got.body)
	}
	if got := send(t, "GET", rig.base+api.ViewPath("turn-1"), "", bearer("old"), nil); got.status != 403 {
		t.Fatalf("the replaced token: %d", got.status)
	}
	newer()
	if got := send(t, "GET", rig.base+api.ViewPath("turn-1"), "", bearer("new"), nil); got.status != 403 {
		t.Fatalf("after release: %d", got.status)
	}
}
