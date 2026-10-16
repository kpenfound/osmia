package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestRepoCharterUsesProjectAuthorityAndPreservesLegacyHistory(t *testing.T) {
	primary, root, p := create(t)
	ctx := context.Background()
	member := p
	member.ID = "p_00000000000000000000000000000002"
	// An existing repo's independent charter remains historical evidence.
	legacy, err := Create(ctx, root, member, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	old, err := legacy.EditCharter(ctx, 1, "1. Historical member rule.\n", at)
	if err != nil {
		t.Fatal(err)
	}
	before := charterRevisions(t, legacy)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	member.GroupID = p.ID
	repo, err := Open(root, member)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if _, err := repo.Charter(ctx, at); err == nil {
		t.Fatal("unbound member used its local charter")
	}
	repo.SetCharterResolver(func(id config.ProjectID) (*Repository, error) {
		if id != p.ID {
			t.Fatalf("resolved %s", id)
		}
		return primary, nil
	})
	current, err := primary.EditCharter(ctx, 1, "1. Project policy.\n", at)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Charter(ctx, at)
	if err != nil || got != current {
		t.Fatalf("member charter: %+v %v", got, err)
	}
	got, err = repo.EditCharter(ctx, current.Revision, "1. Shared edited policy.\n", at)
	if err != nil || got.Project != p.ID || got.Revision != 3 {
		t.Fatalf("member edit: %+v %v", got, err)
	}
	if _, err := primary.EditCharter(ctx, current.Revision, "stale", at); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if direct, err := primary.Charter(ctx, at); err != nil || direct != got {
		t.Fatalf("default charter: %+v %v", direct, err)
	}
	if after := charterRevisions(t, repo); !reflect.DeepEqual(before, after) {
		t.Fatalf("member history changed: %+v", after)
	}
	if data, err := os.ReadFile(filepath.Join(repo.directory, "charter.md")); err != nil || string(data) != old.Content {
		t.Fatalf("legacy file: %q %v", data, err)
	}
	docs, err := repo.ProjectDocuments()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Path == "charter.md" && d.Project != p.ID {
			t.Fatalf("local charter in context: %+v", d)
		}
	}
	local := old
	local.Revision++
	if err := repo.Append(ctx, local); !errors.Is(err, ErrConflict) {
		t.Fatalf("competing charter writer: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = Open(root, member)
	if err != nil {
		t.Fatal(err)
	}
	repo.SetCharterResolver(func(config.ProjectID) (*Repository, error) { return primary, nil })
	if reopened, err := repo.Charter(ctx, at); err != nil || reopened != got {
		t.Fatalf("reopened charter: %+v %v", reopened, err)
	}
	// No fallback to legacy policy when the authority cannot be read.
	if err := os.Remove(filepath.Join(primary.directory, "charter.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Charter(ctx, at); err == nil {
		t.Fatal("missing authority fell back to local policy")
	}
}

func TestNewMemberRepoHasNoCharterOfItsOwn(t *testing.T) {
	primary, root, p := create(t)
	p.GroupID, p.ID = p.ID, "p_00000000000000000000000000000002"
	repo, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if _, err := os.Stat(filepath.Join(repo.directory, "charter.md")); !os.IsNotExist(err) {
		t.Fatalf("member charter exists: %v", err)
	}
	if docs := charterRevisions(t, repo); len(docs) != 0 {
		t.Fatalf("member charter revisions: %+v", docs)
	}
	repo.SetCharterResolver(func(config.ProjectID) (*Repository, error) { return primary, nil })
	if d, err := repo.Charter(context.Background(), at); err != nil || d.Project != p.GroupID {
		t.Fatalf("shared charter: %+v %v", d, err)
	}
}
