package service

import (
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) reservationWaitPass(repository *trace.Repository) {
	rec := s.slots
	project := repository.Project()
	waits := rec.Find(slotspans.Wait, slotspans.SourceReservation, func(iv slotspans.Interval) bool {
		return iv.Recovered && iv.Project == project
	})
	if len(waits) == 0 {
		return
	}
	streams, err := intake.NonSessionWorkstreams(repository)
	if err != nil {
		return
	}
	for _, iv := range waits {
		outcome, ended := s.reservationWaitFate(repository, streams, iv)
		if ended {
			rec.EndRecovered(iv, rec.Now(), outcome)
		}
	}
}

func (s *Service) reservationWaitFate(repository *trace.Repository, streams []config.WorkstreamID, iv slotspans.Interval) (string, bool) {
	if !slices.Contains(streams, iv.Workstream) {
		return slotspans.Removed, true
	}
	records, err := repository.Operations(iv.Workstream)
	if err != nil {
		return "", false
	}
	hold := s.holding(repository)
	pending, held := 0, 0
	for _, record := range records {
		if record.Acknowledged || modelOperationRole(record.Operation) != iv.Role {
			continue
		}
		pending++
		if hold(iv.Workstream, record.Operation) {
			held++
		}
	}
	switch {
	case pending == 0:
		if gone, err := abandoned(repository, iv.Workstream); err != nil {
			return "", false
		} else if gone {
			return slotspans.Canceled, true
		}
		return slotspans.Completed, true
	case held == pending:
		return slotspans.Blocked, true
	}
	return "", false
}
