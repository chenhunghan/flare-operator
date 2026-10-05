package account_test

//lint:file-ignore SA1019 these tests assert the deprecated v1alpha1 status fields are still dual-written until v1beta1 (docs/api-versioning.md)

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/account"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

var env *testenv.Env

// The KV kind stands in for any managed kind in the usage-protection tests (no KV controller
// runs here).
func TestMain(m *testing.M) {
	testenv.Main(m, &env, testenv.Options{AddToScheme: []func(*runtime.Scheme) error{kvv1alpha1.AddToScheme}})
}

func TestAccountReady(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)

	a := e.CreateReadyAccount(t, ns, "good")
	st := a.Status
	if st.TokenStatus != "active" || st.TokenType != cloudflarev1alpha1.TokenTypeAccount || st.TokenID == "" {
		t.Errorf("token status %+v", st)
	}
	if st.ObservedGeneration != a.Generation || st.LastVerifiedTime == nil {
		t.Errorf("observedGeneration/lastVerified %+v", st)
	}
	// status.id and status.atProvider follow the convention of every kind.
	if st.ID != a.AccountID || st.AtProvider.ID != st.TokenID || st.AtProvider.Status != "active" {
		t.Errorf("status.id %q, atProvider %+v (account %s, token %s)", st.ID, st.AtProvider, a.AccountID, st.TokenID)
	}
	if c := meta.FindStatusCondition(st.Conditions, commonv1alpha1.ConditionSynced); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Synced %+v", c)
	}
	// The verify call went to the account-scoped endpoint.
	if n := testenv.Count(testenv.ForAccount(e.Journal(t), a.AccountID), "GET", "/tokens/verify"); n == 0 {
		t.Error("no /accounts/{id}/tokens/verify call journaled")
	}
}

func TestAccountNotReady(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)

	cases := []struct {
		name   string
		opts   testenv.AccountOptions
		reason string
	}{
		{"bad-token", testenv.AccountOptions{NoRegister: true}, cloudflarev1alpha1.ReasonTokenInvalid},
		{"wrong-account", testenv.AccountOptions{FakeToken: fake.Token{AccountID: testenv.RandomAccountID()}}, cloudflarev1alpha1.ReasonAccountMismatch},
		{"missing-secret", testenv.AccountOptions{NoSecret: true}, cloudflarev1alpha1.ReasonSecretNotFound},
		{"disabled", testenv.AccountOptions{FakeToken: fake.Token{Status: "disabled"}}, cloudflarev1alpha1.ReasonTokenDisabled},
		{"expired", testenv.AccountOptions{FakeToken: fake.Token{Status: "expired"}}, cloudflarev1alpha1.ReasonTokenExpired},
		{"user-token-other-account", testenv.AccountOptions{FakeToken: fake.Token{Kind: "user", AccountID: testenv.RandomAccountID()}}, cloudflarev1alpha1.ReasonAccountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.CreateAccount(t, ns, tc.name, tc.opts)
			acct := e.WaitAccountCondition(t, ns, tc.name, metav1.ConditionFalse, tc.reason)
			if tc.reason != cloudflarev1alpha1.ReasonTokenDisabled && tc.reason != cloudflarev1alpha1.ReasonTokenExpired && acct.Status.TokenStatus != "" {
				t.Errorf("token status should be cleared, got %q", acct.Status.TokenStatus)
			}
			if acct.Status.ID != "" {
				t.Errorf("status.id %q on a not-Ready account", acct.Status.ID)
			}
			if (tc.reason == cloudflarev1alpha1.ReasonTokenDisabled || tc.reason == cloudflarev1alpha1.ReasonTokenExpired) != (acct.Status.AtProvider.Status != "") {
				t.Errorf("atProvider %+v for reason %s", acct.Status.AtProvider, tc.reason)
			}
		})
	}
}

