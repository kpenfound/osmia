package checks

import "testing"

var webDriverStepLists = []string{
	"goto .; snapshot; screenshot; console; requests",
	"goto notes; fill id=note | Remember the milk; click text=Add note; snapshot",
	"goto notes; snapshot; fill id=note | Ada%3A first; click e10; snapshot; screenshot full",
	"goto %2Fnotes; fill id=note | a%3Ab%23c%3Fd; press id=note | Enter; snapshot",
}

func TestWebDriverStepListsPassPreviewValidationAsLeverArguments(t *testing.T) {
	source := t.TempDir()
	for _, steps := range webDriverStepLists {
		for _, call := range [][]string{
			{"verify", "--steps=" + steps},
			{"verify", "--steps", steps},
			{"verification-fixture", "verify", "--steps=" + steps},
		} {
			if err := ValidatePreviewCall(call); err != nil {
				t.Errorf("%q: %v", call, err)
			}
			if err := ValidatePreviewSource(source, call); err != nil {
				t.Errorf("%q: %v", call, err)
			}
		}
	}
}

func TestWebDriverStepListsRefuseUnencodedColons(t *testing.T) {
	for _, steps := range []string{"goto http://app/", "fill id=note | a:b"} {
		if err := ValidatePreviewCall([]string{"verify", "--steps=" + steps}); err == nil {
			t.Errorf("%q passed preview validation", steps)
		}
	}
}
