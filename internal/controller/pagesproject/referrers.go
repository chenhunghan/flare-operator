package pagesproject

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/generic"
)

// Cloudflare does not check a Pages project's bindings when the bound KV namespace, D1
// database, queue or Worker is deleted (UNVERIFIED for Pages; recording 0091 shows the same for
// Workers bindings). PagesProjects are registered as referrers of those kinds, so their
// finalizers wait (DependencyNotReady) until no PagesProject binds them.
func init() {
	pick := func(get func(*pagesv1alpha1.PagesDeploymentConfig) []*commonv1alpha1.LocalRef) func(client.Object) []string {
		return func(o client.Object) []string {
			pp, ok := o.(*pagesv1alpha1.PagesProject)
			if !ok {
				return nil
			}
			var out []string
			seen := map[string]bool{}
			for _, c := range configs(pp) {
				for _, ref := range get(c) {
					if ref != nil && ref.Name != "" && !seen[ref.Name] {
						seen[ref.Name] = true
						out = append(out, ref.Name)
					}
				}
			}
			return out
		}
	}
	refs := map[schema.GroupKind]func(client.Object) []string{
		{Group: kvv1alpha1.GroupVersion.Group, Kind: "KVNamespace"}: pick(func(c *pagesv1alpha1.PagesDeploymentConfig) (out []*commonv1alpha1.LocalRef) {
			for _, b := range c.KVNamespaces {
				out = append(out, b.KVNamespaceRef)
			}
			return out
		}),
		{Group: d1v1alpha1.GroupVersion.Group, Kind: "D1Database"}: pick(func(c *pagesv1alpha1.PagesDeploymentConfig) (out []*commonv1alpha1.LocalRef) {
			for _, b := range c.D1Databases {
				out = append(out, b.D1DatabaseRef)
			}
			return out
		}),
		{Group: queuesv1alpha1.GroupVersion.Group, Kind: "Queue"}: pick(func(c *pagesv1alpha1.PagesDeploymentConfig) (out []*commonv1alpha1.LocalRef) {
			for _, b := range c.QueueProducers {
				out = append(out, b.QueueRef)
			}
			return out
		}),
		{Group: workersv1alpha1.GroupVersion.Group, Kind: "WorkerScript"}: pick(func(c *pagesv1alpha1.PagesDeploymentConfig) (out []*commonv1alpha1.LocalRef) {
			for _, b := range c.Services {
				out = append(out, b.ServiceRef)
			}
			return out
		}),
	}
	for gk, referenced := range refs {
		generic.RegisterReferrer(gk, generic.Referrer{
			Kind:       "PagesProject",
			Object:     &pagesv1alpha1.PagesProject{},
			NewList:    func() client.ObjectList { return &pagesv1alpha1.PagesProjectList{} },
			Referenced: referenced,
		})
	}
}
