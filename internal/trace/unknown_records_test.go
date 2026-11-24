package trace

import (
	"context"
	"strings"
	"testing"
)

func TestStoredRecordOfAnUnknownKindIsRefused(t *testing.T) {
	r, root, p := create(t)
	s := mustScoped(t, r, streamID)
	d := specimens()[0].(Document)
	if err := r.Append(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"osmia.trace.unknown-record","version":1}`
	storeExec(t, s, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES ((SELECT max(seq) FROM commits), ?, ?, ?, ?, ?)", recordPath(d), "osmia.trace.unknown-record", "unknown", 1, body)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if reopened != nil {
		defer reopened.Close()
	}
	want := `unsupported record schema "osmia.trace.unknown-record"`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("open error = %v, want %q", err, want)
	}
}

func TestActorOfAnUnknownKindIsRefused(t *testing.T) {
	for _, kind := range []string{"hive", "apiary"} {
		t.Run(kind, func(t *testing.T) {
			r, _, _ := create(t)
			d := specimens()[0].(Document)
			d.Actor = Actor{Kind: kind, ID: "someone"}
			if err := r.Append(context.Background(), d); err == nil {
				t.Fatalf("a record attributed to a %q actor was accepted", kind)
			}
		})
	}
}
