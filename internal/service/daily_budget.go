package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

type dailyBudget struct {
	s          *Service
	repository *trace.Repository
}

var factoryTarget = runtime.Target{Scope: "factory"}

func (d dailyBudget) Pass(ctx context.Context) error {
	day := d.s.localDay()
	limitText := d.s.current().Budget.PerDay
	spend := spendTotal{known: new(big.Rat)}
	if limitText != "" {
		own, err := daySpend(day, d.repository)
		if err != nil {
			return err
		}
		_, projects := d.s.runtimes()
		members := make([]config.ProjectID, len(projects))
		for i, p := range projects {
			members[i] = p.repository.Project()
		}
		var changed bool
		spend, changed = d.s.spends.total(d.repository.Project(), day.date, own, members)
		if changed {
			d.s.hub.Publish(Event{Kind: EventSpend})
		}
	}
	return d.enforce(day, limitText, spend)
}

func (d dailyBudget) enforce(day calendarDay, limitText string, spend spendTotal) error {
	state, _ := d.s.store.Snapshot()
	var current *runtime.Pause
	for _, p := range state.Pauses {
		if p.Target == factoryTarget {
			current = &p
		}
	}
	if current != nil && current.Source == runtime.PauseDailyBudget && day.of(current.SetAt) != day.date {
		err := d.s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget)
		if err != nil && !errors.Is(err, runtime.ErrValidation) {
			return err
		}
		if err == nil {
			d.s.log().Info("pause cleared", "scope", factoryTarget.Scope, "reason", "the daily budget pause expired at the next local day")
		}
		return d.enforce(day, limitText, spend)
	}
	if limitText == "" || current != nil || state.BudgetPausedOn == day.date {
		return nil
	}
	limit, ok := new(big.Rat).SetString(limitText)
	if !ok {
		return fmt.Errorf("invalid per-day budget %q", limitText)
	}
	if spend.known.Cmp(limit) < 0 {
		return nil
	}
	reason := fmt.Sprintf("Daily budget reached: known spend on %s is USD %s of the USD %s per-day budget; dispatch resumes at the next local day", day.date, spend, limitText)
	if spend.unknown > 0 {
		reason += fmt.Sprintf("; %d attempt(s) have unknown cost, so actual spend may be higher", spend.unknown)
	}
	err := d.s.store.SetBudgetPause(runtime.Pause{Target: factoryTarget, Mode: "soft", Reason: reason, Source: runtime.PauseDailyBudget, SetAt: d.s.now()}, day.date)
	if errors.Is(err, runtime.ErrValidation) {
		return nil
	}
	if err == nil {
		d.s.log().Info("pause set", "scope", factoryTarget.Scope, "mode", "soft", "reason", reason)
	}
	return err
}

type daySpends struct {
	mu       sync.Mutex
	projects map[config.ProjectID]projectDaySpend
}

type projectDaySpend struct {
	date  string
	spend spendTotal
}

func (d *daySpends) total(project config.ProjectID, date string, own spendTotal, members []config.ProjectID) (spendTotal, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.projects == nil {
		d.projects = map[config.ProjectID]projectDaySpend{}
	}
	previous, ok := d.projects[project]
	changed := !ok || previous.date != date || !previous.spend.equal(own)
	d.projects[project] = projectDaySpend{date, own}
	for id := range d.projects {
		if id != project && !slices.Contains(members, id) {
			delete(d.projects, id)
			changed = true
		}
	}
	return own.plus(d.sum(date, members, project)), changed
}

func (d *daySpends) of(date string, members []config.ProjectID) spendTotal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sum(date, members, "")
}

func (d *daySpends) sum(date string, members []config.ProjectID, except config.ProjectID) spendTotal {
	total := spendTotal{known: new(big.Rat)}
	for id, published := range d.projects {
		if id != except && published.date == date && slices.Contains(members, id) {
			total = total.plus(published.spend)
		}
	}
	return total
}

type calendarDay struct {
	date       string
	start, end time.Time
	location   *time.Location
}

func (d calendarDay) of(t time.Time) string {
	return t.In(d.location).Format(runtime.DayLayout)
}

func (s *Service) localDay() calendarDay {
	location := s.options.Location
	if location == nil {
		location = time.Local
	}
	y, m, day := s.now().In(location).Date()
	start := time.Date(y, m, day, 0, 0, 0, 0, location)
	return calendarDay{date: start.Format(runtime.DayLayout), start: start, end: time.Date(y, m, day+1, 0, 0, 0, 0, location), location: location}
}

func daySpend(day calendarDay, repositories ...*trace.Repository) (spendTotal, error) {
	var today []trace.Cost
	for _, repository := range repositories {
		var costs []trace.Cost
		err := repository.WithWorkstreamDiscovery(func([]config.WorkstreamID) error {
			var err error
			costs, err = repository.Costs()
			return err
		})
		if err != nil {
			return spendTotal{}, err
		}
		for _, c := range costs {
			if !c.Entry.At.Before(day.start) && c.Entry.At.Before(day.end) {
				today = append(today, c)
			}
		}
	}
	return sumCosts(today)
}

func (s *Service) dailyBudgetStatus() *DailyBudgetStatus {
	cfg, projects := s.runtimes()
	if cfg == nil || cfg.Budget.PerDay == "" {
		return nil
	}
	day := s.localDay()
	var members []config.ProjectID
	for _, p := range projects {
		if cfg.Active(p.id) {
			members = append(members, p.repository.Project())
		}
	}
	spend := s.spends.of(day.date, members)
	return &DailyBudgetStatus{Day: day.date, SpendUSD: spend.String(), LimitUSD: cfg.Budget.PerDay, UnknownCosts: spend.unknown, LowerBound: spend.unknown > 0}
}
