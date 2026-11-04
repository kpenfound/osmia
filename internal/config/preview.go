package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	DefaultPreviewPath    = "/"
	DefaultPreviewTimeout = "5m"
)

// Preview describes the Dagger Service and the URL to capture.
type Preview struct {
	Call    []string `toml:"call" json:"call"`
	Port    int      `toml:"port" json:"port"`
	Path    string   `toml:"path" json:"path"`
	Timeout string   `toml:"timeout" json:"timeout"`
}

func (p Preview) validate(path string) error {
	if len(p.Call) == 0 {
		return fieldError(path, "preview.call", "a non-empty argv list is required")
	}
	if p.Port < 1 || p.Port > 65535 {
		return fieldError(path, "preview.port", "required; must be from 1 to 65535")
	}
	if !strings.HasPrefix(p.Path, "/") {
		return fieldError(path, "preview.path", "must start with /")
	}
	if d, err := time.ParseDuration(p.Timeout); err != nil || d < time.Minute || d > 30*time.Minute {
		return fieldError(path, "preview.timeout", "must be a Go duration from 1m to 30m")
	}
	return nil
}

// DecodePreviewJSON applies the project loader's defaults and validation to
// the API object. Errors name the field without quoting supplied values.
func DecodePreviewJSON(data []byte) (*Preview, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fieldError("", "preview", "expected an object")
	}
	p := &Preview{Path: DefaultPreviewPath, Timeout: DefaultPreviewTimeout}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		var dest any
		switch key {
		case "call":
			dest = &p.Call
		case "port":
			dest = &p.Port
		case "path":
			dest = &p.Path
		case "timeout":
			dest = &p.Timeout
		default:
			return nil, fieldError("", "preview."+key, "unknown configuration key")
		}
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return nil, fieldError("", "preview."+key, "value has the wrong type")
		}
		if err := json.Unmarshal(fields[key], dest); err != nil {
			return nil, fieldError("", "preview."+key, "value has the wrong type")
		}
		if key == "call" {
			var args []json.RawMessage
			if err := json.Unmarshal(fields[key], &args); err != nil {
				return nil, fieldError("", "preview.call", "value has the wrong type")
			}
			for _, arg := range args {
				if bytes.Equal(bytes.TrimSpace(arg), []byte("null")) {
					return nil, fieldError("", "preview.call", "value has the wrong type")
				}
			}
		}
	}
	if err := p.validate(""); err != nil {
		return nil, err
	}
	return p, nil
}

// ErrProjectDrift means the file differs from the bytes last loaded or saved.
var ErrProjectDrift = errors.New("config.toml byte drift; reload the configuration before saving preview settings")

// SavePreview edits only preview statements and returns a project with the
// saved setting and byte baseline. Even a no-op checks the baseline first.
func (p Project) SavePreview(path string, preview *Preview) (Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if p.configIdentity == "" || fmt.Sprintf("%x", sha256.Sum256(data)) != p.configIdentity {
		return p, fmt.Errorf("%s: %w", path, ErrProjectDrift)
	}
	if preview != nil {
		if err := preview.validate(path); err != nil {
			return p, err
		}
	}
	if reflect.DeepEqual(p.Preview, preview) {
		return p, nil
	}
	edited, err := editPreview(string(data), preview)
	if err != nil {
		return p, err
	}
	var check Project
	md, err := decodeBytes(path, []byte(edited), &check, true)
	if err != nil {
		return p, err
	}
	if check.Preview != nil {
		if !md.IsDefined("preview", "path") {
			check.Preview.Path = DefaultPreviewPath
		}
		if !md.IsDefined("preview", "timeout") {
			check.Preview.Timeout = DefaultPreviewTimeout
		}
		if err := check.Preview.validate(path); err != nil {
			return p, err
		}
	}
	if !reflect.DeepEqual(check.Preview, preview) {
		return p, fmt.Errorf("%s: preview: edited text did not round-trip", path)
	}
	// Recheck after parsing the edit so a hand edit during validation is
	// refused as well.
	latest, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if !bytes.Equal(latest, data) {
		return p, fmt.Errorf("%s: %w", path, ErrProjectDrift)
	}
	if err := replaceFile(path, []byte(edited)); err != nil {
		return p, err
	}
	p.Preview = check.Preview
	p.configIdentity = fmt.Sprintf("%x", sha256.Sum256([]byte(edited)))
	return p, nil
}

func previewTable(p *Preview) string {
	var b strings.Builder
	b.WriteString("[preview]\ncall = [")
	for i, arg := range p.Call {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlString(arg))
	}
	fmt.Fprintf(&b, "]\nport = %d\npath = %s\ntimeout = %s\n", p.Port, tomlString(p.Path), tomlString(p.Timeout))
	return b.String()
}

// editPreview recognizes complete TOML statements, including arrays and
// multiline strings. Only preview headers and assignments are removed;
// surrounding comments, whitespace and all other statements keep their bytes.
func editPreview(text string, p *Preview) (string, error) {
	var out strings.Builder
	header := ""
	for _, part := range tomlStatements(text) {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out.WriteString(part)
			continue
		}
		isHeader := strings.HasPrefix(trimmed, "[")
		source := header + part
		if isHeader {
			source = part
			header = part
		}
		var values map[string]any
		if _, err := toml.Decode(source, &values); err != nil {
			return "", err
		}
		if _, preview := values["preview"]; !preview {
			out.WriteString(part)
		}
	}
	if p != nil {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(previewTable(p))
	}
	return out.String(), nil
}

// tomlStatements splits at newlines outside strings, arrays and inline tables.
func tomlStatements(text string) []string {
	var parts []string
	start, depth := 0, 0
	quote := ""
	comment := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if comment {
			if c != '\n' {
				continue
			}
			comment = false
		} else if quote != "" {
			if quote[0] == '"' && c == '\\' {
				i++
				continue
			}
			if strings.HasPrefix(text[i:], quote) {
				n := len(quote)
				i += n - 1
				// A multiline string may end with one or two additional quote characters.
				if n == 3 {
					for i+1 < len(text) && text[i+1] == quote[0] {
						i++
					}
				}
				quote = ""
			}
			continue
		} else {
			switch c {
			case '#':
				comment = true
			case '"', '\'':
				quote = text[i : i+1]
				if strings.HasPrefix(text[i:], strings.Repeat(quote, 3)) {
					quote = strings.Repeat(quote, 3)
					i += 2
				}
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
		if c == '\n' && quote == "" && depth == 0 {
			parts = append(parts, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		parts = append(parts, text[start:])
	}
	return parts
}
