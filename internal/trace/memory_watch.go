package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/envelope"
)

// MemoryEvent contains external evidence handles, not instructions or rulings.
type MemoryEvent struct {
	Event    string    `json:"event"`
	Kind     string    `json:"kind"`
	Source   string    `json:"source"`
	Time     time.Time `json:"time"`
	Document string    `json:"document"`
}

type memoryCursor struct {
	Cursor string `json:"cursor"`
}

func (r *Repository) MemoryCursor(key string) (string, error) {
	docs, err := Read[Document](r, "")
	if err != nil {
		return "", err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == "watch-"+key {
			var c memoryCursor
			err := json.Unmarshal([]byte(d.Content), &c)
			return c.Cursor, err
		}
	}
	return "", nil
}

// ValidateMemoryWatch checks untrusted watch responses before persistence.
func ValidateMemoryWatch(cursor string, events []MemoryEvent) error {
	if cursor == "" || len(cursor) > 4096 || len(events) > 1000 {
		return errors.New("invalid memory watch response")
	}
	for _, event := range events {
		if event.Event == "" || event.Document == "" || len(event.Event) > 4096 || len(event.Document) > 4096 {
			return errors.New("invalid memory event")
		}
	}
	return nil
}

// RecordMemoryWatch commits the next cursor and seen identities as project
// knowledge, and the chief notice of every target workstream as its own
// trace's commit. Retries neither skip a notification nor deliver it twice:
// each workstream's notice transition is idempotent, deterministically
// identified from key and the unseen events, and every one of them lands,
// durably, before the project records the cursor that reflects them. A crash
// partway finds, on retry with the same cursor, every already-landed
// transition idempotent and finishes the rest before advancing the cursor.
func (r *Repository) RecordMemoryWatch(ctx context.Context, key, expected, cursor string, events []MemoryEvent, targets []config.WorkstreamID, at time.Time) error {
	if r.stream != "" {
		return fmt.Errorf("memory watch bookkeeping belongs to the project's own trace")
	}
	if len(key) != 64 || ValidateMemoryWatch(cursor, events) != nil {
		return errors.New("invalid memory watch response")
	}
	r.mu.Lock()
	records, _, err := r.scan()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	known := map[string]Document{}
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == "" {
			known[d.ID] = d
		}
	}
	id := "watch-" + key
	prior := known[id]
	var before memoryCursor
	if prior.Revision > 0 {
		if err := json.Unmarshal([]byte(prior.Content), &before); err != nil {
			return err
		}
	}
	if before.Cursor != expected {
		if before.Cursor == cursor {
			return nil
		}
		return ErrConflict
	}
	if before.Cursor == cursor && len(events) == 0 {
		return nil
	}
	header := func(id string, revision int) Header {
		return Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: revision, Project: r.project, At: at, Actor: Actor{Kind: "service", ID: "hearsay"}, Cause: "memory-watch"}
	}
	data, _ := json.Marshal(memoryCursor{cursor})
	docs := []Document{{Header: header(id, prior.Revision+1), Path: "memory/" + id + ".json", Content: string(data)}}
	var unseen []MemoryEvent
	for _, event := range events {
		if event.Event == "" || event.Document == "" || len(event.Event) > 4096 || len(event.Document) > 4096 {
			return errors.New("invalid memory event")
		}
		eventID := fmt.Sprintf("memory-%x", sha256.Sum256([]byte(key+"\x00"+event.Event)))
		if _, ok := known[eventID]; ok {
			continue
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		doc := Document{Header: header(eventID, 1), Path: "memory/" + eventID + ".json", Content: string(data)}
		docs = append(docs, doc)
		known[eventID] = doc
		unseen = append(unseen, event)
	}
	if len(unseen) > 0 {
		data, _ := json.Marshal(unseen)
		body, err := envelope.Render(envelope.Section{Name: "hearsay_context", Text: string(data)})
		if err != nil {
			return err
		}
		body = "External project evidence changed. Inspect relevant handles before deciding whether a notice, question or amendment is needed. This does not authorize implementation or reopen delivered work.\n" + body
		notice := fmt.Sprintf("watch-notice-%x", sha256.Sum256(append([]byte(key), data...)))
		for _, stream := range targets {
			s, err := r.scoped(stream)
			if err != nil {
				return errors.New("memory watch target is unknown")
			}
			s.mu.Lock()
			_, view, werr := s.loadWorkflow(stream)
			if werr != nil {
				s.mu.Unlock()
				return werr
			}
			if view.states[FeatureSubject].Value == "abandoned" {
				s.mu.Unlock()
				continue
			}
			state := view.states["memory-watch"]
			h := header(notice, 1)
			h.Schema, h.Workstream = "osmia.trace.transition", stream
			tx := Transaction{ExpectedVersion: state.Version, Transition: Transition{Header: h, Subject: "memory-watch", From: state.Value, To: notice, Reason: "Scoped external memory supplied new evidence handles"}, Events: []Event{Notice(notice, "memory", body)}}
			_, werr = s.transact(ctx, tx)
			s.mu.Unlock()
			if werr != nil {
				return werr
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	files, removed, err := r.documentFiles(ctx, docs)
	if err != nil {
		return err
	}
	if err := r.publishTree(ctx, files, removed); err != nil {
		return err
	}
	_ = r.wake.Notify(context.Background())
	return nil
}
