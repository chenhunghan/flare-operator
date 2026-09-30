package workerscript

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
)

// RegisterIndexes registers the field index SetupWithManager registers, for a Reconciler that
// a test drives outside the manager (listing through the manager's cache).
func RegisterIndexes(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &workersv1alpha1.WorkerScript{}, indexRefs, refKeys)
}
