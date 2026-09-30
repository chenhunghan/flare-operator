package pagesproject_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

func (h *harness) kv(name string, policy commonv1alpha1.DeletionPolicy) *kvv1alpha1.KVNamespace {
	o := &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kvv1alpha1.KVNamespaceSpec{ResourceSpec: accountRef(), ForProvider: kvv1alpha1.KVNamespaceParameters{Title: str(h.ns + "-" + name)}}}
	o.Spec.DeletionPolicy = policy
	h.create(o)
	return o
}

func (h *harness) queue(name string) *queuesv1alpha1.Queue {
	o := &queuesv1alpha1.Queue{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: queuesv1alpha1.QueueSpec{ResourceSpec: accountRef(), ForProvider: queuesv1alpha1.QueueParameters{QueueName: str(h.ns + "-" + name)}}}
	h.create(o)
	return o
}

func (h *harness) d1(name string) *d1v1alpha1.D1Database {
	o := &d1v1alpha1.D1Database{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: d1v1alpha1.D1DatabaseSpec{ResourceSpec: accountRef(), ForProvider: d1v1alpha1.D1DatabaseParameters{Name: str(h.ns + "-" + name)}}}
	h.create(o)
	return o
}

func projectReady(pp *pagesv1alpha1.PagesProject) bool { return ready(pp) }

// A project with every supported binding (references resolved to IDs), plain and secret
// environment variables; then an unchanged object makes no writes.
func TestProjectWithBindings(t *testing.T) {
	h := start(t)
	kv := wait(h, h.kv("cache", ""), ready[*kvv1alpha1.KVNamespace])
	q := wait(h, h.queue("jobs"), ready[*queuesv1alpha1.Queue])
	db := wait(h, h.d1("db"), ready[*d1v1alpha1.D1Database])
	h.secret("api", map[string]string{"key": "s3cret"}, true)
	name := h.projectName("site")
	m := h.mark()
	h.newProject("site", &pagesv1alpha1.PagesProjectParameters{
		Name: name, ProductionBranch: "main",
		BuildConfig: &pagesv1alpha1.PagesBuildConfig{BuildCommand: str("npm run build"), DestinationDir: str("dist")},
		DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Production: &pagesv1alpha1.PagesDeploymentConfig{
			CompatibilityDate: "2026-09-01", CompatibilityFlags: []string{"nodejs_compat"}, FailOpen: ptr.To(false),
			EnvVars: []pagesv1alpha1.PagesEnvVar{
				{Name: "GREETING", Type: "plain_text", Value: str("hello")},
				{Name: "API_KEY", Type: "secret_text", SecretKeyRef: &pagesv1alpha1.PagesSecretKeyRef{Name: "api", Key: "key"}},
			},
			KVNamespaces:   []pagesv1alpha1.PagesKVBinding{{Name: "CACHE", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "cache"}}},
			D1Databases:    []pagesv1alpha1.PagesD1Binding{{Name: "DB", D1DatabaseRef: &commonv1alpha1.LocalRef{Name: "db"}}},
			QueueProducers: []pagesv1alpha1.PagesQueueBinding{{Name: "JOBS", QueueRef: &commonv1alpha1.LocalRef{Name: "jobs"}}},
			R2Buckets:      []pagesv1alpha1.PagesR2Binding{{Name: "FILES", BucketName: "flare-spike-files", Jurisdiction: str("eu")}},
			Services:       []pagesv1alpha1.PagesServiceBinding{{Name: "API", Service: str("api-worker"), Entrypoint: str("Api")}},
		}},
	}, nil)
	pp := h.waitProject("site", projectReady)
	j := h.since(m)
	if n := testenv.Count(j, http.MethodPost, "/pages/projects"); n != 1 {
		t.Fatalf("creates %d, want 1:\n%s", n, testenv.Summary(j))
	}
	if n := testenv.Count(j, http.MethodPatch, "/pages/projects/"); n != 0 {
		t.Fatalf("PATCHes after the create %d, want 0:\n%s", n, testenv.Summary(j))
	}
	a := pp.Status.AtProvider
	if pp.Status.ID != name || a.ID == "" || a.Subdomain != name+".pages.dev" || a.URL != "https://"+name+".pages.dev" ||
		a.ProductionBranch != "main" || strings.Join(a.ProductionEnvVars, ",") != "API_KEY,GREETING" {
		t.Fatalf("status %+v", pp.Status)
	}
	if pp.Status.SettingsHash == "" || pp.Status.WriteOnlyHash == "" || !reconcile.HasOwnershipProof(pp, name) {
		t.Fatalf("hashes %q %q, annotations %v", pp.Status.SettingsHash, pp.Status.WriteOnlyHash, pp.Annotations)
	}
	c := h.config(name, "production")
	want := map[string]string{
		"env_vars":        "map[API_KEY:map[type:secret_text value:s3cret] GREETING:map[type:plain_text value:hello]]",
		"kv_namespaces":   fmt.Sprintf("map[CACHE:map[namespace_id:%s]]", kv.Status.ID),
		"d1_databases":    fmt.Sprintf("map[DB:map[id:%s]]", db.Status.ID),
		"queue_producers": fmt.Sprintf("map[JOBS:map[name:%s]]", *q.Status.AtProvider.QueueName),
		"r2_buckets":      "map[FILES:map[jurisdiction:eu name:flare-spike-files]]",
		"services":        "map[API:map[entrypoint:Api environment:production service:api-worker]]",
	}
	for k, w := range want {
		if got := fmt.Sprint(c[k]); got != w {
			t.Errorf("%s = %s, want %s", k, got, w)
		}
	}
	if c["compatibility_date"] != "2026-09-01" || fmt.Sprint(c["compatibility_flags"]) != "[nodejs_compat]" || c["fail_open"] != false {
		t.Errorf("config scalars %v", c)
	}
	h.assertNoWrites("site", name)
}

