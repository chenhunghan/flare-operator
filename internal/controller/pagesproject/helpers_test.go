package pagesproject_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/pagesdeployment"
	"flare.dev/operator/internal/controller/pagesproject"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	_ "flare.dev/operator/internal/generic/kinds" // KVNamespace, Queue, D1Database controllers
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	generic.ReferrerRetry = 500 * time.Millisecond
	testenv.Main(m, &env, testenv.Options{})
}

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
			return (&pagesproject.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				Recorder: mgr.GetEventRecorder(pagesproject.Name), APIReader: mgr.GetAPIReader(),
				DependencyRetry: 500 * time.Millisecond}).SetupWithManager(mgr)
		},
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&pagesdeployment.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Artifacts: d.Artifacts,
				Recorder: mgr.GetEventRecorder(pagesdeployment.Name), APIReader: mgr.GetAPIReader(),
				DependencyRetry: 500 * time.Millisecond, StagePollInterval: 200 * time.Millisecond}).SetupWithManager(mgr)
		})
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

func accountRef() commonv1alpha1.ResourceSpec {
	return commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}}
}

func (h *harness) create(obj client.Object) {
	h.t.Helper()
	obj.SetNamespace(h.ns)
	if err := h.e.Client.Create(h.ctx(), obj); err != nil {
		h.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

// projectName is a unique Cloudflare project name for this test.
func (h *harness) projectName(n string) string { return h.ns + "-" + n }

func (h *harness) newProject(name string, fp *pagesv1alpha1.PagesProjectParameters, mut func(*pagesv1alpha1.PagesProject)) *pagesv1alpha1.PagesProject {
	h.t.Helper()
	pp := &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pagesv1alpha1.PagesProjectSpec{ResourceSpec: accountRef(), ForProvider: fp}}
	if mut != nil {
		mut(pp)
	}
	h.create(pp)
	return pp
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

func (h *harness) get(obj client.Object) error {
	return h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: obj.GetName()}, obj)
}

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

func synced[T commonv1alpha1.Managed](reason string) func(T) bool {
	return func(o T) bool {
		return hasCond(o.GetResourceStatus().Conditions, o.GetGeneration(), "Synced", metav1.ConditionFalse, reason)
	}
}

func (h *harness) waitProject(name string, ok func(*pagesv1alpha1.PagesProject) bool) *pagesv1alpha1.PagesProject {
	h.t.Helper()
	return wait(h, &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: name}}, ok)
}

func (h *harness) updateProject(name string, mut func(*pagesv1alpha1.PagesProject)) {
	h.t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var pp pagesv1alpha1.PagesProject
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &pp); err != nil {
			return err
		}
		mut(&pp)
		return h.e.Client.Update(h.ctx(), &pp)
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
		msg := fmt.Sprintf("%s still exists (err %v)", obj.GetName(), err)
		if mg, ok := obj.(commonv1alpha1.Managed); ok {
			msg += " " + condOf(mg.GetResourceStatus().Conditions, "Ready")
		}
		return false, msg
	})
}

func (h *harness) mark() int { return len(h.e.Journal(h.t)) }

func (h *harness) since(mark int) []fake.JournalEntry {
	j := h.e.Journal(h.t)
	if mark > len(j) {
		mark = len(j)
	}
	return testenv.ForAccount(j[mark:], h.acct.AccountID)
}

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

// settle pokes the project and waits until it was reconciled after the poke.
func (h *harness) settle(name string) {
	h.t.Helper()
	mark := h.m.Mark()
	h.poke(&pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: name}})
	h.m.WaitReconciled(h.t, pagesproject.Name, client.ObjectKey{Namespace: h.ns, Name: name}, mark, 1, 2*time.Minute)
}

// assertNoWrites reconciles the project again and asserts that nothing was written.
func (h *harness) assertNoWrites(name, project string) {
	h.t.Helper()
	m := h.mark()
	h.settle(name)
	j := h.since(m)
	if testenv.CountPath(j, http.MethodGet, "/accounts/"+h.acct.AccountID+"/pages/projects/"+project) == 0 {
		h.t.Fatalf("the reconcile after the poke did not read project %s:\n%s", project, testenv.Summary(j))
	}
	if w := testenv.Writes(j); len(w) != 0 {
		h.t.Fatalf("a reconcile without changes wrote to Cloudflare:\n%s", testenv.Summary(w))
	}
}

func (h *harness) apiDo(method, p string, body any) (json.RawMessage, error) {
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: method, Path: "/accounts/" + h.acct.AccountID + p, Body: body})
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// config returns the stored deployment config (secret values included).
func (h *harness) config(project, envName string) map[string]any {
	h.t.Helper()
	c, ok := h.e.Fake.PagesProjectConfig(h.acct.AccountID, project, envName)
	if !ok {
		h.t.Fatalf("no project %s in flarefake", project)
	}
	return c
}

func (h *harness) secret(name string, data map[string]string, optIn bool) *corev1.Secret {
	h.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, StringData: data}
	if optIn {
		s.Labels = map[string]string{pagesv1alpha1.LabelSecretOptIn: "true"}
	}
	h.create(s)
	return s
}
