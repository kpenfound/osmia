package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// tomlStatement is one table header or key/value assignment of a TOML file,
// with the byte offsets that let an edit leave every other byte alone.
type tomlStatement struct {
	header bool
	// table is the header's own table path, or for an assignment the path of
	// the table it sits in. key is the assignment's key path inside it.
	table, key []string
	// start is the offset of the first byte of the statement's first line and
	// end the offset after its last line, newline included.
	start, end int
	// valueStart and valueEnd bound an assignment's value.
	valueStart, valueEnd int
}

func (s tomlStatement) path() []string { return append(slices.Clone(s.table), s.key...) }

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// quoteKey writes one key segment as a bare or basic-string key.
func quoteKey(k string) string {
	if bareKey.MatchString(k) {
		return k
	}
	return tomlString(k)
}

// scanTOML splits a file into its statements without interpreting values.
func scanTOML(text string) ([]tomlStatement, error) {
	var out []tomlStatement
	var table []string
	i := 0
	for i < len(text) {
		switch text[i] {
		case ' ', '\t', '\r', '\n':
			i++
			continue
		case '#':
			i = lineEnd(text, i)
			continue
		}
		start := strings.LastIndexByte(text[:i], '\n') + 1
		if text[i] == '[' {
			open := 1
			if i+1 < len(text) && text[i+1] == '[' {
				open = 2
			}
			path, next, err := scanKey(text, i+open)
			if err != nil {
				return nil, err
			}
			next = skipBlanks(text, next, len(text))
			if !strings.HasPrefix(text[next:], strings.Repeat("]", open)) {
				return nil, fmt.Errorf("unterminated table header at byte %d", i)
			}
			end, err := finishLine(text, next+open)
			if err != nil {
				return nil, err
			}
			table = path
			out = append(out, tomlStatement{header: true, table: path, start: start, end: end})
			i = end
			continue
		}
		key, next, err := scanKey(text, i)
		if err != nil {
			return nil, err
		}
		next = skipBlanks(text, next, len(text))
		if next >= len(text) || text[next] != '=' {
			return nil, fmt.Errorf("expected = at byte %d", next)
		}
		valueStart := skipBlanks(text, next+1, len(text))
		valueEnd, err := scanValue(text, valueStart)
		if err != nil {
			return nil, err
		}
		end, err := finishLine(text, valueEnd)
		if err != nil {
			return nil, err
		}
		out = append(out, tomlStatement{table: slices.Clone(table), key: key, start: start, end: end, valueStart: valueStart, valueEnd: valueEnd})
		i = end
	}
	return out, nil
}

