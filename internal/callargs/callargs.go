// Package callargs holds the rules for argument lists that agents or project
// settings supply to a `dagger call`: the lists are checked before any Dagger
// process starts, so an agent cannot reach host-writing steps, client options,
// provider schemes or paths outside the exported source.
package callargs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

var cliFlags = strings.Fields(`help workdir verbose quiet silent debug progress interactive interactivecommand web noexit autoapply dotoutput dotfocusfield dotshowinternal cloud scaleout mod nomod allowllm eagerruntime output json engine workspace loadmodule shellonerror shellcommandonerror env xrelease profile profileebpf lock`)

var clientVariables = strings.Fields(`PATH HOME TMPDIR NO_COLOR DOCKER_HOST _EXPERIMENTAL_DAGGER_RUNNER_HOST DAGGER_ENGINE DAGGER_X_RELEASE DAGGER_CLOUD_TOKEN`)

// Name normalises an option or step name for comparison: case, dashes,
// underscores, dots and spaces are ignored.
func Name(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(s))
}

// Elements returns every value an argument can stand for: the argument itself,
// the parts of an option=value pair, comma-separated items, and the parts
// before a # or ? suffix, with and without quotes and surrounding spaces.
func Elements(arg string) []string {
	values := func(value string) []string {
		out := []string{value}
		if strings.Contains(value, ",") {
			out = append(out, strings.Split(value, ",")...)
		}
		for _, part := range out {
			for _, separator := range []string{"#", "?"} {
				if base, _, ok := strings.Cut(part, separator); ok {
					out = append(out, base)
				}
			}
		}
		return out
	}
	split := func(value string) []string {
		if strings.HasPrefix(strings.TrimSpace(value), "-") {
			if name, rest, ok := strings.Cut(value, "="); ok {
				return append([]string{name}, values(rest)...)
			}
		}
		return values(value)
	}
	elements := split(arg)
	normalized := strings.NewReplacer(`"`, "", "'", "", "`", "").Replace(arg)
	if normalized != arg {
		elements = append(elements, split(normalized)...)
	}
	for _, element := range elements {
		if trimmed := strings.TrimSpace(element); trimmed != element {
			elements = append(elements, trimmed)
		}
	}
	return elements
}

// Validate checks agent-supplied arguments before an operation is pinned.
// Errors name the arguments as subject, for example "preview call".
func Validate(subject string, call []string) error {
	if len(call) == 0 {
		return fmt.Errorf("%s must be a non-empty argument list", subject)
	}
	for _, arg := range call {
		if strings.TrimSpace(arg) == "" || strings.ContainsFunc(arg, unicode.IsControl) {
			return fmt.Errorf("%s arguments must be non-empty and contain no control characters", subject)
		}
		for _, element := range Elements(arg) {
			if element == "" {
				return fmt.Errorf("%s values must not be empty", subject)
			}
			if strings.HasPrefix(element, "-") {
				name, _, _ := strings.Cut(element, "=")
				for _, flag := range cliFlags {
					if Name(name) == flag {
						return fmt.Errorf("%s cannot use CLI option %s", subject, name)
					}
				}
				if !strings.HasPrefix(element, "--") {
					if _, err := strconv.ParseFloat(element, 64); err != nil {
						return fmt.Errorf("%s cannot use shorthand CLI options", subject)
					}
				} else if element == "--" {
					return fmt.Errorf("%s cannot end CLI option parsing", subject)
				}
			}
			if strings.Contains(element, ":") {
				return fmt.Errorf("%s values cannot use URI or provider schemes", subject)
			}
			path := strings.ReplaceAll(element, `\`, "/")
			if strings.HasPrefix(path, "/") || filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
				return fmt.Errorf("%s paths must stay within the exported source", subject)
			}
			for _, part := range strings.Split(path, "/") {
				if part == ".." {
					return fmt.Errorf("%s paths cannot contain parent-directory components", subject)
				}
			}
			variableName, _, _ := strings.Cut(element, "?")
			for _, variable := range clientVariables {
				if variableName == variable {
					return fmt.Errorf("%s cannot name a Dagger client environment variable", subject)
				}
			}
			switch Name(element) {
			case "export", "terminal", "up", "apply", "tunnel", "findup", "host", "unixsocket":
				return fmt.Errorf("%s cannot use host-writing or interactive step %s", subject, element)
			}
		}
	}
	last := strings.TrimSpace(strings.NewReplacer(`"`, "", "'", "", "`", "").Replace(call[len(call)-1]))
	if strings.HasPrefix(last, "-") && !strings.Contains(last, "=") {
		if _, err := strconv.ParseFloat(last, 64); err != nil {
			return fmt.Errorf("%s must not end in a flag without an explicit =value", subject)
		}
	}
	return nil
}

// ValidateSource checks the same values through the exported source's
// root. Root.Stat follows symlinks while refusing any escape, even through a
// nonexistent descendant. It does not open files, sockets or devices.
func ValidateSource(subject, dir string, call []string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, arg := range call {
		for _, element := range Elements(arg) {
			_, err := root.Stat(element)
			if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return fmt.Errorf("%s path cannot resolve outside the exported source or through an unreadable symlink", subject)
			}
		}
	}
	return nil
}

// RunIDFlag is the option through which the service gives a lever its run ID.
// The service places it immediately after the project's base arguments, and
// agent-supplied lists and project settings may not set it.
const RunIDFlag = "--run-id"

// SetsRunID reports whether any argument is the run ID option in any
// spelling, with or without an =value, so the service alone chooses the run ID.
func SetsRunID(call []string) bool {
	for _, arg := range call {
		for _, element := range Elements(arg) {
			if trimmed := strings.TrimSpace(element); strings.HasPrefix(trimmed, "-") && Name(trimmed) == "runid" {
				return true
			}
		}
	}
	return false
}
