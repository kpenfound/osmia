package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
)

const project config.ProjectID = "p_00000000000000000000000000000001"

var node = api.ClientIdentity{ID: "node-1", Name: "laptop"}

type mcpCall struct {
	turn, auth, path, body string
}

// server is a fake Osmia server: the real client registry and file transport
// behind the real client routes, with every answer a test can set.
type server struct {
	t    *testing.T
	http *httptest.Server
	repo string

	mu        sync.Mutex
	registry  *fleet.Registry
	files     *filetransport.Registry
	uploads   string
	down      bool
	results   map[string][]api.TurnResult
	resultAns map[string][]api.ViewAnswer
	viewAns   map[string][]api.ViewAnswer
	viewTries map[string]int
	uploaded  map[string]string
	reports   []api.CapabilityReport
	gitBodies []string
	mcp       []mcpCall
	beats     int
	connects  int
	drains    int
	leaves    int
}

func newServer(t *testing.T, repo string, options fleet.Options) *server {
	t.Helper()
	s := &server{t: t, repo: repo, results: map[string][]api.TurnResult{}, resultAns: map[string][]api.ViewAnswer{}, viewAns: map[string][]api.ViewAnswer{},
		viewTries: map[string]int{}, uploaded: map[string]string{}}
	root := t.TempDir()
	s.uploads = filepath.Join(root, "uploads")
	s.files = filetransport.NewRegistry(filepath.Join(root, "staging"))
	s.restart(options)
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.http.Close)
	return s
}

// restart replaces the registry, as a server restart does: every lease is gone.
func (s *server) restart(options fleet.Options) *fleet.Registry {
	reg := fleet.NewRegistry(options)
	reg.SetHooks(s.hooks())
	s.mu.Lock()
	s.registry = reg
	s.mu.Unlock()
	return reg
}

func (s *server) hooks() fleet.Hooks { return fleet.Hooks{Result: s.takeResult} }

func (s *server) reg() *fleet.Registry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registry
}

func (s *server) takeResult(_ context.Context, _ fleet.Client, turn string, res api.TurnResult) (api.ViewAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[turn] = append(s.results[turn], res)
	answers := s.resultAns[turn]
	if len(answers) == 0 {
		return api.ViewAccepted, nil
	}
	s.resultAns[turn] = answers[1:]
	return answers[0], nil
}

// setDown makes the server unreachable, dropping every open connection, or
// reachable again.
func (s *server) setDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
	if down {
		s.http.CloseClientConnections()
	}
}

func (s *server) isDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

// register lets a turn download and upload views with token. Uploads are kept
// under the turn's name.
func (s *server) register(turn, token, download string) {
	s.files.Register(turn, filetransport.Turn{Token: token, Download: download, Upload: func(_ context.Context, turn, staging string) (api.ViewAnswer, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.viewTries[turn]++
		answers := s.viewAns[turn]
		if len(answers) > 0 {
			s.viewAns[turn] = answers[1:]
			if answers[0] == "" {
				return "", io.ErrUnexpectedEOF
			}
		}
		dest := filepath.Join(s.uploads, turn)
		_ = os.RemoveAll(dest)
		if err := os.MkdirAll(s.uploads, 0o700); err != nil {
			return "", err
		}
		if err := os.Rename(staging, dest); err != nil {
			return "", err
		}
		s.uploaded[turn] = dest
		if len(answers) > 0 {
			return answers[0], nil
		}
		return api.ViewAccepted, nil
	}})
}

func (s *server) uploadedView(turn string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, ok := s.uploaded[turn]
	return dir, ok
}

func (s *server) resultsOf(turn string) []api.TurnResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.TurnResult(nil), s.results[turn]...)
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	if apiErr, ok := err.(*api.APIError); ok {
		status := http.StatusConflict
		if apiErr.Code == api.Unavailable {
			status = http.StatusServiceUnavailable
		}
		respond(w, status, api.ErrorResponse{Error: *apiErr})
		return
	}
	respond(w, http.StatusInternalServerError, api.ErrorResponse{Error: api.APIError{Code: api.Internal, Message: err.Error()}})
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	if s.isDown() {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	path := r.URL.Path
	switch {
	case path == api.ClientConnectPath:
		var req api.ClientConnectRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			fail(w, &api.APIError{Code: api.Malformed, Message: "bad request"})
			return
		}
		s.mu.Lock()
		s.connects++
		s.mu.Unlock()
		out, err := s.reg().Connect(r.Context(), node, req)
		answer(w, out, err)
	case strings.HasPrefix(path, api.Prefix+"/clients/"):
		s.clientRoute(w, r)
	case strings.HasPrefix(path, api.Prefix+"/git/"):
		s.git(w, r)
	case strings.HasPrefix(path, api.Prefix+"/turns/"):
		s.turnRoute(w, r)
	default:
		http.NotFound(w, r)
	}
}

