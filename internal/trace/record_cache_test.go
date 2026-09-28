package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestRecordReadsOwnMutableFields(t *testing.T) {
	r, _, _ := create(t)
	want := specimens()[3].(Ruling)
	if err := r.Append(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := Read[Ruling](r, streamID)
		if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("read = %#v, %v", got, err)
		}
		got[0].Changes[0] = "caller edit"
	}
}

func TestRecordReadsDetectChangedBytes(t *testing.T) {
	r, _, _ := create(t)
	d := specimens()[0].(Document)
	if err := r.Append(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := Read[Document](r, streamID); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(r.directory, recordPath(d))
	original, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		bytes.Replace(original, []byte(`"revision":1`), []byte(`"revision":9`), 1),
		append(bytes.Clone(original), []byte("{broken\n")...),
		original[:len(original)-1],
	} {
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if _, err := Read[Document](r, streamID); err == nil {
			t.Fatal("changed history accepted")
		}
	}
	if err := os.WriteFile(name, original, 0600); err != nil {
		t.Fatal(err)
	}
	checkTyped[Document](t, r, d)
	d.Revision++
	d.Content += "owner revision\n"
	if err := r.Append(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	got, err := Read[Document](r, streamID)
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got[1], d) {
		t.Fatalf("appended revision = %#v, %v", got, err)
	}
}

func TestRecordReadsRejectAliasesAfterCaching(t *testing.T) {
	for _, link := range []struct {
		name string
		make func(string, string) error
	}{{"symlink", os.Symlink}, {"hardlink", os.Link}} {
		t.Run(link.name, func(t *testing.T) {
			r, _, p := create(t)
			if _, err := Read[Document](r, streamID); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(r.directory, "workstreams", string(streamID), "documents.jsonl")
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(p.Clone, "records.jsonl")
			if err := os.WriteFile(external, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := link.make(external, name); err != nil {
				t.Fatal(err)
			}
			if _, err := Read[Document](r, streamID); err == nil {
				t.Fatal("aliased history accepted")
			}
		})
	}
}

func TestCloneRecordMutableFields(t *testing.T) {
	values := append(specimens(),
		Question{Escalation: &Escalation{Questions: []string{"q"}, Options: []string{"o"}}},
		Amendment{Citations: []string{"c"}, Upstream: &UpstreamMove{From: "a"}},
		Status{StatusContent: StatusContent{Agents: []string{"a"}}},
		PriorityChange{Order: []config.WorkstreamID{streamID}},
		TurnResponse{Result: coreadapter.SessionResult{
			Outcome:    &coreadapter.Outcome{Card: &coreadapter.Card{Headline: "h"}},
			ToolCounts: map[string]int{"tool": 1}, Limit: &coreadapter.ProviderLimit{Kind: "quota"},
		}, Classification: &TurnClassification{ToolCounts: map[string]int{"tool": 1}},
			ClassifierUsage: []coreadapter.Usage{{Turns: 1}}, Stop: &TurnStop{Cause: "pause"}},
	)
	for _, original := range values {
		before, _ := json.Marshal(original)
		copy := cloneRecord(original)
		if !reflect.DeepEqual(original, copy) {
			t.Fatalf("clone differs for %T", original)
		}
		mutateReferences(reflect.ValueOf(copy))
		after, _ := json.Marshal(original)
		if !bytes.Equal(before, after) {
			t.Fatalf("clone shares mutable state for %T: %s", original, after)
		}
	}
}

// Walk all reference fields so additions to records participate in the alias test.
func mutateReferences(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			mutateReferences(v.Elem())
			v.Elem().SetZero()
		}
	case reflect.Slice:
		if v.Len() > 0 {
			mutateReferences(v.Index(0))
			v.Index(0).SetZero()
		}
	case reflect.Map:
		v.Clear()
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				mutateReferences(v.Field(i))
			}
		}
	}
}

func TestDecodeRejectsAmbiguousJSON(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `{"a":{"b":1,"b":2}}`,
		`{"a":[{"b":1,"b":2}]}`, `{"a":1} {"a":2}`, `{"a":1} junk`,
		`{"a":`, `{"unknown":1}`,
	} {
		t.Run(input, func(t *testing.T) {
			var out struct {
				A any `json:"a"`
			}
			if err := decode([]byte(input), &out); err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
	var out map[string]any
	if err := decode([]byte(`{"a":{"x":1},"b":{"x":2}}`), &out); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRecordDecoding(b *testing.B) {
	values := specimens()
	d := values[0].(Document)
	d.Content = strings.Repeat("trace content\n", 100)
	values[0] = d
	var data []byte
	for i := range 100 {
		line, err := json.Marshal(values[i%len(values)])
		if err != nil {
			b.Fatal(err)
		}
		data = append(data, append(line, '\n')...)
	}
	for _, cached := range []bool{false, true} {
		name := "uncached"
		if cached {
			name = "cached"
		}
		b.Run(name, func(b *testing.B) {
			r := &Repository{recordFiles: map[string]recordFile{}}
			if cached {
				r.recordFiles["records.jsonl"] = r.decodedRecords("records.jsonl", bytes.Clone(data))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				file := r.decodedRecords("records.jsonl", data)
				for _, record := range file.records {
					_ = cloneRecord(record.record)
				}
			}
		})
	}
}
