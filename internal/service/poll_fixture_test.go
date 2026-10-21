package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func nextPollDue(p *pullRequestAttempts, project config.ProjectID, interval time.Duration, now time.Time) time.Time {
	var attempts []intakefixture.PollAttempt
	for _, a := range p.AttemptsForTest() {
		attempts = append(attempts, intakefixture.PollAttempt{Project: a.Project, At: a.At})
	}
	return intakefixture.NextPollDue(attempts, project, interval, now)
}
