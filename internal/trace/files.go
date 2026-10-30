package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/go-json-experiment/json/jsontext"
)

func decode(data []byte, v any) error {
	// Validate object names without allocating a Go value for every JSON token.
	if _, err := jsontext.NewDecoder(bytes.NewBuffer(data), jsontext.AllowInvalidUTF8(true)).ReadValue(); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func decodeRecord(data []byte) (Record, error) {
	var marker struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, err
	}
	var r Record
	switch marker.Schema {
	case "osmia.trace.image":
		r = Image{}
	case "osmia.trace.image-reference":
		r = ImageReference{}
	case "osmia.trace.document":
		r = Document{}
	case "osmia.trace.transition":
		r = Transition{}
	case "osmia.trace.question":
		r = Question{}
	case "osmia.trace.amendment":
		r = Amendment{}
	case "osmia.trace.ruling":
		r = Ruling{}
	case "osmia.trace.agent":
		r = Agent{}
	case "osmia.trace.turn-request":
		r = TurnRequest{}
	case "osmia.trace.turn-response":
		r = TurnResponse{}
	case "osmia.trace.cost":
		r = Cost{}
	case "osmia.trace.status":
		r = Status{}
	case "osmia.trace.priority":
		r = PriorityChange{}
	default:
		return nil, fmt.Errorf("unsupported record schema %q", marker.Schema)
	}
	// Decode into the concrete value without exposing pointers as record variants.
	switch v := r.(type) {
	case Image:
		err := decode(data, &v)
		return v, err
	case ImageReference:
		err := decode(data, &v)
		return v, err
	case Document:
		err := decode(data, &v)
		return v, err
	case Transition:
		err := decode(data, &v)
		return v, err
	case Question:
		err := decode(data, &v)
		return v, err
	case Amendment:
		err := decode(data, &v)
		return v, err
	case Ruling:
		err := decode(data, &v)
		return v, err
	case Agent:
		err := decode(data, &v)
		return v, err
	case TurnRequest:
		err := decode(data, &v)
		return v, err
	case TurnResponse:
		err := decode(data, &v)
		return v, err
	case Cost:
		err := decode(data, &v)
		return v, err
	case Status:
		err := decode(data, &v)
		return v, err
	case PriorityChange:
		err := decode(data, &v)
		return v, err
	}
	panic("unreachable")
}

// syncDirectory flushes the directory entries of directory.
func syncDirectory(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
