package filetransport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type entry struct {
	Kind    string
	Perm    fs.FileMode
	Content string
	Link    string
}

func snapshot(t *testing.T, root string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			out[rel] = entry{Kind: "dir", Perm: info.Mode().Perm()}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			out[rel] = entry{Kind: "symlink", Link: link}
			return err
		default:
			data, err := os.ReadFile(p)
			out[rel] = entry{Kind: "file", Perm: info.Mode().Perm(), Content: string(data)}
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, root, name, content string, perm fs.FileMode) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRoundTripsFilesModesAndEmptyDirectories(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	write(t, src, "main.go", "package main\n", 0o644)
	write(t, src, "bin/run.sh", "#!/bin/sh\n", 0o755)
	write(t, src, "secret.txt", "s", 0o600)
	write(t, src, "docs/read-only.md", "ro", 0o444)
	write(t, src, "docs/deep/empty.txt", "", 0o644)
	for _, dir := range []string{"empty", "docs/also-empty"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("main.go", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../main.go", filepath.Join(src, "docs/up")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "docs"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "docs"), 0o755) })

	var buf bytes.Buffer
	if err := Pack(&buf, src); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Unpack(&buf, dest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dest, "docs"), 0o755) })
	if got, want := snapshot(t, dest), snapshot(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("unpacked view differs:\n got %+v\nwant %+v", got, want)
	}
}

func TestArchiveOfAnEmptyViewUnpacksToAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Pack(&buf, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Unpack(&buf, dest); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dest); len(got) != 0 {
		t.Fatalf("unpacked %+v", got)
	}
}

func TestPackRefusesASpecialFile(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	if err := syscallMkfifo(filepath.Join(src, "pipe")); err != nil {
		t.Skip(err)
	}
	if err := Pack(&bytes.Buffer{}, src); err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("pack: %v", err)
	}
}

type member struct {
	tar.Header
	Body string
}

func archive(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zip := gzip.NewWriter(&buf)
	w := tar.NewWriter(zip)
	for _, m := range members {
		h := m.Header
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.Body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte(m.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func file(name, body string) member { return member{Header: tar.Header{Name: name}, Body: body} }
func dir(name string) member {
	return member{Header: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}
func symlink(name, target string) member {
	return member{Header: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

func TestUnpackRefusesEntriesThatEscapeTheRootAndWritesNothingOutside(t *testing.T) {
	t.Parallel()
	for name, members := range map[string][]member{
		"parent segment":         {file("ok.txt", "x"), file("../escaped.txt", "x")},
		"nested parent segment":  {file("a/../../escaped.txt", "x")},
		"parent inside the path": {file("a/../b.txt", "x")},
		"parent directory":       {dir("../escaped")},
		"absolute path":          {file("/tmp/escaped.txt", "x")},
		"absolute directory":     {dir("/tmp/escaped/")},
		"absolute symlink":       {symlink("link", "/etc")},
		"symlink to the parent":  {symlink("link", "..")},
		"outward symlink":        {symlink("link", "../sentinel")},
		"deep outward symlink":   {dir("a"), dir("a/b"), symlink("a/b/link", "../../../sentinel")},
		"symlink dot-dot after a name": {
			dir("sub"), symlink("sub/up", ".."), symlink("l2", "sub/up/../.."),
		},
		"write through an inward symlink": {dir("real"), symlink("link", "real"), file("link/pwn", "x")},
		"write through an outward symlink": {
			symlink("link", "../sentinel"), file("link/pwn", "x"),
		},
		"directory through a symlink": {dir("real"), symlink("link", "real"), dir("link/sub")},
		"hard link":                   {file("a", "x"), {Header: tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"}}},
		"device":                      {{Header: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}},
		"duplicate":                   {file("a", "1"), file("a", "2")},
		"file below a file":           {file("a", "1"), file("a/b", "2")},
		"empty symlink target":        {symlink("link", "")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outer := t.TempDir()
			dest := filepath.Join(outer, "dest")
			sentinel := filepath.Join(outer, "sentinel")
			for _, d := range []string{dest, sentinel} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := Unpack(bytes.NewReader(archive(t, members...)), dest); err == nil {
				t.Fatalf("the archive was unpacked into %+v", snapshot(t, dest))
			}
			for p := range snapshot(t, outer) {
				if p != "dest" && p != "sentinel" && !strings.HasPrefix(p, "dest"+string(filepath.Separator)) {
					t.Errorf("%s was written outside the destination root", p)
				}
			}
			if left, _ := os.ReadDir(sentinel); len(left) != 0 {
				t.Errorf("the sentinel directory holds %v", left)
			}
		})
	}
}

func TestUnpackRefusesAnEscapeAfterValidEntries(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	dest := filepath.Join(outer, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Unpack(bytes.NewReader(archive(t, file("fine.txt", "x"), file("../escaped.txt", "x"))), dest)
	if !errors.Is(err, ErrEscapes) {
		t.Fatalf("unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outer, "escaped.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("escaped file: %v", err)
	}
}

func TestUnpackAcceptsAnInwardSymlinkAndImplicitParents(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	err := Unpack(bytes.NewReader(archive(t, file("a/b/c.txt", "c"), symlink("a/link", "b/c.txt"), symlink("a/b/up", "../b"), file("./d.txt", "d"), dir("./"))), dest)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]entry{
		"a": {Kind: "dir", Perm: 0o700}, "a/b": {Kind: "dir", Perm: 0o700},
		"a/b/c.txt": {Kind: "file", Perm: 0o644, Content: "c"},
		"a/link":    {Kind: "symlink", Link: "b/c.txt"}, "a/b/up": {Kind: "symlink", Link: "../b"},
		"d.txt": {Kind: "file", Perm: 0o644, Content: "d"},
	}
	if got := snapshot(t, dest); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestUnpackBoundsTheArchive(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		limits  Limits
		members []member
	}{
		"bytes":   {Limits{Bytes: 5, Entries: 10}, []member{file("a", "123"), file("b", "456")}},
		"entries": {Limits{Bytes: 100, Entries: 2}, []member{file("a", "1"), file("b", "2"), file("c", "3")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := unpack(bytes.NewReader(archive(t, c.members...)), t.TempDir(), c.limits); err == nil {
				t.Fatal("unpacked past the limit")
			}
		})
	}
}

func TestUnpackRefusesWhatIsNotAnArchive(t *testing.T) {
	t.Parallel()
	for name, data := range map[string][]byte{
		"empty":     nil,
		"plain":     []byte("not an archive"),
		"truncated": archive(t, file("a", strings.Repeat("x", 4096)))[:40],
	} {
		if err := Unpack(bytes.NewReader(data), t.TempDir()); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
}
