package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

const sampleDiff = `diff --git a/docs/guide.md b/docs/guide.md
index 1111111..2222222 100644
--- a/docs/guide.md
+++ b/docs/guide.md
@@ -1,3 +1,4 @@
 # Guide
+Added near the top.
 Body
 End
@@ -40,2 +41,2 @@ section
-old tail
+new tail
 last
diff --git a/internal/a b.go b/internal/a b.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/internal/a b.go
@@ -0,0 +1,2 @@
+package internal
+// ++ counted as an added line
diff --git "a/internal/caf\303\251.go" "b/internal/caf\303\251.go"
deleted file mode 100644
index 4444444..0000000
--- "a/internal/caf\303\251.go"
+++ /dev/null
@@ -1 +0,0 @@
-package internal
\ No newline at end of file
diff --git a/logo.png b/logo.png
new file mode 100644
index 0000000..5555555
GIT binary patch
literal 4
LcmZQzWMT#Y01f~L

literal 0
HcmV?d00001

`

func TestSelectedDiffRendersLikeGitDiff(t *testing.T) {
	t.Parallel()
	files := gitdiff.Parse(sampleDiff)
	if _, text, truncated := (selectedDiff{}).render(files); truncated || !strings.HasPrefix(text, "diff --git a/docs/guide.md") || !strings.Contains(text, "(binary content not shown)") || strings.Contains(text, "LcmZQzWMT") {
		t.Fatalf("whole diff truncated=%t:\n%s", truncated, text)
	}
	listed, text, _ := selectedDiff{FilesOnly: true, Paths: []string{"internal"}}.render(files)
	if text != "" || len(listed) != 2 || listed[0] != (ChangedFile{Path: "internal/a b.go", Added: 2}) || listed[1].Path != "internal/café.go" {
		t.Fatalf("files only: %+v %q", listed, text)
	}
	_, text, _ = selectedDiff{Paths: []string{"docs/guide.md"}, Start: 40, End: 41}.render(files)
	if !strings.Contains(text, "+new tail") || strings.Contains(text, "Added near the top") || !strings.HasPrefix(text, "diff --git a/docs/guide.md") {
		t.Fatalf("line range:\n%s", text)
	}
	if _, text, _ := (selectedDiff{Paths: []string{"docs/guide.md"}, Start: 10, End: 20}).render(files); text != "" {
		t.Fatalf("range between hunks returned:\n%s", text)
	}
	if _, text, _ := (selectedDiff{Paths: []string{"doc"}}).render(files); text != "" {
		t.Fatalf("a path prefix that is not a directory matched:\n%s", text)
	}
	large := gitdiff.Parse("diff --git a/big b/big\n--- a/big\n+++ b/big\n@@ -0,0 +1,5000 @@\n" + strings.Repeat("+0123456789abcdef\n", 5000))
	_, text, truncated := (selectedDiff{}).render(large)
	if !truncated || len(text) > diffOutputLimit || !strings.HasSuffix(text, "\n") {
		t.Fatalf("large diff: truncated=%t bytes=%d", truncated, len(text))
	}
}

// A recorded review prompt pins the identity of the candidate it reviewed.
func TestReviewIdentityInPromptReadsThePinnedIdentity(t *testing.T) {
	t.Parallel()
	identity := UnitReviewIdentity{Subject: "stream/resume", Candidate: coreadapter.Candidate{Revision: "candidate", BaseRevision: "base", SpecRevision: "1", PlanRevision: "2"}, DiffSHA256: "digest", Report: "report", Seal: 1}
	content, _ := json.MarshalIndent(identity, "", "  ")
	got, err := IdentityInPrompt("Review this.\nThe candidate identity is:\n" + string(content) + "\n\nChanged files, with added and removed lines; read the diff with workstream_diff:\n- x (+1 -0)\n")
	if err != nil || got != identity {
		t.Fatalf("identity %+v %v", got, err)
	}
}

// A tool serving several changes needs the change named, and serves each
// with its own commits.
func TestDiffToolSelectsTheNamedChange(t *testing.T) {
	t.Parallel()
	serve := func(text string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return text, nil }
	}
	tool := DiffTool(
		PinnedDiff{Name: "feature", About: "the feature's change", From: "base", To: "before", Read: serve(sampleDiff)},
		PinnedDiff{Name: "resolution", About: "the resolved candidate", From: "upstream", To: "candidate", Read: serve("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n")},
	)
	if !strings.Contains(string(tool.InputSchema), `"required":["change"]`) || !strings.Contains(tool.Description, "resolution is the resolved candidate") {
		t.Fatalf("tool %s: %s", tool.Description, tool.InputSchema)
	}
	for _, input := range []string{`{}`, `{"change":"other"}`} {
		if _, err := tool.Handle(context.Background(), json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	out, err := tool.Handle(context.Background(), json.RawMessage(`{"change":"resolution","files_only":true}`))
	fixturekit.Must(t, err)
	var got struct {
		Change, From, To string
		Files            []ChangedFile
	}
	fixturekit.Must(t, json.Unmarshal(out, &got))
	if got.Change != "resolution" || got.From != "upstream" || got.To != "candidate" || len(got.Files) != 1 || got.Files[0] != (ChangedFile{Path: "x", Added: 1, Removed: 1}) {
		t.Fatalf("resolution: %+v", got)
	}
}
