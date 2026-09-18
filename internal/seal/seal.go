// Package seal defines the seal of a ratified workstream: the upstream commit
// and spec hash it was ratified against, the footprints its plan's units
// touch, and the feature branch built on it, as recorded in seal.json.
package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// Version is the only seal.json schema version.
	Version = 1
	// Path is the workstream path of the seal and DocumentID its trace
	// record ID.
	Path       = "seal.json"
	DocumentID = "seal"
)

// Seal is the content of seal.json.
type Seal struct {
	Version int `json:"version"`
	// Seal numbers the sealing that recorded it.
	Seal int `json:"seal"`
	// Round is the round the owner ratified in, and Revision the revisions
	// of spec.md and plan.json they ratified.
	Round    int      `json:"round"`
	Revision shed.Pin `json:"revision"`
	// SpecHash is the hash of the ratified spec revision's content.
	SpecHash string `json:"spec_hash"`
	// Base is the upstream commit the feature branch descends from.
	Base Base `json:"base"`
	// Branch is the feature branch of the clone and Workspace the directory
	// the service checked it out in.
	Branch    string `json:"branch"`
	Workspace string `json:"workspace"`
	// Footprints are the plan's units' footprints, resolved when the seal
	// was taken.
	Footprints []Footprint `json:"footprints"`
}

// Base names the upstream commit of the seal: the remote it was fetched
// from, the branch and the commit.
type Base struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// Footprint is what one unit of the plan touches: the entities its footprint
// names and every entity that is part of them, and their path patterns.
type Footprint struct {
	Unit     string   `json:"unit"`
	Entities []string `json:"entities"`
	Paths    []string `json:"paths"`
}

// SpecHash returns the hash the seal records of a spec revision's content.
func SpecHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Take resolves the footprint of every unit of the plan against the entity
// map, in plan order. Unresolved lists, as `<unit>: <name>`, every footprint
// name that matches no entity with a path pattern; the footprints are complete
// only when it is empty.
func Take(p plan.Plan, m kb.Map) (footprints []Footprint, unresolved []string) {
	footprints = []Footprint{}
	for _, u := range p.Units {
		resolved := m.ResolveEntities(u.Footprint)
		for _, name := range resolved.Unresolved {
			unresolved = append(unresolved, u.ID+": "+name)
		}
		footprints = append(footprints, Footprint{Unit: u.ID, Entities: resolved.Entities, Paths: resolved.Paths})
	}
	return footprints, unresolved
}

func (s Seal) check() error {
	switch {
	case s.Version != Version:
		return fmt.Errorf("unsupported seal version %d", s.Version)
	case s.Seal < 1 || s.Round < 1:
		return errors.New("a seal requires the sealing and the round that recorded it")
	case s.Revision.Spec < 1 || s.Revision.Plan < 1:
		return errors.New("a seal requires the revisions it seals")
	case !strings.HasPrefix(s.SpecHash, "sha256:") || len(s.SpecHash) != len("sha256:")+sha256.Size*2:
		return errors.New("a seal requires the hash of the spec")
	case s.Base.Remote == "" || s.Base.Branch == "" || s.Base.Commit == "":
		return errors.New("a seal requires the upstream remote, branch and commit")
	case s.Branch == "" || s.Workspace == "":
		return errors.New("a seal requires the feature branch and its workspace")
	}
	seen := map[string]bool{}
	for _, f := range s.Footprints {
		if f.Unit == "" || seen[f.Unit] {
			return fmt.Errorf("footprint of unit %q is missing its unit or repeats one", f.Unit)
		}
		seen[f.Unit] = true
	}
	return nil
}

// Encode returns the file content of a valid seal.
func Encode(s Seal) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	s.Footprints = slices.Clone(s.Footprints)
	if s.Footprints == nil {
		s.Footprints = []Footprint{}
	}
	for i := range s.Footprints {
		if s.Footprints[i].Entities == nil {
			s.Footprints[i].Entities = []string{}
		}
		if s.Footprints[i].Paths == nil {
			s.Footprints[i].Paths = []string{}
		}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(s); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Parse reads a seal's file content, refusing unknown fields and invalid
// seals.
func Parse(data []byte) (Seal, error) {
	var s Seal
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Seal{}, fmt.Errorf("seal: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Seal{}, errors.New("seal must be one object")
	}
	return s, s.check()
}

// Latest returns the latest recorded seal of the workstream and its document,
// and whether one is recorded.
func Latest(repository *trace.Repository, stream config.WorkstreamID) (Seal, trace.Document, bool, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Seal{}, trace.Document{}, false, err
	}
	var latest trace.Document
	found := false
	for _, d := range documents {
		if d.Path == Path {
			latest, found = d, true
		}
	}
	if !found {
		return Seal{}, trace.Document{}, false, nil
	}
	s, err := Parse([]byte(latest.Content))
	if err != nil {
		return Seal{}, trace.Document{}, false, fmt.Errorf("%s: %w", Path, err)
	}
	return s, latest, true, nil
}