func TestUserTokenAndExpiry(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)

	exp := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	e.CreateAccount(t, ns, "user", testenv.AccountOptions{FakeToken: fake.Token{Kind: "user", ExpiresOn: &exp}})
	acct := e.WaitAccountCondition(t, ns, "user", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	if acct.Status.TokenType != cloudflarev1alpha1.TokenTypeUser {
		t.Errorf("token type %q", acct.Status.TokenType)
	}
	if acct.Status.TokenExpiresOn == nil || !acct.Status.TokenExpiresOn.Time.Equal(exp) {
		t.Errorf("expiry %v want %v", acct.Status.TokenExpiresOn, exp)
	}
	if acct.Status.AtProvider.ExpiresOn == nil || !acct.Status.AtProvider.ExpiresOn.Time.Equal(exp) {
		t.Errorf("atProvider.expires_on %v want %v", acct.Status.AtProvider.ExpiresOn, exp)
	}
}

func TestSecretWatch(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, time.Minute)

	// Missing Secret → SecretNotFound; creating it makes the account Ready without touching it.
	a := e.CreateAccount(t, ns, "late", testenv.AccountOptions{NoSecret: true})
	e.WaitAccountCondition(t, ns, "late", metav1.ConditionFalse, cloudflarev1alpha1.ReasonSecretNotFound)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "late-token"},
		Data:       map[string][]byte{"other": []byte(a.Token)},
	}
	if err := e.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	e.WaitAccountCondition(t, ns, "late", metav1.ConditionFalse, cloudflarev1alpha1.ReasonSecretKeyMissing)
	setSecretData(t, e, sec, map[string][]byte{"token": []byte(a.Token)})
	e.WaitAccountCondition(t, ns, "late", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)

	// Rotating the Secret to an unknown token flips it to TokenInvalid.
	setSecretData(t, e, sec, map[string][]byte{"token": []byte("rotated-but-unknown")})
	e.WaitAccountCondition(t, ns, "late", metav1.ConditionFalse, cloudflarev1alpha1.ReasonTokenInvalid)
}

