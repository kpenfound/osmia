package gitdiff

import "testing"

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

func TestParseSplitsFilesAndHunks(t *testing.T) {
	t.Parallel()
	files := Parse(sampleDiff)
	type summary struct {
		path           string
		added, removed int
		binary         bool
		hunks          int
	}
	var got []summary
	for _, f := range files {
		got = append(got, summary{f.Path, f.Added, f.Removed, f.Binary, len(f.Hunks)})
	}
	want := []summary{
		{"docs/guide.md", 2, 1, false, 2},
		{"internal/a b.go", 2, 0, false, 1},
		{"internal/café.go", 0, 1, false, 1},
		{"logo.png", 0, 0, true, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("files %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if h := files[0].Hunks[1]; h.Start != 41 || h.End != 42 {
		t.Fatalf("second hunk covers %d-%d", h.Start, h.End)
	}
	if h := files[2].Hunks[0]; h.Start != 1 || h.End != 1 {
		t.Fatalf("deletion hunk covers %d-%d", h.Start, h.End)
	}
}

func TestChangedFilesListsLineCounts(t *testing.T) {
	t.Parallel()
	if got, want := ChangedFiles(sampleDiff), "- docs/guide.md (+2 -1)\n- internal/a b.go (+2 -0)\n- internal/café.go (+0 -1)\n- logo.png (+0 -0)\n"; got != want {
		t.Fatalf("changed files %q, want %q", got, want)
	}
	if got := ChangedFiles(""); got != "(no changed files)\n" {
		t.Fatalf("empty diff lists %q", got)
	}
}
