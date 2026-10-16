package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// memberQuestionHistory is routing evidence captured before owner retirement.
// Original questions and rulings remain in their original scope; this document
// preserves their exact revisions and links when those traces are unavailable.
type memberQuestionHistory struct {
	Workgroup  config.WorkgroupID  `json:"workgroup"`
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Questions  []QuestionState     `json:"questions"`
	Rulings    []Ruling            `json:"rulings"`
}

func questionHistoryID(stream config.WorkstreamID) string {
	return "member-questions-" + string(stream)
}

// PreserveMemberQuestions records the available routing evidence in the original
// group authority before archive. Repeating identical evidence is a no-op.
func (r *Repository) PreserveMemberQuestions(ctx context.Context, group Workgroup, member CrossRepoMember, at time.Time, actor Actor) error {
	if !slices.Contains(group.Members[1:], member.Workstream) {
		return ErrConflict
	}
	return member.Project.WithMemberQuestionSnapshot(member.Workstream, func(snapshot MemberQuestionSnapshot) error {
		return r.PreserveMemberQuestionSnapshot(ctx, group, member, snapshot, at, actor)
	})
}

// MemberQuestionSnapshot is an exact routing view captured under its member
// mutex. It is evidence for the original group authority, never a new ruling.
type MemberQuestionSnapshot struct {
	Feature   WorkflowState
	Workgroup config.WorkgroupID
	Documents []Document
	Questions []QuestionState
	Rulings   []Ruling
}

// WithMemberQuestionSnapshot serializes retirement publication with question
// writes. The callback must not resolve or lock this member again.
func (r *Repository) WithMemberQuestionSnapshot(stream config.WorkstreamID, commit func(MemberQuestionSnapshot) error) error {
	member, err := r.lockQuestionRead(stream)
	if err != nil {
		return err
	}
	defer member.mu.Unlock()
	_, v, err := member.loadWorkflow(stream)
	if err != nil {
		return err
	}
	records, _, err := member.scan()
	if err != nil {
		return err
	}
	snapshot := MemberQuestionSnapshot{Feature: v.states[FeatureSubject], Workgroup: config.WorkgroupID(v.states[CrossRepoWorkgroupSubject].Value), Questions: questions(records, v, stream)}
	for _, record := range records {
		if doc, ok := record.(Document); ok && doc.Workstream == stream {
			snapshot.Documents = append(snapshot.Documents, doc)
		}
		if ruling, ok := record.(Ruling); ok && ruling.Workstream == stream {
			snapshot.Rulings = append(snapshot.Rulings, ruling)
		}
	}
	return commit(snapshot)
}

// PreserveMemberQuestionSnapshot publishes an already captured view without
// resolving the member, so retirement can commit while holding its mutex.
func (r *Repository) PreserveMemberQuestionSnapshot(ctx context.Context, group Workgroup, member CrossRepoMember, snapshot MemberQuestionSnapshot, at time.Time, actor Actor) error {
	if !slices.Contains(group.Members[1:], member.Workstream) {
		return ErrConflict
	}
	evidence := memberQuestionHistory{Workgroup: group.ID, Project: member.Project.Project(), Workstream: member.Workstream, Questions: snapshot.Questions, Rulings: snapshot.Rulings}
	if err := validateMemberQuestionHistory(evidence); err != nil {
		return err
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	docs, err := Read[Document](r, group.Members[0])
	if err != nil {
		return err
	}
	id := questionHistoryID(member.Workstream)
	revision := 0
	for _, d := range slices.Backward(docs) {
		if d.ID != id {
			continue
		}
		revision = d.Revision
		if d.Content == string(data) {
			return nil
		}
		break
	}
	return r.RecordDocuments(ctx, []Document{{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: revision + 1, Project: r.Project(), Workstream: group.Members[0], At: at, Actor: actor, Cause: "member-archive"}, Path: "tools/" + id + ".json", Content: string(data)}})
}

