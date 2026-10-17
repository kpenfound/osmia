// Package legacytrace builds Git traces in the layout trace.Migrate moves to
// trace databases, for tests of that migration.
package legacytrace

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Config is a Git trace's isolated .git/config.
const Config = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n"

// Build creates a Git trace in directory, which may already exist, whose
// HEAD holds files, by slash-separated path, in one commit.
func Build(directory string, files map[string][]byte) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
		return fmt.Errorf("%s already holds a Git trace", directory)
	}
	if _, err := git(directory, "init", "--quiet", "--initial-branch=main"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, ".git", "config"), []byte(Config), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, ".git", "osmia.lock"), nil, 0600); err != nil {
		return err
	}
	_, err := Commit(directory, files, "Create trace")
	return err
}

// Commit writes files into the work tree of the Git trace in directory and
// commits them on top of HEAD. It returns the new commit.
func Commit(directory string, files map[string][]byte, message string) (string, error) {
	if err := Write(directory, files); err != nil {
		return "", err
	}
	names := slices.Sorted(maps.Keys(files))
	if len(names) > 0 {
		if _, err := git(directory, append([]string{"add", "--force", "--"}, names...)...); err != nil {
			return "", err
		}
	}
	if _, err := git(directory, "commit", "--quiet", "--allow-empty", "--no-verify", "-m", message); err != nil {
		return "", err
	}
	return Head(directory)
}

// Write writes files into the work tree of the trace in directory without
// committing them.
func Write(directory string, files map[string][]byte) error {
	for name, data := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

// Head returns the commit HEAD names in the Git trace in directory.
func Head(directory string) (string, error) {
	data, err := os.ReadFile(filepath.Join(directory, ".git", "refs", "heads", "main"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Publication writes the trace's publication journal: a publication of
// commit on top of parent that writes paths.
func Publication(directory, parent, commit string, paths ...string) error {
	data, err := json.Marshal(struct {
		Version int      `json:"version"`
		Parent  string   `json:"parent"`
		Commit  string   `json:"commit"`
		Paths   []string `json:"paths"`
	}{1, parent, commit, paths})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, ".git", "osmia-publication.json"), data, 0600)
}

// JSONL returns values as JSON lines.
func JSONL(values ...any) ([]byte, error) {
	var out []byte
	for _, v := range values {
		line, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(append(out, line...), '\n')
	}
	return out, nil
}

type actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type header struct {
	Schema     string    `json:"schema"`
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	Revision   int       `json:"revision"`
	Project    string    `json:"project"`
	Workstream string    `json:"workstream,omitempty"`
	At         time.Time `json:"at"`
	Actor      actor     `json:"actor"`
	Cause      string    `json:"cause"`
	Depth      int       `json:"depth"`
}

func manifest(h header) []byte {
	data, _ := json.MarshalIndent(h, "", "  ")
	return append(data, '\n')
}

// ProjectManifest is the project.json of project's trace.
func ProjectManifest(project string, at time.Time) []byte {
	return manifest(header{Schema: "osmia.trace.project", Version: 1, ID: project, Revision: 1, Project: project, At: at, Actor: actor{"owner", "local"}, Cause: "project-create"})
}

// WorkstreamManifest is the workstreams/<stream>/workstream.json of
// stream's trace.
func WorkstreamManifest(project, stream string, at time.Time) []byte {
	return manifest(header{Schema: "osmia.trace.workstream", Version: 1, ID: stream, Revision: 1, Project: project, Workstream: stream, At: at, Actor: actor{"owner", "local"}, Cause: "workstream-create"})
}

// Workstream returns the files of a new workstream's Git trace: its
// manifest and empty documents, events and ledger.
func Workstream(project, stream string, at time.Time) map[string][]byte {
	prefix := "workstreams/" + stream + "/"
	return map[string][]byte{prefix + "workstream.json": WorkstreamManifest(project, stream, at), prefix + "documents.jsonl": nil, prefix + "events.jsonl": nil, prefix + "ledger.jsonl": nil}
}

func git(directory string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", directory, "-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Osmia", "GIT_AUTHOR_EMAIL=osmia@localhost", "GIT_COMMITTER_NAME=Osmia", "GIT_COMMITTER_EMAIL=osmia@localhost", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return out, nil
}
