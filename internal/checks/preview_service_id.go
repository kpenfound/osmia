package checks

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
)

// previewServiceID verifies the CLI's scalar ServiceID: a base64 protobuf DAG
// naming a Service, either by a recipe's root Call or an opaque engine result.
// Object display strings, provider addresses and filesystem paths are not IDs.
func previewServiceID(value string) bool {
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return false
	}
	dag, err := previewProtoFields(data)
	if err != nil {
		return false
	}
	if len(dag) == 1 && len(dag[2]) == 1 {
		return previewEngineServiceRef(dag[2][0])
	}
	// Recipe DAGs may be wrapped in the DAG value oneof.
	if len(dag) == 1 && len(dag[1]) == 1 {
		dag, err = previewProtoFields(dag[1][0])
	}
	if err != nil || len(dag[1]) != 1 || len(dag[1][0]) == 0 {
		return false
	}
	root := string(dag[1][0])
	var selected []byte
	seen := map[string]bool{}
	for _, entry := range dag[2] {
		pair, err := previewProtoFields(entry)
		if err != nil || len(pair[1]) != 1 || len(pair[2]) != 1 {
			return false
		}
		digest := string(pair[1][0])
		if seen[digest] {
			return false
		}
		seen[digest] = true
		if digest == root {
			selected = pair[2][0]
		}
	}
	call, err := previewProtoFields(selected)
	if err != nil || len(call[9]) != 1 || string(call[9][0]) != root || len(call[2]) != 1 || len(call[3]) != 1 || len(call[3][0]) == 0 {
		return false
	}
	return previewServiceType(call[2][0])
}

func previewServiceType(data []byte) bool {
	typ, err := previewProtoFields(data)
	return err == nil && len(typ[1]) == 1 && string(typ[1][0]) == "Service" && len(typ[2]) == 0
}

// EngineResultRef contains a nonzero uint64 resultID followed by its rootType.
func previewEngineServiceRef(data []byte) bool {
	if len(data) == 0 || data[0] != 8 {
		return false
	}
	result, n := binary.Uvarint(data[1:])
	if n <= 0 || result == 0 {
		return false
	}
	data = data[1+n:]
	if len(data) == 0 || data[0] != 18 {
		return false
	}
	size, n := binary.Uvarint(data[1:])
	if n <= 0 || size != uint64(len(data)-1-n) {
		return false
	}
	return previewServiceType(data[1+n:])
}

// previewProtoFields reads length-delimited fields and validates the remaining
// protobuf wire data. Only the DAG root, Call type and digest are needed here;
// the engine resolves the ID's full call graph.
func previewProtoFields(data []byte) (map[uint64][][]byte, error) {
	fields := map[uint64][][]byte{}
	invalid := errors.New("invalid Service ID protobuf")
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag>>3 == 0 {
			return nil, invalid
		}
		data = data[n:]
		var size uint64
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(data)
			if n <= 0 {
				return nil, invalid
			}
			size = uint64(n)
		case 1:
			size = 8
		case 2:
			size, n = binary.Uvarint(data)
			if n <= 0 {
				return nil, invalid
			}
			data = data[n:]
			if size > uint64(len(data)) {
				return nil, invalid
			}
			fields[tag>>3] = append(fields[tag>>3], data[:int(size)])
		case 5:
			size = 4
		default:
			return nil, invalid
		}
		if size > uint64(len(data)) {
			return nil, invalid
		}
		data = data[int(size):]
	}
	return fields, nil
}
