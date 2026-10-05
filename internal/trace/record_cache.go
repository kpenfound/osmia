package trace

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
)

type decodedRecord struct {
	record Record
	err    error
}

type recordFile struct {
	data    []byte
	records []decodedRecord
}

// decodedRecords reuses decoding of the file's previous bytes: all of it when
// the bytes match, and every line before the appended ones when the file grew
// by whole lines. Callers still read through the confined filesystem and check
// cross-record invariants. The caller holds r.mu and clones records before
// exposing or modifying their fields.
func (r *Repository) decodedRecords(name string, data []byte) recordFile {
	cached, ok := r.recordFiles[name]
	if ok && bytes.Equal(cached.data, data) {
		return cached
	}
	file := recordFile{data: data}
	if ok && len(cached.data) > 0 && len(data) > len(cached.data) && cached.data[len(cached.data)-1] == '\n' && bytes.Equal(data[:len(cached.data)], cached.data) {
		file.records = slices.Clip(cached.records)
		data = data[len(cached.data):]
	}
	lines := bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			continue
		}
		v, err := decodeRecord(line)
		if err == nil && i == len(lines)-1 {
			err = fmt.Errorf("incomplete JSONL record (missing newline)")
		}
		if err == nil {
			err = validate(v)
		}
		file.records = append(file.records, decodedRecord{v, err})
	}
	return file
}

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneRecord(record Record) Record {
	switch v := record.(type) {
	case Question:
		v.Escalation = clonePointer(v.Escalation)
		if v.Escalation != nil {
			v.Escalation.Questions = slices.Clone(v.Escalation.Questions)
			v.Escalation.Options = slices.Clone(v.Escalation.Options)
		}
		return v
	case Amendment:
		v.Citations = slices.Clone(v.Citations)
		v.Upstream = clonePointer(v.Upstream)
		return v
	case Ruling:
		v.Citations = slices.Clone(v.Citations)
		v.Changes = slices.Clone(v.Changes)
		return v
	case TurnRequest:
		v.Resume = clonePointer(v.Resume)
		return v
	case TurnResponse:
		v.Result.Outcome = clonePointer(v.Result.Outcome)
		if v.Result.Outcome != nil {
			v.Result.Outcome.Card = clonePointer(v.Result.Outcome.Card)
		}
		v.Result.ToolCounts = maps.Clone(v.Result.ToolCounts)
		v.Result.Limit = clonePointer(v.Result.Limit)
		v.Classification = clonePointer(v.Classification)
		if v.Classification != nil {
			v.Classification.ToolCounts = maps.Clone(v.Classification.ToolCounts)
		}
		v.ClassifierUsage = slices.Clone(v.ClassifierUsage)
		v.Stop = clonePointer(v.Stop)
		return v
	case Status:
		v.Agents = slices.Clone(v.Agents)
		return v
	case PriorityChange:
		v.Order = slices.Clone(v.Order)
		return v
	case Document, Transition, Agent, Cost, nil:
		return record
	default:
		panic(fmt.Sprintf("unsupported cached record %T", record))
	}
}
