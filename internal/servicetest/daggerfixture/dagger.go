// Package daggerfixture provides an executable Dagger double. Its address
// matching is independent of checkselect's grouping implementation.
package daggerfixture

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	PackageA = "dag+check://?check=test&go-module=.&go-package=a"
	TestB    = "dag+check://?check=test&go-module=.&go-package=b&go-test=T1"
	Version  = "dag+check://release/version"
	Refusal  = "addresses with different filters require a union of workspace views; use shared filter flags"
)

// Leaves contains two tests per package, including identical names in different
// packages, so widening or a cross-product introduces observable extra checks.
var Leaves = []string{
	PackageA + "&go-test=T1", PackageA + "&go-test=T2",
	"dag+check://?check=test&go-module=.&go-package=b&go-test=T1",
	"dag+check://?check=test&go-module=.&go-package=b&go-test=T2",
	Version,
}

type Config struct {
	Links    []string
	Leaves   []string
	Fail     string
	RefuseOn string
	ExitOn   string
	Delay    time.Duration
	Attempts []Attempt
}

// Attempt scripts a command's outcome after the CLI has validated its links.
// A failure reports only the checks up to it, modelling --fail-fast cancellation.
type Attempt struct {
	Fail     string
	Refusal  string
	ExitCode int
	Delay    time.Duration
}

type Event struct {
	Kind   string
	At     time.Time
	Args   []string
	Links  []string
	Active int
}

type CLI struct {
	Log    string
	leaves []string
}

// Install launches this test binary as a fake dagger process. The importing
// test package supplies TestDaggerFixtureProcess calling Process.
func Install(t *testing.T, config Config) CLI {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(config)
	must(t, err)
	path := filepath.Join(dir, "config.json")
	must(t, os.WriteFile(path, data, 0600))
	binary, err := os.Executable()
	must(t, err)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexec " + quote(binary) + " -test.run=^TestDaggerFixtureProcess$ -- osmia-fake-dagger " + quote(path) + " \"$@\"\n"
	must(t, os.WriteFile(filepath.Join(dir, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	leaves := config.Leaves
	if leaves == nil {
		leaves = Leaves
	}
	return CLI{Log: path + ".log", leaves: leaves}
}

// Process serves the fake executable only when invoked by Install's wrapper.
func Process() {
	i := slices.Index(os.Args, "osmia-fake-dagger")
	if i < 0 {
		return
	}
	path := os.Args[i+1]
	var c Config
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &c) != nil {
		os.Exit(98)
	}
	os.Exit(run(c, path+".log", os.Stdout, os.Stderr, os.Args[i+2:]))
}

// Validate rejects the address shape the real CLI refuses, before any report.
func Validate(links []string) error {
	var first *url.URL
	for _, link := range links {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		if first != nil && (u.Scheme != first.Scheme || !reflect.DeepEqual(u.Query(), first.Query())) {
			return fmt.Errorf("%s", Refusal)
		}
		first = u
	}
	return nil
}

func run(c Config, log string, stdout, stderr io.Writer, args []string) int {
	if slices.Equal(args, []string{"list", "checks", "--all", "--format=link"}) {
		for _, link := range c.Links {
			fmt.Fprintln(stdout, link)
		}
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		return 97
	}
	var links, skip []string
	parallel := ""
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--parallel":
			i++
			if i < len(args) {
				parallel = args[i]
			}
		case args[i] == "--skip":
			i++
			if i < len(args) {
				skip = append(skip, args[i])
			}
		case strings.HasPrefix(args[i], "dag"):
			links = append(links, args[i])
		}
	}
	if parallel != "4" {
		fmt.Fprintln(stderr, "Error: parallel must be 4")
		return 96
	}
	event := func(kind string, selected []string) {
		f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(98)
		}
		defer f.Close()
		_ = json.NewEncoder(f).Encode(Event{Kind: kind, At: time.Now(), Args: args, Links: selected, Active: min(4, len(selected))})
	}
	if err := Validate(links); err != nil {
		event("refused", nil)
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	leaves := c.Leaves
	if leaves == nil {
		leaves = Leaves
	}
	selected := selectedFrom(links, leaves)
	selected = slices.DeleteFunc(selected, func(l string) bool { return slices.Contains(selectedFrom(skip, leaves), l) && len(skip) > 0 })
	var attempt Attempt
	if c.Attempts != nil {
		index := 0
		if f, err := os.Open(log); err == nil {
			decoder := json.NewDecoder(f)
			for {
				var e Event
				if decoder.Decode(&e) != nil {
					break
				}
				if e.Kind == "start" {
					index++
				}
			}
			f.Close()
		}
		if index >= len(c.Attempts) {
			fmt.Fprintln(stderr, "unexpected extra command")
			return 95
		}
		attempt = c.Attempts[index]
		c.Fail = attempt.Fail
		c.Delay = attempt.Delay
	}
	event("start", selected)
	time.Sleep(c.Delay)
	defer event("end", selected)
	if attempt.Refusal != "" {
		fmt.Fprintln(stderr, "Error:", attempt.Refusal)
		return 1
	}
	if attempt.ExitCode != 0 {
		fmt.Fprintln(stderr, "engine closed session")
		return attempt.ExitCode
	}
	if c.Attempts != nil && len(selected) == 0 {
		fmt.Fprintln(stderr, "Error: no checks selected; use 'dagger check -l' to list available checks")
		return 1
	}
	for _, link := range links {
		if c.RefuseOn != "" && strings.Contains(link, c.RefuseOn) {
			fmt.Fprintln(stderr, "Error:", Refusal)
			return 1
		}
		if c.ExitOn != "" && strings.Contains(link, c.ExitOn) {
			fmt.Fprintln(stderr, "engine closed session")
			return 7
		}
	}
	fmt.Fprintln(stdout, "Full trace at https://dagger.cloud/fixture/traces/"+fmt.Sprint(len(selected)))
	failed := slices.Contains(selected, c.Fail)
	if failed {
		fmt.Fprintln(stdout, "CHECKS  ✘ 1 failed")
	} else {
		fmt.Fprintf(stdout, "CHECKS  ✔ %d passed\n", len(selected))
	}
	for _, link := range selected {
		if link == c.Fail {
			fmt.Fprintln(stdout, "✘", link, "1s ERROR\n    wrong answer")
			if c.Attempts != nil {
				break
			}
		} else {
			fmt.Fprintln(stdout, "✔", link, "1s OK")
		}
	}
	if failed {
		return 3
	}
	return 0
}

