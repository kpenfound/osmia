package router_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The step journals only recover from crashes: they commit their next step from
// what the trace and the disk already say, and never ask Jev. The router is the
// only path to Jev's transition judgments, and a file that neither imports the
// router nor Jev, nor calls the router's entry points, cannot
// ask it. Each journal's files are listed here, so a journal that came to depend
// on either fails this test.
var stepJournals = map[string][]string{
	"project registration and creation": {"../service/projects.go", "../service/project_creation.go"},
	"workgroup-change journal":          {"../lifecycle/workgroup_change.go", "../lifecycle/workgroup_change_apply.go", "../lifecycle/workgroup_change_turns.go"},
	"publication operations":            {"../delivery/publication.go", "../delivery/draft_publication.go"},
	"outbox delivery":                   {"../trace/workflow.go"},
	"turn admission":                    {"../scheduler/scheduler.go", "../scheduler/reservations.go", "../scheduler/pause.go"},
}

func TestStepJournalsNeverCallJev(t *testing.T) {
	t.Parallel()
	forbidden := []string{"github.com/kpenfound/osmia/internal/router", "github.com/kpenfound/osmia/internal/jev", "github.com/kpenfound/osmia/internal/jev/jevfixture"}
	for journal, files := range stepJournals {
		for _, file := range files {
			if _, err := os.Stat(file); err != nil {
				t.Errorf("%s: %s is listed as a part of the journal but does not exist: %v", journal, file, err)
				continue
			}
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, imp := range parsed.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				for _, bad := range forbidden {
					if path == bad {
						t.Errorf("%s: %s imports %s, so the journal could ask Jev; a step journal commits its next step on its own", journal, filepath.Base(file), path)
					}
				}
			}
			ast.Inspect(parsed, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "DecideTransition", "RouteExit", "SubmitExit":
					t.Errorf("%s: %s calls %s", journal, filepath.Base(file), sel.Sel.Name)
				}
				return true
			})
		}
	}
}
