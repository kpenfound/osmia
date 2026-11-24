package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

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
