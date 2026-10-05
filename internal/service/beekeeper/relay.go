package beekeeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// RelayActor is the provenance the service records when it mirrors a chief
// of staff's finished reply into the shadow project's trace: the service
// relaying it, never the chief of staff's own project or the Beekeeper
// itself, and never an owner or agent action.
var RelayActor = trace.Actor{Kind: "service", ID: "beekeeper-relay"}

// RelayedReply is one chief of staff's finished reply to a message the
// Beekeeper sent it, recorded once in the shadow project's trace, keyed by
// the Beekeeper message it answers. It does not start a Beekeeper turn.
type RelayedReply struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Text       string              `json:"text"`
	At         time.Time           `json:"at"`
}

// relayPathPrefix names the documents RecordRelayedReply writes. A relayed
// reply is recorded as a project-level notice document, the shape the trace
// already accepts for a record that informs a subsequent turn: it carries no
// workstream of its own, since the shadow project's trace validation does
// not accept a "relay/" path. relayKey's "relay_" prefix keeps these
// distinguishable from any other notice, so RelayedReplies can find them.
const relayPathPrefix = "notices/"

// relayKey derives the deterministic document identity of the relayed
// reply to the chief-of-staff request requestID answers, scoped to its
// project and workstream, so recording it twice leaves one record.
func relayKey(project config.ProjectID, workstream config.WorkstreamID, requestID string) string {
	sum := sha256.Sum256([]byte(string(project) + "\x00" + string(workstream) + "\x00" + requestID))
	return "relay_" + hex.EncodeToString(sum[:16])
}

// RecordRelayedReply records a chief of staff's finished reply to the
// Beekeeper message whose chief-of-staff turn request is identified by
// requestID, once, in the shadow repository's trace, attributed to
// workstream's chief of staff. A relayed reply already recorded for this
// project, workstream and requestID is left as it is; the call still
// reports success.
func RecordRelayedReply(ctx context.Context, shadow *trace.Repository, project config.ProjectID, workstream config.WorkstreamID, requestID, text string, at time.Time) error {
	id := relayKey(project, workstream, requestID)
	content, err := json.Marshal(RelayedReply{Workstream: workstream, Text: text, At: at})
	if err != nil {
		return err
	}
	doc := trace.Document{
		Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1,
			Project: config.ShadowProjectID, At: at, Actor: RelayActor, Cause: requestID},
		Path:    relayPathPrefix + id + ".json",
		Content: string(content),
	}
	if err := shadow.Append(ctx, doc); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		return err
	}
	return nil
}

// RelayedReplies returns every relayed reply recorded in the shadow
// project's trace, oldest first.
func RelayedReplies(shadow *trace.Repository) ([]RelayedReply, error) {
	docs, err := trace.Read[trace.Document](shadow, "")
	if err != nil {
		return nil, err
	}
	out := make([]RelayedReply, 0, len(docs))
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, relayPathPrefix) {
			continue
		}
		var r RelayedReply
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
