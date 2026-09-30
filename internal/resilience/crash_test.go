package resilience_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/tunnel"
	"flare.dev/operator/internal/controller/vpcservice"
	"flare.dev/operator/internal/controller/workerscript"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/generic/kinds"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// crashCase is one kind under the crash test.
type crashCase struct {
	name   string
	tagger reconcile.Tagger
	// kind-specific parts
	setup    func(c client.Client) func(ctrl.Manager, controller.Deps) error
	object   func(t *testing.T, cx *crashCtx) reconcile.ManagedObject
	resource func(cx *crashCtx) (listPath, nameField, idField, name string)
	// creates counts the Cloudflare create calls in j.
	creates func(cx *crashCtx, j []fake.JournalEntry) int
	// afterRestart checks extra invariants (optional).
	afterRestart func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, sinceRestart []fake.JournalEntry)
	// faults are injected before the object is created (optional).
	faults func(cx *crashCtx) []fake.Fault
	// rateLimit for the account (optional).
	rateLimit *cloudflarev1alpha1.RateLimitSpec
	// kind is the generated kind of a generic case ("" for the hand-written controllers).
	kind string
}

type crashCtx struct {
	e    *testenv.Env
	ns   string
	acct *testenv.Account
	cf   cfclient.Client
	name string // the Cloudflare name of the resource
	vars map[string]string
}

func (cx *crashCtx) path(p string) string {
	return strings.NewReplacer("{account_id}", cx.acct.AccountID).Replace(p)
}

// crashCases are the kinds under the crash tests.
func crashCases() []crashCase {
	var cases []crashCase
	for _, kind := range []string{"KVNamespace", "Queue", "D1Database", "VectorizeIndex", "SecretsStore", "AIGateway"} {
		for _, tagging := range []bool{true, false} {
			if !tagging && kind != "KVNamespace" && kind != "AIGateway" {
				continue // tagging off changes nothing else in the generic reconciler's create path
			}
			cases = append(cases, genericCrashCase(kind, tagging))
		}
	}
	return append(cases, tunnelCrashCase(true), tunnelCrashCase(false), vpcCrashCase(),
		workerCrashCase(true, false), workerCrashCase(true, true), workerCrashCase(false, false), workerAssetsCrashCase())
}

// TestCrashBetweenCreateAndRecord kills the manager right after each kind's Cloudflare create,
// before the new ID reaches the API server, restarts it, and checks that exactly one Cloudflare
// resource exists, that it was created once, and that the object adopted it with an ownership
// record (and no leftover create-pending record).
func TestCrashBetweenCreateAndRecord(t *testing.T) {
	for _, c := range crashCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runCrash(t, c)
		})
	}
}

// TestCrashThenDeleteBeforeRestart: the manager dies right after the Cloudflare create, the
// object (deletionPolicy Delete) is deleted while no manager runs, and a fresh manager
// finalizes it. The object has no ID, only its create-pending record: the finalizer must find
// the resource through that record and delete it, not drop the finalizer and leak it.
func TestCrashThenDeleteBeforeRestart(t *testing.T) {
	for _, c := range crashCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cr := crashFirst(t, c)
			if _, ok := cr.o.GetAnnotations()[reconcile.AnnotationCreatePending]; !ok {
				t.Fatalf("no create-pending record before the delete: %v", cr.o.GetAnnotations())
			}
			if err := cr.e.Client.Delete(testenv.Context(t, 10*time.Second), cr.o); err != nil {
				t.Fatal(err)
			}
			cr.restart(t, c)
			testenv.Eventually(t, 60*time.Second, func() (bool, string) {
				err := cr.e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(cr.o), cr.o)
				if apierrors.IsNotFound(err) {
					return true, ""
				}
				return false, fmt.Sprintf("%v %s", err, condString(cr.o))
			})
			if ids := cr.find(); len(ids) != 0 {
				t.Errorf("LEAK: the object (deletionPolicy Delete) is gone but Cloudflare still has %v", ids)
			}
			j := cr.e.Journal(t)
			if n := c.creates(cr.cx, testenv.ForAccount(j, cr.cx.acct.AccountID)); n != 1 {
				t.Errorf("%d creates, want 1", n)
			}
		})
	}
}