// Updates: a changed variable, a removed binding (sent as null), a Secret's new value; each is
// one PATCH. Drift made in Cloudflare is repaired.
func TestProjectUpdate(t *testing.T) {
	h := start(t)
	sec := h.secret("api", map[string]string{"key": "one"}, true)
	name := h.projectName("upd")
	h.newProject("upd", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main",
		DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Preview: &pagesv1alpha1.PagesDeploymentConfig{
			EnvVars: []pagesv1alpha1.PagesEnvVar{{Name: "A", Value: str("1")},
				{Name: "S", Type: "secret_text", SecretKeyRef: &pagesv1alpha1.PagesSecretKeyRef{Name: "api", Key: "key"}}},
			R2Buckets: []pagesv1alpha1.PagesR2Binding{{Name: "OLD", BucketName: "flare-spike-old"}},
		}}}, nil)
	pp := h.waitProject("upd", projectReady)
	hash := pp.Status.WriteOnlyHash

	m := h.mark()
	h.updateProject("upd", func(pp *pagesv1alpha1.PagesProject) {
		pv := pp.Spec.ForProvider.DeploymentConfigs.Preview
		pv.EnvVars[0].Value = str("2")
		pv.R2Buckets = nil
		pp.Spec.ForProvider.ProductionBranch = "release"
	})
	h.waitProject("upd", func(pp *pagesv1alpha1.PagesProject) bool {
		return projectReady(pp) && pp.Status.AtProvider.ProductionBranch == "release"
	})
	if n := testenv.Count(h.since(m), http.MethodPatch, "/pages/projects/"+name); n != 1 {
		t.Fatalf("PATCHes %d, want 1:\n%s", n, testenv.Summary(h.since(m)))
	}
	c := h.config(name, "preview")
	if _, has := c["r2_buckets"]; has || fmt.Sprint(c["env_vars"]) != "map[A:map[type:plain_text value:2] S:map[type:secret_text value:one]]" {
		t.Fatalf("preview config after the update: %v", c)
	}

	// A new Secret value is written (the API never returns it: status.writeOnlyHash).
	m = h.mark()
	sec.StringData = map[string]string{"key": "two"}
	if err := h.e.Client.Update(h.ctx(), sec); err != nil {
		t.Fatal(err)
	}
	h.waitProject("upd", func(pp *pagesv1alpha1.PagesProject) bool {
		return projectReady(pp) && pp.Status.WriteOnlyHash != hash
	})
	if got := fmt.Sprint(h.config(name, "preview")["env_vars"]); !strings.Contains(got, "value:two") {
		t.Fatalf("secret not updated: %s", got)
	}
	if n := testenv.Count(h.since(m), http.MethodPatch, "/pages/projects/"+name); n != 1 {
		t.Fatalf("PATCHes for the Secret change %d, want 1", n)
	}

	// Drift: someone adds a variable and changes the branch in Cloudflare.
	if _, err := h.apiDo(http.MethodPatch, "/pages/projects/"+name, map[string]any{"production_branch": "other",
		"deployment_configs": map[string]any{"preview": map[string]any{"env_vars": map[string]any{"X": map[string]any{"type": "plain_text", "value": "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	m = h.mark()
	h.settle("upd")
	h.waitProject("upd", func(pp *pagesv1alpha1.PagesProject) bool {
		return projectReady(pp) && pp.Status.AtProvider.ProductionBranch == "release"
	})
	if got := fmt.Sprint(h.config(name, "preview")["env_vars"]); strings.Contains(got, "X:") {
		t.Fatalf("drifted variable not removed: %s", got)
	}
	if n := testenv.Count(h.since(m), http.MethodPatch, "/pages/projects/"+name); n != 1 {
		t.Fatalf("PATCHes for the drift %d, want 1", n)
	}
	h.assertNoWrites("upd", name)
}

// A reference to an object that is not Ready yet, or a Secret without the opt-in label: nothing
// is created until it is resolved.
func TestProjectDependencies(t *testing.T) {
	h := start(t)
	h.secret("plain", map[string]string{"key": "v"}, false)
	name := h.projectName("dep")
	h.newProject("dep", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main",
		DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Production: &pagesv1alpha1.PagesDeploymentConfig{
			KVNamespaces: []pagesv1alpha1.PagesKVBinding{{Name: "KV", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "later"}}},
			EnvVars:      []pagesv1alpha1.PagesEnvVar{{Name: "S", Type: "secret_text", SecretKeyRef: &pagesv1alpha1.PagesSecretKeyRef{Name: "plain", Key: "key"}}},
		}}}, nil)
	h.waitProject("dep", func(pp *pagesv1alpha1.PagesProject) bool {
		return synced[*pagesv1alpha1.PagesProject](commonv1alpha1.ReasonDependency)(pp) && strings.Contains(condStr(pp, "Synced"), "not usable")
	})
	if _, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, name, "production"); ok {
		t.Fatal("the project was created with a Secret that is not opted in")
	}
	var s corev1.Secret
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "plain"}, &s); err != nil {
		t.Fatal(err)
	}
	s.Labels = map[string]string{pagesv1alpha1.LabelSecretOptIn: "true"}
	if err := h.e.Client.Update(h.ctx(), &s); err != nil {
		t.Fatal(err)
	}
	h.waitProject("dep", func(pp *pagesv1alpha1.PagesProject) bool {
		return strings.Contains(condStr(pp, "Synced"), "KVNamespace later not found")
	})
	if _, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, name, "production"); ok {
		t.Fatal("the project was created before its references resolved")
	}
	wait(h, h.kv("later", ""), ready[*kvv1alpha1.KVNamespace])
	h.waitProject("dep", projectReady)
}

