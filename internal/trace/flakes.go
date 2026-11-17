package trace

import "strings"

// FlakePath is the project document holding one flake group's record, and
// FlakeDocumentID the record ID of its revisions. group is the group's
// identifier: a key.
func FlakePath(group string) string { return "flakes/" + group + ".json" }

// FlakeDocumentID is the record ID of the revisions of FlakePath(group).
func FlakeDocumentID(group string) string { return "flake-" + group }

// flakePath reports whether parts name a flake group's record: flakes/<group>.json.
func flakePath(parts []string) bool {
	return len(parts) == 2 && parts[0] == "flakes" && strings.HasSuffix(parts[1], ".json") && key(strings.TrimSuffix(parts[1], ".json"))
}
