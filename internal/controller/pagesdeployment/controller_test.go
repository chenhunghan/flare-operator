package pagesdeployment_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/controller/pagesdeployment"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

var siteFiles = map[string]string{
	"index.html": "<h1>hello</h1>",
	"about.html": "<p>about</p>",
	"_headers":   "/*\n  X-Frame-Options: DENY\n",
	"_redirects": "/old /about 301\n",
}

// A deployment from ConfigMaps: every asset uploaded once, the routing files sent as parts, the
// deploy stage followed to success; then an unchanged object makes no writes.
func TestDeployFromConfigMaps(t *testing.T) {
	h := start(t)
	project := h.project("site")
	h.siteConfigMap("site-root", siteFiles, map[string][]byte{"logo.png": {0x89, 'P', 'N', 'G'}})
	h.siteConfigMap("site-css", map[string]string{"app.css": "body{}"}, nil)
	src := cmSource("site-root", "site-css")
	src.ConfigMapRef.ConfigMaps[1].Path = "css"
	m := h.mark()
	h.newDeployment("prod", "site", "", src, nil)
	pd := h.waitDeployment("prod", deployed)
	j := h.since(m)
	if n := h.deploys(j, project); n != 1 {
		t.Fatalf("deployments %d, want 1:\n%s", n, testenv.Summary(j))
	}
	for _, p := range []string{"/pages/assets/check-missing", "/pages/assets/upload", "/pages/assets/upsert-hashes"} {
		if n := testenv.CountPath(j, http.MethodPost, p); n != 1 {
			t.Errorf("%s called %d times, want 1:\n%s", p, n, testenv.Summary(j))
		}
	}
	a := pd.Status.AtProvider
	if pd.Status.ID == "" || a.ID != pd.Status.ID || a.ProjectName != project || a.Environment != "production" || a.Branch != "main" ||
		!a.Production || !strings.HasPrefix(a.URL, "https://"+a.ShortID+"."+project+".pages.dev") || len(a.CommitHash) != 40 {
		t.Fatalf("atProvider %+v", a)
	}
	if st := pd.Status.Artifact; st == nil || st.Files != 6 || !strings.HasPrefix(st.Digest, "sha256:") || pd.Status.DeployedHash == "" {
		t.Fatalf("artifact status %+v, deployedHash %q", st, pd.Status.DeployedHash)
	}
	if !reconcile.HasOwnershipProof(pd, pd.Status.ID) {
		t.Fatalf("no ownership record: %v", pd.Annotations)
	}
	manifest, files, ok := h.e.Fake.PagesDeploymentFiles(h.acct.AccountID, project, pd.Status.ID)
	if !ok {
		t.Fatal("deployment not in flarefake")
	}
	want := map[string]string{"/index.html": "<h1>hello</h1>", "/about.html": "<p>about</p>", "/logo.png": "\x89PNG", "/css/app.css": "body{}"}
	if len(manifest) != len(want) {
		t.Fatalf("manifest %v, want the keys of %v", manifest, want)
	}
	for p, content := range want {
		b, ct, ok := h.e.Fake.PagesAsset(h.acct.AccountID, project, manifest[p])
		if !ok || string(b) != content || strings.Contains(ct, ";") {
			t.Errorf("asset %s: %q (%q, %v), want %q", p, b, ct, ok, content)
		}
	}
	if string(files["_headers"]) != siteFiles["_headers"] || string(files["_redirects"]) != siteFiles["_redirects"] {
		t.Errorf("routing files %v", files)
	}
	h.assertNoWrites("prod")
}

