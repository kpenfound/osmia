package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/isolation"
)

var commitName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// clones keeps one cached clone of each project under dir. A clone only ever
// fetches from the server's read-only git route: the client never pushes to it,
// commits to it or writes any other git store.
type clones struct {
	dir  string
	base string

	mu    sync.Mutex
	locks map[config.ProjectID]*sync.Mutex
}

func newClones(dir, base string) *clones {
	return &clones{dir: dir, base: base, locks: map[config.ProjectID]*sync.Mutex{}}
}

func (c *clones) lock(project config.ProjectID) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[project] == nil {
		c.locks[project] = new(sync.Mutex)
	}
	return c.locks[project]
}

func git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// fetch makes sure the project's cached clone holds commit and returns its git
// directory. The first turn of a project creates the clone and fetches into
// it; later turns fetch only what the clone lacks.
func (c *clones) fetch(ctx context.Context, project config.ProjectID, commit string) (string, error) {
	if !commitName.MatchString(commit) {
		return "", fmt.Errorf("%q is not a commit", commit)
	}
	lock := c.lock(project)
	lock.Lock()
	defer lock.Unlock()
	gitDir := filepath.Join(c.dir, string(project)+".git")
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			return "", err
		}
		if _, err := git(ctx, nil, "init", "--bare", "--quiet", gitDir); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, nil, "--git-dir", gitDir, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return gitDir, nil
	}
	url := c.base + api.GitPath(project)
	if _, err := git(ctx, nil, "--git-dir", gitDir, "-c", "credential.helper=", "-c", "gc.auto=0", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url, commit+":refs/osmia/"+commit); err != nil {
		return "", fmt.Errorf("fetch commit %s of project %s: %w", commit, project, err)
	}
	return gitDir, nil
}

type treeEntry struct{ mode, name, object string }

func listTree(ctx context.Context, gitDir, commit string, paths []string) ([]treeEntry, error) {
	args := append([]string{"--git-dir", gitDir, "ls-tree", "-r", "-z", "--full-tree", commit, "--"}, paths...)
	out, err := git(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for record := range strings.SplitSeq(string(out), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unexpected git ls-tree record %q", record)
		}
		entries = append(entries, treeEntry{mode: fields[0], name: name, object: fields[2]})
	}
	return entries, nil
}

// export writes the files of the selected paths at commit into stage, as a
// checkout of the commit would hold them. Like the service's own view, it
// holds no symlink below a selected directory, refuses a selected symlink and
// names a missing path an error.
func (c *clones) export(ctx context.Context, gitDir, commit string, paths []string, stage string) error {
	for _, p := range paths {
		if err := isolation.CheckPath(p); err != nil {
			return fmt.Errorf("selected path %q: %w", p, err)
		}
	}
	entries, err := listTree(ctx, gitDir, commit, paths)
	if err != nil {
		return err
	}
	for _, p := range paths {
		found := false
		for _, e := range entries {
			if e.name == p {
				if e.mode == "120000" {
					return fmt.Errorf("selected path %q: symlinks and special files are not exposed", p)
				}
				found = true
			} else if strings.HasPrefix(e.name, p+"/") {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("selected path %q is not in commit %s", p, commit)
		}
	}
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	cat := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "cat-file", "--batch")
	cat.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	in, err := cat.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cat.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cat.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(out)
	err = func() error {
		for _, e := range entries {
			switch e.mode {
			case "100644", "100755":
				if err := root.MkdirAll(filepath.Dir(e.name), 0o700); err != nil {
					return err
				}
				perm := fs.FileMode(0o600)
				if e.mode == "100755" {
					perm = 0o700
				}
				file, err := root.OpenFile(e.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
				if err != nil {
					return err
				}
				err = errors.Join(blob(in, reader, e.object, file), file.Close())
				if err != nil {
					return fmt.Errorf("read %s from commit %s: %w", e.name, commit, err)
				}
			case "160000":
				if err := root.MkdirAll(e.name, 0o700); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	_ = in.Close()
	return errors.Join(err, cat.Wait())
}

func blob(in io.Writer, out *bufio.Reader, object string, dst io.Writer) error {
	if _, err := fmt.Fprintf(in, "%s\n", object); err != nil {
		return err
	}
	header, err := out.ReadString('\n')
	if err != nil {
		return err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[1] != "blob" {
		return fmt.Errorf("git has no blob %s", object)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(dst, out, size); err != nil {
		return err
	}
	_, err = out.ReadByte()
	return err
}

// buildView makes the turn's view under views: from the cached clone at the
// turn's starting commit when the offer names one, otherwise from the archive
// the server holds for the turn. The view holds what the service builds for a
// local turn: the selected files with no .git, and the empty Git repository
// Dagger needs where the turn runs Dagger.
func (c *Client) buildView(ctx context.Context, offer api.TurnOffer, token, views string) (*isolation.FileView, error) {
	access := coreadapter.ReadOnly
	if offer.Capabilities.WriteFiles {
		access = coreadapter.ReadWrite
	}
	if err := os.MkdirAll(views, 0o700); err != nil {
		return nil, err
	}
	var view *isolation.FileView
	switch offer.View.Kind {
	case api.ViewGit:
		project, err := config.ParseProjectID(offer.View.Project)
		if err != nil {
			return nil, err
		}
		gitDir, err := c.clones.fetch(ctx, project, offer.View.Commit)
		if err != nil {
			return nil, err
		}
		stage, err := os.MkdirTemp(filepath.Join(c.dir, "stage"), "turn-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		if len(offer.View.Paths) != 0 {
			if err := c.clones.export(ctx, gitDir, offer.View.Commit, offer.View.Paths, stage); err != nil {
				return nil, err
			}
		}
		view, err = (isolation.Views{Directory: views}).Create(ctx, coreadapter.Workspace{Directory: stage, Revision: offer.View.Commit, Access: access}, offer.View.Paths)
		if err != nil {
			return nil, err
		}
	case api.ViewArchive:
		dir, err := os.MkdirTemp(views, "turn-")
		if err != nil {
			return nil, err
		}
		if err := c.download(ctx, offer.Turn, token, dir); err != nil {
			return nil, errors.Join(err, os.RemoveAll(dir))
		}
		if view, err = isolation.AdoptView(dir, access); err != nil {
			return nil, errors.Join(err, os.RemoveAll(dir))
		}
	default:
		return nil, fmt.Errorf("the turn's view is %q, which this client does not know", offer.View.Kind)
	}
	if offer.RunsDagger {
		dir := view.Workspace().Directory
		if _, err := os.Lstat(filepath.Join(dir, ".git")); errors.Is(err, fs.ErrNotExist) {
			if err := isolation.InitRepository(ctx, dir); err != nil {
				return nil, errors.Join(err, view.Release(context.WithoutCancel(ctx)))
			}
		}
	}
	return view, nil
}

func (c *Client) download(ctx context.Context, turn, token, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+api.ViewPath(turn), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readRefusal(resp)
	}
	c.touch()
	return filetransport.Unpack(resp.Body, dest)
}

// upload sends the finished view in dir to the server and returns its answer.
func (c *Client) upload(ctx context.Context, turn, token, dir string) (api.ViewAnswer, error) {
	pipeR, pipeW := io.Pipe()
	go func() { pipeW.CloseWithError(filetransport.Pack(pipeW, dir)) }()
	defer pipeR.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+api.ViewPath(turn), pipeR)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", api.ViewArchiveType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", readRefusal(resp)
	}
	c.touch()
	var out api.ViewUploadResponse
	if err := decodeJSON(resp.Body, &out); err != nil {
		return "", err
	}
	return out.Answer, nil
}
