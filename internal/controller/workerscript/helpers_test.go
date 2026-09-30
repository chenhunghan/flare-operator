package workerscript_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/vpcservice"
	"flare.dev/operator/internal/controller/workerscript"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	_ "flare.dev/operator/internal/generic/kinds" // KVNamespace, Queue, D1Database controllers
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	generic.ReferrerRetry = 500 * time.Millisecond
	flag.Parse()
	opts := testenv.Options{}
	if !testing.Short() {
		// R2 buckets (r2bucket_test.go) are served by the fake's generic profile, which loads
		// the pinned spec; those tests skip with -short.
		opts.Fake.Generic = fake.GeneratedGenericKinds()
	}
	testenv.Main(m, &env, opts)
}

const fetchModule = `export default {
  async fetch(req, env) {
    return new Response("ok");
  }
};
`

// harness is one test's manager, namespace and Ready account.
type harness struct {
	t    *testing.T
	e    *testenv.Env
	m    *testenv.Manager
	ns   string
	acct *testenv.Account
	cf   cfclient.Client
}

func start(t *testing.T) *harness { return startWith(t, testenv.ManagerOptions{}) }

func startWith(t *testing.T, o testenv.ManagerOptions) *harness {
	t.Helper()
	e := testenv.Require(t, env)
	o.Controllers = append(o.Controllers, "kvnamespace", "queue", "d1database")
	o.Setup = append(o.Setup,
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&workerscript.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				Recorder: mgr.GetEventRecorder(workerscript.Name), APIReader: mgr.GetAPIReader(),
				DependencyRetry: 500 * time.Millisecond, Artifacts: d.Artifacts}).SetupWithManager(mgr)
		},
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&vpcservice.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts,
				Recorder: mgr.GetEventRecorder(vpcservice.Name), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr)
		})
	// The manager sees only this test's namespace: objects that earlier
	// tests left behind (envtest runs no namespace controller) are not reconciled meanwhile.
	ns := e.Namespace(t)
	o.Namespaces = []string{ns}
	h := &harness{t: t, e: e, m: e.StartManager(t, o), ns: ns}
	h.acct = e.CreateReadyAccount(t, h.ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	return h
}

func (h *harness) ctx() context.Context { return testenv.Context(h.t, 20*time.Second) }

func str(s string) *string { return &s }

// params is a minimal inline script.
func params(code string) *workersv1alpha1.WorkerScriptParameters {
	return &workersv1alpha1.WorkerScriptParameters{
		MainModule:        "index.js",
		CompatibilityDate: "2026-09-01",
		Modules:           map[string]workersv1alpha1.WorkerModule{"index.js": {Type: "esm", Content: code}},
	}
}

func (h *harness) create(obj client.Object) {
	h.t.Helper()
	obj.SetNamespace(h.ns)
	if err := h.e.Client.Create(h.ctx(), obj); err != nil {
		h.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func (h *harness) newScript(name string, fp *workersv1alpha1.WorkerScriptParameters, mut func(*workersv1alpha1.WorkerScript)) *workersv1alpha1.WorkerScript {
	h.t.Helper()
	ws := &workersv1alpha1.WorkerScript{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: workersv1alpha1.WorkerScriptSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider:  fp,
		},
	}
	if mut != nil {
		mut(ws)
	}
	h.create(ws)
	return ws
}

func hasCond(conds []metav1.Condition, gen int64, typ string, st metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(conds, typ)
	return c != nil && c.Status == st && (reason == "" || c.Reason == reason) && c.ObservedGeneration == gen
}

func condOf(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return typ + "=<none>"
	}
	return fmt.Sprintf("%s=%s/%s %q (gen %d)", typ, c.Status, c.Reason, c.Message, c.ObservedGeneration)
}

// get fetches obj (which carries namespace and name) fresh from the API server.
func (h *harness) get(obj client.Object) error {
	return h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: obj.GetName()}, obj)
}

