package main

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/chenhunghan/flare-operator/internal/vk"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// implementations are the parts of the virtual kubelet that workstreams A (internal/workerlogs)
// and C (internal/vk/standin) provide; wire builds them.
type implementations struct {
	StatusMapper standin.StatusMapper
	Streamer     workerlogs.Streamer
	ParseOptions vk.ParseOptionsFunc
	// SetupStandIn adds the stand-in controller (one Pod per WorkerScript) to the manager.
	SetupStandIn func(ctrl.Manager) error
}
