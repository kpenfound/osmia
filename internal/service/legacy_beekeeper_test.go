package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestStartOnFreshRootCreatesNoBeekeeperDirectory shows that starting the
// service on a root that has never held a Beekeeper creates no "beekeeper"
// directory and no shadow project: the removed feature leaves nothing
// behind on a fresh install.
func TestStartOnFreshRootCreatesNoBeekeeperDirectory(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	start(t, opts)
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "beekeeper")); !os.IsNotExist(err) {
		t.Fatalf("a fresh root got a beekeeper directory: %v", err)
	}
}

// TestStartWithAPreexistingBeekeeperDirectoryLeavesItUntouched shows that
// starting the service on a root that already holds a "beekeeper"
// directory, left behind by an earlier version, starts normally and leaves
// that directory's contents byte-for-byte unchanged: the service neither
// reads it nor deletes it.
func TestStartWithAPreexistingBeekeeperDirectoryLeavesItUntouched(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	dir := filepath.Join(opts.Config.Root, "beekeeper")
	must(t, os.MkdirAll(filepath.Join(dir, "workstreams", "w_bebebebebebebebebebebebebebebebe"), 0700))
	must(t, os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"legacy":"beekeeper state"}`), 0600))
	must(t, os.WriteFile(filepath.Join(dir, "workstreams", "w_bebebebebebebebebebebebebebebebe", "workstream.json"), []byte(`{"legacy":"thread"}`), 0600))
	before := snapshot(t, dir)

	start(t, opts)

	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("beekeeper directory changed:\nbefore %v\nafter  %v", before, after)
	}
}
