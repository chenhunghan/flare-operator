package workerscript_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	d1v1alpha1 "github.com/chenhunghan/flare-operator/api/d1/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "github.com/chenhunghan/flare-operator/api/workersvpc/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

func accountRef() commonv1alpha1.ResourceSpec {
	return commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}}
}

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

// apiTunnel creates a tunnel directly in Cloudflare (a VPC service needs one).
func (h *harness) apiTunnel(name string) string {
	h.t.Helper()
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodPost, Path: "/accounts/" + h.acct.AccountID + "/cfd_tunnel",
		Body: map[string]string{"name": name, "config_src": "cloudflare"}})
	if err != nil {
		h.t.Fatal(err)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.ID
}

func (h *harness) vpc(name, tunnelID string) *workersvpcv1alpha1.VPCService {
	o := &workersvpcv1alpha1.VPCService{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: workersvpcv1alpha1.VPCServiceSpec{ResourceSpec: accountRef(), ForProvider: &workersvpcv1alpha1.VPCServiceParameters{
			Name: h.ns + "-" + name, Type: "tcp", TCPPort: ptr.To[int32](5432),
			Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.7"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: &tunnelID}},
		}}}
	h.create(o)
	return o
}

func scriptReady(ws *workersv1alpha1.WorkerScript) bool { return ready(ws) }