// wait polls the managed object until ok.
func wait[T interface {
	client.Object
	commonv1alpha1.Managed
}](h *harness, obj T, ok func(T) bool) T {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		if err := h.get(obj); err != nil {
			return false, err.Error()
		}
		st := obj.GetResourceStatus()
		return ok(obj), condOf(st.Conditions, "Ready") + " " + condOf(st.Conditions, "Synced")
	})
	return obj
}

func ready[T commonv1alpha1.Managed](o T) bool {
	st := o.GetResourceStatus()
	return hasCond(st.Conditions, o.GetGeneration(), "Ready", metav1.ConditionTrue, "") &&
		hasCond(st.Conditions, o.GetGeneration(), "Synced", metav1.ConditionTrue, "")
}

func (h *harness) waitScript(name string, ok func(*workersv1alpha1.WorkerScript) bool) *workersv1alpha1.WorkerScript {
	h.t.Helper()
	return wait(h, &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: name}}, ok)
}

func (h *harness) updateScript(name string, mut func(*workersv1alpha1.WorkerScript)) {
	h.t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws workersv1alpha1.WorkerScript
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &ws); err != nil {
			return err
		}
		mut(&ws)
		return h.e.Client.Update(h.ctx(), &ws)
	}); err != nil {
		h.t.Fatalf("update %s: %v", name, err)
	}
}

func (h *harness) delete(obj client.Object) {
	h.t.Helper()
	if err := h.e.Client.Delete(h.ctx(), obj); err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("delete %s: %v", obj.GetName(), err)
	}
}

func (h *harness) waitGone(obj client.Object) {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		err := h.get(obj)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		return false, fmt.Sprintf("%s still exists (err %v)", obj.GetName(), err)
	})
}

// mark returns the current journal length; since(mark) returns this account's entries after it.
func (h *harness) mark() int { return len(h.e.Journal(h.t)) }

func (h *harness) since(mark int) []fake.JournalEntry {
	j := h.e.Journal(h.t)
	if mark > len(j) {
		mark = len(j)
	}
	return testenv.ForAccount(j[mark:], h.acct.AccountID)
}

// scriptPath is the account-relative path of a script.
func (h *harness) scriptPath(name string) string {
	return "/accounts/" + h.acct.AccountID + "/workers/scripts/" + name
}

// uploads counts the uploads (PUT) of script name in j.
func (h *harness) uploads(j []fake.JournalEntry, name string) int {
	return len(testenv.Filter(j, func(e fake.JournalEntry) bool {
		return e.Method == http.MethodPut && e.Path == h.scriptPath(name)
	}))
}

// patches counts the settings PATCHes of script name in j.
func (h *harness) patches(j []fake.JournalEntry, name string) int {
	return testenv.Count(j, http.MethodPatch, "/workers/scripts/"+name+"/settings")
}

