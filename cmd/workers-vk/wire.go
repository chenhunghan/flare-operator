package main

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/chenhunghan/flare-operator/internal/vk/standin"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// StandInControllerName names the stand-in controller's event recorder.
const StandInControllerName = "workers-vk-standin"

// wire builds the implementations of workstream A (internal/workerlogs) and C
// (internal/vk/standin).
func wire(o Options) (implementations, error) {
	sel, err := standin.ParseNamespaceSelector(o.VK.NamespaceSelector)
	if err != nil {
		return implementations{}, err
	}
	if o.VK.PodImage == "" {
		o.VK.PodImage = standin.DefaultImage
	}
	builder := standin.NewBuilder(o.PodConfig())
	src := workerlogs.NewSource(o.VK.Logs)
	return implementations{
		StatusMapper: standin.NewStatusMapper(),
		Streamer:     workerlogs.NewStreamer(src, workerlogs.NewFormatter(), o.VK.Logs, time.Now),
		ParseOptions: workerlogs.ParseOptions,
		SetupStandIn: func(mgr ctrl.Manager) error {
			return (&standin.Reconciler{
				Client:            mgr.GetClient(),
				APIReader:         mgr.GetAPIReader(),
				Builder:           builder,
				NamespaceSelector: sel,
				Recorder:          mgr.GetEventRecorder(StandInControllerName),
			}).SetupWithManager(mgr)
		},
	}, nil
}
