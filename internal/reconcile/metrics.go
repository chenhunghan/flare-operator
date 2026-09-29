package reconcile

import (
	"reflect"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
)

// MetricSyncFailures counts reconciles that ended with Synced=False, by kind and condition
// reason (a fixed set of constants such as ReconcileError, RateLimited, AccountNotReady,
// DependencyNotReady, NameConflict, Immutable). It complements controller-runtime's
// controller_runtime_reconcile_errors_total{controller}, which misses failures the controllers
// report through conditions and a timed requeue instead of an error.
var MetricSyncFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "flare_managed_sync_failures_total",
	Help: "Reconciles of managed objects that ended with Synced=False, by kind and condition reason.",
}, []string{"kind", "reason"})

func init() { ctrlmetrics.Registry.MustRegister(MetricSyncFailures) }

func countSyncFailure(mg commonv1alpha1.Managed, reason string) {
	t := reflect.TypeOf(mg)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	kind := "unknown"
	if t != nil && t.Name() != "" {
		kind = t.Name()
	}
	MetricSyncFailures.WithLabelValues(kind, reason).Inc()
}
