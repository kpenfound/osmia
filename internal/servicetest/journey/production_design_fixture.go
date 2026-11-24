package journey

import (
	testing "testing"
)

func (f *ProductionDesignFixture) CheckRepos(t *testing.T) {
	t.Helper()
	f.SharedDesign().CheckRepos(t)
}
