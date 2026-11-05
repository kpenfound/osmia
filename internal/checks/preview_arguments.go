package checks

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

// The call command inherits these flags from the CLI root and module loader.
// Names are folded because Dagger accepts camelCase and kebab-case spellings.
var previewCLIFlags = strings.Fields(`help workdir verbose quiet silent debug progress interactive interactivecommand web noexit autoapply dotoutput dotfocusfield dotshowinternal cloud scaleout mod nomod allowllm eagerruntime output json engine workspace loadmodule shellonerror shellcommandonerror env xrelease profile profileebpf lock`)

var previewClientVariables = strings.Fields(`PATH HOME TMPDIR NO_COLOR DOCKER_HOST _EXPERIMENTAL_DAGGER_RUNNER_HOST DAGGER_ENGINE DAGGER_X_RELEASE DAGGER_CLOUD_TOKEN`)

func previewName(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(s))
}

// previewElements mirrors Dagger's quote removal, CSV splitting and trimming
// for typed slices. The original argv is never changed.
func previewElements(arg string) []string {
	values := func(value string) []string {
		out := []string{value}
		if strings.Contains(value, ",") {
			out = append(out, strings.Split(value, ",")...)
		}
		// Address values may separate a local Git ref with # or remove a
		// secret cache query with ?. Keep the literal reading as well.
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
	// Scalar address values keep their spelling; slices remove quotes and
	// whitespace. Check both readings, including filenames with surrounding spaces.
	for _, element := range elements {
		if trimmed := strings.TrimSpace(element); trimmed != element {
			elements = append(elements, trimmed)
		}
	}
	return elements
}

// ValidatePreviewCall checks agent-supplied arguments before an operation is pinned.
func ValidatePreviewCall(call []string) error {
	if len(call) == 0 {
		return errors.New("preview call must be a non-empty argument list returning a Service")
	}
	for _, arg := range call {
		if strings.TrimSpace(arg) == "" || strings.ContainsFunc(arg, unicode.IsControl) {
			return errors.New("preview call arguments must be non-empty and contain no control characters")
		}
		for _, element := range previewElements(arg) {
			if element == "" {
				return errors.New("preview call values must not be empty")
			}
			if strings.HasPrefix(element, "-") {
				name, _, _ := strings.Cut(element, "=")
				for _, flag := range previewCLIFlags {
					if previewName(name) == flag {
						return fmt.Errorf("preview call cannot use CLI option %s", name)
					}
				}
				if !strings.HasPrefix(element, "--") {
					if _, err := strconv.ParseFloat(element, 64); err != nil {
						return errors.New("preview call cannot use shorthand CLI options")
					}
				} else if element == "--" {
					return errors.New("preview call cannot end CLI option parsing")
				}
			}
			if strings.Contains(element, ":") {
				return errors.New("preview call values cannot use URI or provider schemes")
			}
			path := strings.ReplaceAll(element, `\`, "/")
			if strings.HasPrefix(path, "/") || filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
				return errors.New("preview call paths must stay within the exported source")
			}
			for _, part := range strings.Split(path, "/") {
				if part == ".." {
					return errors.New("preview call paths cannot contain parent-directory components")
				}
			}
			// Secret addresses can remove a cacheKey query before reading
			// the bare environment variable name.
			variableName, _, _ := strings.Cut(element, "?")
			for _, variable := range previewClientVariables {
				if variableName == variable {
					return errors.New("preview call cannot name a Dagger client environment variable")
				}
			}
			// These core chain steps write to the host or start interactive sessions.
			switch previewName(element) {
			case "export", "terminal", "up", "apply", "tunnel", "findup", "host", "unixsocket":
				return fmt.Errorf("preview call cannot use host-writing or interactive step %s", element)
			}
		}
	}
	last := strings.TrimSpace(strings.NewReplacer(`"`, "", "'", "", "`", "").Replace(call[len(call)-1]))
	if strings.HasPrefix(last, "-") && !strings.Contains(last, "=") {
		if _, err := strconv.ParseFloat(last, 64); err != nil {
			return errors.New("preview call must not end in a flag without an explicit =value")
		}
	}
	return nil
}

// ValidatePreviewSource checks the same values through the exported source's
// root. Root.Stat follows symlinks while refusing any escape, even through a
// nonexistent descendant. It does not open files, sockets or devices.
func ValidatePreviewSource(dir string, call []string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, arg := range call {
		for _, element := range previewElements(arg) {
			_, err := root.Stat(element)
			if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return errors.New("preview call path cannot resolve outside the exported source or through an unreadable symlink")
			}
		}
	}
	return nil
}
