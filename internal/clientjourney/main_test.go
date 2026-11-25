package clientjourney

import (
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func TestMain(m *testing.M) {
	journey.FreeRouting = true
	os.Exit(journey.Main(m))
}
