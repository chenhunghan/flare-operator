// Package kindsuite is the per-kind conformance suite of the kinds cmd/flaregen generates. For
// each descriptors.Entry it runs the generic reconciler against envtest and the in-process
// flarefake and checks, from the flarefake request journal:
//
//  1. create from a minimal valid forProvider (synthesized from the CRD schema's required
//     fields, or testdata/<Kind>.json) → exactly one create, then Ready and Synced with
//     atProvider covering forProvider;
//  2. idempotency: re-reconciles of an in-sync object make zero Cloudflare writes;
//  3. a change of one mutable field (an UpdateField) → exactly one write, UpdateMethod on the
//     item (skipped when the kind has no update operation);
//  4. a change of an Immutable field → rejected by the API server when a CEL rule covers it,
//     and, applied past the rule, Synced=False/Immutable and zero writes;
//  5. external delete → recreated (same ID when the client chooses it, else a new one);
//  6. an Observe-only object adopting the resource → Ready, zero writes, and its deletion
//     leaves the resource;
//  7. deletion: deletionPolicy Delete → one bodiless DELETE, then 404; Orphan → the resource
//     stays;
//  8. no request violates the pinned spec (known spec defects excepted).
//
// A kind runs against whatever flarefake serves for it: its hand-written profile, or the
// generic profile (internal/fake/generic.go) when the fake has it in Options.Generic. See
// docs/generator-scaleout.md for adding a kind.
package kindsuite

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/generic/kinds"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// Options configures Run.
type Options struct {
	// Poll is the drift-detection interval of the reconcilers under test (default 300ms).
	Poll time.Duration
	// FixturesDir holds optional <Kind>.json fixtures (default: this package's testdata).
	FixturesDir string
	// Skip returns why a kind is not run ("" runs it).
	Skip func(descriptors.Entry) string
}

// Fixture replaces what the suite would synthesize for a kind. The string "{name}" anywhere
// in it is replaced by a unique resource name. Update and Immutable are complete forProvider
// objects (the create one with one mutable, resp. immutable, field changed); "null" disables
// that step.
type Fixture struct {
	Create    json.RawMessage `json:"create"`
	Update    json.RawMessage `json:"update,omitempty"`
	Immutable json.RawMessage `json:"immutable,omitempty"`
}

// KnownSpecDefects are pinned-spec violations the real API accepts (see
// internal/generic/descriptors/emulator_test.go): method ("" = any) and a substring of the
// violation.
var KnownSpecDefects = []struct{ Method, Contains, Why string }{
	{http.MethodPost, "primary_location_hint", "spec enum is lower-case; the API wants upper-case (0019)"},
	{"", `parameter "database_id" in path has an error: input matches more than one oneOf schemas`, "0020"},
	{http.MethodDelete, "request body has an error: value is required but missing", "KV DELETE without a body (0013)"},
}

// IsGeneric reports whether a kind is one of the generator.yaml `emulate: generic` kinds.
func IsGeneric(e descriptors.Entry) bool {
	for _, k := range fake.GeneratedGenericKinds() {
		if k.Group == e.Group && k.Kind == e.Kind {
			return true
		}
	}
	return false
}