func condStr(pp *pagesv1alpha1.PagesProject, typ string) string {
	return condOf(pp.Status.Conditions, typ)
}

// A same-named project this object did not create is a NameConflict until the external-id
// annotation pins it; then it is adopted and made to match forProvider.
func TestProjectNameConflictAndAdoption(t *testing.T) {
	h := start(t)
	name := h.projectName("taken")
	if _, err := h.apiDo(http.MethodPost, "/pages/projects", map[string]any{"name": name, "production_branch": "dev"}); err != nil {
		t.Fatal(err)
	}
	m := h.mark()
	h.newProject("taken", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main"}, nil)
	h.waitProject("taken", synced[*pagesv1alpha1.PagesProject](reconcile.ReasonNameConflict))
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("writes on a NameConflict:\n%s", testenv.Summary(w))
	}
	h.updateProject("taken", func(pp *pagesv1alpha1.PagesProject) {
		pp.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: name}
	})
	pp := h.waitProject("taken", func(pp *pagesv1alpha1.PagesProject) bool {
		return projectReady(pp) && pp.Status.AtProvider.ProductionBranch == "main"
	})
	if !reconcile.HasOwnershipProof(pp, name) {
		t.Fatalf("no ownership record after adoption: %v", pp.Annotations)
	}
}

// Observe-only: the project is read, never written, and kept when the object is deleted.
func TestProjectObserveOnly(t *testing.T) {
	h := start(t)
	name := h.projectName("obs")
	if _, err := h.apiDo(http.MethodPost, "/pages/projects", map[string]any{"name": name, "production_branch": "main"}); err != nil {
		t.Fatal(err)
	}
	m := h.mark()
	h.newProject("obs", nil, func(pp *pagesv1alpha1.PagesProject) {
		pp.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: name}
		pp.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	})
	pp := h.waitProject("obs", func(pp *pagesv1alpha1.PagesProject) bool {
		return hasCond(pp.Status.Conditions, pp.Generation, "Synced", metav1.ConditionTrue, commonv1alpha1.ReasonObserveOnly) && ready(pp)
	})
	if pp.Status.AtProvider.Subdomain != name+".pages.dev" {
		t.Fatalf("observation %+v", pp.Status.AtProvider)
	}
	h.assertNoWrites("obs", name)
	h.delete(pp)
	h.waitGone(pp)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("an observe-only object wrote:\n%s", testenv.Summary(w))
	}
	if _, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, name, "production"); !ok {
		t.Fatal("deleting an observe-only object deleted the project")
	}
}

// Deleting a project deletes it in Cloudflare (default Delete); a KVNamespace it binds waits
// for it, since Cloudflare does not check bindings.
func TestProjectDeleteOrderWithBinding(t *testing.T) {
	h := start(t)
	kv := wait(h, h.kv("bound", commonv1alpha1.DeletionDelete), ready[*kvv1alpha1.KVNamespace])
	name := h.projectName("del")
	pp := h.newProject("del", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main",
		DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Production: &pagesv1alpha1.PagesDeploymentConfig{
			KVNamespaces: []pagesv1alpha1.PagesKVBinding{{Name: "KV", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "bound"}}}}}}, nil)
	h.waitProject("del", projectReady)
	h.delete(kv)
	wait(h, kv, func(o *kvv1alpha1.KVNamespace) bool {
		c := condOf(o.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "PagesProject del")
	})
	h.delete(pp)
	h.waitGone(pp)
	h.waitGone(kv)
	if _, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, name, "production"); ok {
		t.Fatal("the project is still in Cloudflare")
	}
}
