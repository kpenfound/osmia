package intake

import (
	"encoding/json"
)

func PriorityRefusal(reason string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
	}{false, reason})
}
