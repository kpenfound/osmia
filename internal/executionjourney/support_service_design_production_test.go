package executionjourney

import (
	testing "testing"
)

func (f *productionDesignFixture) start(t *testing.T) {
	t.Helper()
	f.architectFixture.start(t)
	f.checkRepos(t)
}

func (f *productionDesignFixture) checkRepos(t *testing.T) {
	t.Helper()
	f.sharedDesign().CheckRepos(t)
}