// lineEnd returns the offset after the newline ending the line at i.
func lineEnd(text string, i int) int {
	if n := strings.IndexByte(text[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(text)
}

// finishLine accepts only blanks and a comment up to the end of the line.
func finishLine(text string, i int) (int, error) {
	i = skipBlanks(text, i, len(text))
	if i < len(text) && text[i] == '\r' {
		i++
	}
	if i >= len(text) {
		return len(text), nil
	}
	switch text[i] {
	case '\n':
		return i + 1, nil
	case '#':
		return lineEnd(text, i), nil
	}
	return 0, fmt.Errorf("unexpected text after a value at byte %d", i)
}

// scanKey reads a possibly dotted key, bare or quoted, from i.
func scanKey(text string, i int) ([]string, int, error) {
	var path []string
	for {
		i = skipBlanks(text, i, len(text))
		if i >= len(text) {
			return nil, 0, errors.New("unexpected end of file in a key")
		}
		var segment string
		switch c := text[i]; {
		case c == '"' || c == '\'':
			end, err := scanQuoted(text, i)
			if err != nil {
				return nil, 0, err
			}
			var decoded map[string]any
			if _, err := toml.Decode(text[i:end]+" = 1", &decoded); err != nil || len(decoded) != 1 {
				return nil, 0, fmt.Errorf("invalid quoted key at byte %d", i)
			}
			for k := range decoded {
				segment = k
			}
			i = end
		case strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-", rune(c)):
			end := i
			for end < len(text) && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-", rune(text[end])) {
				end++
			}
			segment, i = text[i:end], end
		default:
			return nil, 0, fmt.Errorf("unexpected character in a key at byte %d", i)
		}
		path = append(path, segment)
		i = skipBlanks(text, i, len(text))
		if i < len(text) && text[i] == '.' {
			i++
			continue
		}
		return path, i, nil
	}
}

// scanQuoted returns the offset after the single-line string starting at i.
func scanQuoted(text string, i int) (int, error) {
	q := text[i]
	for j := i + 1; j < len(text); j++ {
		switch {
		case text[j] == '\n':
			return 0, errors.New("unterminated string")
		case q == '"' && text[j] == '\\':
			j++
		case text[j] == q:
			return j + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

// scanValue returns the offset after the value starting at i.
func scanValue(text string, i int) (int, error) {
	if i >= len(text) {
		return 0, errors.New("missing value")
	}
	switch {
	case strings.HasPrefix(text[i:], `"""`), strings.HasPrefix(text[i:], `'''`):
		return scanMultiline(text, i)
	case text[i] == '"' || text[i] == '\'':
		return scanQuoted(text, i)
	case text[i] == '[' || text[i] == '{':
		depth := 0
		for j := i; j < len(text); j++ {
			switch c := text[j]; c {
			case '[', '{':
				depth++
			case ']', '}':
				if depth--; depth == 0 {
					return j + 1, nil
				}
			case '#':
				j = lineEnd(text, j) - 1
			case '"', '\'':
				var end int
				var err error
				if strings.HasPrefix(text[j:], strings.Repeat(string(c), 3)) {
					end, err = scanMultiline(text, j)
				} else {
					end, err = scanQuoted(text, j)
				}
				if err != nil {
					return 0, err
				}
				j = end - 1
			}
		}
		return 0, errors.New("unterminated array or inline table")
	}
	end := i
	for end < len(text) && text[end] != '#' && text[end] != '\n' {
		end++
	}
	value := strings.TrimRight(text[i:end], " \t\r")
	if value == "" {
		return 0, errors.New("missing value")
	}
	return i + len(value), nil
}

// scanMultiline returns the offset after a triple-quoted string at i.
func scanMultiline(text string, i int) (int, error) {
	q := text[i]
	closing := strings.Repeat(string(q), 3)
	for j := i + 3; j < len(text); j++ {
		if q == '"' && text[j] == '\\' {
			j++
			continue
		}
		if strings.HasPrefix(text[j:], closing) {
			end := j + 3
			for extra := 0; extra < 2 && end < len(text) && text[end] == q; extra++ {
				end++
			}
			return end, nil
		}
	}
	return 0, errors.New("unterminated multi-line string")
}

// eolOf is the line ending the file already uses.
func eolOf(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

var errLayout = errors.New("the file uses a layout that cannot be edited safely")

// definedAbove reports whether an assignment sets a table that contains path:
// an inline table, which the edit cannot reach into.
func definedAbove(stmts []tomlStatement, path []string) bool {
	for _, s := range stmts {
		if s.header {
			continue
		}
		if p := s.path(); len(p) < len(path) && slices.Equal(p, path[:len(p)]) {
			return true
		}
	}
	return false
}

// setTOMLKey sets the value at path to the TOML literal value. An existing
// assignment keeps its key, spacing and trailing comment; a new key is placed
// with its siblings. Every other byte is unchanged.
func setTOMLKey(text string, path []string, literal string) (string, error) {
	stmts, err := scanTOML(text)
	if err != nil {
		return "", err
	}
	for _, s := range stmts {
		if !s.header && slices.Equal(s.path(), path) {
			return text[:s.valueStart] + literal + text[s.valueEnd:], nil
		}
	}
	if definedAbove(stmts, path) {
		return "", errLayout
	}
	eol := eolOf(text)
	if len(path) == 1 {
		line := quoteKey(path[0]) + " = " + literal + eol
		last := -1
		for i, s := range stmts {
			if s.header {
				break
			}
			last = i
		}
		if last < 0 {
			return line + text, nil
		}
		return insertAfter(text, stmts[last].end, line, eol), nil
	}
	table, name := path[:len(path)-1], path[len(path)-1]
	header := -1
	for i, s := range stmts {
		if s.header && slices.Equal(s.table, table) {
			header = i
		}
	}
	if header >= 0 {
		at := stmts[header].end
		for _, s := range stmts[header+1:] {
			if s.header {
				break
			}
			at = s.end
		}
		return insertAfter(text, at, quoteKey(name)+" = "+literal+eol, eol), nil
	}
	dotted := -1
	for i, s := range stmts {
		if !s.header && len(s.table) == 0 && len(s.key) > len(table) && slices.Equal(s.key[:len(table)], table) {
			dotted = i
		}
	}
	var keys []string
	for _, k := range path {
		keys = append(keys, quoteKey(k))
	}
	if dotted >= 0 {
		return insertAfter(text, stmts[dotted].end, strings.Join(keys, ".")+" = "+literal+eol, eol), nil
	}
	var tables []string
	for _, k := range table {
		tables = append(tables, quoteKey(k))
	}
	block := "[" + strings.Join(tables, ".") + "]" + eol + quoteKey(name) + " = " + literal + eol
	switch {
	case text == "":
	case !strings.HasSuffix(text, "\n"):
		text += eol + eol
	case !strings.HasSuffix(text, eol+eol) && !strings.HasSuffix(text, "\n\n"):
		text += eol
	}
	return text + block, nil
}

// insertAfter places line at offset at, which follows a statement's newline
// unless the statement ended the file without one.
func insertAfter(text string, at int, line, eol string) string {
	if at > 0 && text[at-1] != '\n' {
		line = eol + line
	}
	return text[:at] + line + text[at:]
}

// removeTOMLKey removes the assignment at path with its trailing comment. A
// key that is not set leaves the text as it was.
func removeTOMLKey(text string, path []string) (string, error) {
	stmts, err := scanTOML(text)
	if err != nil {
		return "", err
	}
	for _, s := range stmts {
		if !s.header && slices.Equal(s.path(), path) {
			return text[:s.start] + text[s.end:], nil
		}
	}
	if definedAbove(stmts, path) {
		return "", errLayout
	}
	return text, nil
}

// keyEdit sets path to value (a string or int) or, when remove is true,
// removes it.
type keyEdit struct {
	path   []string
	value  any
	remove bool
}

// editTOML applies the edits to text in order and proves the result decodes
// to the original with exactly those keys changed.
func editTOML(text string, edits []keyEdit) (string, error) {
	var before map[string]any
	if _, err := toml.Decode(text, &before); err != nil {
		return "", err
	}
	expected := cloneTree(before)
	var err error
	out := text
	for _, e := range edits {
		if e.remove {
			out, err = removeTOMLKey(out, e.path)
			deleteTree(expected, e.path)
		} else {
			var literal string
			switch v := e.value.(type) {
			case string:
				literal = tomlString(v)
			case int:
				literal = fmt.Sprint(v)
			default:
				return "", fmt.Errorf("unsupported value for %s", strings.Join(e.path, "."))
			}
			out, err = setTOMLKey(out, e.path, literal)
			setTree(expected, e.path, e.value)
		}
		if err != nil {
			return "", err
		}
	}
	var after map[string]any
	if _, err := toml.Decode(out, &after); err != nil {
		return "", errLayout
	}
	pruneTree(expected)
	pruneTree(after)
	if !treesEqual(expected, after) {
		return "", errLayout
	}
	return out, nil
}

func cloneTree(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = cloneTree(sub)
		}
		out[k] = v
	}
	return out
}

func setTree(m map[string]any, path []string, value any) {
	for _, k := range path[:len(path)-1] {
		sub, ok := m[k].(map[string]any)
		if !ok {
			sub = map[string]any{}
			m[k] = sub
		}
		m = sub
	}
	if n, ok := value.(int); ok {
		value = int64(n)
	}
	m[path[len(path)-1]] = value
}

func deleteTree(m map[string]any, path []string) {
	for _, k := range path[:len(path)-1] {
		sub, ok := m[k].(map[string]any)
		if !ok {
			return
		}
		m = sub
	}
	delete(m, path[len(path)-1])
}

// pruneTree drops empty tables, which a file may or may not spell out.
func pruneTree(m map[string]any) {
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if pruneTree(sub); len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}

func treesEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		as, aok := av.(map[string]any)
		bs, bok := bv.(map[string]any)
		switch {
		case aok != bok:
			return false
		case aok:
			if !treesEqual(as, bs) {
				return false
			}
		default:
			if fmt.Sprintf("%T:%v", av, av) != fmt.Sprintf("%T:%v", bv, bv) {
				return false
			}
		}
	}
	return true
}
