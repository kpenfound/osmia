package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeptRecord is a Hearsay record kept in the project's trace after the trace
// it was built from is gone: a cleaned workstream's, or a removed project's.
// Data is the record exactly as it is posted. Released is set once the record
// no longer needs keeping, because it was posted or because a live trace
// supplies it again; a released entry holds no Data.
type KeptRecord struct {
	Source   string          `json:"source"`
	Key      string          `json:"key"`
	Data     json.RawMessage `json:"data,omitempty"`
	Released bool            `json:"released,omitempty"`
}

// KeptID is the document ID holding the KeptRecord of one artifact.
func KeptID(source, key string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + key))
	return "hearsay-kept-" + hex.EncodeToString(sum[:16])
}

func hearsayKeptPath(parts []string) bool {
	return len(parts) == 2 && parts[0] == "hearsay" && strings.HasSuffix(parts[1], ".json") && strings.HasPrefix(parts[1], "hearsay-kept-") && key(strings.TrimSuffix(parts[1], ".json"))
}

// HearsayKept returns the latest KeptRecord of every artifact the service
// has kept, released entries included. r must be the project's own
// repository.
func (r *Repository) HearsayKept() (map[string]KeptRecord, error) {
	docs, err := Read[Document](r, "")
	kept := map[string]KeptRecord{}
	latest := map[string]Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.ID, "hearsay-kept-") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	for id, d := range latest {
		var k KeptRecord
		if jerr := json.Unmarshal([]byte(d.Content), &k); jerr != nil || KeptID(k.Source, k.Key) != id {
			err = errors.Join(err, fmt.Errorf("%s: invalid kept record", d.Path))
			continue
		}
		kept[id] = k
	}
	return kept, err
}

// KeepHearsay commits entries as the artifacts' latest kept records, in one
// commit. An entry equal to the recorded one is skipped, so keeping the same
// entries again writes nothing.
func (r *Repository) KeepHearsay(ctx context.Context, entries []KeptRecord, at time.Time) error {
	if r.stream != "" {
		return errors.New("kept records belong to the project's own trace")
	}
	if len(entries) == 0 {
		return nil
	}
	docs, err := Read[Document](r, "")
	if err != nil {
		return err
	}
	revisions := map[string]Document{}
	for _, d := range docs {
		if d.Revision > revisions[d.ID].Revision {
			revisions[d.ID] = d
		}
	}
	var ids []string
	latest := map[string]KeptRecord{}
	for _, e := range entries {
		if e.Source == "" || e.Key == "" {
			continue
		}
		id := KeptID(e.Source, e.Key)
		if _, ok := latest[id]; !ok {
			ids = append(ids, id)
		}
		latest[id] = e
	}
	var out []Document
	for _, id := range ids {
		e := latest[id]
		prior := revisions[id]
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if prior.Revision > 0 {
			var p KeptRecord
			if json.Unmarshal([]byte(prior.Content), &p) == nil && p.Released == e.Released && bytes.Equal(p.Data, e.Data) {
				continue
			}
		}
		out = append(out, Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: prior.Revision + 1, Project: r.project, At: at, Actor: Actor{Kind: "service", ID: "hearsay"}, Cause: "hearsay-keep"}, Path: "hearsay/" + id + ".json", Content: string(data)})
	}
	if len(out) == 0 {
		return nil
	}
	return r.RecordDocuments(ctx, out)
}
