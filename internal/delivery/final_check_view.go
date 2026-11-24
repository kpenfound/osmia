package delivery

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const finalChecksView = "final"

// FinalCheckFiles returns the files of the reader's view that carry the
// review's recorded check result, by their path in the view: the latest
// final/checks-<k>.json recorded for the reviewed commit and, for a failed
// Dagger Cloud result, the recorded trace output, or the recorded failure, of
// each of its failed checks that names a trace ID, under
// final/checks-<k>-traces/. It
// returns none when no record of the commit exists, as for a review of a
// delivered workstream.
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
	for name, content := range finalTraceFiles(docs, run) {
		files[name] = content
	}
	return files, nil
}
