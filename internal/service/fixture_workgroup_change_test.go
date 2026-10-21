package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func currentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.changeContext(context.Background(), string(address))
	must(t, err)
	defer close()
	changes, err := readChanges(wt, g.Members[0])
	must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return WorkgroupChange{}
}
