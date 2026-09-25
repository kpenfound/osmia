package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The daily budget's pause and the day it was set on are written together,
// survive reopening, and follow the pause replacement rule.
func TestBudgetPauseRecordsItsDay(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	factory := Target{Scope: "factory"}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	budget := Pause{Target: factory, Mode: "soft", Reason: "Daily budget reached", Source: PauseDailyBudget, SetAt: at}
	if err := s.SetBudgetPause(Pause{Target: factory, Mode: "soft", Reason: "Owner", Source: PauseOwner, SetAt: at}, "2026-09-24"); !errors.Is(err, ErrValidation) {
		t.Fatalf("owner-attributed budget pause: %v", err)
	}
	if err := s.SetBudgetPause(budget, "24 September"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed day: %v", err)
	}
	must(t, s.SetBudgetPause(budget, "2026-09-24"))
	must(t, s.ClearPause(factory, PauseOwner))
	must(t, s.Close())
	s = open(t, in)
	st, _ := s.Snapshot()
	if st.BudgetPausedOn != "2026-09-24" || len(st.Pauses) != 0 {
		t.Fatalf("reopened state: %+v", st)
	}
	must(t, s.SetPause(Pause{Target: factory, Mode: "soft", Reason: "Travelling", Source: PauseOwner, SetAt: at}))
	budget.SetAt = at.Add(24 * time.Hour)
	if err := s.SetBudgetPause(budget, "2026-09-25"); !errors.Is(err, ErrValidation) {
		t.Fatalf("budget replaced the owner's pause: %v", err)
	}
	if st, _ := s.Snapshot(); st.BudgetPausedOn != "2026-09-24" || st.Pauses[0].Source != PauseOwner {
		t.Fatalf("refused budget pause changed state: %+v", st)
	}
	must(t, s.Close())
	must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), []byte(`{"version":1,"budget_paused_on":"2026-9-24"}`), 0600))
	if _, _, err := Open(in); err == nil {
		t.Fatal("opened a malformed budget pause day")
	}
}