func (r *Repository) retiredQuestionHistory(stream config.WorkstreamID) (*memberQuestionHistory, error) {
	groups, _, err := Workgroups(r)
	if err != nil {
		return nil, err
	}
	if len(groups) != 1 || len(groups[0].Members) < 2 {
		return nil, ErrConflict
	}
	g := groups[0]
	binding, ok := r.memberBinding(stream)
	if !ok {
		return nil, ErrConflict
	}
	docs, err := Read[Document](r, g.Members[0])
	if err != nil {
		return nil, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != questionHistoryID(stream) {
			continue
		}
		var evidence memberQuestionHistory
		if err := json.Unmarshal([]byte(d.Content), &evidence); err != nil {
			return nil, err
		}
		if evidence.Workgroup != g.ID || evidence.Workstream != stream || evidence.Project != binding.Project.Project() {
			return nil, fmt.Errorf("%w: retired question routing identity mismatch", ErrConflict)
		}
		if err := validateMemberQuestionHistory(evidence); err != nil {
			return nil, err
		}
		return &evidence, nil
	}
	return nil, &RetiredMemberError{Project: binding.Project.Project(), Workstream: stream}
}

func (r *Repository) retiredQuestions(stream config.WorkstreamID) ([]QuestionState, error) {
	history, err := r.retiredQuestionHistory(stream)
	if err != nil {
		return nil, err
	}
	return history.Questions, nil
}

// questionHistory keeps the original authority across a live-to-retired read.
// Only a verified terminal binding permits the preserved routing view.
func (r *Repository) questionHistory(stream config.WorkstreamID) ([]QuestionState, bool, error) {
	retired, err := r.MemberRetired(stream)
	if err != nil {
		return nil, false, err
	}
	if !retired {
		states, readErr := r.Questions(stream)
		if readErr == nil {
			return states, false, nil
		}
		retired, err = r.retiredRead(stream, readErr)
		if err != nil || !retired {
			return nil, false, err
		}
	}
	states, err := r.retiredQuestions(stream)
	return states, true, err
}

// QuestionRuling reads an exact original ruling revision. Archived routing
// evidence supports reads only; it does not authorize a retired member effect.
func (r *Repository) QuestionRuling(stream config.WorkstreamID, id string, revision int) (Ruling, error) {
	retired, err := r.MemberRetired(stream)
	if err != nil {
		return Ruling{}, err
	}
	if !retired {
		ruling, readErr := Get[Ruling](r, stream, id, revision)
		if readErr == nil {
			return ruling, nil
		}
		retired, err = r.retiredRead(stream, readErr)
		if err != nil || !retired {
			return Ruling{}, err
		}
	}
	history, err := r.retiredQuestionHistory(stream)
	if err != nil {
		return Ruling{}, err
	}
	for _, ruling := range history.Rulings {
		if ruling.ID == id && ruling.Revision == revision {
			return ruling, nil
		}
	}
	return Ruling{}, &RetiredMemberError{Project: history.Project, Workstream: stream}
}

func validateMemberQuestionHistory(evidence memberQuestionHistory) error {
	for _, q := range evidence.Questions {
		if err := validate(q.Asked); err != nil {
			return err
		}
		if err := validate(q.Latest); err != nil {
			return err
		}

		if q.Latest.Project != evidence.Project || q.Latest.Workstream != evidence.Workstream || q.Latest.Unit != q.Asked.Unit || q.Latest.Thread != q.Asked.Thread || q.Latest.Turn != q.Asked.Turn || q.Latest.AskedBy != q.Asked.AskedBy || q.Latest.Question != q.Asked.Question {
			return ErrConflict
		}
		switch q.State {
		case QuestionOpen, QuestionEscalated, QuestionRouted:
			if q.Ruling != nil {
				return ErrConflict
			}
		case QuestionRuled:
			if q.Ruling == nil || q.Ruling.Decision != DecisionRuling || q.Ruling.ReturnedAnswer != "" {
				return ErrConflict
			}
		case QuestionAnswered:
			if q.Ruling == nil || q.Ruling.ReturnedAnswer == "" {
				return ErrConflict
			}
		default:
			return ErrConflict
		}

		if q.Ruling != nil {
			if err := validate(*q.Ruling); err != nil {
				return err
			}
			if q.Ruling.Project != evidence.Project || q.Ruling.Workstream != evidence.Workstream || q.Ruling.QuestionID != q.Asked.ID || q.Ruling.QuestionRevision != q.Latest.Revision {
				return ErrConflict
			}
		}
		if q.Asked.Project != evidence.Project || q.Asked.Workstream != evidence.Workstream || q.Latest.ID != q.Asked.ID || q.Latest.Revision < q.Asked.Revision {
			return fmt.Errorf("%w: retired question routing pins mismatch", ErrConflict)
		}
	}
	for _, ruling := range evidence.Rulings {
		if err := validate(ruling); err != nil {
			return err
		}
		if ruling.Project != evidence.Project || ruling.Workstream != evidence.Workstream {
			return ErrConflict
		}
	}
	return nil
}
