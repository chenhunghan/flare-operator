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

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// Crash consistency of a WorkerScript with static assets. The first upload of such a script
// is: the assets upload session and its bucket uploads, the create-pending record, the script
// upload (redeeming the session's completion token), RecordCreated.

// assetCalls counts the asset sessions and bucket uploads of script in j.
func assetCalls(cx *crashCtx, j []fake.JournalEntry, script string) (sessions, buckets int) {
	return testenv.CountPath(j, http.MethodPost, cx.path("/accounts/{account_id}/workers/scripts/")+script+"/assets-upload-session"),
		testenv.CountPath(j, http.MethodPost, cx.path("/accounts/{account_id}/workers/assets/upload"))
}

// workerAssetsCrashCase is WorkerScript (tagged) with static assets from a labelled ConfigMap:
// after the crash between the script upload and RecordCreated, the restarted manager adopts
// the script without an asset call or another upload (the create-pending record carries the
// assets hash).
func workerAssetsCrashCase() crashCase {
	c := workerCrashCase(true, false)
	c.name = "WorkerScript/assets"
	object := c.object
	c.object = func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: cx.ns,
			Labels: map[string]string{sharedv1alpha1.LabelArtifact: "true"}},
			Data: map[string]string{"index.html": "<h1>crash</h1>", "app.css": "a{}"}}
		if err := cx.e.Client.Create(testenv.Context(t, 10*time.Second), cm); err != nil {
			t.Fatal(err)
		}
		ws := object(t, cx).(*workersv1alpha1.WorkerScript)
		ws.Spec.ForProvider.Assets = &workersv1alpha1.WorkerAssets{Source: sharedv1alpha1.ArtifactSource{
			ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{ConfigMaps: []sharedv1alpha1.ConfigMapArtifact{{Name: "site"}}}}}
		ws.Spec.ForProvider.Bindings = []workersv1alpha1.WorkerBinding{{Name: "ASSETS", Type: "assets"}}
		return ws
	}
	after := c.afterRestart
	c.afterRestart = func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, since []fake.JournalEntry) {
		after(t, cx, obj, since)
		if s, b := assetCalls(cx, since, cx.name); s != 0 || b != 0 {
			t.Errorf("after the restart: %d asset sessions and %d bucket uploads, want none:\n%s", s, b, testenv.Summary(since))
		}
		ws := obj.(*workersv1alpha1.WorkerScript)
		if ws.Status.AssetsHash == "" {
			t.Error("status.assetsHash is empty after the adoption")
		}
		if m, _, ok := cx.e.Fake.WorkerAssets(cx.acct.AccountID, cx.name); !ok || len(m) != 2 {
			t.Errorf("assets of the adopted script: %v %v", m, ok)
		}
	}
	return c
}

// TestCrashAfterAssetsBeforeScriptUpload: the manager dies after the assets were uploaded and
// before the script upload (at the create-pending record, which comes between them). The
// restarted manager opens a new session, which finds every file uploaded (no bucket), and
// uploads the script once.
func TestCrashAfterAssetsBeforeScriptUpload(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	c := workerAssetsCrashCase()
	var cc *crashClient
	m1 := e.StartManager(t, testenv.ManagerOptions{Tagger: c.tagger, Namespaces: []string{ns, "kube-system"},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, d controller.Deps) error {
			cc = newCrashClientAt(mgr.GetClient(), "obj", reconcile.AnnotationCreatePending)
			return c.setup(cc)(mgr, d)
		}}})
	acct := e.CreateAccount(t, ns, "acct", testenv.AccountOptions{})
	acct.CloudflareAccount = e.WaitAccountCondition(t, ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	cx := &crashCtx{e: e, ns: ns, acct: acct, cf: apiClient(t, e, acct), name: "flare-spike-" + testenv.RandomHex(4), vars: map[string]string{}}
	o := c.object(t, cx)
	o.SetNamespace(ns)
	o.SetName("obj")
	if err := e.Client.Create(testenv.Context(t, 10*time.Second), o); err != nil {
		t.Fatalf("create: %v", err)
	}
	waitCrash(t, cc, 30*time.Second, func() string {
		_ = e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(o), o)
		return condString(o) + "\n" + testenv.Summary(accountJournal(t, e, acct.AccountID))
	})
	m1.Stop(t)

	j := accountJournal(t, e, acct.AccountID)
	if s, b := assetCalls(cx, j, cx.name); s != 1 || b != 1 || c.creates(cx, j) != 0 {
		t.Fatalf("before the crash: %d sessions, %d buckets, %d script uploads; want 1, 1, 0:\n%s", s, b, c.creates(cx, j), testenv.Summary(j))
	}
	restart := len(j)
	e.StartManager(t, testenv.ManagerOptions{Tagger: c.tagger, Namespaces: []string{ns, "kube-system"},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, d controller.Deps) error {
			return c.setup(mgr.GetClient())(mgr, d)
		}}})
	waitReadySynced(t, e, o, 60*time.Second)
	since := accountJournal(t, e, acct.AccountID)[restart:]
	if s, b := assetCalls(cx, since, cx.name); s != 1 || b != 0 || c.creates(cx, since) != 1 {
		t.Errorf("after the restart: %d sessions, %d buckets, %d script uploads; want 1, 0, 1:\n%s", s, b, c.creates(cx, since), testenv.Summary(since))
	}
	if m, _, ok := e.Fake.WorkerAssets(acct.AccountID, cx.name); !ok || len(m) != 2 {
		t.Errorf("assets of the script: %v %v", m, ok)
	}
	if ws := o.(*workersv1alpha1.WorkerScript); ws.Status.AssetsHash == "" || !strings.HasPrefix(ws.Status.Artifacts.Assets.Digest, "sha256:") {
		t.Errorf("status %+v", ws.Status)
	}
}
