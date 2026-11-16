package trace

// RecordPath returns the trace file that holds a record's revisions.
func RecordPath(v Record) string { return recordPath(v) }
