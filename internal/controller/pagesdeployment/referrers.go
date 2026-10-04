package pagesdeployment

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesproject"
	"github.com/chenhunghan/flare-operator/internal/generic"
)

// Deleting a Pages project deletes its deployments with it. PagesDeployments are registered as
// referrers of PagesProject, so a PagesProject whose deletion would delete the project waits
// (DependencyNotReady) until the namespace's PagesDeployments of it are gone: each deletes
// (or, for the live production deployment, keeps) its own deployment first, and none is left
// pointing at a project that no longer exists.
func init() {
	generic.RegisterReferrer(pagesproject.PagesProjectKind, generic.Referrer{
		Kind:    "PagesDeployment",
		Object:  &pagesv1alpha1.PagesDeployment{},
		NewList: func() client.ObjectList { return &pagesv1alpha1.PagesDeploymentList{} },
		Referenced: func(o client.Object) []string {
			pd, ok := o.(*pagesv1alpha1.PagesDeployment)
			if !ok || pd.Spec.ForProvider.ProjectRef.Name == "" {
				return nil
			}
			return []string{pd.Spec.ForProvider.ProjectRef.Name}
		},
	})
}
