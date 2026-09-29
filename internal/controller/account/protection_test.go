package account_test

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

func kvUsing(ns, name, account string) *kvv1alpha1.KVNamespace {
	return &kvv1alpha1.KVNamespace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: kvv1alpha1.KVNamespaceSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: account}},
			ForProvider:  kvv1alpha1.KVNamespaceParameters{Title: name},
		},
	}
}

func TestAccountUsageProtection(t *testing.T) {
	e := testenv.Require(t, env)
	m := e.StartManager(t, testenv.ManagerOptions{AccountDependencyRequeue: 300 * time.Millisecond})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 2*time.Minute)

	a := e.CreateReadyAccount(t, ns, "acct")
	other := e.CreateReadyAccount(t, ns, "unused")
	get := func(name string) (*cloudflarev1alpha1.CloudflareAccount, error) {
		var acct cloudflarev1alpha1.CloudflareAccount
		err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &acct)
		return &acct, err
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		acct, err := get("acct")
		if err != nil {
			return false, err.Error()
		}
		return controllerutil.ContainsFinalizer(acct, cloudflarev1alpha1.AccountInUseFinalizer), "waiting for the in-use finalizer"
	})

	// A managed object resolves the account through the shared helper, which labels it.
	kv := kvUsing(ns, "data", "acct")
	// It has a Cloudflare resource (pinned ID), so it may keep using a deleting account.
	kv.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "kv-data-id"}
	if err := e.Client.Create(ctx, kv); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deps.Accounts.Resolve(ctx, kv); err != nil {
		t.Fatal(err)
	}
	var stored kvv1alpha1.KVNamespace
	if err := e.Client.Get(ctx, client.ObjectKeyFromObject(kv), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[reconcile.AccountLabel] != "acct" {
		t.Fatalf("labels %v", stored.Labels)
	}
	// Same label in another namespace does not count.
	ns2 := e.Namespace(t)
	elsewhere := kvUsing(ns2, "elsewhere", "unused")
	elsewhere.Labels = map[string]string{reconcile.AccountLabel: "unused"}
	if err := e.Client.Create(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}

	// An account nothing uses is deleted at once.
	if err := e.Client.Delete(ctx, other.CloudflareAccount); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		_, err := get("unused")
		return apierrors.IsNotFound(err), "waiting for the unused account to go away"
	})

	// A used account is kept, stays Ready (so users can clean up) and says why.
	if err := e.Client.Delete(ctx, a.CloudflareAccount); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		acct, err := get("acct")
		if err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		if s == nil || s.Reason != commonv1alpha1.ReasonDependency || s.Status != metav1.ConditionFalse || s.ObservedGeneration != acct.Generation {
			return false, "waiting for Synced=False/DependencyNotReady, have " + condString(s)
		}
		if !strings.Contains(s.Message, "KVNamespace.kv.cloudflare.flare.dev/data") {
			t.Errorf("message %q", s.Message)
		}
		if !reconcile.AccountReady(acct) {
			t.Errorf("a deleting account must stay Ready for its users: %+v", acct.Status.Conditions)
		}
		return true, ""
	})
	// Users still resolve it while it waits.
	if _, err := m.Deps.Accounts.Resolve(ctx, kv); err != nil {
		t.Fatalf("resolve during account deletion: %v", err)
	}
	// A new object (no Cloudflare resource yet) must not start using a deleting account, and
	// it does not hold the account up.
	late := kvUsing(ns, "late", "acct")
	if err := e.Client.Create(ctx, late); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deps.Accounts.Resolve(ctx, late); !reconcile.IsAccountNotReady(err) || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("new object resolved a deleting account: %v", err)
	}
	// Blocked steady state: requeues re-list users but neither re-verify with Cloudflare nor
	// write status.
	before, err := get("acct")
	if err != nil {
		t.Fatal(err)
	}
	verifies := testenv.Count(testenv.ForAccount(e.Journal(t), a.AccountID), "GET", "/tokens/verify")
	time.Sleep(time.Second) // several requeues
	after, err := get("acct")
	if err != nil {
		t.Fatalf("account deleted while in use: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("blocked deletion wrote the account: rv %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if n := testenv.Count(testenv.ForAccount(e.Journal(t), a.AccountID), "GET", "/tokens/verify"); n != verifies {
		t.Errorf("blocked deletion re-verified the token %d times in 1s", n-verifies)
	}

	// The last user goes away → the account goes away.
	if err := e.Client.Delete(ctx, kv); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		_, err := get("acct")
		return apierrors.IsNotFound(err), "waiting for the account to go away"
	})
}

func TestBaseURLOverrideDisabled(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{BaseURLPolicy: &reconcile.BaseURLPolicy{}})
	ns := e.Namespace(t)
	e.CreateAccount(t, ns, "redirected", testenv.AccountOptions{})
	acct := e.WaitAccountCondition(t, ns, "redirected", metav1.ConditionFalse, cloudflarev1alpha1.ReasonBaseURLNotAllowed)
	if testenv.Count(testenv.ForAccount(e.Journal(t), acct.Spec.AccountID), "GET", "/tokens/verify") != 0 {
		t.Error("the token was sent to a disallowed base URL")
	}
}

func condString(c *metav1.Condition) string {
	if c == nil {
		return "none"
	}
	return string(c.Status) + "/" + c.Reason + ": " + c.Message
}
