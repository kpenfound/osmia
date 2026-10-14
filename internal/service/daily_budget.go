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

// dailyBudget pauses the factory with a soft pause attributed to the daily
// budget once the known spend of the current local calendar day, across every
// active project, reaches budget.per_day. The pause expires at the next local day boundary. The
// budget pauses at most once per local day, so an owner who clears or
// replaces its pause keeps the factory running for the rest of that day.
//
// Each project's pass sums the spend in its own trace alone and counts the
// other projects' spend from what their own passes published to daySpends.
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
		// The spend is published on every pass, paused or not, so status
		// reports it.
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
			d.s.hub.publish(Event{Kind: EventSpend})
		}
	}
	return d.enforce(day, limitText, spend)
}

// enforce clears a daily budget pause of an earlier local day, and pauses
// the factory once spend reaches limitText.
func (d dailyBudget) enforce(day calendarDay, limitText string, spend spendTotal) error {
	state, _ := d.s.store.Snapshot()
	var current *runtime.Pause
	for _, p := range state.Pauses {
		if p.Target == factoryTarget {
			current = &p
		}
	}
	if current != nil && current.Source == runtime.PauseDailyBudget && day.of(current.SetAt) != day.date {
		// The owner may have replaced the pause since the snapshot; it stays.
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
	// A factory pause the owner set since the snapshot stays; the budget
	// cannot replace it.
	if errors.Is(err, runtime.ErrValidation) {
		return nil
	}
	if err == nil {
		d.s.log().Info("pause set", "scope", factoryTarget.Scope, "mode", "soft", "reason", reason)
	}
	return err
}

// daySpends is each active project's known spend of one local day, as the
// project's own daily budget pass last summed it from its own trace, so no
// project reads another's trace for the factory's daily spend. A project's
// published spend trails a cost it records only until its next pass, which
// that cost's record wakes. Its zero value is ready.
type daySpends struct {
	mu       sync.Mutex
	projects map[config.ProjectID]projectDaySpend
}

type projectDaySpend struct {
	date  string
	spend spendTotal
}

// total publishes own as project's spend on date and returns the factory's
// spend on date: own and the spend each other project among members last
// published for date. It forgets the spend of every project not among
// members, and reports whether what it holds changed.
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

// of returns the factory's spend on date: the spend each project among
// members last published for date.
func (d *daySpends) of(date string, members []config.ProjectID) spendTotal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sum(date, members, "")
}

// sum adds up the spend on date that each project among members but except
// last published. Its caller holds mu.
func (d *daySpends) sum(date string, members []config.ProjectID, except config.ProjectID) spendTotal {
	total := spendTotal{known: new(big.Rat)}
	for id, published := range d.projects {
		if id != except && published.date == date && slices.Contains(members, id) {
			total = total.plus(published.spend)
		}
	}
	return total
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

// daySpend sums the costs of every workstream's attempts that started on day
// in the repositories.
func daySpend(day calendarDay, repositories ...*trace.Repository) (spendTotal, error) {
	var today []trace.Cost
	for _, repository := range repositories {
		costs, err := repository.Costs()
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

// dailyBudgetStatus reports today's known spend across every active
// project, as their own daily budget passes last published it, against
// budget.per_day, or nil when no daily budget is configured. It reads no
// trace.
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
