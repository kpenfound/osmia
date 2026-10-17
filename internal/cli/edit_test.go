package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// editorScript writes an executable shell script whose body runs with the
// edited file as $1, and returns its path.
func editorScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor")
	must(t, os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body+"\n"), 0700))
	return path
}

// editDirectories lists the temporary edit directories osmia edit left in
// TMPDIR.
func editDirectories(t *testing.T, tmp string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(tmp, "osmia-edit-*"))
	must(t, err)
	return dirs
}

// keptFile returns the path osmia edit names in diag as keeping the edit.
func keptFile(t *testing.T, diag string) string {
	t.Helper()
	_, rest, ok := strings.Cut(diag, "Your edit is kept in ")
	if !ok {
		t.Fatalf("no kept file named:\n%s", diag)
	}
	path, _, _ := strings.Cut(rest, "\n")
	return path
}

func TestEditCommand(t *testing.T) {
	opts := withShed(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	t.Cleanup(c.Close)
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	charter := func() trace.Document {
		t.Helper()
		d, err := c.Charter(ctx, project)
		must(t, err)
		return d
	}
	draft := func() map[string]trace.Document {
		t.Helper()
		d, err := c.Documents(ctx, stream)
		must(t, err)
		return d
	}

	// Neither VISUAL nor EDITOR is set and no file is given.
	code, out, diag := invoke(t, root, "edit", "charter", project)
	if code != 4 || out != "" || !strings.Contains(diag, "set EDITOR") || !strings.Contains(diag, "--from") {
		t.Fatalf("no editor: %d %q %q", code, out, diag)
	}
	if charter().Revision != 1 {
		t.Fatal("an edit without an editor was recorded")
	}

	// The editor's change is recorded as the next revision, and the
	// temporary file is removed. VISUAL is preferred over EDITOR.
	t.Setenv("EDITOR", "false")
	t.Setenv("VISUAL", editorScript(t, `printf '1. Keep changes small.\n' >> "$1"`))
	before := charter()
	if out := successful(t, root, "edit", "charter", project); out != "Recorded charter.md revision 2.\n" {
		t.Fatalf("charter edit output %q", out)
	}
	if after := charter(); after.Revision != 2 || after.Content != before.Content+"1. Keep changes small.\n" || after.Actor != (trace.Actor{Kind: "owner", ID: "local"}) {
		t.Fatalf("recorded charter %+v", after)
	}
	if dirs := editDirectories(t, tmp); len(dirs) != 0 {
		t.Fatalf("a successful edit left %v", dirs)
	}

	// Unchanged content sends nothing.
	t.Setenv("VISUAL", "true")
	if out := successful(t, root, "edit", "charter", project); out != "No changes.\n" {
		t.Fatalf("unchanged output %q", out)
	}
	if charter().Revision != 2 || len(editDirectories(t, tmp)) != 0 {
		t.Fatal("an unchanged edit was recorded or left its file")
	}

	// --from reads the file and opens no editor.
	t.Setenv("VISUAL", "false")
	from := filepath.Join(t.TempDir(), "rules.md")
	must(t, os.WriteFile(from, []byte("1. Keep changes small.\n2. Test every fix.\n"), 0600))
	if out := successful(t, root, "edit", "charter", project, "--from", from); out != "Recorded charter.md revision 3.\n" {
		t.Fatalf("--from output %q", out)
	}
	if got := charter(); got.Revision != 3 || got.Content != "1. Keep changes small.\n2. Test every fix.\n" {
		t.Fatalf("--from charter %+v", got)
	}
	code, out, diag = invoke(t, root, "edit", "charter", project, "--from", filepath.Join(t.TempDir(), "missing.md"))
	if code != 2 || out != "" || !strings.Contains(diag, "cannot read") {
		t.Fatalf("missing --from file: %d %q %q", code, out, diag)
	}

	// A revision recorded while the owner edits is a conflict: the edit is
	// kept in its file, and retrying with --from records it.
	mark, release := filepath.Join(tmp, "editing"), filepath.Join(tmp, "release")
	t.Setenv("VISUAL", editorScript(t, `printf '3. Document every flag.\n' >> "$1"
touch `+mark+`
while [ ! -f `+release+` ]; do sleep 0.05; done`))
	type result struct {
		code      int
		out, diag string
	}
	done := make(chan result, 1)
	go func() {
		code, out, diag := invoke(t, root, "edit", "charter", project)
		done <- result{code, out, diag}
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the editor never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.EditCharter(ctx, project, service.CharterEdit{Revision: 3, Content: "1. Keep changes small.\n"}); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(release, nil, 0600))
	r := <-done
	if r.code != 5 || r.out != "" || !strings.Contains(r.diag, "conflict:") {
		t.Fatalf("conflicting edit: %d %q %q", r.code, r.out, r.diag)
	}
	kept := keptFile(t, r.diag)
	if !strings.Contains(r.diag, "osmia edit charter "+project+" --from "+kept) {
		t.Fatalf("no retry command:\n%s", r.diag)
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "1. Keep changes small.\n2. Test every fix.\n3. Document every flag.\n" {
		t.Fatalf("kept file %q %v", data, err)
	}
	if got := charter(); got.Revision != 4 || got.Content != "1. Keep changes small.\n" {
		t.Fatalf("a conflicting edit was recorded: %+v", got)
	}
	t.Setenv("VISUAL", "")
	if out := successful(t, root, "edit", "charter", project, "--from", kept); out != "Recorded charter.md revision 5.\n" {
		t.Fatalf("retry output %q", out)
	}

	// Editing the spec sends the plan unchanged at its current revision.
	const spec = "# Uploads\n\nUploads resume where they stopped.\n\n## Acceptance criteria\n\n1. Uploads resume after a restart.\n"
	t.Setenv("VISUAL", editorScript(t, `printf '`+strings.ReplaceAll(spec, "\n", `\n`)+`' > "$1"`))
	old := draft()
	if out := successful(t, root, "edit", "spec", stream); out != "Recorded spec.md revision 2.\n" {
		t.Fatalf("spec edit output %q", out)
	}
	edited := draft()
	if edited["spec"].Revision != 2 || edited["spec"].Content != spec || edited["plan"].Revision != old["plan"].Revision || edited["plan"].Content != old["plan"].Content {
		t.Fatalf("spec edit recorded %+v", edited)
	}

	// An invalid plan is refused with its problems, and the edit is kept.
	t.Setenv("VISUAL", editorScript(t, `printf 'not a plan\n' > "$1"`))
	code, out, diag = invoke(t, root, "edit", "plan", stream)
	if code != 4 || out != "" || !strings.HasPrefix(diag, "validation: ") {
		t.Fatalf("invalid plan: %d %q %q", code, out, diag)
	}
	kept = keptFile(t, diag)
	if data, err := os.ReadFile(kept); err != nil || string(data) != "not a plan\n" || filepath.Base(kept) != "plan.json" {
		t.Fatalf("kept plan %s %q %v", kept, data, err)
	}
	if got := draft(); got["plan"].Revision != old["plan"].Revision || got["spec"].Revision != 2 {
		t.Fatalf("an invalid plan was recorded: %+v", got)
	}

	// The arguments name what is edited.
	for _, args := range [][]string{{"edit", "charter"}, {"edit", "brief", stream}, {"edit", "spec", project}, {"edit", "charter", project, "--json"}, {"status", "--from", from}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