// Every binding type, references resolved to IDs, workers.dev enabled; then no writes.
func TestUploadWithEveryBindingType(t *testing.T) {
	h := start(t)
	kv := wait(h, h.kv("cache", ""), ready[*kvv1alpha1.KVNamespace])
	q := wait(h, h.queue("jobs"), ready[*queuesv1alpha1.Queue])
	db := wait(h, h.d1("db"), ready[*d1v1alpha1.D1Database])
	vs := wait(h, h.vpc("backend", h.apiTunnel(h.ns+"-tun")), ready[*workersvpcv1alpha1.VPCService])
	h.secret("api", map[string]string{"key": "s3cret"})
	h.newScript("backend", params(fetchModule), nil)
	h.waitScript("backend", scriptReady)

	m := h.mark()
	fp := params(fetchModule)
	fp.CompatibilityFlags = []string{"nodejs_compat"}
	fp.WorkersDev = &workersv1alpha1.WorkersDev{Enabled: true}
	fp.Observability = &workersv1alpha1.WorkerObservability{Enabled: true,
		Logs: &workersv1alpha1.WorkerObservabilityLogs{Enabled: true, InvocationLogs: ptr.To(true)}}
	fp.Bindings = []workersv1alpha1.WorkerBinding{
		{Name: "GREETING", Type: "plain_text", Text: str("hello")},
		{Name: "API_KEY", Type: "secret_text", SecretKeyRef: &workersv1alpha1.SecretKeyRef{Name: "api", Key: "key"}},
		{Name: "CONFIG", Type: "json", JSON: &apiextensionsv1.JSON{Raw: []byte(`{"n":1,"on":true}`)}},
		{Name: "CACHE", Type: "kv_namespace", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "cache"}},
		{Name: "RAW_KV", Type: "kv_namespace", NamespaceID: str("0123456789abcdef0123456789abcdef")},
		{Name: "JOBS", Type: "queue", QueueRef: &commonv1alpha1.LocalRef{Name: "jobs"}},
		{Name: "DB", Type: "d1", D1DatabaseRef: &commonv1alpha1.LocalRef{Name: "db"}},
		{Name: "PRIVATE", Type: "vpc_service", VPCServiceRef: &commonv1alpha1.LocalRef{Name: "backend"}},
		{Name: "BACKEND", Type: "service", ServiceRef: &commonv1alpha1.LocalRef{Name: "backend"}, Entrypoint: str("default")},
	}
	h.newScript("front", fp, nil)
	ws := h.waitScript("front", func(ws *workersv1alpha1.WorkerScript) bool {
		return scriptReady(ws) && ws.Status.AtProvider.URL != ""
	})
	j := h.since(m)
	if n := h.uploads(j, "front"); n != 1 {
		t.Fatalf("uploads %d, want 1:\n%s", n, testenv.Summary(j))
	}
	if n := testenv.Count(j, http.MethodPost, "/workers/scripts/front/subdomain"); n != 1 {
		t.Fatalf("subdomain POSTs %d, want 1", n)
	}
	a := ws.Status.AtProvider
	if a.Tag == "" || a.Etag == "" || a.VersionID == "" || a.DeploymentID == "" || len(a.Handlers) != 1 || a.Handlers[0] != "fetch" ||
		a.Subdomain == nil || !a.Subdomain.Enabled || !strings.HasPrefix(a.URL, "https://front.") || len(a.Bindings) != 9 {
		t.Fatalf("atProvider %+v", a)
	}
	if ws.Status.ID != "front" || ws.Status.ContentHash == "" || ws.Status.SettingsHash == "" || ws.Status.WriteOnlyHash == "" {
		t.Fatalf("status %+v", ws.Status)
	}
	if !reconcile.HasOwnershipProof(ws, "front") || h.ownerTag(a.Tag) != "testenv/"+h.ns+"/front" {
		t.Fatalf("ownership: annotations %v, tag %q", ws.Annotations, h.ownerTag(a.Tag))
	}
	s := h.settings("front")
	for name, want := range map[string]map[string]any{
		"GREETING": {"type": "plain_text", "text": "hello"},
		"CONFIG":   {"type": "json", "json": map[string]any{"n": float64(1), "on": true}},
		"CACHE":    {"type": "kv_namespace", "namespace_id": kv.Status.ID},
		"RAW_KV":   {"type": "kv_namespace", "namespace_id": "0123456789abcdef0123456789abcdef"},
		"JOBS":     {"type": "queue", "queue_name": *q.Status.AtProvider.QueueName},
		"DB":       {"type": "d1", "database_id": db.Status.ID},
		"PRIVATE":  {"type": "vpc_service", "service_id": vs.Status.ID},
		"BACKEND":  {"type": "service", "service": "backend", "entrypoint": "default"},
		"API_KEY":  {"type": "secret_text"},
	} {
		b := binding(s, name)
		if b == nil {
			t.Fatalf("binding %s missing: %v", name, s.Bindings)
		}
		for k, v := range want {
			if bj, _ := json.Marshal(b[k]); string(bj) != string(must(json.Marshal(v))) {
				t.Errorf("binding %s.%s = %v, want %v", name, k, b[k], v)
			}
		}
	}
	if s.CompatibilityDate != "2026-09-01" || len(s.CompatibilityFlags) != 1 {
		t.Errorf("settings %+v", s)
	}
	h.assertNoWrites("front", "backend")
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// A reference that is not Ready yet uploads nothing; once it is, one upload follows (watch).
func TestPendingReferenceNoUpload(t *testing.T) {
	h := start(t)
	m := h.mark()
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "CACHE", Type: "kv_namespace", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "later"}}}
	h.newScript("waits", fp, nil)
	ws := h.waitScript("waits", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonDependency)
	})
	if c := condOf(ws.Status.Conditions, "Synced"); !strings.Contains(c, "KVNamespace later not found") {
		t.Fatalf("condition %s", c)
	}
	h.settle(script("waits")) // one more reconcile with the reference still pending
	if n := h.uploads(h.since(m), "waits"); n != 0 {
		t.Fatalf("%d uploads while the reference is pending", n)
	}
	kv := wait(h, h.kv("later", ""), ready[*kvv1alpha1.KVNamespace])
	h.waitScript("waits", scriptReady)
	if n := h.uploads(h.since(m), "waits"); n != 1 {
		t.Fatalf("uploads %d, want 1", n)
	}
	if b := binding(h.settings("waits"), "CACHE"); b == nil || b["namespace_id"] != kv.Status.ID {
		t.Fatalf("binding %v, want namespace_id %s", b, kv.Status.ID)
	}
}

