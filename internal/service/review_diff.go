package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/trace"
)

const workstreamDiffTool = "workstream_diff"

// diffOutputLimit bounds one workstream_diff response.
const diffOutputLimit = 64 * 1024

// selectedDiff is what a workstream_diff call asks for.
type selectedDiff struct {
	FilesOnly bool     `json:"files_only"`
	Paths     []string `json:"paths"`
	Start     int      `json:"start"`
	End       int      `json:"end"`
}

func (s selectedDiff) matches(path string) bool {
	if len(s.Paths) == 0 {
		return true
	}
	for _, p := range s.Paths {
		if p == "." || path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

type changedFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary,omitempty"`
}

// render returns the selected files, or the selected part of the diff cut at
// a line boundary within diffOutputLimit.
func (s selectedDiff) render(files []gitdiff.File) (listed []changedFile, text string, truncated bool) {
	var out strings.Builder
	for _, f := range files {
		if !s.matches(f.Path) {
			continue
		}
		if s.FilesOnly {
			listed = append(listed, changedFile{Path: f.Path, Added: f.Added, Removed: f.Removed, Binary: f.Binary})
			continue
		}
		var hunks []gitdiff.Hunk
		for _, h := range f.Hunks {
			if s.Start == 0 || h.Start <= s.End && h.End >= s.Start {
				hunks = append(hunks, h)
			}
		}
		if s.Start != 0 && len(hunks) == 0 {
			continue
		}
		lines := slices.Clone(f.Header)
		if f.Binary && len(f.Hunks) == 0 {
			lines = append(lines, "(binary content not shown)\n")
		}
		for _, h := range hunks {
			lines = append(lines, h.Lines...)
		}
		for _, line := range lines {
			if out.Len()+len(line) > diffOutputLimit {
				return nil, out.String(), true
			}
			out.WriteString(line)
		}
	}
	return listed, out.String(), false
}

// pinnedDiff is one change a reviewer may read: the diff between two
// recorded commits.
type pinnedDiff struct {
	// Name selects the change when a tool serves more than one.
	Name string
	// About says what the change is, for the tool description.
	About    string
	From, To string
	Read     func(ctx context.Context) (string, error)
}

// diffTool reads the pinned changes of a review, as git diff shows them.
func diffTool(diffs ...pinnedDiff) coreadapter.Tool {
	description := "Read the exact diff this review is pinned to, as git diff shows it. With no fields it returns the whole diff. files_only lists the changed files with added and removed line counts. paths limits the diff to those files or directories. start and end keep only the hunks that touch those one-based lines, on the changed side, of the named paths. A response over 64 KiB is cut and says truncated; narrow paths or lines to read the rest."
	schema := `{"type":"object","properties":{"files_only":{"type":"boolean"},"paths":{"type":"array","items":{"type":"string"}},"start":{"type":"integer"},"end":{"type":"integer"}},"additionalProperties":false}`
	if len(diffs) > 1 {
		var names, about []string
		for _, d := range diffs {
			names = append(names, strconv.Quote(d.Name))
			about = append(about, d.Name+" is "+d.About)
		}
		description += " change selects the diff: " + strings.Join(about, "; ") + "."
		schema = `{"type":"object","properties":{"change":{"type":"string","enum":[` + strings.Join(names, ",") + `]},"files_only":{"type":"boolean"},"paths":{"type":"array","items":{"type":"string"}},"start":{"type":"integer"},"end":{"type":"integer"}},"required":["change"],"additionalProperties":false}`
	} else if len(diffs) == 1 {
		description += " The diff is " + diffs[0].About + "."
	}
	return coreadapter.Tool{Name: workstreamDiffTool, Effect: coreadapter.ToolRead, Description: description, InputSchema: json.RawMessage(schema),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Change string `json:"change"`
				selectedDiff
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&in); err != nil {
				return nil, fmt.Errorf("tool input: %w", err)
			}
			i := slices.IndexFunc(diffs, func(d pinnedDiff) bool { return d.Name == in.Change })
			if len(diffs) == 1 && in.Change == "" {
				i = 0
			}
			if i < 0 {
				return nil, fmt.Errorf("change %q is not one this review reads", in.Change)
			}
			for _, p := range in.Paths {
				if !fs.ValidPath(p) {
					return nil, fmt.Errorf("path %q is not a relative repository path", p)
				}
			}
			if in.Start != 0 || in.End != 0 {
				if len(in.Paths) == 0 || in.FilesOnly {
					return nil, errors.New("start and end select lines of named paths; give paths and leave files_only unset")
				}
				if in.Start < 1 || in.End < in.Start {
					return nil, errors.New("start and end must be one-based lines with start at or before end")
				}
			}
			diff, err := diffs[i].Read(ctx)
			if err != nil {
				return nil, err
			}
			listed, text, truncated := in.render(gitdiff.Parse(diff))
			response := struct {
				Change    string        `json:"change,omitempty"`
				From      string        `json:"from"`
				To        string        `json:"to"`
				Files     []changedFile `json:"files,omitempty"`
				Diff      string        `json:"diff,omitempty"`
				Truncated bool          `json:"truncated,omitempty"`
				Note      string        `json:"note,omitempty"`
			}{Change: in.Change, From: diffs[i].From, To: diffs[i].To, Files: listed, Diff: text, Truncated: truncated}
			if len(listed) == 0 && text == "" {
				response.Note = "no changed file matches the selection"
			}
			return json.Marshal(response)
		}}
}

// reviewDiffTool serves the exact diff a unit review is pinned to: the
// candidate against its base, verified against the recorded digest.
func reviewDiffTool(cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, identity UnitReviewIdentity) coreadapter.Tool {
	return diffTool(pinnedDiff{About: "the unit's candidate against the feature branch commit it was built on", From: identity.Candidate.BaseRevision, To: identity.Candidate.Revision,
		Read: func(ctx context.Context) (string, error) {
			g, err := newUnitWorkspaces(cfg, r).of(config.WorkstreamID(scope.Workstream))
			if err != nil {
				return "", err
			}
			diff, err := g.Diff(ctx, identity.Candidate.BaseRevision, identity.Candidate.Revision)
			if err != nil {
				return "", fmt.Errorf("candidate diff: %w", err)
			}
			sum := sha256.Sum256([]byte(diff))
			if hex.EncodeToString(sum[:]) != identity.DiffSHA256 {
				return "", errors.New("the candidate diff differs from the one this review is pinned to")
			}
			return diff, nil
		}})
}
