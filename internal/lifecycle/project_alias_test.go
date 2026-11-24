package lifecycle_test

import (
	runtime "github.com/kpenfound/osmia/internal/runtime"
)

var factoryTarget = runtime.Target{Scope: "factory"}

const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"
