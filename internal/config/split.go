package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// ProjectGroupSource reads migration input without exposing grouping in Project.
func ProjectGroupSource(path string) (ProjectID, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var legacy struct {
		Group string `toml:"group_id"`
	}
	md, err := toml.Decode(string(data), &legacy)
	if err != nil {
		return "", false, err
	}
	if legacy.Group == "" {
		return "", md.IsDefined("group_id"), nil
	}
	id, err := ParseProjectID(legacy.Group)
	return id, true, err
}

// RemoveProjectGroup removes only the top-level assignment. Whitespace and
// comments, including a comment beside the value, retain their original bytes.
func RemoveProjectGroup(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	var values map[string]any
	if _, err := toml.Decode(text, &values); err != nil {
		return err
	}
	if _, exists := values["group_id"]; !exists {
		return nil
	}
	// Track multiline strings so text resembling an assignment in another
	// setting's value is never edited.
	quote := ""
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if quote == "" {
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			key, rest, ok := strings.Cut(trimmed, "=")
			// Decode the key independently of its value so quoted escapes follow
			// TOML semantics while the edit uses offsets in the original text.
			var assignment map[string]any
			_, keyErr := toml.Decode(key+` = ""`, &assignment)
			_, groupKey := assignment["group_id"].(string)
			if ok && keyErr == nil && groupKey {
				start := offset + len(line) - len(trimmed)
				valueStart := offset + len(line) - len(rest)
				end := valueStart
				// Locate the complete value using the TOML parser; the remainder must
				// contain only whitespace and an optional comment.
				for end < len(text) {
					end++
					var check struct {
						Group string `toml:"group_id"`
					}
					if _, e := toml.Decode(text[start:end], &check); e != nil {
						continue
					}
					tail := strings.TrimLeft(text[end:], " \t\r")
					if tail != "" && tail[0] != '#' && tail[0] != '\n' {
						continue
					}
					edited := text[:start] + text[end:]
					var after map[string]any
					if _, e := toml.Decode(edited, &after); e != nil {
						return e
					}
					if _, exists := after["group_id"]; exists {
						return fmt.Errorf("%s: group_id remains after edit", path)
					}
					return replaceFile(path, []byte(edited))
				}
				return fmt.Errorf("%s: cannot locate group_id value", path)
			}
		}
		// Scan strings and comments; ordinary strings end on this line.
		for i := 0; i < len(line); {
			if quote != "" {
				if strings.HasPrefix(line[i:], quote) {
					i += len(quote)
					quote = ""
				} else if quote == `"""` && line[i] == '\\' {
					i += 2
				} else {
					i++
				}
				continue
			}
			if line[i] == '#' {
				break
			}
			if strings.HasPrefix(line[i:], `"""`) || strings.HasPrefix(line[i:], "'''") {
				quote = line[i : i+3]
				i += 3
				continue
			}
			if line[i] == '"' || line[i] == '\'' {
				q := line[i]
				i++
				for i < len(line) {
					if line[i] == q {
						i++
						break
					}
					if q == '"' && line[i] == '\\' {
						i++
					}
					i++
				}
				continue
			}
			i++
		}
		offset += len(line)
	}
	return fmt.Errorf("%s: cannot locate group_id assignment", path)
}