// Run starts one manager with a generic reconciler per entry and runs every kind as a
// parallel subtest in its own namespace and account.
func Run(t *testing.T, env *testenv.Env, entries []descriptors.Entry, o Options) {
	e := testenv.Require(t, env)
	if o.Poll == 0 {
		o.Poll = 300 * time.Millisecond
	}
	if o.FixturesDir == "" {
		o.FixturesDir = filepath.Join(testenv.RepoRoot(), "internal", "generic", "kindsuite", "testdata")
	}
	e.StartManager(t, testenv.ManagerOptions{Setup: []func(ctrl.Manager, controller.Deps) error{
		func(m ctrl.Manager, d controller.Deps) error {
			for _, en := range entries {
				r := kinds.NewReconciler(en, m.GetClient(), d)
				r.PollInterval = o.Poll
				if err := r.SetupWithManager(m, kinds.Name(en)); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	for _, en := range entries {
		t.Run(en.Kind, func(t *testing.T) {
			t.Parallel()
			if o.Skip != nil {
				if why := o.Skip(en); why != "" {
					t.Skip(why)
				}
			}
			switch {
			case en.Singleton:
				t.Skip("singleton kinds are not covered by the suite yet")
			case en.Scope == "zone":
				t.Skip("zone-scoped kinds need a zone (not covered yet)")
			}
			newKindTest(t, e, en, o).run()
		})
	}
}

type kindTest struct {
	t    *testing.T
	e    *testenv.Env
	en   descriptors.Entry
	o    Options
	ns   string
	acct *testenv.Account
	cf   cfclient.Client

	schema   *Schema
	fixture  *Fixture
	clientID bool // the create body carries the ID (it survives an external delete)
}

func newKindTest(t *testing.T, e *testenv.Env, en descriptors.Entry, o Options) *kindTest {
	k := &kindTest{t: t, e: e, en: en, o: o}
	s, err := LoadSchema(testenv.RepoRoot(), en)
	if err != nil {
		t.Fatal(err)
	}
	k.schema = s
	if raw, err := os.ReadFile(filepath.Join(o.FixturesDir, en.Kind+".json")); err == nil {
		k.fixture = &Fixture{}
		if err := json.Unmarshal(raw, k.fixture); err != nil {
			t.Fatalf("fixture %s.json: %v", en.Kind, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	k.clientID = has(en.CreateFields, en.IDField) || (en.NameField != "" && en.NameField == en.IDField)
	k.ns = e.Namespace(t)
	k.acct = e.CreateReadyAccount(t, k.ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: k.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	k.cf = cf
	return k
}

// ---- the steps -------------------------------------------------------------------------------

func (k *kindTest) run() {
	t, en := k.t, k.en
	name := "flare-spike-" + testenv.RandomHex(4)
	fp := k.createFP(name)
	t.Logf("forProvider: %s", mustJSON(fp))

	// 1. Create.
	obj := k.newObj("obj", map[string]any{"deletionPolicy": "Delete", "forProvider": fp})
	mark := k.mark()
	k.create(obj)
	k.waitSynced(obj, "created")
	id := obj.GetResourceStatus().ID
	if id == "" || obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != id {
		t.Fatalf("status.id %q, external-id annotation %q", id, obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID])
	}
	if n := testenv.Count(k.writesSince(mark), http.MethodPost, k.path(en.CreatePath, "")); n != 1 {
		t.Errorf("%d creates, want 1:\n%s", n, testenv.Summary(k.writesSince(mark)))
	}
	item := k.path(en.ItemPath, id)

	// 2. Idempotent.
	k.assertNoWrites("in-sync object", 3, item)

	// 3. Update one mutable field.
	cur := fp
	if upd, what, ok := k.updateFP(fp, name); ok {
		t.Logf("update %s: %s", what, mustJSON(upd))
		mark = k.mark()
		k.setForProvider(obj, upd)
		k.waitSynced(obj, "updated")
		w := k.writesSince(mark)
		if len(w) != 1 || w[0].Method != en.UpdateMethod || w[0].Path != item {
			t.Fatalf("update of %s: want exactly one %s %s, got:\n%s", what, en.UpdateMethod, item, testenv.Summary(w))
		}
		k.assertNoWrites("after update", 2, item)
		cur = upd
	} else {
		t.Logf("no update step: %s", what)
	}

	// 4. Immutable change.
	if imm, what, ok := k.immutableFP(cur, name); ok {
		t.Logf("immutable change %s: %s", what, mustJSON(imm))
		k.setImmutable(obj, imm)
		k.waitFor(obj, "Synced=False/Immutable", func() (bool, string) {
			return condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, commonv1alpha1.ReasonImmutable), "not Immutable"
		})
		k.assertNoWrites("immutable change", 2, item)
		k.setImmutable(obj, cur)
		k.waitSynced(obj, "immutable change reverted")
	} else {
		t.Logf("no immutable step: %s", what)
	}

	// 5. External delete → recreate.
	mark = k.mark()
	k.mustAPI(http.MethodDelete, item)
	k.waitFor(obj, "recreated", func() (bool, string) {
		if testenv.Count(k.writesSince(mark), http.MethodPost, k.path(en.CreatePath, "")) == 0 {
			return false, "no create yet"
		}
		nid := obj.GetResourceStatus().ID
		switch {
		case nid == "" || obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != nid:
			return false, "id " + nid
		case k.clientID && nid != id:
			return false, fmt.Sprintf("client-chosen id changed %s → %s", id, nid)
		case !k.clientID && nid == id:
			return false, "same id " + nid
		}
		return true, ""
	})
	k.waitSynced(obj, "recreated")
	id = obj.GetResourceStatus().ID
	item = k.path(en.ItemPath, id)
	// The status still shows the pre-delete sync until the recreating reconcile ends, and that
	// reconcile may write more after the create (e.g. R2Bucket's CORS policy): let it finish.
	k.quiesce(mark, item)
	k.assertNoWrites("after recreate", 2, item)

	// 6. Observe-only.
	observer := k.newObj("observer", map[string]any{"managementPolicies": []string{"Observe"}, "forProvider": map[string]any{}})
	observer.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
	mark = k.mark()
	k.create(observer)
	k.waitFor(observer, "observer Ready and Synced", func() (bool, string) {
		return condIs(observer, commonv1alpha1.ConditionReady, metav1.ConditionTrue, "") &&
			condIs(observer, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") &&
			observer.GetResourceStatus().ID == id, "conditions"
	})
	k.assertNoWrites("observe-only object", 2, item)
	k.delete(observer)
	k.waitGone(observer)
	if w := k.writesSince(mark); len(w) != 0 {
		t.Errorf("observe-only object: %d writes, want 0:\n%s", len(w), testenv.Summary(w))
	}
	if _, err := k.api(http.MethodGet, item); err != nil {
		t.Errorf("after deleting the observer: GET %s: %v", item, err)
	}

	// 7a. deletionPolicy Delete.
	mark = k.mark()
	k.delete(obj)
	k.waitGone(obj)
	if n := testenv.Count(k.writesSince(mark), http.MethodDelete, item); n != 1 {
		t.Errorf("deletionPolicy Delete: %d DELETEs of %s, want 1:\n%s", n, item, testenv.Summary(k.writesSince(mark)))
	}
	if _, err := k.api(http.MethodGet, item); !cfclient.IsNotFound(err) {
		t.Errorf("GET after delete: %v, want 404", err)
	}

	// 7b. deletionPolicy Orphan.
	name2 := "flare-spike-" + testenv.RandomHex(4)
	orphan := k.newObj("orphan", map[string]any{"deletionPolicy": "Orphan", "forProvider": k.createFP(name2)})
	k.create(orphan)
	k.waitSynced(orphan, "orphan created")
	item2 := k.path(en.ItemPath, orphan.GetResourceStatus().ID)
	mark = k.mark()
	k.delete(orphan)
	k.waitGone(orphan)
	if n := testenv.Count(k.writesSince(mark), http.MethodDelete, item2); n != 0 {
		t.Errorf("deletionPolicy Orphan: %d DELETEs of %s", n, item2)
	}
	if _, err := k.api(http.MethodGet, item2); err != nil {
		t.Errorf("orphaned resource: GET %s: %v", item2, err)
	} else {
		k.mustAPI(http.MethodDelete, item2)
	}

	// 9. Untaggable kinds: a same-named resource made outside the operator is not adopted.
	if en.TagResourceType == "" && (en.NameField != "" || k.clientID) {
		k.foreignNamesake()
	}

	// 8. Spec conformance of every request.
	for _, j := range testenv.ForAccount(k.e.Journal(t), k.acct.AccountID) {
		if j.SchemaViolation != "" && !knownDefect(j) {
			t.Errorf("request violates the pinned spec: %s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}

// foreignNamesake: for a kind that cannot carry an owner tag, a resource another tool made
// with the object's name (or client-chosen ID) proves nothing about ownership. The object
// (deletionPolicy Delete) reports NameConflict, writes nothing, pins nothing, and its deletion
// leaves the resource. Pinned through the external-id annotation, it is adopted.
func (k *kindTest) foreignNamesake() {
	t, en := k.t, k.en
	name := "flare-spike-" + testenv.RandomHex(4)
	fp := k.createFP(name)
	body := map[string]any{}
	for _, f := range en.CreateFields {
		if v, ok := fp[f]; ok {
			body[f] = v
		}
	}
	resp, err := k.cf.Do(testenv.Context(t, 30*time.Second), cfclient.Request{Method: http.MethodPost, Path: k.path(en.CreatePath, ""), Body: body})
	if err != nil {
		t.Fatalf("create the foreign resource: %v", err)
	}
	var created map[string]any
	if err := json.Unmarshal(resp.Result, &created); err != nil {
		t.Fatalf("create the foreign resource: %v", err)
	}
	id, _ := created[en.IDField].(string)
	if id == "" {
		t.Fatalf("create the foreign resource: no %s in %s", en.IDField, resp.Result)
	}
	item := k.path(en.ItemPath, id)

	mark := k.mark()
	obj := k.newObj("namesake", map[string]any{"deletionPolicy": "Delete", "forProvider": fp})
	k.create(obj)
	k.waitFor(obj, "NameConflict", func() (bool, string) {
		return condIs(obj, commonv1alpha1.ConditionReady, metav1.ConditionFalse, reconcile.ReasonNameConflict) &&
			condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reconcile.ReasonNameConflict), "no NameConflict"
	})
	if a, s := obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID], obj.GetResourceStatus().ID; a != "" || s != "" {
		t.Errorf("namesake pinned the foreign resource: annotation %q, status.id %q", a, s)
	}
	k.delete(obj)
	k.waitGone(obj)
	if w := k.writesSince(mark); len(w) != 0 {
		t.Errorf("namesake of a foreign resource: %d Cloudflare writes, want 0:\n%s", len(w), testenv.Summary(w))
	}
	if _, err := k.api(http.MethodGet, item); err != nil {
		t.Fatalf("the foreign resource after deleting its namesake (deletionPolicy Delete): GET %s: %v", item, err)
	}

	// Explicit adoption through the annotation: the user's pin is the proof.
	pinned := k.newObj("pinned", map[string]any{"deletionPolicy": "Delete", "forProvider": fp})
	pinned.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
	k.create(pinned)
	k.waitSynced(pinned, "pinned")
	if got := pinned.GetResourceStatus().ID; got != id {
		t.Errorf("pinned: status.id %q, want %q", got, id)
	}
	mark = k.mark()
	k.delete(pinned)
	k.waitGone(pinned)
	if n := testenv.Count(k.writesSince(mark), http.MethodDelete, item); n != 1 {
		t.Errorf("pinned, deletionPolicy Delete: %d DELETEs of %s, want 1:\n%s", n, item, testenv.Summary(k.writesSince(mark)))
	}
}

// createFP is the fixture's create forProvider, else a synthesized one.
func (k *kindTest) createFP(name string) map[string]any {
	if k.fixture != nil && len(k.fixture.Create) > 0 {
		return k.fixtureFP(k.fixture.Create, name)
	}
	fp, err := k.schema.Synthesize(name)
	if err != nil {
		k.t.Fatalf("synthesize forProvider: %v (add testdata/%s.json)", err, k.en.Kind)
	}
	return fp
}

func (k *kindTest) updateFP(fp map[string]any, name string) (map[string]any, string, bool) {
	if k.fixture != nil && len(k.fixture.Update) > 0 {
		if string(k.fixture.Update) == "null" {
			return nil, "disabled by the fixture", false
		}
		return k.fixtureFP(k.fixture.Update, name), "fixture", true
	}
	if k.en.UpdateMethod == "" || len(k.en.UpdateFields) == 0 {
		return nil, "the kind has no update operation", false
	}
	out, what, ok := k.schema.Mutate(fp, k.en.UpdateFields, func(f string) bool {
		return has(k.en.WriteOnly, f) || has(k.en.Immutable, f) || f == k.en.IDField
	})
	if !ok {
		return nil, "no UpdateField can be changed", false
	}
	return out, what, true
}

func (k *kindTest) immutableFP(fp map[string]any, name string) (map[string]any, string, bool) {
	if k.fixture != nil && len(k.fixture.Immutable) > 0 {
		if string(k.fixture.Immutable) == "null" {
			return nil, "disabled by the fixture", false
		}
		return k.fixtureFP(k.fixture.Immutable, name), "fixture", true
	}
	if len(k.en.Immutable) == 0 {
		return nil, "the kind has no immutable field", false
	}
	out, what, ok := k.schema.Mutate(fp, k.en.Immutable, nil)
	if !ok {
		return nil, "no Immutable field can be changed", false
	}
	return out, what, true
}

func (k *kindTest) fixtureFP(raw json.RawMessage, name string) map[string]any {
	out := map[string]any{}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(raw), "{name}", name)), &out); err != nil {
		k.t.Fatalf("fixture %s.json: %v", k.en.Kind, err)
	}
	return out
}

// ---- helpers ---------------------------------------------------------------------------------

func (k *kindTest) path(p, id string) string {
	return strings.NewReplacer("{account_id}", k.acct.AccountID, "{id}", id).Replace(p)
}

func (k *kindTest) newObj(name string, spec map[string]any) reconcile.ManagedObject {
	k.t.Helper()
	obj := k.en.New().(reconcile.ManagedObject)
	spec["accountRef"] = map[string]any{"name": "acct"}
	if err := json.Unmarshal([]byte(`{"spec":`+mustJSON(spec)+`}`), obj); err != nil {
		k.t.Fatalf("spec: %v", err)
	}
	obj.SetNamespace(k.ns)
	obj.SetName(name)
	return obj
}

func (k *kindTest) create(obj reconcile.ManagedObject) {
	k.t.Helper()
	if err := k.e.Client.Create(testenv.Context(k.t, 30*time.Second), obj); err != nil {
		k.t.Fatalf("create %s: %v", obj.GetName(), err)
	}
}

func (k *kindTest) get(obj reconcile.ManagedObject) error {
	return k.e.Client.Get(testenv.Context(k.t, 30*time.Second), client.ObjectKeyFromObject(obj), obj)
}

func (k *kindTest) delete(obj reconcile.ManagedObject) {
	k.t.Helper()
	if err := k.e.Client.Delete(testenv.Context(k.t, 30*time.Second), obj); err != nil {
		k.t.Fatalf("delete %s: %v", obj.GetName(), err)
	}
}

func (k *kindTest) waitGone(obj reconcile.ManagedObject) {
	k.t.Helper()
	testenv.Eventually(k.t, 30*time.Second, func() (bool, string) {
		err := k.get(obj)
		return apierrors.IsNotFound(err), fmt.Sprintf("%s still there (err %v, finalizers %v)\n%s", obj.GetName(), err, obj.GetFinalizers(), conditions(obj))
	})
}

// setForProvider replaces spec.forProvider (retrying on conflicts with the controller).
func (k *kindTest) setForProvider(obj reconcile.ManagedObject, fp map[string]any) {
	k.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k.get(obj); err != nil {
			return err
		}
		v := reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("ForProvider")
		v.Set(reflect.Zero(v.Type()))
		if err := json.Unmarshal([]byte(mustJSON(fp)), v.Addr().Interface()); err != nil {
			return err
		}
		return k.e.Client.Update(testenv.Context(k.t, 30*time.Second), obj)
	})
	if err != nil {
		k.t.Fatalf("update forProvider: %v", err)
	}
}

// setImmutable applies a forProvider that changes Immutable fields. The API server rejects an
// in-place change of a top-level immutable field once status.id is set (the CRD's CEL
// transition rules), so the change is made in two updates the rules allow, each only removing or
// adding fields: managementPolicies [Observe] with an empty forProvider (no create-required
// rule applies), then fp with the original policies. The controller's own immutable check
// (Synced=False, reason Immutable) then sees the change.
func (k *kindTest) setImmutable(obj reconcile.ManagedObject, fp map[string]any) {
	k.t.Helper()
	if err := k.tryForProvider(obj, fp, nil); err == nil {
		k.t.Logf("immutable change accepted by the API server (no CEL rule covers it)")
		return
	} else if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "immutable") {
		k.t.Fatalf("immutable change: want the API server's immutable error, got %v", err)
	}
	pols := append([]commonv1alpha1.ManagementAction(nil), obj.GetResourceSpec().ManagementPolicies...)
	if err := k.tryForProvider(obj, map[string]any{}, []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}); err != nil {
		k.t.Fatalf("immutable change, step 1 (Observe, empty forProvider): %v", err)
	}
	if pols == nil {
		pols = []commonv1alpha1.ManagementAction{}
	}
	if err := k.tryForProvider(obj, fp, pols); err != nil {
		k.t.Fatalf("immutable change, step 2 (forProvider, original policies): %v", err)
	}
}

// tryForProvider replaces forProvider (and managementPolicies when pols is not nil; an empty
// pols clears them) and returns the update error.
func (k *kindTest) tryForProvider(obj reconcile.ManagedObject, fp map[string]any, pols []commonv1alpha1.ManagementAction) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k.get(obj); err != nil {
			return err
		}
		v := reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("ForProvider")
		v.Set(reflect.Zero(v.Type()))
		if err := json.Unmarshal([]byte(mustJSON(fp)), v.Addr().Interface()); err != nil {
			return err
		}
		if pols != nil {
			obj.GetResourceSpec().ManagementPolicies = nil
			if len(pols) > 0 {
				obj.GetResourceSpec().ManagementPolicies = pols
			}
		}
		return k.e.Client.Update(testenv.Context(k.t, 30*time.Second), obj)
	})
}

