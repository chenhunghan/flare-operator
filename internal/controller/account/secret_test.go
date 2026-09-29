package account_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller/account"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// cleanupFinalizer stands in for a managed controller's finalizer: the object stays until the
// test says its Cloudflare-side delete is done.
const cleanupFinalizer = "test.flare.dev/cloudflare-cleanup"

func secretFinalized(t *testing.T, e *testenv.Env, ns, name string) (*corev1.Secret, bool, error) {
	t.Helper()
	var s corev1.Secret
	err := e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKey{Namespace: ns, Name: name}, &s)
	if err != nil {
		return nil, false, err
	}
	return &s, controllerutil.ContainsFinalizer(&s, cloudflarev1alpha1.AccountTokenFinalizer), nil
}

func waitSecretFinalizer(t *testing.T, e *testenv.Env, ns, name string, want bool) {
	t.Helper()
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		_, has, err := secretFinalized(t, e, ns, name)
		if err != nil {
			return false, err.Error()
		}
		return has == want, "waiting for Secret " + name + " finalizer present=" + map[bool]string{true: "true", false: "false"}[want]
	})
}

func gone(t *testing.T, e *testenv.Env, obj client.Object) (bool, string) {
	err := e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return true, ""
	}
	if err != nil {
		return false, err.Error()
	}
	return false, "waiting for " + obj.GetName() + " to go away (finalizers " + strings.Join(obj.GetFinalizers(), ",") + ")"
}

// A namespace deletion deletes the token Secret with everything else, but the Secret must
// outlive the account's users so they can still delete their Cloudflare resources.
func TestNamespaceDeletionKeepsTokenForCleanup(t *testing.T) {
	e := testenv.Require(t, env)
	m := e.StartManager(t, testenv.ManagerOptions{AccountDependencyRequeue: 300 * time.Millisecond})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 2*time.Minute)

	a := e.CreateReadyAccount(t, ns, "acct")
	secretName := a.Spec.TokenSecretRef.Name
	waitSecretFinalizer(t, e, ns, secretName, true)

	kv := kvUsing(ns, "data", "acct")
	kv.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "kv-data-id"}
	kv.Finalizers = []string{cleanupFinalizer}
	if err := e.Client.Create(ctx, kv); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deps.Accounts.Resolve(ctx, kv); err != nil {
		t.Fatal(err)
	}

	// Delete the namespace, then do what the namespace controller does (envtest runs none):
	// delete every object in it at once.
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := e.Client.Delete(ctx, nsObj); err != nil {
		t.Fatal(err)
	}
	beforeDeletes := m.Mark()
	for _, o := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}},
		&cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "acct"}},
		&kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "data"}},
	} {
		if err := e.Client.Delete(ctx, o); err != nil {
			t.Fatalf("delete %s: %v", o.GetName(), err)
		}
	}

	// The dependent is still cleaning up: the Secret stays (Terminating) and usable, the
	// account stays Ready and blocked.
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "acct"}, &acct); err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		return s != nil && s.Reason == commonv1alpha1.ReasonDependency && reconcile.AccountReady(&acct),
			"waiting for a Ready, blocked account, have Synced " + condString(s)
	})
	// Several blocked requeues (every 300 ms), and the token controller has handled the
	// Secret's deletion, before checking that the Secret is still held.
	m.WaitReconciled(t, account.Name, client.ObjectKey{Namespace: ns, Name: "acct"}, m.Mark(), 2, time.Minute)
	m.WaitReconciled(t, account.TokenControllerName, client.ObjectKey{Namespace: ns, Name: secretName}, beforeDeletes, 1, time.Minute)
	s, has, err := secretFinalized(t, e, ns, secretName)
	if err != nil {
		t.Fatalf("token Secret removed while a user still cleans up: %v", err)
	}
	if !has || s.DeletionTimestamp.IsZero() {
		t.Fatalf("Secret should be Terminating and held: finalizers %v deletion %v", s.Finalizers, s.DeletionTimestamp)
	}
	// Even an operator that lost its in-memory client cache (a restart) can still clean up: a
	// fresh Accounts reads the token from the Secret and the token works.
	fresh := reconcile.NewAccounts(e.Client, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}))
	var stored kvv1alpha1.KVNamespace
	if err := e.Client.Get(ctx, client.ObjectKeyFromObject(kv), &stored); err != nil {
		t.Fatal(err)
	}
	res, err := fresh.Resolve(ctx, &stored)
	if err != nil {
		t.Fatalf("resolve after a simulated restart: %v", err)
	}
	if _, err := res.Client.Do(ctx, cfclient.Request{Method: "GET", Path: "/accounts/" + res.AccountID + "/tokens/verify"}); err != nil {
		t.Fatalf("token unusable during cleanup: %v", err)
	}

	// The dependent finishes its Cloudflare-side delete → account, then Secret go away.
	stored.Finalizers = nil
	if err := e.Client.Update(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{
		&kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "data"}},
		&cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "acct"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}},
	} {
		testenv.Eventually(t, 30*time.Second, func() (bool, string) { return gone(t, e, o) })
	}
	// The namespace itself: its content is gone, so the namespace controller would finalize it.
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	live, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	live.Spec.Finalizers = nil
	if _, err := cs.CoreV1().Namespaces().Finalize(ctx, live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) { return gone(t, e, nsObj) })
}