// TestCrashVPCServiceLaggingList: after the crash, the restarted manager's first lookup misses
// the lost create (a list that lags the create; list lag itself is UNVERIFIED), so the create is
// re-sent and refused as a duplicate name (400/5101, 0059). That refusal proves nothing was
// made, but not that the name is someone else's: the create-pending record must survive it, so
// the next reconcile adopts the service instead of reporting NameConflict for its own service.
func TestCrashVPCServiceLaggingList(t *testing.T) {
	c := vpcCrashCase()
	cr := crashFirst(t, c)
	if err := cr.e.Fake.InjectFault(fake.Fault{Method: http.MethodGet,
		PathRegex: "^" + cr.cx.path("/accounts/{account_id}/connectivity/directory/services") + "$", Status: http.StatusOK, Times: 1,
		FaultShape: fake.FaultShape{Body: `{"success":true,"errors":[],"messages":[],"result":[]}`}}); err != nil {
		t.Fatal(err)
	}
	cr.restart(t, c)
	waitReadySynced(t, cr.e, cr.o, 60*time.Second)
	j := accountJournal(t, cr.e, cr.cx.acct.AccountID)
	if n := c.creates(cr.cx, j); n != 2 {
		t.Errorf("%d creates, want 2 (the lost one and the refused duplicate):\n%s", n, testenv.Summary(j))
	}
	if ids := cr.find(); len(ids) != 1 || ids[0] != cr.ids[0] {
		t.Errorf("resources %v, want exactly %v", ids, cr.ids)
	}
	if got := cr.o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; got != cr.ids[0] {
		t.Errorf("external-id annotation %q, want %q", got, cr.ids[0])
	}
}

// crashRun is the state of one crash test after the crash.
type crashRun struct {
	e      *testenv.Env
	cx     *crashCtx
	o      reconcile.ManagedObject
	tagger reconcile.Tagger
	ids    []string // the resource the crash left
	// find lists the live resources with the case's name.
	find func() []string
}

// crashFirst runs a manager that crashes right after c's Cloudflare create, stops it, and
// checks that exactly one resource was made and its ID lost.
func crashFirst(t *testing.T, c crashCase) *crashRun {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	tagger := c.tagger
	if tagger == nil {
		tagger = reconcile.NoopTagger{}
	}
	var cc *crashClient
	m1 := e.StartManager(t, testenv.ManagerOptions{Tagger: tagger, Namespaces: []string{ns, "kube-system"},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, d controller.Deps) error {
			cc = newCrashClient(mgr.GetClient(), "obj")
			return c.setup(cc)(mgr, d)
		}}})
	acct := e.CreateAccount(t, ns, "acct", testenv.AccountOptions{RateLimit: c.rateLimit})
	acct.CloudflareAccount = e.WaitAccountCondition(t, ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	cx := &crashCtx{e: e, ns: ns, acct: acct, cf: apiClient(t, e, acct), name: "flare-spike-" + testenv.RandomHex(4), vars: map[string]string{}}
	if c.faults != nil {
		for _, f := range c.faults(cx) {
			if err := e.Fake.InjectFault(f); err != nil {
				t.Fatal(err)
			}
		}
	}
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

	find := finder(t, c, cx)
	// The crash left a Cloudflare resource the object does not know.
	ids := find()
	if len(ids) != 1 {
		_, _, _, name := c.resource(cx)
		t.Fatalf("after the crash: %d resources named %q (%v), want 1", len(ids), name, ids)
	}
	if err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	if got := o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; got != "" {
		t.Fatalf("the crash should have lost the ID, but the external-id annotation is %q", got)
	}
	return &crashRun{e: e, cx: cx, o: o, tagger: tagger, ids: ids, find: find}
}

