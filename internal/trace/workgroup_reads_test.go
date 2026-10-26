package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/config"
	"reflect"
	"testing"
)

func TestWorkgroupApprovalReadsDecodeOnlyLatestRecord(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wt, g := createCrossRepoWorkgroup(t, root, r1, r2)
	host := g.Members[0]
	scoped := mustScoped(t, wt, host)
	ctx := context.Background()
	if _, found, err := ApprovedWorkgroupRevision(wt, host); err != nil || found {
		t.Fatalf("absent revision: %v %v", found, err)
	}
	if _, _, found, err := ReadGroupApproval(wt, host); err != nil || found {
		t.Fatalf("absent approval: %v %v", found, err)
	}
	docs := make([]Document, 1200)
	for i := range docs {
		docs[i] = Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: fmt.Sprintf("noise-%d", i), Revision: 1, Project: wt.Project(), Workstream: host, At: at, Actor: owner, Cause: "fixture"}, Path: fmt.Sprintf("tools/noise-%d.json", i), Content: "{}"}
	}
	if err := wt.RecordDocuments(ctx, docs); err != nil {
		t.Fatal(err)
	}
	spec := ProjectSpec{Behavior: "Approved contract", Slices: []Slice{{Workstream: crossMember1}, {Workstream: crossMember2}}}
	for _, change := range []string{"first", "second"} {
		if err := RecordWorkgroupRevision(ctx, wt, g, change, spec, at, owner); err != nil {
			t.Fatal(err)
		}
	}
	decision := GroupApproval{Workgroup: g.ID, ProjectSpec: spec, Members: []GroupMemberApproval{{Project: r1.Project(), Workstream: crossMember1, Record: json.RawMessage(`{}`)}}}
	if err := RecordGroupApproval(ctx, wt, host, decision, at, owner); err != nil {
		t.Fatal(err)
	}
	decision.Complete = true
	if err := RecordGroupApproval(ctx, wt, host, decision, at, owner); err != nil {
		t.Fatal(err)
	}
	// Empty the whole-record cache, as on a freshly opened host handle.
	scoped.cache.mu.Lock()
	scoped.resetCache()
	scoped.cache.mu.Unlock()
	for range 3 {
		before := scoped.decoded.Load()
		revision, found, err := ApprovedWorkgroupRevision(wt, host)
		if err != nil || !found || revision.Change != "second" || !reflect.DeepEqual(revision.Spec, spec) {
			t.Fatalf("revision=%+v found=%v err=%v", revision, found, err)
		}
		if n := scoped.decoded.Load() - before; n != 1 {
			t.Fatalf("revision decoded %d records", n)
		}
		before = scoped.decoded.Load()
		got, doc, found, err := ReadGroupApproval(wt, host)
		if err != nil || !found || doc.Revision != 2 || !reflect.DeepEqual(got, decision) {
			t.Fatalf("approval=%+v doc=%+v found=%v err=%v", got, doc, found, err)
		}
		if n := scoped.decoded.Load() - before; n != 1 {
			t.Fatalf("approval decoded %d records", n)
		}
	}
	// Identical retries remain no-ops; conflicting retries remain conflicts.
	before, err := wt.CommitCount(string(host))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordWorkgroupRevision(ctx, wt, g, "second", spec, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := RecordGroupApproval(ctx, wt, host, decision, at, owner); err != nil {
		t.Fatal(err)
	}
	after, err := wt.CommitCount(string(host))
	if err != nil || after != before {
		t.Fatalf("retry commits %d -> %d: %v", before, after, err)
	}
	conflict := spec
	conflict.Behavior = "Conflicting intent"
	if err := RecordWorkgroupRevision(ctx, wt, g, "second", conflict, at, owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("revision conflict: %v", err)
	}
	decision.Complete = false
	if err := RecordGroupApproval(ctx, wt, host, decision, at, owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval conflict: %v", err)
	}
	d, err := wt.latestDocumentByID(host, WorkgroupRevisionDocument, WorkgroupRevisionPath)
	if err != nil || d.Revision != 2 {
		t.Fatalf("revision number=%d: %v", d.Revision, err)
	}
}

func TestWorkgroupApprovalReadsRejectDamagedLatestRecord(t *testing.T) {
	for _, id := range []string{WorkgroupRevisionDocument, GroupApprovalDocument} {
		for _, damage := range []string{"outer-json", "content-json", "content-type", "identity", "stored-revision", "record-validation"} {
			t.Run(id+"/"+damage, func(t *testing.T) {
				r, _, _ := create(t)
				s := mustScoped(t, r, streamID)
				path := WorkgroupRevisionPath
				content := `{"change":"approved","spec":{"behavior":"contract"}}`
				if id == GroupApprovalDocument {
					path = GroupApprovalPath
					content = `{"complete":true,"members":[{}]}`
				}
				d := Document{Header: header("document", id), Path: path, Content: content}
				for rev := 1; rev <= 2; rev++ {
					d.Revision = rev
					if err := r.RecordDocuments(context.Background(), []Document{d}); err != nil {
						t.Fatal(err)
					}
				}
				body := "{"
				switch damage {
				case "content-json":
					d.Content = "{"
				case "content-type":
					d.Content = `{"change":false,"complete":"yes"}`
				case "identity":
					d.ID = "other-document"
				case "stored-revision":
					d.Revision = 1
				case "record-validation":
					d.Revision = 0
				}
				if damage != "outer-json" {
					data, err := json.Marshal(d)
					if err != nil {
						t.Fatal(err)
					}
					body = string(data)
				}
				if _, err := s.db.Exec("UPDATE records SET body = ? WHERE id = ? AND revision = 2", body, id); err != nil {
					t.Fatal(err)
				}
				before := s.decoded.Load()
				if id == WorkgroupRevisionDocument {
					if _, _, err := ApprovedWorkgroupRevision(r, streamID); err == nil {
						t.Fatal("damaged latest revision accepted")
					}
				} else {
					if _, _, _, err := ReadGroupApproval(r, streamID); err == nil {
						t.Fatal("damaged latest approval accepted")
					}
				}
				if n := s.decoded.Load() - before; n != 1 {
					t.Fatalf("decoded %d records", n)
				}
			})
		}
	}
}

func TestWorkgroupResolutionHintTracksReopenAndNewMembers(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	wt, g := createCrossRepoWorkgroup(t, root, r1, r2)
	ctx := context.Background()
	mark := func() {
		t.Helper()
		if _, err := MarkWorkgroupsResolved(ctx, wt, []Workgroup{g}, func(_ config.WorkstreamID) (bool, error) { return true, nil }, at, owner, "fixture-resolution"); err != nil {
			t.Fatal(err)
		}
		if !WorkgroupResolutionCurrent(g.ID) {
			t.Fatal("recorded resolution not cached")
		}
	}
	mark()
	if err := ReopenWorkgroup(ctx, wt, g.ID, at, owner, "approved-reopen"); err != nil {
		t.Fatal(err)
	}
	if WorkgroupResolutionCurrent(g.ID) {
		t.Fatal("reopen did not invalidate hint")
	}
	mark()
	extra := config.WorkstreamID("w_00000000000000000000000000000023")
	if err := r1.CreateWorkstream(ctx, extra, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := AddWorkgroupMember(ctx, wt, g.ID, extra, at, owner, "approved-followup"); err != nil {
		t.Fatal(err)
	}
	if WorkgroupResolutionCurrent(g.ID) {
		t.Fatal("new member did not invalidate hint")
	}
}
