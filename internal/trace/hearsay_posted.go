package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PostedRecord is what the service last successfully posted for one
// artifact: the digest of its content and the update time it carried. Source
// and Key identify the artifact. An empty Digest is a cleared entry, which
// the next pass posts again.
type PostedRecord struct {
	Source    string    `json:"source"`
	Key       string    `json:"key"`
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PostedID is the document ID holding the PostedRecord of one artifact.
func PostedID(source, key string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + key))
	return "hearsay-posted-" + hex.EncodeToString(sum[:16])
}

func hearsayPostedPath(parts []string) bool {
	return len(parts) == 2 && parts[0] == "hearsay" && strings.HasSuffix(parts[1], ".json") && strings.HasPrefix(parts[1], "hearsay-posted-") && key(strings.TrimSuffix(parts[1], ".json"))
}

// HearsayPosted returns the latest PostedRecord of every artifact the
// service has posted, keyed by PostedID. The digests live in the project's
// own trace and are never themselves posted. r must be the project's own
// repository.
func (r *Repository) HearsayPosted() (map[string]PostedRecord, error) {
	docs, err := Read[Document](r, "")
	posted := map[string]PostedRecord{}
	latest := map[string]Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.ID, "hearsay-posted-") && d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	for id, d := range latest {
		var p PostedRecord
		if jerr := json.Unmarshal([]byte(d.Content), &p); jerr != nil || PostedID(p.Source, p.Key) != id {
			err = errors.Join(err, fmt.Errorf("%s: invalid posted record", d.Path))
			continue
		}
		posted[id] = p
	}
	return posted, err
}

// RecordHearsayPosted commits entries as the artifacts' latest posted
// digests, in one commit. An entry equal to the recorded one is skipped, so
// recording the same entries again writes nothing.
func (r *Repository) RecordHearsayPosted(ctx context.Context, entries []PostedRecord, at time.Time) error {
	if r.stream != "" {
		return errors.New("posted digests belong to the project's own trace")
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
	// An artifact posted in several versions arrives as several entries; the
	// last is the one that was posted most recently.
	var ids []string
	latest := map[string]PostedRecord{}
	for _, e := range entries {
		id := PostedID(e.Source, e.Key)
		if e.Source == "" || e.Key == "" {
			continue
		}
		if _, ok := latest[id]; !ok {
			ids = append(ids, id)
		}
		latest[id] = e
	}
	var out []Document
	for _, id := range ids {
		e := latest[id]
		prior := revisions[id]
		if prior.Revision > 0 {
			var p PostedRecord
			if json.Unmarshal([]byte(prior.Content), &p) == nil && p.Digest == e.Digest && p.UpdatedAt.Equal(e.UpdatedAt) {
				continue
			}
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out = append(out, Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: prior.Revision + 1, Project: r.project, At: at, Actor: Actor{Kind: "service", ID: "hearsay"}, Cause: "hearsay-publish"}, Path: "hearsay/" + id + ".json", Content: string(data)})
	}
	if len(out) == 0 {
		return nil
	}
	return r.RecordDocuments(ctx, out)
}
