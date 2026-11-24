package lifecycle_test

import (
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func TestMain(m *testing.M) {
	os.Exit(journey.Main(m))
}
