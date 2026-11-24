package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

const removedStatus = "removed"

var streamSources = []publishSource{workstreamRecords, sealedSpecRecords, workgroupSpecRecords, decisionRecords}

func keptRecords(repo *trace.Repository, live []hearsay.Record) ([]hearsay.Record, error) {
	stored, err := repo.HearsayKept()
	supplied := make(map[string]bool, len(live))
	for _, rec := range live {
		supplied[rec.Ref()] = true
	}
	var out []hearsay.Record
	for _, k := range stored {
		if k.Released {
			continue
		}
		var rec hearsay.Record
		if jerr := json.Unmarshal(k.Data, &rec); jerr != nil || rec.Source != k.Source || rec.Key != k.Key {
			err = errors.Join(err, fmt.Errorf("kept record %s: unreadable", hearsay.Ref(k.Source, k.Key)))
			continue
		}
		if !supplied[rec.Ref()] {
			out = append(out, rec)
		}
	}
	slices.SortStableFunc(out, func(a, b hearsay.Record) int {
		if a.Tier != b.Tier {
			return a.Tier - b.Tier
		}
		return compareStrings(a.Ref(), b.Ref())
	})
	return out, err
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func releaseKept(ctx context.Context, repo *trace.Repository, kept []hearsay.Record, posted map[string]hearsay.Posted, at time.Time) error {
	var released []trace.KeptRecord
	for _, rec := range kept {
		if current := rec.Current(); current == "" || posted[rec.Ref()].Digest == current {
			released = append(released, trace.KeptRecord{Source: rec.Source, Key: rec.Key, Released: true})
		}
	}
	return repo.KeepHearsay(ctx, released, at)
}

func keepPending(ctx context.Context, repo *trace.Repository, records []hearsay.Record, at time.Time) error {
	stored, err := repo.HearsayPosted()
	if err != nil {
		return err
	}
	posted := make(map[string]hearsay.Posted, len(stored))
	for _, p := range stored {
		posted[hearsay.Ref(p.Source, p.Key)] = hearsay.Posted{Digest: p.Digest, UpdatedAt: p.UpdatedAt}
	}
	existing, err := repo.HearsayKept()
	if err != nil {
		return err
	}
	var entries []trace.KeptRecord
	for _, rec := range records {
		pending, ok := hearsay.Unposted(rec, posted[rec.Ref()])
		if !ok {
			if k, held := existing[trace.KeptID(rec.Source, rec.Key)]; held && !k.Released {
				entries = append(entries, trace.KeptRecord{Source: rec.Source, Key: rec.Key, Released: true})
			}
			continue
		}
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		entries = append(entries, trace.KeptRecord{Source: rec.Source, Key: rec.Key, Data: data})
	}
	return repo.KeepHearsay(ctx, entries, at)
}

func (s *Service) keepForCleanup(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) error {
	cfg := s.about(repo)
	if !cfg.Hearsay.Publishing() {
		return nil
	}
	in, err := streamPublishInputs(ctx, repo, cfg, stream)
	if err != nil || in == nil {
		return err
	}
	var records []hearsay.Record
	for _, source := range streamSources {
		records = append(records, source(in)...)
	}
	return keepPending(ctx, repo, records, s.now())
}

func streamPublishInputs(ctx context.Context, repo *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (*publishInputs, error) {
	publish := cfg.Hearsay.Publish
	in := &publishInputs{cfg: cfg, owner: cfg.Hearsay.Principal, workSource: publish.WorkSource, ownerSource: publish.OwnerSource, project: repo.Project()}
	known, err := repo.Workstreams()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(known, stream) {
		return nil, nil
	}
	eligible, err := intake.NonSessionWorkstreams(repo)
	if err != nil {
		return nil, err
	}
	statuses, err := repo.Statuses()
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if status.Workstream != stream {
			continue
		}
		if status.Kind == trace.SessionKind || !slices.Contains(eligible, stream) || !hearsay.ValidID(string(stream)) {
			return nil, nil
		}
		st, err := readPublishStream(repo, status)
		if err != nil {
			return nil, fmt.Errorf("workstream %s: %w", stream, err)
		}
		if !st.decisionsRead {
			return nil, fmt.Errorf("workstream %s: its decisions cannot be read", stream)
		}
		in.streams = append(in.streams, st)
		return in, nil
	}
	return nil, nil
}

func (in *publishInputs) removalRecord(removedAt time.Time) hearsay.Record {
	rec := projectRecords(in)
	if len(rec) == 0 {
		return hearsay.Record{}
	}
	w := rec[0].Versions[0]
	w.State, w.Status = coreAbandoned, removedStatus
	w.UpdatedAt = hearsay.Time(removedAt)
	rec[0].Versions = []hearsay.Wire{w}
	return rec[0]
}

func (s *Service) keepForRemoval(ctx context.Context, repo *trace.Repository) error {
	cfg := s.about(repo)
	if !s.current().Hearsay.Publishing() {
		return nil
	}
	c := *cfg
	c.Hearsay = s.current().Hearsay
	cfg = &c
	in, err := collectPublishInputs(ctx, repo, cfg)
	if err != nil {
		return err
	}
	s.removed.unsettle(repo.Project())
	removal := in.removalRecord(s.now())
	if removal.Key == "" {
		return nil
	}
	var records []hearsay.Record
	for _, source := range publishSources {
		for _, rec := range source(in) {
			if rec.Ref() != removal.Ref() {
				records = append(records, rec)
			}
		}
	}
	return keepPending(ctx, repo, append(records, removal), s.now())
}

type removedPublication struct {
	mu       sync.Mutex
	settled  map[config.ProjectID]bool
	reported map[config.ProjectID]string
}

func (r *removedPublication) isSettled(id config.ProjectID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled[id]
}

func (r *removedPublication) unsettle(id config.ProjectID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.settled, id)
}