// finder lists the live resources with c's name.
func finder(t *testing.T, c crashCase, cx *crashCtx) func() []string {
	return func() []string {
		listPath, nameField, idField, name := c.resource(cx)
		var ids []string
		items, err := cfclient.ListAllInto[map[string]any](testenv.Context(t, 20*time.Second), cx.cf, cfclient.Request{Path: listPath})
		if err != nil {
			t.Fatalf("list %s: %v", listPath, err)
		}
		for _, it := range items {
			if it[nameField] == name && it["deleted_at"] == nil { // tunnels are soft-deleted
				id, _ := it[idField].(string)
				ids = append(ids, id)
			}
		}
		return ids
	}
}

// TestCrashRecreateThenDeleteBeforeRestart: a generic object's resource X is deleted out of
// band, so the next reconcile recreates it (a new create-pending record, then the create), and
// the manager dies before RecordCreated. The object still carries X (external-id annotation,
// status.id) next to the record. It is deleted (deletionPolicy Delete) while no manager runs,
// and a fresh manager finalizes it: the finalizer must see that X is gone, find the new
// resource through the record and delete it, not delete only the gone X and leak the new one.
func TestCrashRecreateThenDeleteBeforeRestart(t *testing.T) {
	for _, c := range crashCases() {
		if c.kind == "" {
			// Tunnel and VPCService report a vanished pinned resource as NotFound instead of
			// recreating it, and a WorkerScript's ID is its script name, which the recreate
			// keeps: they cannot reach this state through a recreate.
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runRecreateCrash(t, c)
		})
	}
}

func runRecreateCrash(t *testing.T, c crashCase) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	tagger := c.tagger
	if tagger == nil {
		tagger = reconcile.NoopTagger{}
	}
	opts := func(setup func(ctrl.Manager, controller.Deps) error) testenv.ManagerOptions {
		return testenv.ManagerOptions{Tagger: tagger, Namespaces: []string{ns, "kube-system"},
			Setup: []func(ctrl.Manager, controller.Deps) error{setup}}
	}
	plain := func(mgr ctrl.Manager, d controller.Deps) error { return c.setup(mgr.GetClient())(mgr, d) }

	// 1. A normal manager creates the resource X and records it.
	m0 := e.StartManager(t, opts(plain))
	acct := e.CreateAccount(t, ns, "acct", testenv.AccountOptions{RateLimit: c.rateLimit})
	acct.CloudflareAccount = e.WaitAccountCondition(t, ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	cx := &crashCtx{e: e, ns: ns, acct: acct, cf: apiClient(t, e, acct), name: "flare-spike-" + testenv.RandomHex(4), vars: map[string]string{}}
	o := c.object(t, cx)
	o.SetNamespace(ns)
	o.SetName("obj")
	if err := e.Client.Create(testenv.Context(t, 10*time.Second), o); err != nil {
		t.Fatalf("create: %v", err)
	}
	waitReadySynced(t, e, o, 60*time.Second)
	old := reconcile.ExternalID(o)
	if old == "" || o.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != old {
		t.Fatalf("no recorded ID after the create: %v", o.GetAnnotations())
	}
	m0.Stop(t)

	// 2. X goes away out of band.
	en := entryByKind(c.kind)
	item := strings.NewReplacer("{account_id}", acct.AccountID, "{id}", old).Replace(en.ItemPath)
	if _, err := cx.cf.Do(testenv.Context(t, 10*time.Second), cfclient.Request{Method: http.MethodDelete, Path: item}); err != nil {
		t.Fatalf("delete %s out of band: %v", item, err)
	}
	find := finder(t, c, cx)
	if ids := find(); len(ids) != 0 {
		t.Fatalf("after the out-of-band delete: resources %v, want none", ids)
	}

	// 3. A manager recreates it and dies before the new ID reaches the API server.
	var cc *crashClient
	m1 := e.StartManager(t, opts(func(mgr ctrl.Manager, d controller.Deps) error {
		cc = newCrashClient(mgr.GetClient(), "obj")
		return c.setup(cc)(mgr, d)
	}))
	// Wake the object up (its poll interval is an hour).
	poke(t, e, o)
	waitCrash(t, cc, 60*time.Second, func() string {
		_ = e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(o), o)
		return condString(o) + "\n" + testenv.Summary(accountJournal(t, e, acct.AccountID))
	})
	m1.Stop(t)
	ids := find()
	if len(ids) != 1 {
		t.Fatalf("after the crash: resources %v, want exactly the recreated one", ids)
	}
	if err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	if got := o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; got != old {
		t.Fatalf("the crash should have kept the old ID %s, but the external-id annotation is %q", old, got)
	}
	if _, ok := reconcile.PendingCreate(o); !ok {
		t.Fatalf("no create-pending record of the recreate: %v", o.GetAnnotations())
	}

	// 4. The object is deleted while no manager runs; a fresh manager finalizes it.
	if err := e.Client.Delete(testenv.Context(t, 10*time.Second), o); err != nil {
		t.Fatal(err)
	}
	e.StartManager(t, opts(plain))
	testenv.Eventually(t, 90*time.Second, func() (bool, string) {
		err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(o), o)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		return false, fmt.Sprintf("%v %s", err, condString(o))
	})
	if left := find(); len(left) != 0 {
		t.Errorf("LEAK: the object (deletionPolicy Delete) is gone but Cloudflare still has %v (the recreate of %s)", left, old)
	}
	if n := c.creates(cx, accountJournal(t, e, acct.AccountID)); n != 2 {
		t.Errorf("%d creates, want 2 (the first and the recreate)", n)
	}
}

