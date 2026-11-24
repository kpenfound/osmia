package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func editorScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor")
	must(t, os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body+"\n"), 0700))
	return path
}

func editDirectories(t *testing.T, tmp string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(tmp, "osmia-edit-*"))
	must(t, err)
	return dirs
}

func keptFile(t *testing.T, diag string) string {
	t.Helper()
	_, rest, ok := strings.Cut(diag, "Your edit is kept in ")
	if !ok {
		t.Fatalf("no kept file named:\n%s", diag)
	}
	path, _, _ := strings.Cut(rest, "\n")
	return path
}

type editFixture struct {
	root string
	c    *service.Client
	tmp  string
}

func newEditFixture(t *testing.T) *editFixture {
	t.Helper()
	opts := withShed(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	t.Cleanup(c.Close)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	return &editFixture{root: root, c: c, tmp: tmp}
}

func (f *editFixture) charter(t *testing.T) trace.Document {
	t.Helper()
	d, err := f.c.Charter(context.Background(), project)
	must(t, err)
	return d
}

func (f *editFixture) draft(t *testing.T) map[string]trace.Document {
	t.Helper()
	d, err := f.c.Documents(context.Background(), stream)
	must(t, err)
	return d
}

func TestEditWithoutAnEditorNamesEditorAndFrom(t *testing.T) {
	f := newEditFixture(t)
	code, out, diag := invoke(t, f.root, "edit", "charter", project)
	if code != 4 || out != "" || !strings.Contains(diag, "set EDITOR") || !strings.Contains(diag, "--from") {
		t.Fatalf("no editor: %d %q %q", code, out, diag)
	}
	if f.charter(t).Revision != 1 {
		t.Fatal("an edit without an editor was recorded")
	}
}

func TestEditRecordsTheEditorsChangeAsTheNextRevision(t *testing.T) {
	f := newEditFixture(t)
	t.Setenv("EDITOR", "false")
	t.Setenv("VISUAL", editorScript(t, `printf '1. Keep changes small.\n' >> "$1"`))
	before := f.charter(t)
	if out := successful(t, f.root, "edit", "charter", project); out != "Recorded charter.md revision 2.\n" {
		t.Fatalf("charter edit output %q", out)
	}
	if after := f.charter(t); after.Revision != 2 || after.Content != before.Content+"1. Keep changes small.\n" || after.Actor != (trace.Actor{Kind: "owner", ID: "local"}) {
		t.Fatalf("recorded charter %+v", after)
	}
	if dirs := editDirectories(t, f.tmp); len(dirs) != 0 {
		t.Fatalf("a successful edit left %v", dirs)
	}
}

func TestEditWithUnchangedContentRecordsNothing(t *testing.T) {
	f := newEditFixture(t)
	t.Setenv("VISUAL", "true")
	if out := successful(t, f.root, "edit", "charter", project); out != "No changes.\n" {
		t.Fatalf("unchanged output %q", out)
	}
	if f.charter(t).Revision != 1 || len(editDirectories(t, f.tmp)) != 0 {
		t.Fatal("an unchanged edit was recorded or left its file")
	}
}

func TestEditFromAFileRecordsItWithoutOpeningAnEditor(t *testing.T) {
	f := newEditFixture(t)
	t.Setenv("VISUAL", "false")
	from := filepath.Join(t.TempDir(), "rules.md")
	must(t, os.WriteFile(from, []byte("1. Keep changes small.\n2. Test every fix.\n"), 0600))
	if out := successful(t, f.root, "edit", "charter", project, "--from", from); out != "Recorded charter.md revision 2.\n" {
		t.Fatalf("--from output %q", out)
	}
	if got := f.charter(t); got.Revision != 2 || got.Content != "1. Keep changes small.\n2. Test every fix.\n" {
		t.Fatalf("--from charter %+v", got)
	}
}

func TestEditFromAMissingFileIsRefused(t *testing.T) {
	f := newEditFixture(t)
	code, out, diag := invoke(t, f.root, "edit", "charter", project, "--from", filepath.Join(t.TempDir(), "missing.md"))
	if code != 2 || out != "" || !strings.Contains(diag, "cannot read") {
		t.Fatalf("missing --from file: %d %q %q", code, out, diag)
	}
}

type conflictingEdit struct {
	f      *editFixture
	before trace.Document
	code   int
	out    string
	diag   string
}

func runConflictingEdit(t *testing.T) conflictingEdit {
	t.Helper()
	f := newEditFixture(t)
	before := f.charter(t)
	mark, release := filepath.Join(f.tmp, "editing"), filepath.Join(f.tmp, "release")
	t.Setenv("VISUAL", editorScript(t, `printf '3. Document every flag.\n' >> "$1"
touch `+mark+`
while [ ! -f `+release+` ]; do sleep 0.05; done`))
	type result struct {
		code      int
		out, diag string
	}
	done := make(chan result, 1)
	go func() {
		code, out, diag := invoke(t, f.root, "edit", "charter", project)
		done <- result{code, out, diag}
	}()
	eventually(t, time.Minute, func() (bool, string) {
		_, err := os.Stat(mark)
		return err == nil, "the editor never ran"
	})
	if _, err := f.c.EditCharter(context.Background(), project, wire.CharterEdit{Revision: before.Revision, Content: "1. Keep changes small.\n"}); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(release, nil, 0600))
	r := <-done
	return conflictingEdit{f: f, before: before, code: r.code, out: r.out, diag: r.diag}
}