// The finalizer follows spec.tokenSecretRef and is kept while any unreleased account uses the
// Secret.
func TestTokenSecretFinalizerFollowsReferences(t *testing.T) {
	e := testenv.Require(t, env)
	m := e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 3*time.Minute)

	a := e.CreateReadyAccount(t, ns, "one")
	waitSecretFinalizer(t, e, ns, "one-token", true)

	// A second account shares the Secret.
	e.CreateAccount(t, ns, "two", testenv.AccountOptions{AccountID: a.AccountID, Token: a.Token, SecretName: "one-token", NoSecret: true, NoRegister: true})
	e.WaitAccountCondition(t, ns, "two", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)

	// "one" moves to a new Secret; "one-token" stays held by "two".
	moved := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "moved"}, Data: map[string][]byte{"token": []byte(a.Token)}}
	if err := e.Client.Create(ctx, moved); err != nil {
		t.Fatal(err)
	}
	var one cloudflarev1alpha1.CloudflareAccount
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "one"}, &one); err != nil {
		t.Fatal(err)
	}
	one.Spec.TokenSecretRef.Name = "moved"
	mark := m.Mark()
	if err := e.Client.Update(ctx, &one); err != nil {
		t.Fatal(err)
	}
	waitSecretFinalizer(t, e, ns, "moved", true)
	e.WaitAccountCondition(t, ns, "one", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	// The token controller has decided on the old Secret after the move (the account's update
	// event enqueues every finalized Secret of the namespace).
	m.WaitReconciled(t, account.TokenControllerName, client.ObjectKey{Namespace: ns, Name: "one-token"}, mark, 1, time.Minute)
	if _, has, err := secretFinalized(t, e, ns, "one-token"); err != nil || !has {
		t.Fatalf("shared Secret released while still referenced by another account (err %v)", err)
	}

	// "two" is deleted (nothing uses it) → "one-token" is referenced by nobody → released.
	if err := e.Client.Delete(ctx, &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "two"}}); err != nil {
		t.Fatal(err)
	}
	waitSecretFinalizer(t, e, ns, "one-token", false)

	// "one" points somewhere else again → "moved" is released without any deletion.
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "one"}, &one); err != nil {
		t.Fatal(err)
	}
	one.Spec.TokenSecretRef.Name = "one-token"
	if err := e.Client.Update(ctx, &one); err != nil {
		t.Fatal(err)
	}
	waitSecretFinalizer(t, e, ns, "moved", false)
	waitSecretFinalizer(t, e, ns, "one-token", true)

	// A held Secret that is deleted stays until the account is deleted, then goes away.
	mark = m.Mark()
	if err := e.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "one-token"}}); err != nil {
		t.Fatal(err)
	}
	// Both controllers have handled the deletion event (the Secret is watched by both).
	m.WaitReconciled(t, account.TokenControllerName, client.ObjectKey{Namespace: ns, Name: "one-token"}, mark, 1, time.Minute)
	m.WaitReconciled(t, account.Name, client.ObjectKey{Namespace: ns, Name: "one"}, mark, 1, time.Minute)
	if _, _, err := secretFinalized(t, e, ns, "one-token"); err != nil {
		t.Fatalf("held Secret deleted: %v", err)
	}
	if err := e.Client.Delete(ctx, &one); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		return gone(t, e, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "one-token"}})
	})
}