// setSecretData replaces sec's data with a patch (the operator adds its finalizer to token
// Secrets, so sec's resourceVersion may be stale).
func setSecretData(t *testing.T, e *testenv.Env, sec *corev1.Secret, data map[string][]byte) {
	t.Helper()
	base := sec.DeepCopy()
	sec.Data = data
	if err := e.Client.Patch(testenv.Context(t, 10*time.Second), sec, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

func TestSpecChangeReverifies(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, time.Minute)

	a := e.CreateReadyAccount(t, ns, "moving")
	var acct cloudflarev1alpha1.CloudflareAccount
	if err := e.Client.Get(ctx, client.ObjectKeyFromObject(a.CloudflareAccount), &acct); err != nil {
		t.Fatal(err)
	}
	// accountID is immutable (its dependents' resources live in that account).
	moved := acct.DeepCopy()
	moved.Spec.AccountID = testenv.RandomAccountID()
	if err := e.Client.Update(ctx, moved); !apierrors.IsInvalid(err) {
		t.Fatalf("accountID change: %v, want Invalid", err)
	}
	// Another token key is re-verified at once.
	acct.Spec.TokenSecretRef.Key = "missing"
	if err := e.Client.Update(ctx, &acct); err != nil {
		t.Fatal(err)
	}
	e.WaitAccountCondition(t, ns, "moving", metav1.ConditionFalse, cloudflarev1alpha1.ReasonSecretKeyMissing)
}

// TestTransientErrorKeepsReady: a transient verify failure (503) reports Synced=False but keeps
// Ready=True, so a blip does not flip every managed object to AccountNotReady.
//
// The test drives the Reconciler directly, with a controllable clock, instead of a manager and
// a short VerifyInterval: that version raced its periodic re-verify against the manager's
// cache and failed under load. When the cache had not caught up with the first verify's
// Ready=True yet, a due re-verify ran on the stale object, found no Ready to keep, and wrote
// Ready=False (Unavailable). Step 2 reproduces that stale read with an intercepting client:
// the controller must wait for the cache instead of verifying.
func TestTransientErrorKeepsReady(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, time.Minute)
	a := e.CreateAccount(t, ns, "blip", testenv.AccountOptions{RateLimit: &cloudflarev1alpha1.RateLimitSpec{
		RequestsPerFiveMinutes: 300000, Burst: 1000, MaxRetries: ptr.To[int32](0)}}) // one GET per verify
	key := client.ObjectKeyFromObject(a.CloudflareAccount)

	direct, err := client.NewWithWatch(e.Config, client.Options{Scheme: e.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	var stale *cloudflarev1alpha1.CloudflareAccount // non-nil: a Get of the account returns this copy
	c := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if acct, ok := obj.(*cloudflarev1alpha1.CloudflareAccount); ok && stale != nil && k == key {
				stale.DeepCopyInto(acct)
				return nil
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	now := time.Now()
	// Shorter than the controller's cacheSettle (5 s): a status the cache does not show that
	// soon after the verify that wrote it is taken for cache lag, not for a lost write.
	const interval = 2 * time.Second
	r := &account.Reconciler{Client: c, VerifyInterval: interval, Now: func() time.Time { return now },
		Accounts: reconcile.NewAccounts(c, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}))}
	req := ctrl.Request{NamespacedName: key}
	live := func() *cloudflarev1alpha1.CloudflareAccount {
		t.Helper()
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, key, &acct); err != nil {
			t.Fatal(err)
		}
		return &acct
	}
	cond := func(acct *cloudflarev1alpha1.CloudflareAccount, typ string) string {
		c := meta.FindStatusCondition(acct.Status.Conditions, typ)
		if c == nil {
			return "<none>"
		}
		return string(c.Status) + "/" + c.Reason
	}
	readyTrue := "True/" + commonv1alpha1.ReasonAvailable

	// 1. The first verify succeeds: Ready=True, the next verify is due after interval.
	res, err := r.Reconcile(ctx, req)
	if err != nil || res.RequeueAfter != interval {
		t.Fatalf("first reconcile: %+v, %v", res, err)
	}
	if got := cond(live(), commonv1alpha1.ConditionReady); got != readyTrue {
		t.Fatalf("Ready after the first verify: %s", got)
	}

	if err := e.Control.InjectFault(ctx, fake.Fault{Method: "GET", PathRegex: "^/accounts/" + a.AccountID + "/tokens/verify$",
		Status: 503, Code: 10000, Message: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Control.ClearFaults(testenv.Context(t, 10*time.Second)) })
	now = now.Add(interval + time.Second) // the re-verify is due, 3 s after the first

	// 2. A cache that does not show the first verify's status write yet (the object as it was
	// between the finalizer patch and the status patch): wait for it, although a verify is due.
	stale = live()
	stale.Status = cloudflarev1alpha1.CloudflareAccountStatus{}
	before := verifies(t, e, a.AccountID)
	res, err = r.Reconcile(ctx, req)
	if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter >= interval {
		t.Errorf("reconcile on a stale cache: %+v, %v; want a short requeue and no error", res, err)
	}
	if n := verifies(t, e, a.AccountID); n != before {
		t.Errorf("%d verifies on a stale cache, want none", n-before)
	}
	if got := cond(live(), commonv1alpha1.ConditionReady); got != readyTrue {
		t.Errorf("Ready after a reconcile on a stale cache: %s", got)
	}

	// 3. The cache caught up: the due verify gets the 503, reports Synced=False and keeps Ready.
	stale = nil
	if _, err = r.Reconcile(ctx, req); err == nil {
		t.Error("a verify that got a 503 returned no error (no retry)")
	}
	if n := verifies(t, e, a.AccountID); n != before+1 {
		t.Errorf("%d verifies once the cache caught up, want 1", n-before)
	}
	got := live()
	if s := cond(got, commonv1alpha1.ConditionSynced); s != "False/"+commonv1alpha1.ReasonReconcileError {
		t.Errorf("Synced after a 503: %s, want False/%s", s, commonv1alpha1.ReasonReconcileError)
	}
	if rd := cond(got, commonv1alpha1.ConditionReady); rd != readyTrue {
		t.Errorf("a transient 503 must not clear Ready: %s", rd)
	}
}