func TestEditConflictIsRefused(t *testing.T) {
	c := runConflictingEdit(t)
	if c.code != 5 || c.out != "" || !strings.Contains(c.diag, "conflict:") {
		t.Fatalf("conflicting edit: %d %q %q", c.code, c.out, c.diag)
	}
}

func TestEditConflictNamesTheRetryCommand(t *testing.T) {
	c := runConflictingEdit(t)
	kept := keptFile(t, c.diag)
	if !strings.Contains(c.diag, "osmia edit charter "+project+" --from "+kept) {
		t.Fatalf("no retry command:\n%s", c.diag)
	}
}

func TestEditConflictKeepsTheEditedText(t *testing.T) {
	c := runConflictingEdit(t)
	kept := keptFile(t, c.diag)
	if data, err := os.ReadFile(kept); err != nil || string(data) != c.before.Content+"3. Document every flag.\n" {
		t.Fatalf("kept file %q %v", data, err)
	}
}

func TestEditConflictRecordsNothing(t *testing.T) {
	c := runConflictingEdit(t)
	if got := c.f.charter(t); got.Revision != c.before.Revision+1 || got.Content != "1. Keep changes small.\n" {
		t.Fatalf("a conflicting edit was recorded: %+v", got)
	}
}

func TestEditRetryFromTheKeptFileRecordsTheEdit(t *testing.T) {
	c := runConflictingEdit(t)
	kept := keptFile(t, c.diag)
	if out := successful(t, c.f.root, "edit", "charter", project, "--from", kept); out != fmt.Sprintf("Recorded charter.md revision %d.\n", c.before.Revision+2) {
		t.Fatalf("retry output %q", out)
	}
}

func TestEditingTheSpecSendsThePlanUnchanged(t *testing.T) {
	f := newEditFixture(t)
	const spec = "# Uploads\n\nUploads resume where they stopped.\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n"
	t.Setenv("VISUAL", editorScript(t, `printf '`+strings.ReplaceAll(spec, "\n", `\n`)+`' > "$1"`))
	old := f.draft(t)
	if out := successful(t, f.root, "edit", "spec", stream); out != "Recorded spec.md revision 2.\n" {
		t.Fatalf("spec edit output %q", out)
	}
	edited := f.draft(t)
	if edited["spec"].Revision != 2 || edited["spec"].Content != spec || edited["plan"].Revision != old["plan"].Revision || edited["plan"].Content != old["plan"].Content {
		t.Fatalf("spec edit recorded %+v", edited)
	}
}

func TestEditRefusesAnInvalidPlanAndKeepsTheEdit(t *testing.T) {
	f := newEditFixture(t)
	old := f.draft(t)
	t.Setenv("VISUAL", editorScript(t, `printf 'not a plan\n' > "$1"`))
	code, out, diag := invoke(t, f.root, "edit", "plan", stream)
	if code != 4 || out != "" || !strings.HasPrefix(diag, "validation: ") {
		t.Fatalf("invalid plan: %d %q %q", code, out, diag)
	}
	kept := keptFile(t, diag)
	if data, err := os.ReadFile(kept); err != nil || string(data) != "not a plan\n" || filepath.Base(kept) != "plan.json" {
		t.Fatalf("kept plan %s %q %v", kept, data, err)
	}
	if got := f.draft(t); got["plan"].Revision != old["plan"].Revision || got["spec"].Revision != old["spec"].Revision {
		t.Fatalf("an invalid plan was recorded: %+v", got)
	}
}

func TestEditUsage(t *testing.T) {
	f := newEditFixture(t)
	t.Setenv("EDITOR", "true")
	from := filepath.Join(t.TempDir(), "rules.md")
	for _, args := range [][]string{{"edit", "charter"}, {"edit", "brief", stream}, {"edit", "spec", project}, {"edit", "charter", project, "--json"}, {"status", "--from", from}} {
		if code, _, _ := invoke(t, f.root, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
