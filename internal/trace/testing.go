package trace

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
)

// CommitCount reports how many commits the trace holds: the project's own,
// or, for a workstream, that workstream's.
func (r *Repository) CommitCount(stream string) (int, error) {
	target, err := r.scoped(config.WorkstreamID(stream))
	if err != nil {
		return 0, err
	}
	if target.reads == nil {
		return 0, fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	var n int
	err = target.reads.QueryRowContext(context.Background(), "SELECT count(*) FROM commits").Scan(&n)
	return n, err
}

// RecordDecodeCount reports stored record decodes by this handle in scope.
func (r *Repository) RecordDecodeCount(stream config.WorkstreamID) (uint64, error) {
	target, err := r.scoped(stream)
	if err != nil {
		return 0, err
	}
	return target.decoded.Load(), nil
}

// WorkgroupTraceOpenCount reports attempts to acquire a shared workgroup
// handle in this process, including acquisitions that reuse an open handle.
func WorkgroupTraceOpenCount(id config.WorkgroupID) uint64 {
	return sharedWorkgroupTraceFor(id).opens.Load()
}

// WorkgroupTraceDiskOpenCount reports how many times this process opened and
// scanned workgroup id's trace, rather than reusing an instance another holder
// had open. It is the cost an acquisition pays when no handle is kept.
func WorkgroupTraceDiskOpenCount(id config.WorkgroupID) uint64 {
	return sharedWorkgroupTraceFor(id).diskOpens.Load()
}

// CrossRepoProjectSpecReadCount reports shared project spec lookup attempts.
func CrossRepoProjectSpecReadCount(id config.WorkgroupID) uint64 {
	return sharedWorkgroupTraceFor(id).projectSpecReads.Load()
}

// WorkgroupReadCount reports workgroup list lookup attempts on this handle.
func (r *Repository) WorkgroupReadCount() uint64 {
	return r.workgroupReads.Load()
}
