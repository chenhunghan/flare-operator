package main

// Controllers join the manager by registering themselves with internal/controller from an
// init function. Blank-import each controller package here (one line per workstream).
import (
	_ "flare.dev/operator/internal/controller/account"
	_ "flare.dev/operator/internal/controller/tunnel"
	_ "flare.dev/operator/internal/controller/vpcservice"
)
