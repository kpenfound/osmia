package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/BurntSushi/toml"
)

// AddActiveProject appends id to active_projects in the top-level configuration
// file as a text edit: every other byte, including comments and key order, is
// left as it was. An id already listed is left in place.
func AddActiveProject(path string, id ProjectID) error {
	return editActiveProjects(path, id, true)
}

// RemoveActiveProject removes id from active_projects as a text edit. An id
// that is not listed leaves the file untouched.
func RemoveActiveProject(path string, id ProjectID) error {
	return editActiveProjects(path, id, false)
}

func editActiveProjects(path string, id ProjectID, add bool) error {
	if err := CheckProjectIDs(id); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	current, err := activeProjects(text)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	listed := slices.Contains(current, string(id))
	if listed == add {
		return nil
	}
	want := slices.Clone(current)
	if add {
		want = append(want, string(id))
	} else {
		want = slices.DeleteFunc(want, func(s string) bool { return s == string(id) })
	}
	edited, err := rewriteActiveProjects(text, id, add)
	if err != nil {
		return fmt.Errorf("%s: active_projects: %w", path, err)
	}
	got, err := activeProjects(edited)
	if err != nil || !slices.Equal(got, want) {
		return fmt.Errorf("%s: active_projects: edited text would not parse as expected; edit the file by hand", path)
	}
	return replaceFile(path, []byte(edited))
}

func activeProjects(text string) ([]string, error) {
	var v struct {
		ActiveProjects []string `toml:"active_projects"`
	}
	if _, err := toml.Decode(text, &v); err != nil {
		return nil, err
	}
	return v.ActiveProjects, nil
}

// span is a half-open byte range within the file text.
type span struct{ start, end int }

// arrayLocation finds the top-level active_projects array. It returns the span
// of the text between its brackets and the string tokens inside it.
func arrayLocation(text string) (inner span, items []span, found bool, err error) {
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(trimmed, "[") {
			return span{}, nil, false, nil // Later keys belong to tables.
		}
		key, rest, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(key) == "active_projects" {
			start := offset + len(line) - len(rest)
			open := start + len(rest) - len(strings.TrimLeftFunc(rest, unicode.IsSpace))
			if open >= len(text) || text[open] != '[' {
				return span{}, nil, false, errors.New("expected an array value")
			}
			inner, items, err = scanArray(text, open+1)
			return inner, items, true, err
		}
		offset += len(line)
	}
	return span{}, nil, false, nil
}

// scanArray tokenizes strings until the closing bracket, skipping comments.
func scanArray(text string, from int) (span, []span, error) {
	var items []span
	for i := from; i < len(text); i++ {
		switch c := text[i]; c {
		case ']':
			return span{from, i}, items, nil
		case '#':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case '"', '\'':
			start := i
			for i++; i < len(text) && text[i] != c; i++ {
				if c == '"' && text[i] == '\\' {
					i++
				}
				if text[i] == '\n' {
					return span{}, nil, errors.New("unterminated string")
				}
			}
			if i >= len(text) {
				return span{}, nil, errors.New("unterminated string")
			}
			items = append(items, span{start, i + 1})
		case ',', ' ', '\t', '\r', '\n':
		default:
			return span{}, nil, fmt.Errorf("unsupported array element at byte %d", i)
		}
	}
	return span{}, nil, errors.New("unterminated array")
}

func rewriteActiveProjects(text string, id ProjectID, add bool) (string, error) {
	inner, items, found, err := arrayLocation(text)
	if err != nil {
		return "", err
	}
	quoted := `"` + string(id) + `"`
	if !found {
		if !add {
			return text, nil
		}
		return insertTopLevelLine(text, "active_projects = ["+quoted+"]\n"), nil
	}
	if add {
		if len(items) == 0 {
			return text[:inner.start] + quoted + text[inner.start:], nil
		}
		last := items[len(items)-1]
		return text[:last.end] + ", " + quoted + text[last.end:], nil
	}
	for _, item := range items {
		value := text[item.start:item.end]
		if value != quoted && value != "'"+string(id)+"'" {
			continue
		}
		cut := item
		if next := skipBlanks(text, item.end, inner.end); next < inner.end && text[next] == ',' {
			cut.end = skipBlanks(text, next+1, inner.end)
		} else if previous := rewindBlanks(text, item.start, inner.start); previous > inner.start && text[previous-1] == ',' {
			cut.start = previous - 1
		}
		return text[:cut.start] + text[cut.end:], nil
	}
	return text, nil
}

// skipBlanks advances over spaces and tabs, staying on the current line.
func skipBlanks(text string, i, limit int) int {
	for i < limit && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i
}
func rewindBlanks(text string, i, limit int) int {
	for i > limit && (text[i-1] == ' ' || text[i-1] == '\t' || text[i-1] == '\n' || text[i-1] == '\r') {
		i--
	}
	return i
}

// insertTopLevelLine places a key after the version line, before any table.
func insertTopLevelLine(text, line string) string {
	offset := 0
	for _, l := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeftFunc(l, unicode.IsSpace)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(key) == "version" {
			end := offset + len(l)
			if !strings.HasSuffix(l, "\n") {
				line = "\n" + line
			}
			return text[:end] + line + text[end:]
		}
		offset += len(l)
	}
	return line + text
}

// WriteProjectConfig writes a project configuration file with the registered
// identity fields and explicit defaults, replacing any file at path atomically.
// Callers own the directory: a fresh project identity never has one.
func WriteProjectConfig(path string, p Project) error {
	if !ValidRepository(p.Upstream) || !ValidRepository(p.Fork) || strings.EqualFold(p.Upstream, p.Fork) || !filepath.IsAbs(p.Clone) || !ValidBranch(p.BaseBranch) {
		return errors.New("invalid project configuration")
	}
	text := fmt.Sprintf("version = 1\nname = %s\nupstream = %s\nfork = %s\nclone = %s\nbase_branch = %s\nlanding = \"commit-per-unit\"\n",
		tomlString(p.Name), tomlString(p.Upstream), tomlString(p.Fork), tomlString(p.Clone), tomlString(p.BaseBranch))
	var check Project
	if _, err := toml.Decode(text, &check); err != nil {
		return err
	}
	if check.Name != p.Name || check.Clone != p.Clone {
		return errors.New("project configuration did not round-trip")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeAtomically(path, []byte(text), 0600)
}

// tomlString quotes a basic string, escaping control characters portably.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// replaceFile keeps the original mode and replaces the file atomically.
func replaceFile(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: expected a regular file", path)
	}
	return writeAtomically(path, data, info.Mode().Perm())
}

// writeAtomically publishes data through a synced temporary file and rename.
func writeAtomically(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s: expected a regular file", path)
	}
	tmp := filepath.Join(filepath.Dir(path), ".config-"+rand.Text()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
