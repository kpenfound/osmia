package trace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// A legacy trace is a Git repository whose HEAD holds the trace's files:
// JSONL record logs, workflow.json, manifests and the materialized file of
// each document. Migration alone reads one.

const (
	legacyLockFile        = ".git/osmia.lock"
	legacyPublicationFile = ".git/osmia-publication.json"
	legacyReclaimFile     = ".git/osmia-migration-reclaim.json"
)

var legacyObjectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// legacyTrace is an open legacy trace directory.
type legacyTrace struct {
	directory string
	lock      *os.File
}

// hasLegacyTrace reports whether directory holds a legacy trace's .git.
func hasLegacyTrace(directory string) (bool, error) {
	info, err := os.Lstat(filepath.Join(directory, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s/.git: expected a directory", directory)
	}
	return true, nil
}

// openLegacyTrace takes the legacy trace's exclusive process lock.
func openLegacyTrace(directory string) (*legacyTrace, error) {
	name := filepath.Join(directory, legacyLockFile)
	if info, err := os.Lstat(name); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("%s: expected a regular file", name)
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return &legacyTrace{directory: directory, lock: f}, nil
}

func (l *legacyTrace) Close() error {
	if l.lock == nil {
		return nil
	}
	err := errors.Join(syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN), l.lock.Close())
	l.lock = nil
	return err
}

// git runs Git against the legacy trace with no inherited configuration,
// credentials, hooks or signing.
func (l *legacyTrace) git(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + l.directory + "/.git", "--work-tree=" + l.directory, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Dir = l.directory
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	cmd.Stdin = input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("legacy trace git %s: %w", args[0], ctxErr)
		}
		return nil, fmt.Errorf("legacy trace git %s: %w: %s", args[0], err, stderr.Bytes())
	}
	return out, nil
}

// head returns the commit HEAD names.
func (l *legacyTrace) head() (string, error) {
	data, err := os.ReadFile(filepath.Join(l.directory, ".git/refs/heads/main"))
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(data))
	if !legacyObjectID.MatchString(head) {
		return "", fmt.Errorf("invalid legacy trace HEAD %q", head)
	}
	return head, nil
}

// legacyPublication is the legacy trace's publication journal.
type legacyPublication struct {
	Version int      `json:"version"`
	Parent  string   `json:"parent"`
	Commit  string   `json:"commit"`
	Paths   []string `json:"paths"`
	Removed []string `json:"removed,omitempty"`
}

// finishPublication completes an interrupted publication: when its commit
// was published, the work tree is brought up to it; either way the journal
// is removed.
func (l *legacyTrace) finishPublication(ctx context.Context) error {
	name := filepath.Join(l.directory, legacyPublicationFile)
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p legacyPublication
	if err := decode(data, &p); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if p.Version != 1 || !legacyObjectID.MatchString(p.Parent) || !legacyObjectID.MatchString(p.Commit) || len(p.Paths) == 0 {
		return fmt.Errorf("%s: invalid publication journal", name)
	}
	for _, path := range append(slices.Clone(p.Paths), p.Removed...) {
		if err := relative(path); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	head, err := l.head()
	if err != nil {
		return err
	}
	switch head {
	case p.Parent:
	case p.Commit:
		for _, path := range p.Paths {
			blob, err := l.git(ctx, nil, "cat-file", "blob", p.Commit+":"+path)
			if err != nil {
				return err
			}
			if err := writeLegacyFile(filepath.Join(l.directory, filepath.FromSlash(path)), blob); err != nil {
				return err
			}
		}
		for _, path := range p.Removed {
			if err := os.Remove(filepath.Join(l.directory, filepath.FromSlash(path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: the publication's ref differs from both journal identities", name)
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	return syncDirectory(filepath.Join(l.directory, ".git"))
}

// writeLegacyFile replaces the work tree file name with data.
func writeLegacyFile(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".trace-"+rand.Text()+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(name))
}

// tree returns the blob of every file HEAD holds, by path.
func (l *legacyTrace) tree(ctx context.Context, head string) (map[string]string, error) {
	out, err := l.git(ctx, nil, "ls-tree", "-rz", head)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !legacyObjectID.MatchString(fields[2]) {
			return nil, fmt.Errorf("invalid legacy trace tree entry %q", entry)
		}
		if err := relative(name); err != nil {
			return nil, err
		}
		blobs[name] = fields[2]
	}
	return blobs, nil
}

// blobs reads every blob of tree, by path.
func (l *legacyTrace) blobs(ctx context.Context, tree map[string]string) (map[string][]byte, error) {
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range tree {
			if !yield(name) {
				return
			}
		}
	})
	var input bytes.Buffer
	for _, name := range names {
		input.WriteString(tree[name] + "\n")
	}
	out, err := l.git(ctx, &input, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(out))
	files := make(map[string][]byte, len(names))
	for _, name := range names {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("legacy trace blob of %s: %w", name, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != tree[name] || fields[1] != "blob" {
			return nil, fmt.Errorf("legacy trace blob of %s: unexpected %q", name, strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("legacy trace blob of %s: %w", name, err)
		}
		if _, err := reader.Discard(1); err != nil {
			return nil, err
		}
		files[name] = data
	}
	return files, nil
}

// workTree reads every file of the work tree outside .git, by path.
func (l *legacyTrace) workTree() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(l.directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(l.directory, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if rel == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			files[rel] = nil
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	return files, err
}

// walkOrder sorts paths in the order a lexical walk of the work tree visits
// them: component by component.
func walkOrder(paths []string) {
	slices.SortFunc(paths, func(a, b string) int {
		return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
	})
}

// legacyRecordFile reports whether a legacy trace file is a JSONL record
// log. Handed documents keep their original name, whatever it is.
func legacyRecordFile(name string) bool {
	parts := strings.Split(name, "/")
	return strings.HasSuffix(name, ".jsonl") && !(len(parts) == 4 && parts[0] == "workstreams" && parts[2] == "handed")
}

// legacyLines splits a JSONL log into its lines, without their newlines.
func legacyLines(data []byte) [][]byte {
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// legacyIdentity is the identity a record line names.
type legacyIdentity struct {
	Schema   string `json:"schema"`
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

func identityOf(line []byte) (legacyIdentity, error) {
	var id legacyIdentity
	if err := json.Unmarshal(line, &id); err != nil {
		return id, err
	}
	return id, nil
}

// legacyFile reports whether a legacy trace file is kept as a file of the
// trace database.
func legacyFile(name string) bool {
	parts := strings.Split(name, "/")
	switch {
	case name == "project.json" || name == inboxCounterFile:
		return true
	case len(parts) == 2 && parts[0] == "notes" && strings.HasSuffix(parts[1], ".md"):
		return true
	case len(parts) == 3 && parts[0] == "workstreams" && parts[2] == "workstream.json":
		return true
	}
	return false
}

// workstreamPrefix is the path prefix of stream's files in its trace.
func workstreamPrefix(stream string) string { return path.Join("workstreams", stream) + "/" }
