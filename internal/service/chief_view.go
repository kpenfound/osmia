package service

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefDocumentsGuidance tells every chief-of-staff turn what its view holds.
const chiefDocumentsGuidance = "Your read-only view holds the latest revision of every document this workstream records: spec.md, plan.json, what the owner handed in under handed/, the shed rounds under shed/, and the amendment, unit, final review and delivery records once they exist. Call file_read with a path to read a file or list a directory; \".\" lists the whole view. Read the documents a question or event concerns before you answer, escalate or report on it."

// stageChiefDocuments writes the latest revision of every document stream
// records, but its tool-call and inspection records, into workspace, replacing
// what an earlier turn staged there. It returns the top-level paths to select.
func stageChiefDocuments(r *trace.Repository, stream config.WorkstreamID, workspace string) ([]string, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if top, _, _ := strings.Cut(doc.Path, "/"); top == "tools" || top == "inspections" {
			continue
		}
		if prior, ok := latest[doc.Path]; !ok || doc.Revision >= prior.Revision {
			latest[doc.Path] = doc
		}
	}
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	var paths []string
	for _, name := range slices.Sorted(maps.Keys(latest)) {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(latest[name].Content), 0600); err != nil {
			return nil, err
		}
		if top, _, _ := strings.Cut(name, "/"); !slices.Contains(paths, top) {
			paths = append(paths, top)
		}
	}
	return paths, nil
}
