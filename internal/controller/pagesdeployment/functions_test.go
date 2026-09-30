package pagesdeployment_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	"flare.dev/operator/internal/controller/pagesdeployment"
	"flare.dev/operator/internal/testenv"
)

// An artifact that needs a build step wrangler would run is refused, not deployed without it:
// a _worker.js directory (bundling) and a top-level functions directory (Pages Functions,
// compiled into the Worker). With a _worker.js file the functions directory is ignored, as
// wrangler ignores it.
func TestArtifactsNeedingABuildAreRefused(t *testing.T) {
	h := start(t)
	project := h.project("site")
	h.siteConfigMap("site-files", map[string]string{"index.html": "hi"}, nil)
	h.siteConfigMap("fn-files", map[string]string{"api.js": "export function onRequest() {}"}, nil)
	h.siteConfigMap("worker-file", map[string]string{"_worker.js": "export default {}"}, nil)
	src := func(extra ...sharedv1alpha1.ConfigMapArtifact) *sharedv1alpha1.ArtifactSource {
		s := cmSource("site-files")
		s.ConfigMapRef.ConfigMaps = append(s.ConfigMapRef.ConfigMaps, extra...)
		return s
	}
	m := h.mark()
	h.newDeployment("functions", "site", "fn", src(sharedv1alpha1.ConfigMapArtifact{Name: "fn-files", Path: "functions"}), nil)
	h.newDeployment("worker-dir", "site", "wd", src(sharedv1alpha1.ConfigMapArtifact{Name: "fn-files", Path: "_worker.js"}), nil)
	for name, want := range map[string]string{"functions": "functions directory", "worker-dir": "_worker.js directory"} {
		pd := h.waitDeployment(name, func(pd *pagesv1alpha1.PagesDeployment) bool {
			return hasCond(pd.Status.Conditions, pd.Generation, "Ready", metav1.ConditionFalse, pagesdeployment.ReasonInvalidArtifact) &&
				hasCond(pd.Status.Conditions, pd.Generation, "Synced", metav1.ConditionFalse, pagesdeployment.ReasonInvalidArtifact)
		})
		if c := meta.FindStatusCondition(pd.Status.Conditions, "Ready"); !strings.Contains(c.Message, want) {
			t.Errorf("%s: Ready message %q does not name the %s", name, c.Message, want)
		}
	}
	if n := h.deploys(h.since(m), project); n != 0 {
		t.Fatalf("refused artifacts made %d deployments:\n%s", n, testenv.Summary(h.since(m)))
	}

	h.newDeployment("with-worker", "site", "ww", src(sharedv1alpha1.ConfigMapArtifact{Name: "fn-files", Path: "functions"},
		sharedv1alpha1.ConfigMapArtifact{Name: "worker-file"}), nil)
	h.waitDeployment("with-worker", deployed)
	if n := h.deploys(h.since(m), project); n != 1 {
		t.Fatalf("made %d deployments, want 1 (with-worker's):\n%s", n, testenv.Summary(h.since(m)))
	}
}
