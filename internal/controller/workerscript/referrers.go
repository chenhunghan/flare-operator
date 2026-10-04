package workerscript

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	d1v1alpha1 "github.com/chenhunghan/flare-operator/api/d1/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "github.com/chenhunghan/flare-operator/api/workersvpc/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/r2bind"
	"github.com/chenhunghan/flare-operator/internal/generic"
)

// Group kinds a WorkerScript binding can reference.
var (
	KVNamespaceKind  = schema.GroupKind{Group: kvv1alpha1.GroupVersion.Group, Kind: "KVNamespace"}
	QueueKind        = schema.GroupKind{Group: queuesv1alpha1.GroupVersion.Group, Kind: "Queue"}
	D1DatabaseKind   = schema.GroupKind{Group: d1v1alpha1.GroupVersion.Group, Kind: "D1Database"}
	VPCServiceKind   = schema.GroupKind{Group: workersvpcv1alpha1.GroupVersion.Group, Kind: "VPCService"}
	WorkerScriptKind = schema.GroupKind{Group: workersv1alpha1.GroupVersion.Group, Kind: "WorkerScript"}
	R2BucketKind     = r2bind.Kind
)

// refNames returns the names that ws's bindings reference through pick.
func refNames(o client.Object, pick func(b workersv1alpha1.WorkerBinding) string) []string {
	ws, ok := o.(*workersv1alpha1.WorkerScript)
	if !ok {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, b := range ws.Bindings() {
		if n := pick(b); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// Pickers of each binding reference.
var (
	pickKV = func(b workersv1alpha1.WorkerBinding) string {
		if b.KVNamespaceRef == nil {
			return ""
		}
		return b.KVNamespaceRef.Name
	}
	pickQueue = func(b workersv1alpha1.WorkerBinding) string {
		if b.QueueRef == nil {
			return ""
		}
		return b.QueueRef.Name
	}
	pickD1 = func(b workersv1alpha1.WorkerBinding) string {
		if b.D1DatabaseRef == nil {
			return ""
		}
		return b.D1DatabaseRef.Name
	}
	pickVPC = func(b workersv1alpha1.WorkerBinding) string {
		if b.VPCServiceRef == nil {
			return ""
		}
		return b.VPCServiceRef.Name
	}
	pickService = func(b workersv1alpha1.WorkerBinding) string {
		if b.ServiceRef == nil {
			return ""
		}
		return b.ServiceRef.Name
	}
	pickR2 = func(b workersv1alpha1.WorkerBinding) string {
		if b.R2BucketRef == nil {
			return ""
		}
		return b.R2BucketRef.Name
	}
)

// Cloudflare deletes a KV namespace, queue, D1 database, VPC service, R2 bucket or Worker while
// a Worker still binds it (0091 shows a Worker keeping its vpc_service binding to a deleted
// service; the other kinds are UNVERIFIED). WorkerScripts are registered as referrers of those
// kinds, so their finalizers wait (DependencyNotReady) until no WorkerScript binds them.
func init() {
	for gk, pick := range map[schema.GroupKind]func(workersv1alpha1.WorkerBinding) string{
		KVNamespaceKind: pickKV, QueueKind: pickQueue, D1DatabaseKind: pickD1, VPCServiceKind: pickVPC, WorkerScriptKind: pickService,
		R2BucketKind: pickR2,
	} {
		pick := pick
		generic.RegisterReferrer(gk, generic.Referrer{
			Kind:       "WorkerScript",
			Object:     &workersv1alpha1.WorkerScript{},
			NewList:    func() client.ObjectList { return &workersv1alpha1.WorkerScriptList{} },
			Referenced: func(o client.Object) []string { return refNames(o, pick) },
		})
	}
}
