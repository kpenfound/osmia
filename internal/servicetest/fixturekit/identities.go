// Package fixturekit contains service-independent fake boundaries and trace inputs.
package fixturekit

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const Quiet config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
const SessionRole = "session"
const MasonRole = "mason"
const JJCheckTimeout = 5 * time.Second

var OwnerActor = trace.Actor{Kind: "owner", ID: "local"}
var ServiceActor = trace.Actor{Kind: "service", ID: "osmia"}
