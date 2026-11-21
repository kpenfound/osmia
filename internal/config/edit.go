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

	"github.com/BurntSushi/toml"
)

// skipBlanks advances over spaces and tabs, staying on the current line.
func skipBlanks(text string, i, limit int) int {
	for i < limit && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i
}

// ProjectConfigBytes is the project configuration a newly registered project
// starts with: its registered identity fields and explicit defaults, in the
// project file's TOML form.
func ProjectConfigBytes(p Project) ([]byte, error) {
	if !ValidRepository(p.Upstream) || (p.Fork != "" && !ValidRepository(p.Fork)) || !filepath.IsAbs(p.Clone) || !ValidBranch(p.BaseBranch) {
		return nil, errors.New("invalid project configuration")
	}
	delivery := p.Delivery
	if delivery == "" {
		delivery = DeliveryPullRequest
	}
	if !slices.Contains([]string{DeliveryPullRequest, DeliveryJev}, delivery) || delivery == DeliveryJev && !strings.EqualFold(p.PushRepository(), p.Upstream) {
		return nil, errors.New("invalid project configuration: delivery")
	}
	text := fmt.Sprintf("version = 1\nname = %s\nupstream = %s\nfork = %s\nclone = %s\nbase_branch = %s\nlanding = \"commit-per-unit\"\ndelivery = %s\n",
		tomlString(p.Name), tomlString(p.Upstream), tomlString(p.Fork), tomlString(p.Clone), tomlString(p.BaseBranch), tomlString(delivery))
	if p.Fork == "" || strings.EqualFold(p.Upstream, p.Fork) {
		text = strings.Replace(text, "fork = "+tomlString(p.Fork)+"\n", "", 1)
	}
	var check Project
	if _, err := toml.Decode(text, &check); err != nil {
		return nil, err
	}
	if check.Name != p.Name || check.Clone != p.Clone {
		return nil, errors.New("project configuration did not round-trip")
	}
	return []byte(text), nil
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
	return writeAtomicallyBefore(path, data, mode, nil)
}

// writeAtomicallyBefore is writeAtomically with a step that runs on the
// synced temporary file just before it replaces path.
func writeAtomicallyBefore(path string, data []byte, mode os.FileMode, beforeRename func(tmp string) error) error {
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
	if beforeRename != nil {
		if err := beforeRename(tmp); err != nil {
			return err
		}
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