// Only a change of the content or the branch deploys again; a commit message edit does not.
// Unchanged files are not uploaded twice.
func TestRedeployOnContentOrBranchChange(t *testing.T) {
	h := start(t)
	project := h.project("re")
	cm := h.siteConfigMap("re-root", map[string]string{"index.html": "v1", "static.txt": "same"}, nil)
	h.newDeployment("dep", "re", "", cmSource("re-root"), nil)
	pd := h.waitDeployment("dep", deployed)
	first := pd.Status.ID

	m := h.mark()
	h.update(pd, func() { pd.Spec.ForProvider.CommitMessage = "just a message" })
	h.settle("dep")
	if n := h.deploys(h.since(m), project); n != 0 {
		t.Fatalf("a commit message edit deployed %d times", n)
	}

	m = h.mark()
	h.update(cm, func() { cm.Data["index.html"] = "v2" })
	pd = h.waitDeployment("dep", func(pd *pagesv1alpha1.PagesDeployment) bool { return deployed(pd) && pd.Status.ID != first })
	if n := h.deploys(h.since(m), project); n != 1 {
		t.Fatalf("content change: %d deployments, want 1", n)
	}
	manifest, _, _ := h.e.Fake.PagesDeploymentFiles(h.acct.AccountID, project, pd.Status.ID)
	if b, _, _ := h.e.Fake.PagesAsset(h.acct.AccountID, project, manifest["/index.html"]); string(b) != "v2" {
		t.Fatalf("new index.html %q", b)
	}
	if !h.deploymentExists(project, first) {
		t.Fatal("the earlier deployment is gone from the project's history")
	}
	second := pd.Status.ID

	m = h.mark()
	h.update(pd, func() { pd.Spec.ForProvider.Branch = "Feature/One" })
	pd = h.waitDeployment("dep", func(pd *pagesv1alpha1.PagesDeployment) bool { return deployed(pd) && pd.Status.ID != second })
	if n := h.deploys(h.since(m), project); n != 1 {
		t.Fatalf("branch change: %d deployments, want 1", n)
	}
	a := pd.Status.AtProvider
	if a.Environment != "preview" || a.Branch != "Feature/One" || a.Production || len(a.Aliases) != 1 ||
		a.Aliases[0] != "https://feature-one."+project+".pages.dev" {
		t.Fatalf("preview deployment %+v", a)
	}
	h.assertNoWrites("dep")
}

func targz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

