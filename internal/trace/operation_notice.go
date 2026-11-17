package trace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// OperationNoticeSubject is the workflow subject whose state is the latest
// notice an operation raised about its own attempts, such as a streak of
// failures. Each notice moves it to a state of its own, so the notice is
// recorded once however often the same attempt is replayed.
const OperationNoticeSubject = "operation-notice"

// OperationNotice is one notice to the chief of staff about an operation.
type OperationNotice struct {
	// ID names the notice: a retried write of the same ID records nothing more.
	ID string
	// Operation is the ID of the operation the notice is about; it is the cause
	// the notice records.
	Operation string
	// Actor raises the notice.
	Actor  Actor
	At     time.Time
	Reason string
	Body   string
}

// RecordOperationNotice records the notice as a transition of the workstream's
// OperationNoticeSubject with one outbox event for the chief of staff. The same
// ID recorded again is not an error and records nothing more.
func (r *Repository) RecordOperationNotice(ctx context.Context, stream config.WorkstreamID, n OperationNotice) error {
	id := "operation-notice-" + n.ID
	var err error
	// Another notice of the workstream can move the subject between the read
	// and the write; the write is then made again from the new state.
	for range 3 {
		var state WorkflowState
		if state, err = r.Workflow(stream, OperationNoticeSubject); err != nil {
			return err
		}
		h := Header{Schema: "osmia.trace.transition", Version: Version, ID: id, Revision: 1, Project: r.Project(), Workstream: stream, At: n.At, Actor: n.Actor, Cause: n.Operation}
		tx := Transaction{ExpectedVersion: state.Version,
			Transition: Transition{Header: h, Subject: OperationNoticeSubject, From: state.Value, To: id, Reason: n.Reason},
			Events:     []Event{Notice(id, "operation", n.Body)}}
		if _, err = r.Transact(ctx, tx); err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			break
		}
		if recorded, readErr := r.operationNoticeRecorded(stream, id); readErr == nil && recorded {
			return nil
		}
	}
	return fmt.Errorf("record the notice %s: %w", n.ID, err)
}

func (r *Repository) operationNoticeRecorded(stream config.WorkstreamID, id string) (bool, error) {
	transitions, err := Read[Transition](r, stream)
	if err != nil {
		return false, err
	}
	for _, t := range transitions {
		if t.ID == id {
			return true, nil
		}
	}
	return false, nil
}