func answer(w http.ResponseWriter, out any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

func (s *server) clientRoute(w http.ResponseWriter, r *http.Request) {
	lease, rest, _ := api.ClientRouteParts(r.URL.Path)
	reg := s.reg()
	switch {
	case rest == "heartbeat":
		var req api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.beats++
		s.mu.Unlock()
		out, err := reg.Heartbeat(lease, node, req)
		answer(w, out, err)
	case rest == "capabilities":
		var req api.CapabilityReport
		_ = json.NewDecoder(r.Body).Decode(&req)
		current, err := reg.Report(lease, node, req)
		if err == nil {
			s.mu.Lock()
			s.reports = append(s.reports, req)
			s.mu.Unlock()
		}
		answer(w, api.CapabilityResponse{Current: current}, err)
	case rest == "drain":
		s.mu.Lock()
		s.drains++
		s.mu.Unlock()
		err := reg.Drain(lease, node)
		answer(w, api.DrainResponse{Draining: true}, err)
	case rest == "leave":
		s.mu.Lock()
		s.leaves++
		s.mu.Unlock()
		err := reg.Leave(lease, node)
		answer(w, api.LeaveResponse{Ended: true}, err)
	case rest == "poll":
		var req api.PollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		messages, err := reg.Poll(r.Context(), lease, node, req)
		if messages == nil {
			messages = []api.Message{}
		}
		answer(w, api.PollResponse{Messages: messages}, err)
	case strings.HasPrefix(rest, "turns/"):
		turn, what, _ := strings.Cut(strings.TrimPrefix(rest, "turns/"), "/")
		switch what {
		case "claim":
			var req api.ClaimRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			got, err := reg.Claim(lease, node, turn, req)
			answer(w, api.ClaimResponse{Answer: got}, err)
		case "result":
			var res api.TurnResult
			_ = json.NewDecoder(r.Body).Decode(&res)
			got, err := reg.Result(r.Context(), lease, node, turn, res)
			answer(w, api.TurnResultResponse{Answer: got}, err)
		default:
			http.NotFound(w, r)
		}
	case strings.HasPrefix(rest, "requests/"):
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "requests/"), "/reply")
		var reply api.ServerReply
		_ = json.NewDecoder(r.Body).Decode(&reply)
		got, err := reg.Reply(lease, node, id, reply)
		answer(w, api.ServerReplyResponse{Answered: got}, err)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) git(w http.ResponseWriter, r *http.Request) {
	_, rest, _ := api.GitRoute(r.URL.Path)
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		plain := body
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				plain, _ = io.ReadAll(zr)
			}
		}
		s.mu.Lock()
		s.gitBodies = append(s.gitBodies, string(plain))
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if refusal := filetransport.ServeGit(w, r, s.repo, rest); refusal != nil {
		fail(w, refusal)
	}
}

func (s *server) turnRoute(w http.ResponseWriter, r *http.Request) {
	if turn, ok := api.ViewRoute(r.URL.Path); ok {
		switch r.Method {
		case http.MethodGet:
			if refusal := s.files.Download(w, r, turn); refusal != nil {
				fail(w, refusal)
			}
		case http.MethodPut:
			out, refusal := s.files.Upload(w, r, turn)
			if refusal != nil {
				fail(w, refusal)
				return
			}
			respond(w, http.StatusOK, out)
		}
		return
	}
	if turn, _, ok := api.MCPRoute(r.URL.Path); ok {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.mcp = append(s.mcp, mcpCall{turn: turn, auth: r.Header.Get("Authorization"), path: r.URL.Path, body: string(body)})
		s.mu.Unlock()
		respond(w, http.StatusOK, map[string]string{"relayed": turn})
		return
	}
	http.NotFound(w, r)
}

func (s *server) mcpCalls() []mcpCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcpCall(nil), s.mcp...)
}

func (s *server) counts() (beats, connects, drains int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beats, s.connects, s.drains
}

func (s *server) gitRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gitBodies...)
}

func (s *server) lastReport() (api.CapabilityReport, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) == 0 {
		return api.CapabilityReport{}, 0
	}
	return s.reports[len(s.reports)-1], len(s.reports)
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(data), 0o600))
}

// repository makes a Git repository at dir and commits what it holds, with
// commit returning the new commit.
type repository struct {
	t   *testing.T
	dir string
}

func newRepository(t *testing.T) *repository {
	t.Helper()
	r := &repository{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "-b", "main")
	return r
}

func (r *repository) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repository) commit(message string) string {
	r.git("add", "-A")
	r.git("commit", "--quiet", "-m", message)
	return r.git("rev-parse", "HEAD")
}