// A deployment from an HTTPS archive pinned by SHA-256.
func TestDeployFromURL(t *testing.T) {
	archive := targz(t, map[string]string{"dist/index.html": "from a tarball", "dist/_routes.json": `{"version":1,"include":["/*"],"exclude":[]}`, "src/x.ts": "no"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	loader, err := artifact.NewLoader(artifact.Options{RootCAs: pool, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	h := startWith(t, testenv.ManagerOptions{Artifacts: loader})
	project := h.project("url")
	sum := sha256.Sum256(archive)
	h.newDeployment("dep", "url", "", &sharedv1alpha1.ArtifactSource{URL: &sharedv1alpha1.URLArtifactSource{
		URL: srv.URL + "/site.tgz", SHA256: hex.EncodeToString(sum[:]), Path: "dist"}}, nil)
	pd := h.waitDeployment("dep", deployed)
	manifest, files, _ := h.e.Fake.PagesDeploymentFiles(h.acct.AccountID, project, pd.Status.ID)
	if len(manifest) != 1 || manifest["/index.html"] == "" || !strings.Contains(string(files["_routes.json"]), `"version":1`) {
		t.Fatalf("manifest %v files %v", manifest, files)
	}
}

// Nothing is deployed while the project is missing or the ConfigMap is not opted in.
func TestDeploymentDependencies(t *testing.T) {
	h := start(t)
	h.create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled"}, Data: map[string]string{"index.html": "x"}})
	m := h.mark()
	pd := h.newDeployment("dep", "later", "", cmSource("unlabelled"), nil)
	h.waitDeployment("dep", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return hasCond(pd.Status.Conditions, pd.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonDependency) &&
			strings.Contains(condOf(pd.Status.Conditions, "Synced"), "PagesProject later not found")
	})
	project := h.project("later")
	h.waitDeployment("dep", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return strings.Contains(condOf(pd.Status.Conditions, "Synced"), sharedv1alpha1.LabelArtifact)
	})
	if n := h.deploys(h.since(m), project); n != 0 {
		t.Fatalf("deployed %d times with an unusable ConfigMap", n)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled"}}
	h.update(cm, func() { cm.Labels = map[string]string{sharedv1alpha1.LabelArtifact: "true"} })
	h.waitDeployment("dep", deployed)
	_ = pd
}

// A failed deploy stage is reported and not retried until the content changes.
func TestDeploymentFailed(t *testing.T) {
	h := start(t)
	project := h.project("fail")
	cm := h.siteConfigMap("fail-root", map[string]string{"index.html": "v1"}, nil)
	h.e.Fake.FailPagesDeployments(1)
	h.newDeployment("dep", "fail", "", cmSource("fail-root"), nil)
	h.waitDeployment("dep", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return hasCond(pd.Status.Conditions, pd.Generation, "Ready", metav1.ConditionFalse, pagesdeployment.ReasonDeploymentFailed)
	})
	m := h.mark()
	h.settle("dep")
	if n := h.deploys(h.since(m), project); n != 0 {
		t.Fatalf("a failed deployment was retried %d times without a change", n)
	}
	h.update(cm, func() { cm.Data["index.html"] = "v2" })
	h.waitDeployment("dep", deployed)
}

// Deletion order: the project waits for its PagesDeployments; a preview deployment and a
// superseded production deployment are deleted in Cloudflare; the live production deployment
// is kept (Cloudflare refuses to delete it) and its object still goes away.
func TestDeleteOrder(t *testing.T) {
	h := start(t)
	project := h.project("del")
	h.siteConfigMap("del-a", map[string]string{"index.html": "a"}, nil)
	h.siteConfigMap("del-b", map[string]string{"index.html": "b"}, nil)
	h.newDeployment("old", "del", "", cmSource("del-a"), nil)
	h.waitDeployment("old", deployed)
	h.newDeployment("live", "del", "main", cmSource("del-b"), nil)
	live := h.waitDeployment("live", deployed)
	h.newDeployment("pre", "del", "preview", cmSource("del-a"), nil)
	pre := h.waitDeployment("pre", deployed)
	h.settle("old") // status is refreshed on the next reconcile
	old := h.waitDeployment("old", func(pd *pagesv1alpha1.PagesDeployment) bool { return !pd.Status.AtProvider.Production })
	if !live.Status.AtProvider.Production {
		t.Fatalf("the newer production deployment is not live: %+v", live.Status.AtProvider)
	}

	pp := &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: "del", Namespace: h.ns}}
	h.delete(pp)
	wait(h, pp, func(pp *pagesv1alpha1.PagesProject) bool {
		c := condOf(pp.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "PagesDeployment live") &&
			strings.Contains(c, "PagesDeployment old") && strings.Contains(c, "PagesDeployment pre")
	})

	h.delete(pre)
	h.waitGone(pre)
	if h.deploymentExists(project, pre.Status.ID) {
		t.Fatal("the preview deployment (with its branch alias) is still in Cloudflare")
	}
	h.delete(old)
	h.waitGone(old)
	if h.deploymentExists(project, old.Status.ID) {
		t.Fatal("the superseded production deployment is still in Cloudflare")
	}
	m := h.mark()
	h.delete(live)
	h.waitGone(live)
	h.waitEvent("live", reconcile.EventReasonExternalResourceKept, live.Status.ID, "live production deployment")
	h.waitGone(pp) // the project goes, and its deployments with it
	j := h.since(m)
	if n := testenv.Count(j, http.MethodDelete, "/deployments/"+live.Status.ID); n != 0 {
		t.Fatalf("the live production deployment was deleted (%d DELETEs):\n%s", n, testenv.Summary(j))
	}
	if n := testenv.CountPath(j, http.MethodDelete, "/accounts/"+h.acct.AccountID+"/pages/projects/"+project); n != 1 {
		t.Fatalf("project DELETEs %d, want 1", n)
	}
	if _, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, project, "production"); ok {
		t.Fatal("the project is still in Cloudflare")
	}
}

// Observe-only: the live production deployment is reported; nothing is written, and deleting
// the object leaves the deployment.
func TestDeploymentObserveOnly(t *testing.T) {
	h := start(t)
	project := h.project("obs")
	h.siteConfigMap("obs-root", map[string]string{"index.html": "x"}, nil)
	h.newDeployment("made", "obs", "", cmSource("obs-root"), nil)
	made := h.waitDeployment("made", deployed)
	m := h.mark()
	h.newDeployment("watch", "obs", "", nil, func(pd *pagesv1alpha1.PagesDeployment) {
		pd.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	})
	obs := h.waitDeployment("watch", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return ready(pd) && hasCond(pd.Status.Conditions, pd.Generation, "Synced", metav1.ConditionTrue, commonv1alpha1.ReasonObserveOnly)
	})
	if obs.Status.ID != made.Status.ID || !obs.Status.AtProvider.Production {
		t.Fatalf("observed %+v, want the live deployment %s", obs.Status, made.Status.ID)
	}
	h.delete(obs)
	h.waitGone(obs)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("an observe-only object wrote:\n%s", testenv.Summary(w))
	}
	if !h.deploymentExists(project, made.Status.ID) {
		t.Fatal("deleting the observe-only object deleted the deployment")
	}
}
