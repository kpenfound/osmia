package verifytool

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// EvidenceDir is the top-level directory of a view that holds the retained
// files of lever runs, one directory per run ID. Evidence kept here is never
// captured into a mason's workspace.
const EvidenceDir = isolation.EvidenceDirectory

// NewRunID returns a fresh run ID for one lever run. Every verify call and
// every service rerun takes its own.
func NewRunID() string { return "v" + rand.Text() }

// Dir is the directory a run's files take in a view.
func Dir(runID string) string { return EvidenceDir + "/" + runID }

// Ref is the view path of a file a run's report references.
func Ref(runID, name string) string { return Dir(runID) + "/" + name }

// ImagePath is the view path of an image of a run under images/, flattened
// into one name that starts with the run ID.
func ImagePath(runID, name string) string {
	return "images/" + runID + "-" + strings.ReplaceAll(name, "/", "-")
}

// Outcome is what a lever run came to: either a limitation that says why it
// gave no report, or the validated report and the files retained for it.
type Outcome struct {
	// Limitation is set when no run result exists: the arguments were
	// refused, the run failed or timed out, or its evidence could not be kept.
	Limitation string `json:"limitation,omitempty"`
	// Result is the validated report. A rejected report holds its reason.
	Result verification.Result `json:"result"`
	// Diagnostics is the tail of the run's output, with known secrets removed.
	Diagnostics string `json:"diagnostics,omitempty"`
	// Files lists the retained files by their path in the run's Directory.
	Files []string `json:"files,omitempty"`
	// Images lists the retained files that are images.
	Images []string `json:"images,omitempty"`
}

// Retain copies the report of a finished lever run and every file the report
// references out of the run's temporary export into the evidence store of
// stream's trace, outside the target repository, keyed by runID. The caller
// closes the run after Retain returns. A run retains once: a retry for the
// same run ID returns what the first call kept. It sets the files of outcome
// and stores outcome with them.
func Retain(repo *trace.Repository, stream config.WorkstreamID, runID string, run *checks.LeverRun, outcome Outcome) (Outcome, error) {
	root, err := os.OpenRoot(run.Dir)
	if err != nil {
		return outcome, err
	}
	defer root.Close()
	var files []trace.EvidenceInput
	var total int64
	add := func(name string) error {
		info, err := root.Stat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New(name + " is not a regular file")
		}
		total += info.Size()
		if total > trace.MaxEvidenceBytes {
			return errors.New("the run's files are larger than the evidence limit")
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		files = append(files, trace.EvidenceInput{Path: name, Data: data, Image: checks.IsImageFile(name, data[:min(len(data), 512)])})
		return nil
	}
	if info, err := root.Stat(verification.ReportFile); err == nil && info.Mode().IsRegular() && info.Size() <= verification.MaxReportBytes {
		if err := add(verification.ReportFile); err != nil {
			return outcome, err
		}
	}
	for _, name := range run.Result.Evidence {
		if err := add(name); err != nil {
			return outcome, err
		}
	}
	outcome.Files, outcome.Images = nil, nil
	for _, f := range files {
		outcome.Files = append(outcome.Files, f.Path)
		if f.Image {
			outcome.Images = append(outcome.Images, f.Path)
		}
	}
	meta, err := json.Marshal(outcome)
	if err != nil {
		return outcome, err
	}
	manifest, err := repo.RetainEvidence(stream, runID, files, meta)
	if err != nil {
		return outcome, err
	}
	var kept Outcome
	if err := json.Unmarshal(manifest.Meta, &kept); err != nil {
		return outcome, err
	}
	return kept, nil
}

// Retained returns the outcome stored with a run's retained files, and whether
// the run retained anything.
func Retained(repo *trace.Repository, stream config.WorkstreamID, runID string) (Outcome, bool, error) {
	manifest, ok, err := repo.RetainedEvidence(stream, runID)
	if err != nil || !ok {
		return Outcome{}, false, err
	}
	var outcome Outcome
	if err := json.Unmarshal(manifest.Meta, &outcome); err != nil {
		return Outcome{}, false, err
	}
	return outcome, true, nil
}

// Placed names what Place put into a view.
type Placed struct {
	Dir    string   `json:"dir"`
	Files  []string `json:"files,omitempty"`
	Images []string `json:"images,omitempty"`
}

// Place puts the retained files of runID, kept in stream's trace, into view:
// each under the run's directory at its path in the report, and, when images is
// set, each image also under images/. It writes only what differs and runs no
// lever. A run that retained nothing places nothing.
func Place(repo *trace.Repository, stream config.WorkstreamID, runID string, view isolation.View, images bool) (Placed, error) {
	manifest, ok, err := repo.RetainedEvidence(stream, runID)
	if err != nil || !ok {
		return Placed{}, err
	}
	placed := Placed{Dir: Dir(runID)}
	for _, f := range manifest.Files {
		data, err := repo.ReadEvidence(stream, runID, f.Path)
		if err != nil {
			return placed, err
		}
		if err := put(view, Ref(runID, f.Path), data); err != nil {
			return placed, err
		}
		placed.Files = append(placed.Files, Ref(runID, f.Path))
		if f.Image && images {
			if err := put(view, ImagePath(runID, f.Path), data); err != nil {
				return placed, err
			}
			placed.Images = append(placed.Images, ImagePath(runID, f.Path))
		}
	}
	return placed, nil
}

func put(view isolation.View, name string, data []byte) error {
	if old, err := view.Read(name); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return view.Put(name, data)
}