// Selected interprets an OR of addresses over Leaves, using URL query values
// as OR within each dimension and AND between dimensions.
func Selected(links []string) []string {
	return selectedFrom(links, Leaves)
}

func selectedFrom(links, leaves []string) []string {
	var selected []string
	for _, leaf := range leaves {
		l, _ := url.Parse(leaf)
		matches := len(links) == 0
		for _, link := range links {
			u, err := url.Parse(link)
			if err != nil {
				continue
			}
			if u.Host != "" && u.Host != l.Host {
				continue
			}
			if u.Path != "" && u.Path != l.Path {
				continue
			}
			ok := true
			for key, values := range u.Query() {
				if !slices.Contains(values, l.Query().Get(key)) {
					ok = false
				}
			}
			matches = matches || ok
		}
		if matches {
			selected = append(selected, leaf)
		}
	}
	return selected
}

func (c CLI) Events(t *testing.T) []Event {
	t.Helper()
	f, err := os.Open(c.Log)
	must(t, err)
	defer f.Close()
	var events []Event
	decoder := json.NewDecoder(f)
	for {
		var e Event
		err := decoder.Decode(&e)
		if err == io.EOF {
			return events
		}
		must(t, err)
		events = append(events, e)
	}
}

// AssertRun verifies the selected leaves and the aggregate concurrency from
// actual command start/end events, also checking the flags of every command.
func (c CLI) AssertRun(t *testing.T, links []string) {
	t.Helper()
	var ran []string
	active, peak := 0, 0
	for _, e := range c.Events(t) {
		if !slices.Contains(e.Args, "--parallel") || !slices.Contains(e.Args, "4") {
			t.Fatalf("flags %q", e.Args)
		}
		switch e.Kind {
		case "start":
			active += e.Active
			peak = max(peak, active)
			ran = append(ran, e.Links...)
		case "end":
			active -= e.Active
		default:
			t.Fatalf("unexpected event %+v", e)
		}
	}
	slices.Sort(ran)
	ran = slices.Compact(ran)
	want := selectedFrom(links, c.leaves)
	slices.Sort(want)
	if active != 0 || peak > 4 || !slices.Equal(ran, want) {
		t.Fatalf("ran %q, want %q; active=%d peak=%d", ran, want, active, peak)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
