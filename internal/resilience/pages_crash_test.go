package resilience_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesdeployment"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesproject"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

func pagesProjectReconciler(c client.Client, mgr ctrl.Manager, d controller.Deps) *pagesproject.Reconciler {
	return &pagesproject.Reconciler{Client: c, Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
		Recorder: mgr.GetEventRecorder(pagesproject.Name), APIReader: mgr.GetAPIReader(), ResyncInterval: time.Hour,
		DependencyRetry: 500 * time.Millisecond}
}

// pagesProjectCrashCase: the crash comes right after POST …/pages/projects.
func pagesProjectCrashCase(tagging bool) crashCase {
	return crashCase{
		name:   "PagesProject/" + tagSuffix(tagging),
		tagger: taggerFor(tagging),
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				return pagesProjectReconciler(c, mgr, d).SetupWithManager(mgr)
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			return &pagesv1alpha1.PagesProject{Spec: pagesv1alpha1.PagesProjectSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider: &pagesv1alpha1.PagesProjectParameters{Name: cx.name, ProductionBranch: "main",
					DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Production: &pagesv1alpha1.PagesDeploymentConfig{
						EnvVars: []pagesv1alpha1.PagesEnvVar{{Name: "A", Type: "plain_text", Value: ptrTo("1")}}}}},
			}}
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			return cx.path("/accounts/{account_id}/pages/projects"), "name", "name", cx.name
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			return testenv.CountPath(j, http.MethodPost, cx.path("/accounts/{account_id}/pages/projects"))
		},
		afterRestart: func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, since []fake.JournalEntry) {
			// The interrupted create is not followed by an update: the create-pending record
			// carried what it applied.
			for _, e := range testenv.Writes(since) {
				if strings.HasPrefix(e.Path, cx.path("/accounts/{account_id}/pages/projects/")) {
					t.Errorf("project write after the restart: %s %s", e.Method, e.Path)
				}
			}
			pp := obj.(*pagesv1alpha1.PagesProject)
			if pp.Status.SettingsHash == "" || pp.Status.AtProvider.ID == "" {
				t.Errorf("status after adoption: %+v", pp.Status)
			}
			if !tagging {
				return
			}
			o, _, err := reconcile.ResourceTagger{}.Owner(testenv.Context(t, 10*time.Second), cx.cf, cx.acct.AccountID,
				reconcile.TagTarget{Type: pagesproject.TagResourceType, ID: pp.Status.AtProvider.ID})
			if want := reconcile.OwnerValue("testenv", cx.ns, "obj"); err != nil || o != want {
				t.Errorf("owner tag %q (err %v), want %q", o, err, want)
			}
		},
	}
}

func ptrTo(s string) *string { return &s }

// pagesDeploymentCrashCase: the crash comes right after POST …/deployments (a preview
// deployment, which a finalizer may delete). The PagesProject "proj" is reconciled by an
// ordinary client.
func pagesDeploymentCrashCase() crashCase {
	return crashCase{
		name: "PagesDeployment",
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				if err := pagesProjectReconciler(mgr.GetClient(), mgr, d).SetupWithManager(mgr); err != nil {
					return err
				}
				return (&pagesdeployment.Reconciler{Client: c, Accounts: d.Accounts, Artifacts: d.Artifacts,
					Recorder: mgr.GetEventRecorder(pagesdeployment.Name), APIReader: mgr.GetAPIReader(), ResyncInterval: time.Hour,
					DependencyRetry: 500 * time.Millisecond, StagePollInterval: 200 * time.Millisecond}).SetupWithManager(mgr)
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			ctx := testenv.Context(t, 10*time.Second)
			if err := cx.e.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cx.ns, Name: "site",
				Labels: map[string]string{sharedv1alpha1.LabelArtifact: "true"}}, Data: map[string]string{"index.html": "<h1>crash</h1>"}}); err != nil {
				t.Fatal(err)
			}
			pp := &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Namespace: cx.ns, Name: "proj"}, Spec: pagesv1alpha1.PagesProjectSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider:  &pagesv1alpha1.PagesProjectParameters{Name: cx.name, ProductionBranch: "main"}}}
			if err := cx.e.Client.Create(ctx, pp); err != nil {
				t.Fatal(err)
			}
			waitReadySynced(t, cx.e, pp, 30*time.Second)
			return &pagesv1alpha1.PagesDeployment{Spec: pagesv1alpha1.PagesDeploymentSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider: pagesv1alpha1.PagesDeploymentParameters{ProjectRef: commonv1alpha1.LocalRef{Name: "proj"}, Branch: "preview",
					Source: &sharedv1alpha1.ArtifactSource{ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{
						ConfigMaps: []sharedv1alpha1.ConfigMapArtifact{{Name: "site"}}}}},
			}}
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			return cx.path("/accounts/{account_id}/pages/projects/" + cx.name + "/deployments"), "project_name", "id", cx.name
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			return testenv.CountPath(j, http.MethodPost, cx.path("/accounts/{account_id}/pages/projects/"+cx.name+"/deployments"))
		},
		afterRestart: func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, since []fake.JournalEntry) {
			pd := obj.(*pagesv1alpha1.PagesDeployment)
			if pd.Status.DeployedHash == "" || pd.Status.Artifact == nil || pd.Status.AtProvider.ProjectName != cx.name {
				t.Errorf("status after adoption: %+v", pd.Status)
			}
			if n := testenv.Count(since, http.MethodGet, "/upload-token"); n != 0 {
				t.Errorf("the restarted manager prepared another upload (%d upload tokens)", n)
			}
		},
	}
}
