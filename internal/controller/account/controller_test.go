package account_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/testenv"
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
	acct.Spec.AccountID = testenv.RandomAccountID()
	if err := e.Client.Update(ctx, &acct); err != nil {
		t.Fatal(err)
	}
	e.WaitAccountCondition(t, ns, "moving", metav1.ConditionFalse, cloudflarev1alpha1.ReasonAccountMismatch)
}

func TestTransientErrorKeepsReady(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{AccountVerifyInterval: 500 * time.Millisecond})
	ns := e.Namespace(t)

	a := e.CreateReadyAccount(t, ns, "blip")
	ctx := testenv.Context(t, time.Minute)
	if err := e.Control.InjectFault(ctx, fake.Fault{Method: "GET", PathRegex: "^/accounts/" + a.AccountID + "/tokens/verify$", Status: 503, Code: 10000, Message: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Control.ClearFaults(testenv.Context(t, 10*time.Second)) })
	var acct cloudflarev1alpha1.CloudflareAccount
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(a.CloudflareAccount), &acct); err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		return s != nil && s.Status == metav1.ConditionFalse && s.Reason == commonv1alpha1.ReasonReconcileError, "waiting for Synced=False"
	})
	if r := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady); r == nil || r.Status != metav1.ConditionTrue {
		t.Errorf("a transient 503 must not clear Ready: %+v", r)
	}
}