func (k *kindTest) waitFor(obj reconcile.ManagedObject, what string, cond func() (bool, string)) {
	k.t.Helper()
	testenv.Eventually(k.t, 30*time.Second, func() (bool, string) {
		if err := k.get(obj); err != nil {
			return false, err.Error()
		}
		ok, msg := cond()
		return ok, what + ": " + msg + "\n" + conditions(obj)
	})
}

// waitSynced waits for Ready and Synced of the current generation with atProvider covering
// forProvider's read-back fields.
func (k *kindTest) waitSynced(obj reconcile.ManagedObject, what string) {
	k.t.Helper()
	k.waitFor(obj, what+": Ready and Synced", func() (bool, string) {
		if !condIs(obj, commonv1alpha1.ConditionReady, metav1.ConditionTrue, "") ||
			!condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") ||
			obj.GetResourceStatus().ObservedGeneration != obj.GetGeneration() {
			return false, "conditions"
		}
		desired, err := generic.ForProvider(obj)
		if err != nil {
			return false, err.Error()
		}
		for _, p := range k.en.WriteOnly {
			deletePath(desired, p)
		}
		// Fields read back under another name (Extension.ObservedAs, e.g. R2's storage_class).
		for f, o := range k.en.Extension.ObservedAs {
			if v, ok := desired[f]; ok {
				delete(desired, f)
				desired[o] = v
			}
		}
		ap := atProvider(obj)
		if !generic.Covers(desired, ap) {
			return false, fmt.Sprintf("atProvider %s does not cover forProvider %s", mustJSON(ap), mustJSON(desired))
		}
		return true, ""
	})
}

