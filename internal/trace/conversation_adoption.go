package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

const conversationOriginPath = "tools/conversation-origin.json"

// ConversationOrigin pins the original design scope. Historical queue records
// keep this provenance even though subsequent turns live in the workgroup trace.
type ConversationOrigin struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
}

type conversationImport struct {
	Origin  ConversationOrigin `json:"origin"`
	Records map[string]string  `json:"records"`
}

// PrepareDesignWorkgroup reconciles a fixed workgroup identity and host. The
// selected members' own traces never move, and the design host is not a member.
func PrepareDesignWorkgroup(ctx context.Context, root config.Root, id config.WorkgroupID, origin ConversationOrigin, members []CrossRepoMember, at time.Time, actor Actor) (*Repository, Workgroup, error) {
	byStream, order, err := crossRepoMembers(members)
	if err != nil {
		return nil, Workgroup{}, err
	}
	directory, err := root.WorkgroupTrace(id)
	if err != nil {
		return nil, Workgroup{}, err
	}
	var r *Repository
	if _, err = os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		r, err = createWorkgroupTraceDirectory(ctx, root, id, at, actor, "design-start")
		if err == nil {
			err = r.Close()
			r = nil
			if err == nil {
				r, err = openSharedWorkgroupTrace(root, id, byStream)
			}
		}
	} else if err == nil {
		r, err = openSharedWorkgroupTrace(root, id, byStream)
	}
	if err != nil {
		if r != nil {
			err = errors.Join(err, r.Close())
		}
		return nil, Workgroup{}, err
	}
	g := Workgroup{ID: id, Members: append([]config.WorkstreamID{origin.Workstream}, order...), Conversation: &origin}
	// A crash may leave the host committed before its chief or group record.
	hostDirectory, err := root.WorkstreamTrace(r.Project(), origin.Workstream)
	if err == nil {
		if _, stat := os.Stat(hostDirectory); errors.Is(stat, os.ErrNotExist) {
			err = r.CreateWorkstream(ctx, origin.Workstream, at, actor)
		} else if stat != nil {
			err = stat
		}
	}
	if err != nil {
		return nil, Workgroup{}, errors.Join(err, r.Close())
	}
	groups, _, err := Workgroups(r)
	if err != nil {
		return nil, Workgroup{}, errors.Join(err, r.Close())
	}
	if len(groups) == 0 {
		err = recordWorkgroup(ctx, r, g, at, actor, "design-start")
	} else if len(groups) != 1 || !equalJSON(groups[0], g) {
		err = ErrConflict
	}
	if err != nil {
		return nil, Workgroup{}, errors.Join(err, r.Close())
	}
	return r, g, nil
}

// AdoptDesignConversation imports the durable queue and its exact original
// records in one trace publication. Import fingerprints authorize only those
// immutable foreign-scope records; they do not relax ordinary record writes.
// The caller fences dispatch until the source queue is retired and routing is
// committed. An exact retry leaves the imported queue unchanged.
func (r *Repository) AdoptDesignConversation(ctx context.Context, source *Repository, stream config.WorkstreamID) error {
	target, err := r.scoped(stream)
	if err != nil {
		return err
	}
	_, err = target.file("workstreams/" + string(stream) + "/" + conversationOriginPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	original, err := source.scoped(stream)
	if err != nil {
		return err
	}
	original.mu.Lock()
	log, _, err := original.loadWorkflow(stream)
	if err != nil {
		original.mu.Unlock()
		return err
	}
	chief, ok := log.Threads[ChiefOfStaff]
	if !ok {
		original.mu.Unlock()
		return fmt.Errorf("design conversation is missing")
	}
	records, _, err := original.scan()
	original.mu.Unlock()
	if err != nil {
		return err
	}
	var imports []Record
	for _, rec := range records {
		switch v := rec.(type) {
		case Agent:
			if v.ID == ChiefOfStaff {
				imports = append(imports, rec)
			}
		case TurnRequest:
			if v.AgentID == ChiefOfStaff {
				imports = append(imports, rec)
			}
		case TurnResponse:
			if v.AgentID == ChiefOfStaff {
				imports = append(imports, rec)
			}
		}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if _, err := target.file("workstreams/" + string(stream) + "/" + conversationOriginPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	targetLog, _, err := target.loadWorkflow(stream)
	if err != nil {
		return err
	}
	old := targetLog.Threads[ChiefOfStaff]
	if len(old.Turns) != 0 {
		return ErrConflict
	}
	targetLog.Threads[ChiefOfStaff] = chief
	imported := conversationImport{Origin: ConversationOrigin{Project: source.Project(), Workstream: stream}, Records: map[string]string{}}
	for _, rec := range imports {
		imported.Records[recordKey(rec)] = conversationRecordHash(rec)
	}
	content, err := json.Marshal(imported)
	if err != nil {
		return err
	}
	origin := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "conversation-origin", Revision: 1, Project: r.Project(), Workstream: stream, At: chief.Identity.At, Actor: chief.Identity.Actor, Cause: "design-start"}, Path: conversationOriginPath, Content: string(content)}
	if err := target.checkRevisions(ctx, []Record{origin}); err != nil {
		return err
	}
	// The imported identity and log replace the host's own, which hold no
	// turns, and the origin precedes the records it admits.
	prefix := "workstreams/" + string(stream) + "/agents/" + ChiefOfStaff + "/"
	c := change{replace: []string{prefix + "identity.jsonl", prefix + "log.jsonl"}, records: append([]Record{origin}, imports...), stream: stream, workflow: &targetLog, message: "Adopt design conversation"}
	return target.publish(ctx, c)
}

func conversationRecordHash(rec Record) string {
	data, _ := json.Marshal(rec)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// importedConversationRecord accepts a foreign scope only at its pinned
// original workstream and only with the immutable fingerprint written at adoption.
// It requires r.cache.mu, and remembers each workstream's import.
func (r *Repository) importedConversationRecord(ctx context.Context, rec Record) bool {
	h := rec.header()
	imported, ok := r.cache.origins[h.Workstream]
	if !ok {
		if d, err := r.latestDocument(ctx, r.reads, h.Workstream, conversationOriginPath); err == nil {
			var i conversationImport
			if json.Unmarshal([]byte(d.Content), &i) == nil {
				imported = &i
			}
		}
		r.cache.origins[h.Workstream] = imported
	}
	if imported == nil || imported.Origin.Project != h.Project || imported.Origin.Workstream != h.Workstream {
		return false
	}
	return imported.Records[recordKey(rec)] == conversationRecordHash(rec)
}

// RetireDesignConversation removes only the active source queue. Its identity,
// requests, responses and all other trace history stay readable at their
// existing location. The cross-repo locator must already have been committed.
func (r *Repository) RetireDesignConversation(ctx context.Context, stream config.WorkstreamID) error {
	if id, err := r.CrossRepoWorkgroupOf(stream); err != nil {
		return err
	} else if id == "" {
		return ErrConflict
	}
	source, err := r.scoped(stream)
	if err != nil {
		return err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	log, _, err := source.loadWorkflow(stream)
	if err != nil {
		return err
	}
	if _, ok := log.Threads[ChiefOfStaff]; !ok {
		return nil
	}
	delete(log.Threads, ChiefOfStaff)
	return source.saveThread(ctx, stream, log)
}

// DesignConversationImported reports whether the host has committed its queue.
func (r *Repository) DesignConversationImported(stream config.WorkstreamID) (bool, error) {
	source, err := r.scoped(stream)
	if err != nil {
		return false, err
	}
	_, err = source.file("workstreams/" + string(stream) + "/" + conversationOriginPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
