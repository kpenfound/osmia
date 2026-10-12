package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// IDs are opaque persistent keys; display names and repository locations are metadata.
type ProjectID string
type WorkstreamID string

var idPattern = regexp.MustCompile(`^[pw]_[0-9a-f]{32}$`)
var ErrCollision = errors.New("identity already exists")

func parseID(s, prefix string) error {
	if !idPattern.MatchString(s) || s[:2] != prefix {
		return fmt.Errorf("invalid identity %q: expected %s followed by 32 lowercase hex digits", s, prefix)
	}
	return nil
}
func ParseProjectID(s string) (ProjectID, error) {
	if err := parseID(s, "p_"); err != nil {
		return "", err
	}
	return ProjectID(s), nil
}
func ParseWorkstreamID(s string) (WorkstreamID, error) {
	if err := parseID(s, "w_"); err != nil {
		return "", err
	}
	return WorkstreamID(s), nil
}
func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// NewProjectID generates an identity without reserving it. Persistence must reserve
// it atomically; a collision with a supplied existing identity returns ErrCollision.
func NewProjectID(existing ...ProjectID) (ProjectID, error) {
	s, err := newID("p_")
	if err != nil {
		return "", err
	}
	id := ProjectID(s)
	if err := CheckProjectIDs(append(existing, id)...); err != nil {
		return "", err
	}
	return id, nil
}
func NewWorkstreamID(existing ...WorkstreamID) (WorkstreamID, error) {
	s, err := newID("w_")
	if err != nil {
		return "", err
	}
	id := WorkstreamID(s)
	if err := CheckWorkstreamIDs(append(existing, id)...); err != nil {
		return "", err
	}
	return id, nil
}
func checkIDs[T ~string](prefix string, ids []T) error {
	seen := map[T]bool{}
	for _, id := range ids {
		if err := parseID(string(id), prefix); err != nil {
			return err
		}
		if seen[id] {
			return fmt.Errorf("%w: %s", ErrCollision, id)
		}
		seen[id] = true
	}
	return nil
}
func CheckProjectIDs(ids ...ProjectID) error       { return checkIDs("p_", ids) }
func CheckWorkstreamIDs(ids ...WorkstreamID) error { return checkIDs("w_", ids) }