func (k *kindTest) mark() int { return len(k.e.Journal(k.t)) }

func (k *kindTest) writesSince(n int) []fake.JournalEntry {
	return testenv.Writes(testenv.ForAccount(k.e.Journal(k.t)[n:], k.acct.AccountID))
}

// quiesce waits until the journal since the mark since shows two GETs of the item after its
// last write of this account: the reconcile that wrote has ended and a later one has started,
// so a following assertNoWrites sees steady state only.
func (k *kindTest) quiesce(since int, item string) {
	k.t.Helper()
	k.e.WaitJournal(k.t, since, 2*time.Minute, func(j []fake.JournalEntry) (bool, string) {
		j = testenv.ForAccount(j, k.acct.AccountID)
		last := -1
		for i, e := range j {
			if e.Method != http.MethodGet {
				last = i
			}
		}
		gets := testenv.CountPath(j[last+1:], http.MethodGet, item)
		return gets >= 2, fmt.Sprintf("%d GETs of %s after the last write, waiting for 2", gets, item)
	})
}

// assertNoWrites waits until the item was re-observed (one GET per reconcile of the poll loop)
// more than reconciles times, so that at least reconciles complete reconciles ran, and fails on
// any Cloudflare write of this kind's account meanwhile. It waits for those reconciles rather
// than for a fixed time, which proves nothing on a slow machine (and failed there when no
// reconcile fit into the window).
func (k *kindTest) assertNoWrites(what string, reconciles int, item string) {
	k.t.Helper()
	n := k.mark()
	j := k.e.WaitJournal(k.t, n, 2*time.Minute, func(j []fake.JournalEntry) (bool, string) {
		got := testenv.CountPath(testenv.ForAccount(j, k.acct.AccountID), http.MethodGet, item)
		return got > reconciles, fmt.Sprintf("%s: %d GETs of %s, waiting for %d: the object is not re-observed", what, got, item, reconciles+1)
	})
	if w := testenv.Writes(testenv.ForAccount(j, k.acct.AccountID)); len(w) != 0 {
		k.t.Errorf("%s: %d Cloudflare writes, want 0:\n%s", what, len(w), testenv.Summary(w))
	}
}