// poke changes an annotation so the object is reconciled again without a spec change.
func (h *harness) poke(obj client.Object) {
	h.t.Helper()
	if err := h.get(obj); err != nil {
		h.t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a["test.flare.dev/poke"] = time.Now().Format(time.RFC3339Nano)
	obj.SetAnnotations(a)
	if err := h.e.Client.Patch(h.ctx(), obj, client.MergeFrom(base)); err != nil {
		h.t.Fatalf("poke %s: %v", obj.GetName(), err)
	}
}

// settle pokes objs and waits until each was reconciled after the poke, with no reconcile of it
// still running: whatever an earlier reconcile was doing has finished (reconciles of one
// object are serialized), and one more reconcile ran on the current state. It is the positive
// signal to wait for before asserting that something did not happen.
func (h *harness) settle(objs ...client.Object) {
	h.t.Helper()
	mark := h.m.Mark()
	for _, o := range objs {
		h.poke(o)
	}
	for _, o := range objs {
		h.m.WaitReconciled(h.t, controllerOf(o), client.ObjectKey{Namespace: h.ns, Name: o.GetName()}, mark, 1, 2*time.Minute)
	}
}

// controllerOf is the name of the controller that reconciles obj ("" = any).
func controllerOf(obj client.Object) string {
	switch obj.(type) {
	case *workersv1alpha1.WorkerScript:
		return workerscript.Name
	case *workersvpcv1alpha1.VPCService:
		return vpcservice.Name
	case *kvv1alpha1.KVNamespace:
		return "kvnamespace"
	}
	return ""
}

// script is a WorkerScript reference by name (for poke and settle).
func script(name string) *workersv1alpha1.WorkerScript {
	return &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// assertNoWrites pokes the scripts, waits until each was reconciled after the poke (and re-read:
// GET …/settings) and asserts that nothing was written to the fake.
func (h *harness) assertNoWrites(names ...string) {
	h.t.Helper()
	m := h.mark()
	objs := make([]client.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, script(n))
	}
	h.settle(objs...)
	j := h.since(m)
	for _, n := range names {
		if testenv.CountPath(j, http.MethodGet, "/accounts/"+h.acct.AccountID+"/workers/scripts/"+n+"/settings") == 0 {
			h.t.Fatalf("the reconcile after the poke did not read the settings of %s:\n%s", n, testenv.Summary(j))
		}
	}
	if w := testenv.Writes(j); len(w) != 0 {
		h.t.Fatalf("a reconcile without changes wrote to Cloudflare:\n%s", testenv.Summary(w))
	}
}

// apiGet GETs an account-relative path into out.
func (h *harness) apiGet(p string, out any) error {
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + p})
	if err != nil {
		return err
	}
	return json.Unmarshal(resp.Result, out)
}

type settings struct {
	CompatibilityDate  string           `json:"compatibility_date"`
	CompatibilityFlags []string         `json:"compatibility_flags"`
	Bindings           []map[string]any `json:"bindings"`
}

func (h *harness) settings(name string) settings {
	h.t.Helper()
	var s settings
	if err := h.apiGet("/workers/scripts/"+name+"/settings", &s); err != nil {
		h.t.Fatalf("GET settings of %s: %v", name, err)
	}
	return s
}

func (h *harness) scriptExists(name string) bool {
	h.t.Helper()
	var s settings
	err := h.apiGet("/workers/scripts/"+name+"/settings", &s)
	if cfclient.IsNotFound(err) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return true
}

func binding(s settings, name string) map[string]any {
	for _, b := range s.Bindings {
		if b["name"] == name {
			return b
		}
	}
	return nil
}

// apiUpload uploads a script directly (as 0036 does), bypassing the operator.
func (h *harness) apiUpload(name, code string) {
	h.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	add := func(name, ct string, data []byte) {
		hd := textproto.MIMEHeader{}
		hd.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, name, name))
		hd.Set("Content-Type", ct)
		p, err := w.CreatePart(hd)
		if err != nil {
			h.t.Fatal(err)
		}
		_, _ = p.Write(data)
	}
	add("metadata", "application/json", []byte(`{"main_module":"index.js","compatibility_date":"2026-09-01"}`))
	add("index.js", "application/javascript+module", []byte(code))
	_ = w.Close()
	if _, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodPut, Path: "/accounts/" + h.acct.AccountID + "/workers/scripts/" + name,
		RawBody: buf.Bytes(), ContentType: w.FormDataContentType()}); err != nil {
		h.t.Fatalf("upload %s: %v", name, err)
	}
}

// ownerTag returns the script's flare.dev/owner tag ("" when untagged).
func (h *harness) ownerTag(tag string) string {
	h.t.Helper()
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + "/tags",
		Query: url.Values{"resource_type": {"worker"}, "resource_id": {tag}}})
	if err != nil {
		if ae, ok := cfclient.AsAPIError(err); ok && ae.Status == http.StatusInternalServerError {
			return ""
		}
		h.t.Fatalf("GET tags: %v", err)
	}
	var out struct{ Tags map[string]string }
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.Tags["flare.dev/owner"]
}

func (h *harness) secret(name string, data map[string]string) *corev1.Secret {
	h.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{workerscript.LabelWorkerBinding: "true"}}, StringData: data}
	h.create(s)
	return s
}
