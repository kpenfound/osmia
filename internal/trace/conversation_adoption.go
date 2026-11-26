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

// conversationImport lists the records a design start imported into a
// workgroup trace before the chief of staff began a thread of its own at
// Start. Such traces still hold them, and reading them keeps working.
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

func conversationRecordHash(rec Record) string {
	data, _ := json.Marshal(rec)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

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
