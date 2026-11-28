package trace

import (
	"slices"
	"testing"
)

func TestDisplayRoleMapsTheStoredManagerNameAndNothingElse(t *testing.T) {
	for stored, want := range map[string]string{
		Manager:   ManagerDisplay,
		"mason":   "mason",
		"owner":   "owner",
		"jev":     "jev",
		"manager": "manager",
		"":        "",
	} {
		if got := DisplayRole(stored); got != want {
			t.Errorf("DisplayRole(%q) = %q, want %q", stored, got, want)
		}
	}
	if ManagerDisplay != "manager" {
		t.Fatalf("the display name of the manager is %q", ManagerDisplay)
	}
	if DisplayRole(DecidedByManager) != "manager" {
		t.Fatalf("the manager's stored decider shows as %q", DisplayRole(DecidedByManager))
	}
}

func TestStoredRoleInvertsDisplayRoleAndKeepsTheStoredName(t *testing.T) {
	for _, role := range []string{Manager, "mason", "reviewer", "committee", "architect"} {
		if got := StoredRole(DisplayRole(role)); got != role {
			t.Errorf("StoredRole(DisplayRole(%q)) = %q", role, got)
		}
	}
	if got := StoredRole(Manager); got != Manager {
		t.Errorf("StoredRole of the stored name = %q, want it unchanged", got)
	}
}

func TestDisplayRolesMapsEachRoleWithoutChangingTheInput(t *testing.T) {
	in := []string{"mason", Manager}
	got := DisplayRoles(in)
	if !slices.Equal(got, []string{"mason", "manager"}) {
		t.Fatalf("DisplayRoles = %v", got)
	}
	if in[1] != Manager {
		t.Fatalf("the input changed to %v", in)
	}
	if DisplayRoles(nil) != nil {
		t.Fatal("a nil list maps to a non-nil list")
	}
}

func TestActorDisplayMapsOnlyTheID(t *testing.T) {
	stored := Actor{Kind: "agent", ID: Manager}
	if got := stored.Display(); got != (Actor{Kind: "agent", ID: "manager"}) {
		t.Fatalf("Display = %+v", got)
	}
	if stored.ID != Manager {
		t.Fatalf("the actor changed to %+v", stored)
	}
	if owner := (Actor{Kind: "owner", ID: "local"}); owner.Display() != owner {
		t.Fatalf("an owner shows as %+v", owner.Display())
	}
}

func TestDisplayRulerMapsTheStoredRulerNameOfTheManager(t *testing.T) {
	for by, want := range map[string]string{ManagerRulerName: "manager", "owner": "owner", "jev": "jev", "": ""} {
		if got := DisplayRuler(by); got != want {
			t.Errorf("DisplayRuler(%q) = %q, want %q", by, got, want)
		}
	}
	if ManagerRulerName != "chief of staff" {
		t.Fatalf("the stored ruler name changed to %q", ManagerRulerName)
	}
}