// poke changes an annotation of o, which triggers a reconcile.
func poke(t *testing.T, e *testenv.Env, o reconcile.ManagedObject) {
	t.Helper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"test.flare.dev/poke":%q}}}`, time.Now().Format(time.RFC3339Nano))
	if err := e.Client.Patch(testenv.Context(t, 10*time.Second), o, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("poke: %v", err)
	}
}

// restart starts a fresh manager for c (without the crash point).
func (cr *crashRun) restart(t *testing.T, c crashCase) {
	cr.e.StartManager(t, testenv.ManagerOptions{Tagger: cr.tagger, Namespaces: []string{cr.cx.ns, "kube-system"},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, d controller.Deps) error {
			return c.setup(mgr.GetClient())(mgr, d)
		}}})
}

func runCrash(t *testing.T, c crashCase) {
	cr := crashFirst(t, c)
	e, cx, o, ids := cr.e, cr.cx, cr.o, cr.ids
	restart := len(accountJournal(t, e, cx.acct.AccountID))
	cr.restart(t, c)
	waitReadySynced(t, e, o, 60*time.Second)
	j := accountJournal(t, e, cx.acct.AccountID)
	if n := c.creates(cx, j); n != 1 {
		t.Errorf("%d creates, want 1:\n%s", n, testenv.Summary(j))
	}
	if ids2 := cr.find(); len(ids2) != 1 || ids2[0] != ids[0] {
		t.Errorf("after the restart: resources %v, want exactly %v", ids2, ids)
	}
	a := o.GetAnnotations()
	if a[commonv1alpha1.AnnotationExternalID] != ids[0] || o.GetResourceStatus().ID != ids[0] {
		t.Errorf("external-id annotation %q, status.id %q, want %q", a[commonv1alpha1.AnnotationExternalID], o.GetResourceStatus().ID, ids[0])
	}
	if want := string(o.GetUID()) + "/" + ids[0]; a[reconcile.AnnotationOwnershipProof] != want {
		t.Errorf("ownership proof %q, want %q", a[reconcile.AnnotationOwnershipProof], want)
	}
	if v, ok := a[reconcile.AnnotationCreatePending]; ok {
		t.Errorf("create-pending record %q left behind", v)
	}
	if c.afterRestart != nil {
		c.afterRestart(t, cx, o, j[restart:])
	}
}

func taggerFor(tagging bool) reconcile.Tagger {
	if tagging {
		return reconcile.ResourceTagger{}
	}
	return reconcile.NoopTagger{}
}

func tagSuffix(tagging bool) string {
	if tagging {
		return "tagged"
	}
	return "untagged"
}

func genericCrashCase(kind string, tagging bool) crashCase {
	return crashCase{
		name:   kind + "/" + tagSuffix(tagging),
		kind:   kind,
		tagger: taggerFor(tagging),
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				en := entryByKind(kind)
				r := kinds.NewReconciler(en, c, d)
				r.PollInterval = time.Hour
				return r.SetupWithManager(mgr, kinds.Name(en))
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			en := entry(t, kind)
			needsGenericProfile(t, en)
			fp := forProvider(t, en, cx.name)
			field := en.NameField
			if field == "" {
				field = en.IDField
			}
			v, _ := fp[field].(string)
			cx.vars["name"] = v
			return newGeneric(t, en, cx.ns, "obj", fp, "Delete")
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			en := entryByKind(kind)
			field := en.NameField
			if field == "" {
				field = en.IDField
			}
			return cx.path(en.ListPath), field, en.IDField, cx.vars["name"]
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			en := entryByKind(kind)
			p := cx.path(en.CreatePath)
			return len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Method == http.MethodPost && e.Path == p }))
		},
		afterRestart: func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, since []fake.JournalEntry) {
			en := entryByKind(kind)
			if !tagging || en.TagResourceType == "" {
				return
			}
			o, _, err := reconcile.ResourceTagger{}.Owner(testenv.Context(t, 10*time.Second), cx.cf, cx.acct.AccountID,
				reconcile.TagTarget{Type: en.TagResourceType, ID: obj.GetResourceStatus().ID})
			if want := reconcile.OwnerValue("testenv", cx.ns, "obj"); err != nil || o != want {
				t.Errorf("owner tag %q (err %v), want %q", o, err, want)
			}
		},
	}
}

func tunnelCrashCase(tagging bool) crashCase {
	return crashCase{
		name:   "Tunnel/" + tagSuffix(tagging),
		tagger: taggerFor(tagging),
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				return (&tunnel.Reconciler{Client: c, Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
					Recorder: mgr.GetEventRecorder(tunnel.Name), APIReader: mgr.GetAPIReader(), ResyncInterval: time.Hour}).SetupWithManager(mgr)
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			return &tunnelsv1alpha1.Tunnel{Spec: tunnelsv1alpha1.TunnelSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider:  tunnelsv1alpha1.TunnelParameters{Name: cx.name},
				Connector:    tunnelsv1alpha1.ConnectorSpec{Replicas: ptr.To[int32](0)},
			}}
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			return cx.path("/accounts/{account_id}/cfd_tunnel"), "name", "id", cx.name
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			p := cx.path("/accounts/{account_id}/cfd_tunnel")
			return len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Method == http.MethodPost && e.Path == p }))
		},
	}
}

func vpcCrashCase() crashCase {
	return crashCase{
		name: "VPCService",
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				return (&vpcservice.Reconciler{Client: c, Accounts: d.Accounts, Recorder: mgr.GetEventRecorder(vpcservice.Name),
					APIReader: mgr.GetAPIReader(), ResyncInterval: time.Hour}).SetupWithManager(mgr)
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			// A tunnel made directly in Cloudflare (not a Tunnel object) carries the service.
			resp, err := cx.cf.Do(testenv.Context(t, 10*time.Second), cfclient.Request{Method: http.MethodPost,
				Path: cx.path("/accounts/{account_id}/cfd_tunnel"), Body: map[string]string{"name": cx.name + "-tun", "config_src": "cloudflare"}})
			if err != nil {
				t.Fatal(err)
			}
			var tun struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(resp.Result, &tun); err != nil {
				t.Fatal(err)
			}
			return &workersvpcv1alpha1.VPCService{Spec: workersvpcv1alpha1.VPCServiceSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider: &workersvpcv1alpha1.VPCServiceParameters{Name: cx.name, Type: "tcp", TCPPort: ptr.To[int32](5432),
					Host: workersvpcv1alpha1.VPCServiceHost{IPv4: ptr.To("10.0.0.5"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: ptr.To(tun.ID)}}},
			}}
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			return cx.path("/accounts/{account_id}/connectivity/directory/services"), "name", "service_id", cx.name
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			p := cx.path("/accounts/{account_id}/connectivity/directory/services")
			return len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Method == http.MethodPost && e.Path == p }))
		},
	}
}

// workerCrashCase: failTag makes the owner-tag write fail before the crash, so the restarted
// manager finds a script without an owner tag.
func workerCrashCase(tagging, failTag bool) crashCase {
	name := "WorkerScript/" + tagSuffix(tagging)
	if failTag {
		name += "-tag-write-failed"
	}
	c := crashCase{
		name:   name,
		tagger: taggerFor(tagging),
		setup: func(c client.Client) func(ctrl.Manager, controller.Deps) error {
			return func(mgr ctrl.Manager, d controller.Deps) error {
				return (&workerscript.Reconciler{Client: c, Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
					Recorder: mgr.GetEventRecorder(workerscript.Name), APIReader: mgr.GetAPIReader(), ResyncInterval: time.Hour,
					DependencyRetry: 500 * time.Millisecond, Artifacts: d.Artifacts}).SetupWithManager(mgr)
			}
		},
		object: func(t *testing.T, cx *crashCtx) reconcile.ManagedObject {
			return &workersv1alpha1.WorkerScript{Spec: workersv1alpha1.WorkerScriptSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
				ForProvider: &workersv1alpha1.WorkerScriptParameters{
					ScriptName: cx.name, MainModule: "index.js", CompatibilityDate: "2026-09-01",
					Modules: map[string]workersv1alpha1.WorkerModule{"index.js": {Type: "esm",
						Content: "export default { async fetch() { return new Response(\"ok\"); } };\n"}},
				},
			}}
		},
		resource: func(cx *crashCtx) (string, string, string, string) {
			return cx.path("/accounts/{account_id}/workers/scripts"), "id", "id", cx.name
		},
		creates: func(cx *crashCtx, j []fake.JournalEntry) int {
			p := cx.path("/accounts/{account_id}/workers/scripts/") + cx.name
			return len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Method == http.MethodPut && e.Path == p }))
		},
		afterRestart: func(t *testing.T, cx *crashCtx, obj reconcile.ManagedObject, since []fake.JournalEntry) {
			// The interrupted upload is not repeated (the create-pending record carried its
			// hashes), and nothing else of the script is rewritten.
			p := cx.path("/accounts/{account_id}/workers/scripts/") + cx.name
			for _, e := range testenv.Writes(since) {
				if strings.HasPrefix(e.Path, p) {
					t.Errorf("script write after the restart: %s %s", e.Method, e.Path)
				}
			}
			ws := obj.(*workersv1alpha1.WorkerScript)
			if ws.Status.ContentHash == "" || ws.Status.AtProvider.VersionID == "" {
				t.Errorf("status after adoption: contentHash %q, versionID %q", ws.Status.ContentHash, ws.Status.AtProvider.VersionID)
			}
			if !tagging {
				return
			}
			o, _, err := reconcile.ResourceTagger{}.Owner(testenv.Context(t, 10*time.Second), cx.cf, cx.acct.AccountID,
				reconcile.TagTarget{Type: workerscript.TagResourceType, ID: ws.Status.AtProvider.Tag})
			if want := reconcile.OwnerValue("testenv", cx.ns, "obj"); err != nil || o != want {
				t.Errorf("owner tag %q (err %v), want %q", o, err, want)
			}
		},
	}
	if failTag {
		// No retries, so one fault fails the single tag write of the first manager.
		c.rateLimit = &cloudflarev1alpha1.RateLimitSpec{RequestsPerFiveMinutes: 300000, Burst: 1000, MaxRetries: ptr.To[int32](0)}
		c.faults = func(cx *crashCtx) []fake.Fault {
			return []fake.Fault{{Method: http.MethodPut, PathRegex: "^" + cx.path("/accounts/{account_id}/tags") + "$", Status: 500, Code: 1000,
				Message: "injected tag write failure", Times: 1}}
		}
	}
	return c
}

func entryByKind(kind string) descriptors.Entry {
	for _, e := range descriptors.Entries() {
		if e.Kind == kind {
			return e
		}
	}
	panic(fmt.Sprintf("no generated kind %s", kind))
}
