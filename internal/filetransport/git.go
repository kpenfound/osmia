package filetransport

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const maxGitRequest = 256 << 20

var protocolVersion = regexp.MustCompile(`^version=[0-9]+$`)

// ServeGit answers one request of git's smart HTTP protocol for the
// repository at dir, where rest is the path below the project: info/refs or
// git-upload-pack. It serves fetch and clone only, from any object the
// repository holds, and refuses receive-pack. It returns the refusal to send,
// or nil once it has answered.
func ServeGit(w http.ResponseWriter, r *http.Request, dir, rest string) *api.APIError {
	switch rest {
	case "info/refs":
		if r.Method != http.MethodGet {
			return &api.APIError{Code: api.NotFound, Message: "no such git route"}
		}
		switch r.URL.Query().Get("service") {
		case api.GitService:
			return advertise(w, r, dir)
		case "git-receive-pack":
			return readOnly()
		}
		return &api.APIError{Code: api.NotFound, Message: "only the smart git protocol is served"}
	case "git-receive-pack":
		return readOnly()
	case api.GitService:
		if r.Method != http.MethodPost {
			return &api.APIError{Code: api.NotFound, Message: "no such git route"}
		}
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != api.GitUploadPackRequestType {
			return &api.APIError{Code: api.Malformed, Message: "expected Content-Type " + api.GitUploadPackRequestType}
		}
		return exchange(w, r, dir)
	}
	return &api.APIError{Code: api.NotFound, Message: "no such git route"}
}

func readOnly() *api.APIError {
	return &api.APIError{Code: api.Forbidden, Message: "the git route is read-only: fetch and clone only, no push"}
}

func upload(r *http.Request, dir string, advertise bool) *exec.Cmd {
	args := []string{"-c", "uploadpack.allowAnySHA1InWant=true", "upload-pack", "--stateless-rpc"}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	cmd := exec.CommandContext(r.Context(), "git", append(args, dir)...)
	cmd.Env = []string{"GIT_TERMINAL_PROMPT=0"}
	for _, name := range []string{"PATH", "HOME", "XDG_CONFIG_HOME", "TMPDIR", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	if version := r.Header.Get("Git-Protocol"); protocolVersion.MatchString(version) {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+version)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func unavailable(what string) *api.APIError {
	return &api.APIError{Code: api.Unavailable, Message: what}
}

func advertise(w http.ResponseWriter, r *http.Request, dir string) *api.APIError {
	cmd := upload(r, dir, true)
	out, err := cmd.Output()
	if err != nil {
		return unavailable("the project's repository cannot be read")
	}
	w.Header().Set("Content-Type", api.GitAdvertisementType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Header.Get("Git-Protocol") != "version=2" {
		line := "# service=" + api.GitService + "\n"
		fmt.Fprintf(w, "%04x%s0000", len(line)+4, line)
	}
	_, _ = w.Write(out)
	return nil
}

func exchange(w http.ResponseWriter, r *http.Request, dir string) *api.APIError {
	extendDeadlines(w)
	body := io.Reader(http.MaxBytesReader(w, r.Body, maxGitRequest))
	if r.Header.Get("Content-Encoding") == "gzip" {
		zip, err := gzip.NewReader(body)
		if err != nil {
			return &api.APIError{Code: api.Malformed, Message: "the request body is not gzip"}
		}
		defer zip.Close()
		body = zip
	}
	cmd := upload(r, dir, false)
	cmd.Stdin = body
	out, err := cmd.StdoutPipe()
	if err != nil {
		return unavailable("the project's repository cannot be read")
	}
	if err := cmd.Start(); err != nil {
		return unavailable("the project's repository cannot be read")
	}
	reader := bufio.NewReader(out)
	if _, err := reader.Peek(1); err != nil {
		_ = cmd.Wait()
		return unavailable("the project's repository cannot be read")
	}
	w.Header().Set("Content-Type", api.GitUploadPackResultType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, reader)
	_ = cmd.Wait()
	if copyErr != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}