// A deleting account that is not Ready for a reason other than its Secret says why its users
// are stuck; with no users it is released at once regardless.
func TestDeletingNotReadyAccount(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{AccountDependencyRequeue: 300 * time.Millisecond})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 2*time.Minute)

	e.CreateAccount(t, ns, "expired", testenv.AccountOptions{FakeToken: fake.Token{Status: "expired"}})
	e.WaitAccountCondition(t, ns, "expired", metav1.ConditionFalse, cloudflarev1alpha1.ReasonTokenExpired)
	e.CreateAccount(t, ns, "idle", testenv.AccountOptions{FakeToken: fake.Token{Status: "disabled"}})
	e.WaitAccountCondition(t, ns, "idle", metav1.ConditionFalse, cloudflarev1alpha1.ReasonTokenDisabled)

	kv := kvUsing(ns, "data", "expired")
	kv.Labels = map[string]string{reconcile.AccountLabel: "expired"}
	if err := e.Client.Create(ctx, kv); err != nil {
		t.Fatal(err)
	}

	// Unused and not Ready: released at once, and its Secret with it.
	idle := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "idle"}}
	if err := e.Client.Delete(ctx, idle); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) { return gone(t, e, idle) })
	waitSecretFinalizer(t, e, ns, "idle-token", false)

	// Used and not Ready: blocked, and both conditions say why and what to do.
	acct := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "expired"}}
	if err := e.Client.Delete(ctx, acct); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(acct), acct); err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		r := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		if s == nil || s.Reason != commonv1alpha1.ReasonDependency || r == nil || r.ObservedGeneration != acct.Generation {
			return false, "waiting for DependencyNotReady, have " + condString(s)
		}
		for _, c := range []*metav1.Condition{s, r} {
			if !strings.Contains(c.Message, "not usable (TokenExpired)") || !strings.Contains(c.Message, "put a valid, active token") {
				return false, c.Type + " message lacks the hint: " + c.Message
			}
		}
		if !strings.Contains(s.Message, "KVNamespace.kv.cloudflare.flare.dev/data") {
			return false, "Synced message lacks the user: " + s.Message
		}
		return true, ""
	})
	if err := e.Client.Delete(ctx, kv); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) { return gone(t, e, acct) })
	waitSecretFinalizer(t, e, ns, "expired-token", false)
}

func TestDeletingAccountBaseURLNotAllowed(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{BaseURLPolicy: &reconcile.BaseURLPolicy{}, AccountDependencyRequeue: 300 * time.Millisecond})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 2*time.Minute)

	e.CreateAccount(t, ns, "redirected", testenv.AccountOptions{})
	e.WaitAccountCondition(t, ns, "redirected", metav1.ConditionFalse, cloudflarev1alpha1.ReasonBaseURLNotAllowed)
	kv := kvUsing(ns, "data", "redirected")
	kv.Labels = map[string]string{reconcile.AccountLabel: "redirected"}
	if err := e.Client.Create(ctx, kv); err != nil {
		t.Fatal(err)
	}
	acct := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "redirected"}}
	if err := e.Client.Delete(ctx, acct); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(acct), acct); err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		if s == nil || s.Reason != commonv1alpha1.ReasonDependency ||
			!strings.Contains(s.Message, "not usable (BaseURLNotAllowed)") || !strings.Contains(s.Message, "--allow-base-url-override") {
			return false, "waiting for a blocked deletion with the base URL hint, have " + condString(s)
		}
		return true, ""
	})
	if err := e.Client.Delete(ctx, kv); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) { return gone(t, e, acct) })
}
