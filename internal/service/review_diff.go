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
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const workstreamDiffTool = "workstream_diff"

// diffOutputLimit bounds one workstream_diff response.
const diffOutputLimit = 64 * 1024

// fileDiff is one file's section of a unified git diff.
type fileDiff struct {
	Path    string
	Header  []string
	Hunks   []diffHunk
	Binary  bool
	Added   int
	Removed int
}

// diffHunk is one hunk and the candidate lines it covers.
type diffHunk struct {
	Lines      []string
	Start, End int
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseDiff splits the output of git diff --no-renames into files. Lines keep
// their endings; a binary patch body is dropped.
func parseDiff(diff string) []fileDiff {
	var files []fileDiff
	binaryBody := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			files = append(files, fileDiff{Path: diffPath(strings.TrimSuffix(line, "\n")), Header: []string{line}})
			binaryBody = false
			continue
		}
		if len(files) == 0 || binaryBody {
			continue
		}
		f := &files[len(files)-1]
		switch {
		case strings.HasPrefix(line, "@@ "):
			h := diffHunk{Lines: []string{line}}
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				start, _ := strconv.Atoi(m[1])
				count := 1
				if m[2] != "" {
					count, _ = strconv.Atoi(m[2])
				}
				h.Start, h.End = max(start, 1), max(start+count-1, start, 1)
			}
			f.Hunks = append(f.Hunks, h)
		case len(f.Hunks) != 0:
			h := &f.Hunks[len(f.Hunks)-1]
			h.Lines = append(h.Lines, line)
			switch line[0] {
			case '+':
				f.Added++
			case '-':
				f.Removed++
			}
		case strings.TrimSuffix(line, "\n") == "GIT binary patch":
			f.Binary, binaryBody = true, true
		default:
			if strings.HasPrefix(line, "Binary files ") {
				f.Binary = true
			}
			f.Header = append(f.Header, line)
		}
	}
	return files
}

// diffPath reads the path of a "diff --git a/<path> b/<path>" line, whose two
// paths are equal without rename detection.
func diffPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		if quoted, err := strconv.QuotedPrefix(rest); err == nil {
			if path, err := strconv.Unquote(quoted); err == nil {
				return strings.TrimPrefix(path, "a/")
			}
		}
	}
	n := (len(rest) - len("a/ b/")) / 2
	if n <= 0 || !strings.HasPrefix(rest, "a/") {
		return rest
	}
	return rest[len("a/") : len("a/")+n]
}

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
func (s selectedDiff) render(files []fileDiff) (listed []changedFile, text string, truncated bool) {
	var out strings.Builder
	for _, f := range files {
		if !s.matches(f.Path) {
			continue
		}
		if s.FilesOnly {
			listed = append(listed, changedFile{Path: f.Path, Added: f.Added, Removed: f.Removed, Binary: f.Binary})
			continue
		}
		var hunks []diffHunk
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
			listed, text, truncated := in.render(parseDiff(diff))
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

// changedFiles lists a diff's files with their added and removed lines, one
// per line, for a review prompt.
func changedFiles(diff string) string {
	var out strings.Builder
	for _, f := range parseDiff(diff) {
		fmt.Fprintf(&out, "- %s (+%d -%d)\n", f.Path, f.Added, f.Removed)
	}
	if out.Len() == 0 {
		return "(no changed files)\n"
	}
	return out.String()
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
