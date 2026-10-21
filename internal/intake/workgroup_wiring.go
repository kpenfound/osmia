package intake

import (
	"errors"
)

var ErrWorkgroupJoining = errors.New("the workgroup is still joining its members")
