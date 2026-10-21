package intake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	LibrarianRole   = "librarian"
	LibrarianAgent  = "agent_librarian"
	LibrarianThread = "thread_librarian"
	// maxExtractionAttempts bounds the librarian turns one extraction may
	// start after service stops interrupt earlier ones.
	MaxExtractionAttempts = 3
)

// librarianWorkstream is the project's workstream holding the librarian
// thread and its extraction operations, derived from the project ID.
func LibrarianWorkstream(project config.ProjectID) config.WorkstreamID {
	sum := sha256.Sum256([]byte("librarian:" + string(project)))
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}

// progress derives an operation's state from its record: running while a
// live claim has started the effect, pending while it is queued or waiting to
// retry (with the retry's reason), otherwise its result's outcome, with the
// failure's evidence as the reason. The time is the record's last action up to
// the result, which fixes it: a later acknowledgement does not move it.
func Progress(record trace.OperationRecord) (state, reason string, at time.Time) {
	state, at = "pending", record.Transition.At
	for _, a := range record.History {
		at = a.At
		if a.Kind == "result" {
			break
		}
		switch a.Kind {
		case "retry":
			reason = a.Failure
		case "claim":
			reason = ""
		}
	}
	switch {
	case record.Result != nil:
		state, reason = record.Result.Outcome, ""
		if state == "failed" {
			reason = record.Result.Evidence
		}
	case record.EffectStarted && record.Claim != nil:
		state, reason = "running", ""
	}
	return state, reason, at
}

// stagedWorkspaces lends a service-staged directory as a turn's workspace.
type StagedWorkspaces struct{}

type NoLease struct{}

func (NoLease) Release(context.Context) error { return nil }

func (StagedWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	if req.SourceDirectory == "" {
		return coreadapter.WorkspaceLease{}, errors.New("staged workspace requires a source directory")
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: filepath.Base(req.SourceDirectory), Directory: req.SourceDirectory, Access: req.Access}, Lease: NoLease{}}, nil
}

// copyTracked copies the clone's tracked regular files into dst. Git only
// lists the index; the clone is never written.
func CopyTracked(ctx context.Context, clone, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", clone, "ls-files", "-z")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list tracked files of the clone: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	src, err := os.OpenRoot(clone)
	if err != nil {
		return err
	}
	defer src.Close()
	target, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer target.Close()
	for _, entry := range bytes.Split(out, []byte{0}) {
		name := string(entry)
		if name == "" || !fs.ValidPath(name) {
			continue
		}
		info, err := src.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := src.ReadFile(name)
		if err != nil {
			return err
		}
		if err := target.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if err := target.WriteFile(name, data, 0600|(info.Mode().Perm()&0100)); err != nil {
			return err
		}
	}
	return nil
}
