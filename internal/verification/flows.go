package verification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxFlowListBytes caps the text a lever's list call may return.
const MaxFlowListBytes = 64 << 10

// ParseFlowList parses the output of a lever's list function: a JSON array of
// flow names, or an object whose "flows" member is that array. Names must be
// non-empty single-line strings and may not repeat. The names come back in the
// order the lever listed them.
func ParseFlowList(data []byte) ([]string, error) {
	data = bytes.TrimSpace(data)
	if len(data) > MaxFlowListBytes {
		return nil, fmt.Errorf("list output is larger than %d bytes", MaxFlowListBytes)
	}
	if len(data) == 0 {
		return nil, errors.New("list output is empty")
	}
	var names []string
	if data[0] == '{' {
		var object struct {
			Flows *[]string `json:"flows"`
		}
		if err := json.Unmarshal(data, &object); err != nil || object.Flows == nil {
			return nil, errors.New("list output is not a JSON array of flow names")
		}
		names = *object.Flows
	} else if err := json.Unmarshal(data, &names); err != nil || names == nil {
		return nil, errors.New("list output is not a JSON array of flow names")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("list output holds an empty or multi-line flow name")
		}
		if seen[name] {
			return nil, fmt.Errorf("list output names flow %q twice", boundedText(name))
		}
		seen[name] = true
	}
	return names, nil
}
