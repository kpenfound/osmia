package lifecycle

import (
	"context"
	"fmt"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// MergeArchiver archives, as the chief of staff, each delivered workstream of
// its project whose current own pull request is durably recorded merged.
type MergeArchiver struct {
	S          *Controller
	Repository *trace.Repository
	mu         sync.Mutex
	// refused is the reason this instance last logged for not archiving a
	// workstream, so a steady refusal is logged once, not every pass.
	refused map[config.WorkstreamID]string
}

// Pass selects the delivered, unarchived workstreams whose recorded own pull
// request outcome is merged for the current publication, and archives each
// through the owner's archive path. It reads only recorded evidence and makes
// no pull request lookup. An attempt the archive refuses, because the
// evidence changed, a workgroup change holds the archive or the workstream
// changed underneath it, writes nothing and is retried by the next pass.
func (m *MergeArchiver) Pass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(m.Repository)
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, archived := m.S.ArchivedWorkstream(string(stream)); archived {
			continue
		}
		state, err := m.Repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if state.Value != DeliveredState {
			continue
		}
		evidence, err := ReadMemberEvidence(m.Repository, stream)
		if err != nil {
			return err
		}
		if !mergedOwnPullRequest(state.Value, evidence) {
			continue
		}
		if err := m.S.Step("auto-archive-selected"); err != nil {
			return err
		}
		_, api := m.S.ArchiveOnMerge(string(stream))
		if api == nil {
			m.logRefusal(stream, "")
			continue
		}
		m.logRefusal(stream, api.Message)
	}
	return nil
}

// logRefusal logs reason for stream the first time it differs from what this
// instance last logged for it. An empty reason clears what was logged.
func (m *MergeArchiver) logRefusal(stream config.WorkstreamID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reason == "" {
		delete(m.refused, stream)
		return
	}
	if m.refused == nil {
		m.refused = map[config.WorkstreamID]string{}
	}
	if m.refused[stream] == reason {
		return
	}
	m.refused[stream] = reason
	m.S.Log().Info(fmt.Sprintf("the chief of staff did not archive workstream %s of project %s yet: %s", stream, m.Repository.Project(), reason), "component", "archive_on_merge", "project", string(m.Repository.Project()), "workstream", string(stream), "reason", reason)
}
