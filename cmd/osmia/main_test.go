package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
)

const argsEnv = "OSMIA_TEST_MAIN_ARGS"

func TestMainProcess(t *testing.T) {
	args := os.Getenv(argsEnv)
	if args == "" {
		return
	}
	os.Args = append([]string{"osmia"}, strings.Fields(args)...)
	main()
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func TestServeLoadsTheStoreBackedRootInRegistryOrderWithTheCurrentSettings(t *testing.T) {
	base, err := os.MkdirTemp("", "om-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	top := "version = 1\nworkspaces = \"git\"\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(top), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveRoot(root, "")
	if err != nil {
		t.Fatal(err)
	}
	store := projectstore.Open(resolved)
	ids := []config.ProjectID{"p_cccccccccccccccccccccccccccccccc", "p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "p_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	settings := func(id config.ProjectID, branch string) string {
		return fmt.Sprintf("version = 1\nupstream = \"upstream/%s\"\nclone = %q\nbase_branch = %q\n", id[len(id)-1:], filepath.Join(base, string(id)), branch)
	}
	for _, id := range ids {
		gitInit(t, filepath.Join(base, string(id)))
		if _, err := store.WriteFirst(id, []byte(settings(id, "main"))); err != nil {
			t.Fatal(err)
		}
		if err := store.Add(id); err != nil {
			t.Fatal(err)
		}
	}
	current, err := store.Current(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(ids[1], current.Pin, []byte(settings(ids[1], "trunk"))); err != nil {
		t.Fatal(err)
	}

	ready, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcess$")
	cmd.Env = append(os.Environ(), argsEnv+"=serve --root "+root, "OSMIA_SERVICE_READY_FD=3")
	cmd.ExtraFiles = []*os.File{release}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	release.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(ready).ReadString('\n')
		line <- text
	}()
	select {
	case text := <-line:
		if text != "ready\n" {
			t.Fatalf("the service did not report ready: %q", text)
		}
	case err := <-exited:
		t.Fatalf("the service exited before it was ready: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("the service was not ready within a minute")
	}

	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	view, err := c.Configuration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []config.ProjectID
	for _, s := range view.Effective.ActiveProjects {
		got = append(got, config.ProjectID(s))
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("active projects %v, want the registry order %v", got, ids)
	}
	branches := map[config.ProjectID]string{}
	for _, p := range view.Effective.Projects {
		branches[p.ID] = p.BaseBranch
	}
	if branches[ids[0]] != "main" || branches[ids[1]] != "trunk" || branches[ids[2]] != "main" {
		t.Fatalf("effective base branches %v: the current stored settings are not in effect", branches)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the service did not stop cleanly: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the service did not stop within a minute")
	}
}
