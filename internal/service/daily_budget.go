package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// dailyBudget pauses the factory with a soft pause attributed to the daily
// budget once the known spend of the current local calendar day reaches
// budget.per_day. The pause expires at the next local day boundary. The
// budget pauses at most once per local day, so an owner who clears or
// replaces its pause keeps the factory running for the rest of that day.
type dailyBudget struct {
	s          *Service
	repository *trace.Repository
}

var factoryTarget = runtime.Target{Scope: "factory"}

func (d dailyBudget) Pass(ctx context.Context) error {
	day := d.s.localDay()
	state, _ := d.s.store.Snapshot()
	var current *runtime.Pause
	for _, p := range state.Pauses {
		if p.Target == factoryTarget {
			current = &p
		}
	}
	if current != nil && current.Source == runtime.PauseDailyBudget && day.of(current.SetAt) != day.date {
		// The owner may have replaced the pause since the snapshot; it stays.
		if err := d.s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget); err != nil && !errors.Is(err, runtime.ErrValidation) {
			return err
		}
		return d.Pass(ctx)
	}
	limitText := d.s.current().Budget.PerDay
	if limitText == "" || current != nil || state.BudgetPausedOn == day.date {
		return nil
	}
	limit, ok := new(big.Rat).SetString(limitText)
	if !ok {
		return fmt.Errorf("invalid per-day budget %q", limitText)
	}
	spend, err := daySpend(d.repository, day)
	if err != nil {
		return err
	}
	if spend.known.Cmp(limit) < 0 {
		return nil
	}
	reason := fmt.Sprintf("Daily budget reached: known spend on %s is USD %s of the USD %s per-day budget; dispatch resumes at the next local day", day.date, spend, limitText)
	if spend.unknown > 0 {
		reason += fmt.Sprintf("; %d attempt(s) have unknown cost, so actual spend may be higher", spend.unknown)
	}
	err = d.s.store.SetBudgetPause(runtime.Pause{Target: factoryTarget, Mode: "soft", Reason: reason, Source: runtime.PauseDailyBudget, SetAt: d.s.now()}, day.date)
	// A factory pause the owner set since the snapshot stays; the budget
	// cannot replace it.
	if errors.Is(err, runtime.ErrValidation) {
		return nil
	}
	return err
}

// calendarDay is one calendar day of the service host's time zone.
type calendarDay struct {
	date       string
	start, end time.Time
	location   *time.Location
}

// of returns the date of t in the day's time zone.
func (d calendarDay) of(t time.Time) string {
	return t.In(d.location).Format(runtime.DayLayout)
}

// localDay is the current calendar day of the service host.
func (s *Service) localDay() calendarDay {
	location := s.options.Location
	if location == nil {
		location = time.Local
	}
	y, m, day := s.now().In(location).Date()
	start := time.Date(y, m, day, 0, 0, 0, 0, location)
	return calendarDay{date: start.Format(runtime.DayLayout), start: start, end: time.Date(y, m, day+1, 0, 0, 0, 0, location), location: location}
}

// daySpend sums the costs of every workstream's attempts that started on day.
func daySpend(repository *trace.Repository, day calendarDay) (spendTotal, error) {
	costs, err := repository.Costs()
	if err != nil {
		return spendTotal{}, err
	}
	var today []trace.Cost
	for _, c := range costs {
		if !c.Entry.At.Before(day.start) && c.Entry.At.Before(day.end) {
			today = append(today, c)
		}
	}
	return sumCosts(today)
}

// dailyBudgetStatus reports today's known spend against budget.per_day, or
// nil when no daily budget is configured.
func (s *Service) dailyBudgetStatus() (*DailyBudgetStatus, *Diagnostic) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if cfg == nil || cfg.Budget.PerDay == "" {
		return nil, nil
	}
	day := s.localDay()
	spend := spendTotal{known: new(big.Rat)}
	if cfg.HasProject() && active != nil {
		var err error
		if spend, err = daySpend(active.repository, day); err != nil {
			return nil, &Diagnostic{"daily_budget", Internal, fmt.Sprintf("cannot read today's spend in project %s; check the trace repository", cfg.Project.ID)}
		}
	}
	return &DailyBudgetStatus{Day: day.date, SpendUSD: spend.String(), LimitUSD: cfg.Budget.PerDay, UnknownCosts: spend.unknown, LowerBound: spend.unknown > 0}, nil
}
