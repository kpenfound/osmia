package flakes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kpenfound/osmia/internal/config"
)

// GroupID is the identifier of a flake group within its project: a hash of the
// group's key, usable in a document path.
func GroupID(group string) string {
	return digest("flake-group", group)
}

// OccurrenceID is the identifier of the occurrence of a run in a group.
func OccurrenceID(run, group string) string {
	return digest("flake-occurrence", run, group)
}

// HandInKey is the hand-in key of the flake-fix workstream of a project's
// flake group in a generation. It is deterministic, differs across projects,
// groups and generations, and is a valid intake hand-in key.
func HandInKey(project config.ProjectID, group string, generation int) string {
	data, _ := json.Marshal([]any{"flake-fix", project, group, generation})
	sum := sha256.Sum256(data)
	return "flake-" + hex.EncodeToString(sum[:16])
}

func digest(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