// A content change is exactly one upload; a settings-only change exactly one settings PATCH
// (and a Secret value change, too); a ConfigMap change one upload.
func TestContentAndSettingsChanges(t *testing.T) {
	h := start(t)
	h.secret("tok", map[string]string{"v": "one"})
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{
		{Name: "MODE", Type: "plain_text", Text: str("a")},
		{Name: "TOKEN", Type: "secret_text", SecretKeyRef: &workersv1alpha1.SecretKeyRef{Name: "tok", Key: "v"}},
	}
	h.newScript("app", fp, nil)
	v1 := h.waitScript("app", scriptReady)

	step := func(what string, mut func(), wantUploads, wantPatches int) *workersv1alpha1.WorkerScript {
		t.Helper()
		m := h.mark()
		prev := h.waitScript("app", scriptReady)
		mut()
		ws := h.waitScript("app", func(ws *workersv1alpha1.WorkerScript) bool {
			return scriptReady(ws) && ws.Status.AtProvider.VersionID != prev.Status.AtProvider.VersionID
		})
		h.settle(script("app")) // stragglers finished; a no-op reconcile must not write either
		j := h.since(m)
		if u, p := h.uploads(j, "app"), h.patches(j, "app"); u != wantUploads || p != wantPatches {
			t.Fatalf("%s: %d uploads and %d settings PATCHes, want %d and %d:\n%s", what, u, p, wantUploads, wantPatches, testenv.Summary(j))
		}
		return ws
	}
	v2 := step("content change", func() {
		h.updateScript("app", func(ws *workersv1alpha1.WorkerScript) {
			ws.Spec.ForProvider.Modules["index.js"] = workersv1alpha1.WorkerModule{Type: "esm", Content: strings.Replace(fetchModule, `"ok"`, `"v2"`, 1)}
		})
	}, 1, 0)
	if v2.Status.ContentHash == v1.Status.ContentHash {
		t.Fatal("content hash did not change")
	}
	step("settings change", func() {
		h.updateScript("app", func(ws *workersv1alpha1.WorkerScript) {
			ws.Spec.ForProvider.Bindings[0].Text = str("b")
			ws.Spec.ForProvider.CompatibilityFlags = []string{"nodejs_compat"}
		})
	}, 0, 1)
	if b := binding(h.settings("app"), "MODE"); b["text"] != "b" {
		t.Fatalf("binding %v", b)
	}
	step("secret change", func() {
		var s corev1.Secret
		s.Name = "tok"
		if err := h.get(&s); err != nil {
			t.Fatal(err)
		}
		s.Data["v"] = []byte("two")
		if err := h.e.Client.Update(h.ctx(), &s); err != nil {
			t.Fatal(err)
		}
	}, 0, 1)
	h.assertNoWrites("app")

	// Out-of-band upload (someone ran wrangler deploy): the next reconcile uploads forProvider again.
	m := h.mark()
	h.apiUpload("app", fetchModule)
	h.poke(&workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		j := h.since(m)
		return h.uploads(j, "app") == 2, testenv.Summary(j)
	})
	h.waitScript("app", scriptReady)
	h.assertNoWrites("app")
}

func TestConfigMapSource(t *testing.T) {
	h := start(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "src"}, Data: map[string]string{
		"index.js": `import msg from "./msg.txt";` + "\n" + fetchModule, "msg.txt": "hi"}}
	h.create(cm)
	h.newScript("cm", &workersv1alpha1.WorkerScriptParameters{MainModule: "index.js", CompatibilityDate: "2026-09-01",
		SourceRef: &workersv1alpha1.WorkerSourceRef{Name: "src"}}, nil)
	v1 := h.waitScript("cm", scriptReady)
	m := h.mark()
	cm.Data["msg.txt"] = "hello"
	if err := h.e.Client.Update(h.ctx(), cm); err != nil {
		t.Fatal(err)
	}
	h.waitScript("cm", func(ws *workersv1alpha1.WorkerScript) bool {
		return scriptReady(ws) && ws.Status.ContentHash != v1.Status.ContentHash
	})
	if n := h.uploads(h.since(m), "cm"); n != 1 {
		t.Fatalf("uploads %d, want 1", n)
	}
	h.assertNoWrites("cm")
}

// An existing script is adopted only through the external-id annotation; then one upload.
func TestAdoptByExternalID(t *testing.T) {
	h := start(t)
	h.apiUpload("legacy", fetchModule)
	m := h.mark()
	h.newScript("legacy", params(fetchModule), nil)
	h.waitScript("legacy", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, workerscriptNameConflict)
	})
	h.settle(script("legacy")) // one more reconcile of the conflict
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("a name conflict wrote:\n%s", testenv.Summary(w))
	}
	h.updateScript("legacy", func(ws *workersv1alpha1.WorkerScript) {
		ws.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "legacy"}
	})
	ws := h.waitScript("legacy", scriptReady)
	if n := h.uploads(h.since(m), "legacy"); n != 1 {
		t.Fatalf("uploads %d, want 1 (adoption applies forProvider once)", n)
	}
	if h.ownerTag(ws.Status.AtProvider.Tag) != "testenv/"+h.ns+"/legacy" || !reconcile.HasOwnershipProof(ws, "legacy") {
		t.Fatalf("not claimed: %v", ws.Annotations)
	}
	h.assertNoWrites("legacy")
}

