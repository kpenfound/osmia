package intake

import (
	"errors"
)

var ErrChangeBoundary = errors.New("an owner-approved change holds work at a unit boundary")