// A reconciler with no record of the account's last status write (a restarted manager)
// starts from a copy older than that write, as a lagging cache serves it, while a verify fails
// transiently. The status it builds from that copy has no Ready to keep and would put
// Ready=False over the stored Ready=True; the status write is preconditioned on the copy's
// resourceVersion (reconcile.PatchStatus), so it conflicts instead, quietly (a short requeue,
// no error), and the stored status is left alone.
func TestStaleCopyStatusWriteConflicts(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, time.Minute)
	a := e.CreateAccount(t, ns, "restart", testenv.AccountOptions{RateLimit: &cloudflarev1alpha1.RateLimitSpec{
		RequestsPerFiveMinutes: 300000, Burst: 1000, MaxRetries: ptr.To[int32](0)}}) // one GET per verify
	key := client.ObjectKeyFromObject(a.CloudflareAccount)
	direct, err := client.NewWithWatch(e.Config, client.Options{Scheme: e.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	var (
		snap  *cloudflarev1alpha1.CloudflareAccount // the account as the finalizer patch left it
		stale *cloudflarev1alpha1.CloudflareAccount // non-nil: a Get of the account returns this copy
	)
	c := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if acct, ok := obj.(*cloudflarev1alpha1.CloudflareAccount); ok && stale != nil && k == key {
				stale.DeepCopyInto(acct)
				return nil
			}
			return c.Get(ctx, k, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			err := c.Patch(ctx, obj, patch, opts...)
			if acct, ok := obj.(*cloudflarev1alpha1.CloudflareAccount); ok && err == nil {
				snap = acct.DeepCopy()
			}
			return err
		},
	})
	newReconciler := func() *account.Reconciler {
		return &account.Reconciler{Client: c,
			Accounts: reconcile.NewAccounts(c, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}))}
	}
	req := ctrl.Request{NamespacedName: key}
	live := func() *cloudflarev1alpha1.CloudflareAccount {
		t.Helper()
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, key, &acct); err != nil {
			t.Fatal(err)
		}
		return &acct
	}
	ready := func(acct *cloudflarev1alpha1.CloudflareAccount) string {
		c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		if c == nil {
			return "<none>"
		}
		return string(c.Status) + "/" + c.Reason
	}
	readyTrue := "True/" + commonv1alpha1.ReasonAvailable

	// One reconcile adds the finalizer (snap) and writes Ready=True.
	if _, err := newReconciler().Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	stored := live()
	if got := ready(stored); got != readyTrue {
		t.Fatalf("Ready after the first verify: %s", got)
	}
	if snap == nil || snap.ResourceVersion == stored.ResourceVersion || len(snap.Status.Conditions) != 0 {
		t.Fatalf("no copy from before the status write: %+v", snap)
	}
	if err := e.Control.InjectFault(ctx, fake.Fault{Method: "GET", PathRegex: "^/accounts/" + a.AccountID + "/tokens/verify$",
		Status: 503, Code: 10000, Message: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Control.ClearFaults(testenv.Context(t, 10*time.Second)) })

	stale = snap
	before := verifies(t, e, a.AccountID)
	res, err := newReconciler().Reconcile(ctx, req) // a fresh reconciler: no memory of the write
	if err != nil || res.RequeueAfter != reconcile.StatusConflictRetry {
		t.Errorf("reconcile from a stale copy: %+v, %v; want a quiet requeue after %s", res, err, reconcile.StatusConflictRetry)
	}
	if n := verifies(t, e, a.AccountID); n != before+1 {
		t.Fatalf("%d verifies from the stale copy, want 1 (the test must exercise the status write)", n-before)
	}
	got := live()
	if r := ready(got); r != readyTrue {
		t.Errorf("a status built from a stale copy replaced Ready=True: %s", r)
	}
	if got.ResourceVersion != stored.ResourceVersion {
		t.Errorf("the stale reconcile wrote the account (resourceVersion %s → %s)", stored.ResourceVersion, got.ResourceVersion)
	}
}