// Observe-only reads and never writes, also on deletion.
func TestObserveOnly(t *testing.T) {
	h := start(t)
	h.apiUpload("seen", fetchModule)
	m := h.mark()
	h.newScript("seen", nil, func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	})
	ws := h.waitScript("seen", scriptReady)
	if ws.Status.AtProvider.Tag == "" || ws.Status.AtProvider.VersionID == "" || ws.Status.AtProvider.CompatibilityDate != "2026-09-01" {
		t.Fatalf("atProvider %+v", ws.Status.AtProvider)
	}
	if c := condOf(ws.Status.Conditions, "Synced"); !strings.Contains(c, commonv1alpha1.ReasonObserveOnly) {
		t.Fatalf("Synced %s", c)
	}
	h.assertNoWrites("seen")
	h.delete(ws)
	h.waitGone(ws)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("observe-only wrote:\n%s", testenv.Summary(w))
	}
	if !h.scriptExists("seen") {
		t.Fatal("observe-only deletion deleted the script")
	}
	// A missing script is ExternalNotFound, not an error.
	h.newScript("absent", nil, func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	})
	h.waitScript("absent", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonNotFound)
	})
}

// Delete (default policy) deletes the script; Orphan keeps it and releases the owner tag.
func TestDeletion(t *testing.T) {
	h := start(t)
	h.newScript("gone", params(fetchModule), nil)
	ws := h.waitScript("gone", scriptReady)
	m := h.mark()
	h.delete(ws)
	h.waitGone(ws)
	if n := testenv.Count(h.since(m), http.MethodDelete, "/workers/scripts/gone"); n != 1 {
		t.Fatalf("DELETEs %d, want 1", n)
	}
	if h.scriptExists("gone") {
		t.Fatal("script still exists")
	}

	h.newScript("kept", params(fetchModule), func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.DeletionPolicy = commonv1alpha1.DeletionOrphan
	})
	ws = h.waitScript("kept", scriptReady)
	tag := ws.Status.AtProvider.Tag
	h.delete(ws)
	h.waitGone(ws)
	if !h.scriptExists("kept") || h.ownerTag(tag) != "" {
		t.Fatalf("orphaned script: exists %v, owner tag %q", h.scriptExists("kept"), h.ownerTag(tag))
	}
}

// A vpc_service binding to an unknown service fails the upload with 400/10180 (0106).
func TestUnknownVPCService(t *testing.T) {
	h := start(t)
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "PRIVATE", Type: "vpc_service", ServiceID: str("00000000-0000-7000-8000-000000000000")}}
	h.newScript("bogus", fp, nil)
	ws := h.waitScript("bogus", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError)
	})
	if c := condOf(ws.Status.Conditions, "Synced"); !strings.Contains(c, "10180") {
		t.Fatalf("Synced %s", c)
	}
	if h.scriptExists("bogus") {
		t.Fatal("the failed upload created a script")
	}
}

// A KVNamespace (deletionPolicy Delete) and a VPCService bound by a WorkerScript wait for it
// before their own Cloudflare delete.
func TestReferencedDeletionWaits(t *testing.T) {
	h := start(t)
	kv := wait(h, h.kv("store", commonv1alpha1.DeletionDelete), ready[*kvv1alpha1.KVNamespace])
	vs := wait(h, h.vpc("svc", h.apiTunnel(h.ns+"-tun")), ready[*workersvpcv1alpha1.VPCService])
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{
		{Name: "STORE", Type: "kv_namespace", KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "store"}},
		{Name: "SVC", Type: "vpc_service", VPCServiceRef: &commonv1alpha1.LocalRef{Name: "svc"}},
	}
	h.newScript("user", fp, nil)
	ws := h.waitScript("user", scriptReady)

	m := h.mark()
	h.delete(kv)
	h.delete(vs)
	wait(h, kv, func(o *kvv1alpha1.KVNamespace) bool {
		c := condOf(o.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "WorkerScript user")
	})
	wait(h, vs, func(o *workersvpcv1alpha1.VPCService) bool {
		return strings.Contains(condOf(o.Status.Conditions, "Ready"), commonv1alpha1.ReasonDependency)
	})
	h.settle(kv, vs) // one more finalizer pass of each while still bound
	if n := testenv.Count(h.since(m), http.MethodDelete, ""); n != 0 {
		t.Fatalf("deleted while bound:\n%s", testenv.Summary(h.since(m)))
	}
	h.delete(ws)
	h.waitGone(ws)
	h.waitGone(kv)
	h.waitGone(vs)
	j := h.since(m)
	if testenv.Count(j, http.MethodDelete, "/storage/kv/namespaces/"+kv.Status.ID) != 1 ||
		testenv.Count(j, http.MethodDelete, "/connectivity/directory/services/"+vs.Status.ID) != 1 {
		t.Fatalf("deletes:\n%s", testenv.Summary(j))
	}
}

