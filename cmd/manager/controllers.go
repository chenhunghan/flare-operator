package main

// Controllers join the manager by registering themselves with internal/controller from an
// init function. Blank-import each controller package here (one line per workstream).
import (
	_ "github.com/chenhunghan/flare-operator/internal/controller/account"
	_ "github.com/chenhunghan/flare-operator/internal/controller/pagesdeployment"
	_ "github.com/chenhunghan/flare-operator/internal/controller/pagesproject"
	_ "github.com/chenhunghan/flare-operator/internal/controller/tunnel"
	_ "github.com/chenhunghan/flare-operator/internal/controller/vpcservice"
	_ "github.com/chenhunghan/flare-operator/internal/controller/workerscript"
	_ "github.com/chenhunghan/flare-operator/internal/generic/kinds" // KVNamespace, Queue, D1Database (generated kinds)
)