func (r *removedPublication) settle(id config.ProjectID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled == nil {
		r.settled = map[config.ProjectID]bool{}
	}
	r.settled[id] = true
}

func (r *removedPublication) report(id config.ProjectID, message string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reported == nil {
		r.reported = map[config.ProjectID]string{}
	}
	if r.reported[id] == message {
		return false
	}
	r.reported[id] = message
	return true
}

func (s *Service) publishRemoved(ctx context.Context) {
	var delay time.Duration
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = publishInterval
		if s.current().Hearsay.Publishing() {
			if retry := s.publishRemovedPass(ctx); retry {
				delay = publishBackoff
			}
		}
		s.hearsayLoopPassed()
	}
}

func (s *Service) publishRemovedPass(ctx context.Context) (backoff bool) {
	cfg := s.current()
	if !cfg.Hearsay.Publishing() {
		return false
	}
	directory, err := cfg.Root.Projects()
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		id := config.ProjectID(entry.Name())
		if ctx.Err() != nil {
			return backoff
		}
		if !entry.IsDir() || config.CheckProjectIDs(id) != nil || s.removed.isSettled(id) {
			continue
		}
		retry, settled := s.publishRemovedProject(ctx, id)
		backoff = backoff || retry
		if settled {
			s.removed.settle(id)
		}
	}
	return backoff
}

func (s *Service) publishRemovedProject(ctx context.Context, id config.ProjectID) (backoff, settled bool) {
	cfg := s.current()
	directory, err := cfg.Root.ProjectTrace(id)
	if err != nil {
		return false, true
	}
	s.projectMu.Lock()
	if s.current().Active(id) {
		s.projectMu.Unlock()
		return false, false
	}
	if ok, err := trace.HasTraceDatabase(directory); err != nil || !ok {
		s.projectMu.Unlock()
		return false, err == nil
	}
	repo, err := trace.OpenByID(cfg.Root, id)
	s.projectMu.Unlock()
	if repo != nil {
		defer repo.Close()
	}
	if err != nil && repo == nil {
		if s.removed.report(id, err.Error()) {
			s.log().Warn("hearsay publishing of a removed project cannot open its trace", "project", string(id), "error", err.Error())
		}
		return true, false
	}
	token := hearsay.PublisherToken(cfg.Hearsay, nil)
	if token == "" {
		return true, false
	}
	removed, err := removalPending(repo, id)
	if err != nil {
		return true, false
	}
	if !removed {
		return false, true
	}
	res, err := s.postRecords(ctx, repo, cfg, token, nil)
	if ctx.Err() != nil {
		return false, false
	}
	if summary := publishSummary(res, err); summary != "" {
		if s.removed.report(id, summary) {
			s.log().Warn("hearsay publishing of a removed project incomplete", "project", string(id), "reason", summary)
		}
		return true, false
	}
	s.removed.report(id, "")
	pending, err := repo.HearsayKept()
	if err != nil {
		return true, false
	}
	for _, k := range pending {
		if !k.Released {
			return true, false
		}
	}
	return false, true
}

func removalPending(repo *trace.Repository, id config.ProjectID) (bool, error) {
	kept, err := repo.HearsayKept()
	if err != nil {
		return false, err
	}
	for _, k := range kept {
		if !k.Released && k.Key == "project/"+string(id) {
			return true, nil
		}
	}
	return false, nil
}
