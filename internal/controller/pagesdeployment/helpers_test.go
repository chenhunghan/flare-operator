package pagesdeployment_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesdeployment"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesproject"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/generic"
	"github.com/chenhunghan/flare-operator/internal/testenv"
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

// project creates a Ready PagesProject object (production branch main) and returns its
// Cloudflare name.
func (h *harness) project(name string) string {
	h.t.Helper()
	cfName := h.ns + "-" + name
	h.create(&pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pagesv1alpha1.PagesProjectSpec{ResourceSpec: accountRef(),
			ForProvider: &pagesv1alpha1.PagesProjectParameters{Name: cfName, ProductionBranch: "main"}}})
	wait(h, &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Name: name}}, ready[*pagesv1alpha1.PagesProject])
	return cfName
}

// siteConfigMap creates a ConfigMap opted in as an artifact.
func (h *harness) siteConfigMap(name string, data map[string]string, binary map[string][]byte) *corev1.ConfigMap {
	h.t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{sharedv1alpha1.LabelArtifact: "true"}},
		Data: data, BinaryData: binary}
	h.create(cm)
	return cm
}

func cmSource(names ...string) *sharedv1alpha1.ArtifactSource {
	src := &sharedv1alpha1.ArtifactSource{ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{}}
	for _, n := range names {
		src.ConfigMapRef.ConfigMaps = append(src.ConfigMapRef.ConfigMaps, sharedv1alpha1.ConfigMapArtifact{Name: n})
	}
	return src
}

func (h *harness) newDeployment(name, project, branch string, src *sharedv1alpha1.ArtifactSource, mut func(*pagesv1alpha1.PagesDeployment)) *pagesv1alpha1.PagesDeployment {
	h.t.Helper()
	pd := &pagesv1alpha1.PagesDeployment{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: pagesv1alpha1.PagesDeploymentSpec{ResourceSpec: accountRef(),
			ForProvider: pagesv1alpha1.PagesDeploymentParameters{ProjectRef: commonv1alpha1.LocalRef{Name: project}, Branch: branch, Source: src}}}
	if mut != nil {
		mut(pd)
	}
	h.create(pd)
	return pd
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

func (h *harness) waitDeployment(name string, ok func(*pagesv1alpha1.PagesDeployment) bool) *pagesv1alpha1.PagesDeployment {
	h.t.Helper()
	return wait(h, &pagesv1alpha1.PagesDeployment{ObjectMeta: metav1.ObjectMeta{Name: name}}, ok)
}

func deployed(pd *pagesv1alpha1.PagesDeployment) bool {
	ls := pd.Status.AtProvider.LatestStage
	return ready(pd) && ls != nil && ls.Name == "deploy" && ls.Status == "success"
}

func (h *harness) update(obj client.Object, mut func()) {
	h.t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := h.get(obj); err != nil {
			return err
		}
		mut()
		return h.e.Client.Update(h.ctx(), obj)
	}); err != nil {
		h.t.Fatalf("update %s: %v", obj.GetName(), err)
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

// since returns the journal after mark: this account's requests plus the asset calls (which
// name no account in their path).
func (h *harness) since(mark int) []fake.JournalEntry {
	j := h.e.Journal(h.t)
	if mark > len(j) {
		mark = len(j)
	}
	return testenv.Filter(j[mark:], func(e fake.JournalEntry) bool {
		return strings.HasPrefix(e.Path, "/accounts/"+h.acct.AccountID+"/") || strings.HasPrefix(e.Path, "/pages/assets/")
	})
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

// settle pokes the deployment and waits until it was reconciled after the poke.
func (h *harness) settle(name string) {
	h.t.Helper()
	mark := h.m.Mark()
	h.poke(&pagesv1alpha1.PagesDeployment{ObjectMeta: metav1.ObjectMeta{Name: name}})
	h.m.WaitReconciled(h.t, pagesdeployment.Name, client.ObjectKey{Namespace: h.ns, Name: name}, mark, 1, 2*time.Minute)
}

// deploys counts the deployment creates of project in j.
func (h *harness) deploys(j []fake.JournalEntry, project string) int {
	return testenv.CountPath(j, http.MethodPost, "/accounts/"+h.acct.AccountID+"/pages/projects/"+project+"/deployments")
}

// assertNoWrites reconciles the deployment again and asserts that nothing was written.
func (h *harness) assertNoWrites(name string) {
	h.t.Helper()
	m := h.mark()
	h.settle(name)
	j := h.since(m)
	if testenv.Count(j, http.MethodGet, "/deployments/") == 0 {
		h.t.Fatalf("the reconcile after the poke did not read the deployment:\n%s", testenv.Summary(j))
	}
	if w := testenv.Filter(j, func(e fake.JournalEntry) bool {
		return testenv.IsWrite(e) || strings.HasPrefix(e.Path, "/pages/assets/") || strings.HasSuffix(e.Path, "/upload-token")
	}); len(w) != 0 {
		h.t.Fatalf("a reconcile without changes wrote (or prepared a write):\n%s", testenv.Summary(w))
	}
}

// waitEvent waits for a Warning Event with reason on the object named regarding whose note
// contains every substring in notes.
func (h *harness) waitEvent(regarding, reason string, notes ...string) {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		var evs eventsv1.EventList
		if err := h.e.Client.List(h.ctx(), &evs, client.InNamespace(h.ns)); err != nil {
			return false, err.Error()
		}
		var seen []string
	next:
		for _, e := range evs.Items {
			seen = append(seen, e.Regarding.Name+"/"+e.Reason+": "+e.Note)
			if e.Regarding.Name != regarding || e.Reason != reason || e.Type != corev1.EventTypeWarning {
				continue
			}
			for _, n := range notes {
				if !strings.Contains(e.Note, n) {
					continue next
				}
			}
			return true, ""
		}
		return false, fmt.Sprintf("no %s Event on %s; events: %q", reason, regarding, seen)
	})
}

// deploymentExists reports whether Cloudflare has deployment id of project.
func (h *harness) deploymentExists(project, id string) bool {
	h.t.Helper()
	_, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + "/pages/projects/" + project + "/deployments/" + id})
	if cfclient.IsNotFound(err) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return true
}
