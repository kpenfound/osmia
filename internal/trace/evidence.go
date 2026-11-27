package trace

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// MaxEvidenceBytes caps what one lever run retains: the evidence Directory's
// 20 MiB plus its report.
const MaxEvidenceBytes = 20<<20 + 64<<10

var evidenceRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

const (
	evidenceDirectory = "evidence"
	evidenceManifest  = "manifest.json"
	evidenceFiles     = "files"
)

// EvidenceFile describes one file a lever run retained.
type EvidenceFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Image  bool   `json:"image,omitempty"`
}

// EvidenceManifest lists the files retained for one lever run, in the order
// they were given.
type EvidenceManifest struct {
	RunID string         `json:"run_id"`
	Files []EvidenceFile `json:"files"`
	Bytes int64          `json:"bytes"`
	// Meta is the retaining caller's own record of the run, kept with the
	// files so a run whose result was never recorded can still be read back.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// EvidenceInput is one file to retain. Path is relative and clean.
type EvidenceInput struct {
	Path  string
	Data  []byte
	Image bool
}

func (r *Repository) evidenceRoot(stream config.WorkstreamID) (string, error) {
	target, err := r.scoped(stream)
	if err != nil {
		return "", err
	}
	if !target.imageStorageScope() {
		return "", errors.New("evidence belongs only to member or session traces")
	}
	return filepath.Join(target.directory, evidenceDirectory), nil
}

func checkEvidencePath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("evidence path %q must be clean and relative", p)
	}
	return nil
}

// RetainEvidence keeps files for the lever run runID in the workstream's own
// trace directory, outside any target repository, and returns the manifest. A
// run retains once: a second call for the same run returns the first manifest
// and keeps nothing more, so a retried operation never rewrites evidence.
func (r *Repository) RetainEvidence(stream config.WorkstreamID, runID string, files []EvidenceInput, meta json.RawMessage) (EvidenceManifest, error) {
	if !evidenceRunID.MatchString(runID) {
		return EvidenceManifest{}, fmt.Errorf("run ID %q is not a plain token", runID)
	}
	base, err := r.evidenceRoot(stream)
	if err != nil {
		return EvidenceManifest{}, err
	}
	if existing, ok, err := r.RetainedEvidence(stream, runID); err != nil || ok {
		return existing, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return EvidenceManifest{}, err
	}
	stage, err := os.MkdirTemp(base, ".stage-")
	if err != nil {
		return EvidenceManifest{}, err
	}
	defer os.RemoveAll(stage)
	root, err := os.OpenRoot(stage)
	if err != nil {
		return EvidenceManifest{}, err
	}
	defer root.Close()
	if err := root.Mkdir(evidenceFiles, 0o700); err != nil {
		return EvidenceManifest{}, err
	}
	manifest := EvidenceManifest{RunID: runID, Files: []EvidenceFile{}, Meta: meta}
	seen := map[string]bool{}
	for _, f := range files {
		if err := checkEvidencePath(f.Path); err != nil {
			return EvidenceManifest{}, err
		}
		if seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		manifest.Bytes += int64(len(f.Data))
		if manifest.Bytes > MaxEvidenceBytes {
			return EvidenceManifest{}, fmt.Errorf("a run retains at most %d bytes of evidence", MaxEvidenceBytes)
		}
		name := path.Join(evidenceFiles, f.Path)
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return EvidenceManifest{}, err
		}
		if err := root.WriteFile(name, f.Data, 0o600); err != nil {
			return EvidenceManifest{}, err
		}
		manifest.Files = append(manifest.Files, EvidenceFile{Path: f.Path, Bytes: int64(len(f.Data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(f.Data)), Image: f.Image})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return EvidenceManifest{}, err
	}
	if err := root.WriteFile(evidenceManifest, data, 0o600); err != nil {
		return EvidenceManifest{}, err
	}
	if err := os.Rename(stage, filepath.Join(base, runID)); err != nil {
		if existing, ok, readErr := r.RetainedEvidence(stream, runID); readErr == nil && ok {
			return existing, nil
		}
		return EvidenceManifest{}, err
	}
	return manifest, nil
}

// RetainedEvidence returns the manifest retained for runID, and whether the
// run retained anything.
func (r *Repository) RetainedEvidence(stream config.WorkstreamID, runID string) (EvidenceManifest, bool, error) {
	if !evidenceRunID.MatchString(runID) {
		return EvidenceManifest{}, false, fmt.Errorf("run ID %q is not a plain token", runID)
	}
	base, err := r.evidenceRoot(stream)
	if err != nil {
		return EvidenceManifest{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(base, runID, evidenceManifest))
	if errors.Is(err, fs.ErrNotExist) {
		return EvidenceManifest{}, false, nil
	}
	if err != nil {
		return EvidenceManifest{}, false, err
	}
	var manifest EvidenceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return EvidenceManifest{}, false, err
	}
	return manifest, true, nil
}

// ReadEvidence returns one retained file of runID. Only a file the manifest
// lists is readable.
func (r *Repository) ReadEvidence(stream config.WorkstreamID, runID, name string) ([]byte, error) {
	manifest, ok, err := r.RetainedEvidence(stream, runID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fs.ErrNotExist
	}
	listed := false
	for _, f := range manifest.Files {
		if f.Path == name {
			listed = true
		}
	}
	if !listed {
		return nil, fs.ErrNotExist
	}
	base, err := r.evidenceRoot(stream)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Join(base, runID, evidenceFiles))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}
