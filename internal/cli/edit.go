package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// editFiles names the file each editable document is edited in.
var editFiles = map[string]string{"charter": "charter.md", "spec": "spec.md", "plan": "plan.json"}

// editor returns the owner's editor command: VISUAL, or else EDITOR.
func editor() string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	return os.Getenv("EDITOR")
}

// runEdit edits the latest revision of a project's charter or a
// workstream's spec or plan, in the owner's editor or from the file from,
// and records the result as the next revision. A failed save keeps the
// edited file and names it, so the edit can be retried with --from.
func runEdit(ctx context.Context, c *service.Client, kind, target, from string, stdin io.Reader, stdout, stderr io.Writer) int {
	invalid := func() int { fmt.Fprintln(stderr, "invalid arguments; use osmia --help"); return 2 }
	command := editor()
	if from == "" && command == "" {
		fmt.Fprintln(stderr, "no editor is set; set EDITOR (or VISUAL), or pass the edited file with --from")
		return 4
	}
	var project config.ProjectID
	var stream config.WorkstreamID
	var current trace.Document
	var draft map[string]trace.Document
	var err error
	if kind == "charter" {
		if project, err = config.ParseProjectID(target); err != nil {
			return invalid()
		}
		if current, err = c.Charter(ctx, project); err != nil {
			return editFailure(stderr, err)
		}
	} else {
		if stream, err = config.ParseWorkstreamID(target); err != nil {
			return invalid()
		}
		if draft, err = c.Documents(ctx, stream); err != nil {
			return editFailure(stderr, err)
		}
		if draft["spec"].Revision < 1 || draft["plan"].Revision < 1 {
			fmt.Fprintf(stderr, "Validation: workstream %s has no drafted spec and plan to edit yet\n", stream)
			return 4
		}
		current = draft[kind]
	}
	var content []byte
	kept := ""
	if from != "" {
		if content, err = os.ReadFile(from); err != nil {
			fmt.Fprintln(stderr, "cannot read the file given with --from")
			return 2
		}
	} else {
		dir, err := os.MkdirTemp("", "osmia-edit-")
		if err != nil {
			fmt.Fprintln(stderr, "cannot create a temporary file to edit")
			return 1
		}
		kept = filepath.Join(dir, editFiles[kind])
		if err := os.WriteFile(kept, []byte(current.Content), 0600); err != nil {
			os.RemoveAll(dir)
			fmt.Fprintln(stderr, "cannot create a temporary file to edit")
			return 1
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", command+` "$@"`, command, kept)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(stderr, "the editor failed, so nothing was saved; your edit is kept in %s\nRetry with: osmia edit %s %s --from %s\n", kept, kind, target, kept)
			return 1
		}
		if content, err = os.ReadFile(kept); err != nil {
			fmt.Fprintf(stderr, "cannot read the edited file %s\n", kept)
			return 1
		}
	}
	done := func() {
		if kept != "" {
			os.RemoveAll(filepath.Dir(kept))
		}
	}
	if string(content) == current.Content {
		done()
		fmt.Fprintln(stdout, "No changes.")
		return 0
	}
	var recorded trace.Document
	if kind == "charter" {
		recorded, err = c.EditCharter(ctx, project, wire.CharterEdit{Revision: current.Revision, Content: string(content)})
	} else {
		edit := wire.DraftEdit{SpecRevision: draft["spec"].Revision, PlanRevision: draft["plan"].Revision, Spec: draft["spec"].Content, Plan: draft["plan"].Content}
		if kind == "spec" {
			edit.Spec = string(content)
		} else {
			edit.Plan = string(content)
		}
		var out map[string]trace.Document
		out, err = c.EditDraft(ctx, stream, edit)
		recorded = out[kind]
	}
	if err != nil {
		code := editFailure(stderr, err)
		if kept != "" {
			fmt.Fprintf(stderr, "Your edit is kept in %s\nRetry with: osmia edit %s %s --from %s\n", kept, kind, target, kept)
		}
		return code
	}
	done()
	fmt.Fprintf(stdout, "Recorded %s revision %d.\n", editFiles[kind], recorded.Revision)
	return 0
}

// editFailure reports a failed read or save of an edited document.
func editFailure(stderr io.Writer, err error) int {
	var api *wire.APIError
	if errors.As(err, &api) && api.Message == "" {
		switch api.Code {
		case wire.Validation:
			err = &wire.APIError{Code: api.Code, Message: "the edit is invalid; documents must be UTF-8 text within the size limit"}
		case wire.NotFound:
			err = &wire.APIError{Code: api.Code, Message: "not found; check the project or workstream ID with status"}
		}
	}
	return report(stderr, err, true)
}
