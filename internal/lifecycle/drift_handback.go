package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const HandBackDriftTool = "hand_back_drift"

// DriftHandback is the document drift/handback-<k>.json: who handed the
// workstream's held drift rebase Held back, and the note the mason and the
// reviewer of drift rebase Drift, which answers it, receive.
type DriftHandback struct {
	Drift int    `json:"drift"`
	Held  int    `json:"held"`
	Note  string `json:"note"`
	By    string `json:"by"`
}

// DriftHandbackPrefix begins the trace record ID of every drift handback.
const DriftHandbackPrefix = "drift-handback-"

func DriftHandbackPath(k int) string {
	return fmt.Sprintf("drift/handback-%d.json", k)
}
func DriftHandbackDocument(k int) string {
	return fmt.Sprintf("%s%d", DriftHandbackPrefix, k)
}

// HandBackDrift asks, as actor, for the drift rebase that follows the
// workstream's held drift rebase, with a note its mason and reviewer receive.
// The handback and the request are recorded together, and the next pass reads
// the drift schedule. It returns the number of the drift rebase that answers
// it.
func (s *Controller) HandBackDrift(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, note string, actor trace.Actor, turn string, at time.Time) (int, *APIError) {
	note = strings.TrimSpace(note)
	if note == "" {
		return 0, &APIError{Code: Validation, Message: "a handback requires a note for the drift mason and reviewer"}
	}
	states, err := repo.WorkflowStates(stream)
	if err != nil {
		return 0, &APIError{Code: Internal, Message: "cannot read the workstream's drift rebases"}
	}
	value := states[DriftSubject].Value
	if !strings.HasPrefix(value, DriftHeld+"-") {
		return 0, &APIError{Code: Conflict, Message: "the workstream's latest drift rebase is not held"}
	}
	held, err := DriftNumber(value)
	if err != nil {
		return 0, &APIError{Code: Internal, Message: err.Error()}
	}
	k := held + 1
	current := states[DriftRequestSubject]
	if requested, err := DriftNumber(current.Value); err != nil {
		return 0, &APIError{Code: Internal, Message: err.Error()}
	} else if requested >= k {
		return 0, &APIError{Code: Conflict, Message: fmt.Sprintf("drift rebase %d is already asked for", k)}
	}
	by := rulerName(actor)
	data, err := json.MarshalIndent(DriftHandback{Drift: k, Held: held, Note: note, By: by}, "", "  ")
	if err != nil {
		return 0, &APIError{Code: Internal, Message: err.Error()}
	}
	cause := turn
	if cause == "" {
		cause = fmt.Sprintf("%s-%s", DriftSubject, value)
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DriftHandbackDocument(k), Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause}
	doc := trace.Document{Header: h, Path: DriftHandbackPath(k), Content: string(data) + "\n"}
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", fmt.Sprintf("%s-%d", DriftRequestSubject, k), doc.ID
	reason := fmt.Sprintf("the %s handed held drift rebase %d back; drift rebase %d of %s onto %s of %s answers it, and its mason and reviewer receive the note: %s", by, held, k, featureBranch(stream), s.About(repo).Project.BaseBranch, s.About(repo).Project.Upstream, note)
	tx := trace.Transaction{ExpectedVersion: current.Version, Transition: trace.Transition{Header: h, Subject: DriftRequestSubject, From: current.Value, To: fmt.Sprintf("requested-%d", k), Reason: reason}}
	if _, err := repo.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return 0, &APIError{Code: Conflict, Message: "the workstream's drift rebases changed since they were read; read them again"}
		}
		return 0, &APIError{Code: Internal, Message: "cannot record the handback"}
	}
	s.DriftAsked.Store(true)
	return k, nil
}

// ChiefHandbacks counts the handbacks the chief of staff recorded on the
// workstream since the owner last asked for one of its drift rebases or
// handed one back.
func ChiefHandbacks(repo *trace.Repository, stream config.WorkstreamID) (int, error) {
	records, err := trace.Read[trace.Record](repo, stream)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			if r.Subject == DriftRequestSubject && r.Actor.Kind == ownerActor.Kind {
				n = 0
			}
		case trace.Document:
			if !strings.HasPrefix(r.ID, DriftHandbackPrefix) {
				continue
			}
			if r.Actor.Kind == ownerActor.Kind {
				n = 0
			} else {
				n++
			}
		}
	}
	return n, nil
}

// DriftHandbackNote is what the mason and the reviewer of drift rebase k
// are told of the handback it answers, or "" when it answers none.
func DriftHandbackNote(repo *trace.Repository, stream config.WorkstreamID, k int) (string, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return "", err
	}
	for _, d := range docs {
		if d.ID != DriftHandbackDocument(k) {
			continue
		}
		var handback DriftHandback
		if err := json.Unmarshal([]byte(d.Content), &handback); err != nil {
			return "", fmt.Errorf("%s: %w", d.Path, err)
		}
		return fmt.Sprintf("\n\nDrift rebase %d was held for the owner, and the %s handed it back with this note, which this drift rebase answers: %s", handback.Held, handback.By, handback.Note), nil
	}
	return "", nil
}

// HandBackDrift returns the hand_back_drift tool of one claimed
// chief-of-staff turn. The chief of staff hands a held drift rebase back on
// the owner's behalf with a recorded recovery decision; in a turn answering the
// owner, it records the owner's own handback. A handback the service refuses
// is an ordinary result, {"recorded":false,"reason":...}, and records
// nothing. A shared chief of staff names the member whose drift rebase it
// hands back.
func (c *Controls) HandBackDrift(repository *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	tool := coreadapter.Tool{Name: HandBackDriftTool, Effect: coreadapter.ToolMemory, Description: "Hand this workstream's held drift rebase back: the service asks for another drift rebase onto upstream, and its drift mason and drift reviewer receive your note. Use it when the drift rebase was held and you know what the mason or the reviewer should do differently; the reason it was held is in drift/rebase.json and the workstream's feed. " + "note: what the drift mason should resolve differently, or what the drift reviewer should take into account, which both receive. Hand back only when you are confident the note resolves the problem; otherwise leave the held drift rebase to the owner. owner_decided: true only when the owner asked for it in the message this turn answers, with the owner's words as the note. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.", InputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"},"owner_decided":{"type":"boolean"},"workstream":{"type":"string"}},"required":["note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Note         string              `json:"note"`
			OwnerDecided bool                `json:"owner_decided"`
			Workstream   config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("drift handbacks are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		if gone, err := abandoned(target, stream); err != nil {
			return nil, err
		} else if gone {
			return priorityRefusal("an abandoned workstream takes no drift rebase")
		}
		actor := chiefActor
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a handback the owner asked for in the message this turn answers")
			}
			actor = turn.Actor
		}
		k, api := s.HandBackDrift(ctx, target, stream, input.Note, actor, scope.Turn, now())
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return priorityRefusal(api.Message)
		}
		return json.Marshal(struct {
			Recorded bool `json:"recorded"`
			Drift    int  `json:"drift"`
		}{true, k})
	}
	return tool
}