func (k *kindTest) api(method, p string) (json.RawMessage, error) {
	resp, err := k.cf.Do(testenv.Context(k.t, 30*time.Second), cfclient.Request{Method: method, Path: p})
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func (k *kindTest) mustAPI(method, p string) {
	k.t.Helper()
	if _, err := k.api(method, p); err != nil {
		k.t.Fatalf("%s %s: %v", method, p, err)
	}
}

func knownDefect(j fake.JournalEntry) bool {
	for _, d := range KnownSpecDefects {
		if (d.Method == "" || d.Method == j.Method) && strings.Contains(j.SchemaViolation, d.Contains) {
			return true
		}
	}
	return false
}

func condIs(obj reconcile.ManagedObject, typ string, st metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(obj.GetResourceStatus().Conditions, typ)
	return c != nil && c.Status == st && (reason == "" || c.Reason == reason) && c.ObservedGeneration == obj.GetGeneration()
}

func conditions(obj reconcile.ManagedObject) string {
	var b strings.Builder
	for _, c := range obj.GetResourceStatus().Conditions {
		fmt.Fprintf(&b, "%s=%s %s %q (gen %d/%d); ", c.Type, c.Status, c.Reason, c.Message, c.ObservedGeneration, obj.GetGeneration())
	}
	return b.String()
}

func atProvider(obj any) map[string]any {
	var m struct {
		Status struct {
			AtProvider map[string]any `json:"atProvider"`
		} `json:"status"`
	}
	_ = json.Unmarshal([]byte(mustJSON(obj)), &m)
	return m.Status.AtProvider
}

func deletePath(m map[string]any, p string) {
	segs := strings.Split(p, ".")
	for _, s := range segs[:len(segs)-1] {
		next, ok := m[s].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, segs[len(segs)-1])
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
