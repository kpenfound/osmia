package questions_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// decideFallback leaves the decide result of question id of the workstream as a
// fallback, as the decide tool does while Jev is unconfigured, unless a result
// is already recorded.
func decideFallback(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) {
	t.Helper()
	if err := jevfixture.DecideQuestion(repo, stream, id); err != nil {
		t.Fatal(err)
	}
}

// decided wraps the chief of staff's outcome tools so a call first leaves a
// fallback decide result for each question it names.
func decided(t *testing.T, repo *trace.Repository, host config.WorkstreamID, list []coreadapter.Tool) []coreadapter.Tool {
	return jevfixture.AutoDecide(repo, host, list)
}

// judgments returns the judgment records of the fixture's workstream.
func (f fixture) judgments(t *testing.T) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	var out []trace.Document
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "judgments/") {
			out = append(out, d)
		}
	}
	return out
}
