package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	RegistrationActor = trace.Actor{Kind: "owner", ID: "local"}
	ServiceActor      = trace.Actor{Kind: "service", ID: "osmia"}
)
