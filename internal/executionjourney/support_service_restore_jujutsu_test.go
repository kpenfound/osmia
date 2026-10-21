package executionjourney

import (
	filepath "path/filepath"
	testing "testing"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func checkpoints(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.Checkpoints(t, jujutsuRepository(f, directory))
}

func interrupted(t *testing.T, f *shedFixture, directory string) (string, bool) {
	t.Helper()
	return intakefixture.Interrupted(t, directory, checkpoints(t, f, directory), operationLog(t, f, directory))
}

func operationLog(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.OperationLog(t, jujutsuRepository(f, directory), filepath.Dir(f.clone))
}

func jujutsuRepository(f *shedFixture, directory string) string {
	return intakefixture.JujutsuRepository(f.s.cfg.Root, f.s.cfg.Project.ID, directory)
}