const workerscriptNameConflict = reconcile.ReasonNameConflict

// The CRD's CEL rules reject malformed bindings and sources.
func TestValidation(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 20*time.Second)
	try := func(what string, mut func(*workersv1alpha1.WorkerScript), wantErr string) {
		t.Helper()
		ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
			Spec: workersv1alpha1.WorkerScriptSpec{ResourceSpec: accountRef(), ForProvider: params(fetchModule)}}
		mut(ws)
		err := e.Client.Create(ctx, ws, client.DryRunAll)
		switch {
		case wantErr == "" && err != nil:
			t.Errorf("%s: %v", what, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("%s: got %v, want an error containing %q", what, err, wantErr)
		}
	}
	try("valid", func(*workersv1alpha1.WorkerScript) {}, "")
	try("kv with ID and ref", func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.ForProvider.Bindings = []workersv1alpha1.WorkerBinding{{Name: "K", Type: "kv_namespace", NamespaceID: str("x"),
			KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "k"}}}
	}, "exactly one of namespace_id or kvNamespaceRef")
	try("text on a queue", func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.ForProvider.Bindings = []workersv1alpha1.WorkerBinding{{Name: "Q", Type: "queue", QueueName: str("q"), Text: str("x")}}
	}, "text is required for (and only valid with) type plain_text")
	try("modules and sourceRef", func(ws *workersv1alpha1.WorkerScript) {
		ws.Spec.ForProvider.SourceRef = &workersv1alpha1.WorkerSourceRef{Name: "cm"}
	}, "exactly one of modules, sourceRef or moduleSource")
	try("main module missing", func(ws *workersv1alpha1.WorkerScript) { ws.Spec.ForProvider.MainModule = "other.js" }, "main_module must name one of modules")
	try("sampling rate", func(ws *workersv1alpha1.WorkerScript) {
		r := intstr.FromString("1.5")
		ws.Spec.ForProvider.Observability = &workersv1alpha1.WorkerObservability{HeadSamplingRate: &r}
	}, "must be between 0 and 1")
	try("sampling rate ok", func(ws *workersv1alpha1.WorkerScript) {
		r, one := intstr.FromString("0.25"), intstr.FromInt32(1)
		ws.Spec.ForProvider.Observability = &workersv1alpha1.WorkerObservability{HeadSamplingRate: &r,
			Logs: &workersv1alpha1.WorkerObservabilityLogs{HeadSamplingRate: &one}}
	}, "")
	try("no forProvider when managing", func(ws *workersv1alpha1.WorkerScript) { ws.Spec.ForProvider = nil }, "forProvider is required")
	try("bad script name", func(ws *workersv1alpha1.WorkerScript) { ws.Spec.ForProvider.ScriptName = "Bad.Name" }, "script_name")

	// script_name is immutable.
	ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "imm", Namespace: ns},
		Spec: workersv1alpha1.WorkerScriptSpec{ResourceSpec: accountRef(), ForProvider: params(fetchModule)}}
	ws.Spec.ForProvider.ScriptName = "one"
	if err := e.Client.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	ws.Spec.ForProvider.ScriptName = "two"
	if err := e.Client.Update(ctx, ws); err == nil || !strings.Contains(err.Error(), "script_name is immutable") {
		t.Fatalf("rename: %v", err)
	}
}

// A WorkerScript bound by another one's serviceRef waits for it before deleting its script.
func TestServiceBindingDeletionWaits(t *testing.T) {
	h := start(t)
	h.newScript("callee", params(fetchModule), nil)
	callee := h.waitScript("callee", scriptReady)
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "CALLEE", Type: "service", ServiceRef: &commonv1alpha1.LocalRef{Name: "callee"}}}
	h.newScript("caller", fp, nil)
	caller := h.waitScript("caller", scriptReady)
	h.delete(callee)
	h.waitScript("callee", func(ws *workersv1alpha1.WorkerScript) bool {
		c := condOf(ws.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "WorkerScript caller")
	})
	if !h.scriptExists("callee") {
		t.Fatal("callee deleted while bound")
	}
	h.delete(caller)
	h.waitGone(caller)
	h.waitGone(callee)
	if h.scriptExists("callee") || h.scriptExists("caller") {
		t.Fatal("scripts left behind")
	}
}
