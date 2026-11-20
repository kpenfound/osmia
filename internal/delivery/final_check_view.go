package delivery

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// finalChecksView is the directory of the final reader's view that holds the
// recorded check result.
const finalChecksView = "final"

// FinalCheckFiles returns the files of the reader's view that carry the
// review's recorded check result, by their path in the view: the latest
// final/checks-<k>.json recorded for the reviewed commit, and the output file
// that record names (OutputPath at OutputRevision). It returns none when no
// record of the commit exists, as for a review of a delivered workstream.
func FinalCheckFiles(repository *trace.Repository, stream config.WorkstreamID, report FinalReport) (map[string]string, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var record *trace.Document
	for _, d := range slices.Backward(docs) {
		if d.ID == FinalChecksDocument(report.Review) {
			record = &d
			break
		}
	}
	if record == nil {
		return nil, nil
	}
	var run FinalCheckRun
	if err := json.Unmarshal([]byte(record.Content), &run); err != nil {
		return nil, fmt.Errorf("%s revision %d: %w", record.Path, record.Revision, err)
	}
	if run.Commit != report.Commit {
		return nil, nil
	}
	files := map[string]string{record.Path: record.Content}
	if run.OutputPath == "" {
		return files, nil
	}
	for _, d := range slices.Backward(docs) {
		if d.Path == run.OutputPath && (run.OutputRevision == 0 || d.Revision == run.OutputRevision) {
			files[d.Path] = d.Content
			return files, nil
		}
	}
	return nil, fmt.Errorf("%s names the check output %s revision %d, which the trace does not record", record.Path, run.OutputPath, run.OutputRevision)
}
