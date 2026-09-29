package account_test

//lint:file-ignore SA1019 these tests assert the deprecated v1alpha1 status fields are still dual-written until v1beta1 (docs/api-versioning.md)

import (
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/controller/account"
	"flare.dev/operator/internal/testenv"
)

func verifies(t *testing.T, e *testenv.Env, accountID string) int {
	t.Helper()
	return testenv.Count(testenv.ForAccount(e.Journal(t), accountID), "GET", "/tokens/verify")
}

// settleSecret is a sync barrier for the account controller: it touches the Secret (a label
// only, an event that must not re-verify either) and waits until each account was reconciled
// after that. Events of the Secret are handed to the controller in order, so the events before
// the touch were handled by then.
func settleSecret(t *testing.T, e *testenv.Env, m *testenv.Manager, ns, secret string, accounts ...string) {
	t.Helper()
	ctx := testenv.Context(t, 30*time.Second)
	mark := m.Mark()
	patch := fmt.Sprintf(`{"metadata":{"labels":{"test.flare.dev/barrier":%q}}}`, testenv.RandomHex(4))
	if err := e.Client.Patch(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secret}},
		client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("touch Secret %s: %v", secret, err)
	}
	for _, a := range accounts {
		m.WaitReconciled(t, account.Name, client.ObjectKey{Namespace: ns, Name: a}, mark, 1, time.Minute)
	}
}

// The account controller's own finalizer patch of the token Secret (and any other Secret event
// that changes neither the spec nor the token) must not verify the token again.
func TestSecretEventsDoNotReverify(t *testing.T) {
	e := testenv.Require(t, env)
	m := e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 3*time.Minute)

	a := e.CreateReadyAccount(t, ns, "quiet")
	waitSecretFinalizer(t, e, ns, a.Spec.TokenSecretRef.Name, true)
	// The Secret event of the finalizer patch has been handled once the account was reconciled
	// for a later event of the same Secret.
	settleSecret(t, e, m, ns, a.Spec.TokenSecretRef.Name, "quiet")
	if n := verifies(t, e, a.AccountID); n != 1 {
		t.Fatalf("%d token verifications for one new account, want 1", n)
	}

	// A metadata-only change of the Secret: no verify.
	var s corev1.Secret
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: a.Spec.TokenSecretRef.Name}, &s); err != nil {
		t.Fatal(err)
	}
	base := s.DeepCopy()
	s.Labels = map[string]string{"touched": "yes"}
	mark := m.Mark()
	if err := e.Client.Patch(ctx, &s, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	m.WaitReconciled(t, account.Name, client.ObjectKey{Namespace: ns, Name: "quiet"}, mark, 1, time.Minute)
	if n := verifies(t, e, a.AccountID); n != 1 {
		t.Errorf("%d token verifications after a label change of the Secret, want 1", n)
	}

	// A second account on the same Secret verifies once, and does not make the first re-verify.
	e.CreateAccount(t, ns, "twin", testenv.AccountOptions{AccountID: a.AccountID, Token: a.Token, SecretName: a.Spec.TokenSecretRef.Name,
		NoSecret: true, NoRegister: true})
	e.WaitAccountCondition(t, ns, "twin", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	settleSecret(t, e, m, ns, a.Spec.TokenSecretRef.Name, "quiet", "twin")
	if n := verifies(t, e, a.AccountID); n != 2 {
		t.Errorf("%d token verifications for two accounts, want 2", n)
	}

	// A new token in the Secret is verified.
	setSecretData(t, e, &s, map[string][]byte{"token": []byte("rotated-but-unknown")})
	e.WaitAccountCondition(t, ns, "quiet", metav1.ConditionFalse, cloudflarev1alpha1.ReasonTokenInvalid)
}

// A held token Secret that is deleted directly stays Terminating while the account uses it; the
// account's Ready message says so and how to rotate instead.
func TestHeldSecretTerminatingIsReported(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, time.Minute)

	a := e.CreateReadyAccount(t, ns, "held")
	secret := a.Spec.TokenSecretRef.Name
	waitSecretFinalizer(t, e, ns, secret, true)
	before := verifies(t, e, a.AccountID)
	if err := e.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secret}}); err != nil {
		t.Fatal(err)
	}
	var acct cloudflarev1alpha1.CloudflareAccount
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "held"}, &acct); err != nil {
			return false, err.Error()
		}
		c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, "was deleted but is kept (Terminating") &&
			strings.Contains(c.Message, secret) && strings.Contains(c.Message, "update the Secret's data in place"), "Ready " + condString(c)
	})
	if n := verifies(t, e, a.AccountID); n != before {
		t.Errorf("deleting the Secret re-verified the token (%d → %d)", before, n)
	}
	if err := e.Client.Delete(ctx, &acct); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		return gone(t, e, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secret}})
	})
}

// When the token Secret cannot be updated (here an admission policy refuses finalizer
// changes), the account still verifies and becomes Ready, and Synced says why the Secret is not
// protected. Once the Secret can be updated, Synced recovers.
func TestSecretSyncFailureReported(t *testing.T) {
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{})
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 8*time.Minute)

	name := "deny-secret-finalizers-" + ns
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			MatchConstraints: &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
					Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"secrets"}},
				},
			}}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: "object.metadata.?finalizers.orValue([]) == oldObject.metadata.?finalizers.orValue([])",
				Message:    "test policy: Secret finalizers are frozen",
			}},
		},
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}},
		},
	}
	for _, o := range []client.Object{policy, binding} {
		if err := e.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Client.Delete(testenv.Context(t, 10*time.Second), o) })
	}
	// Wait until the policy is enforced: it is loaded asynchronously, which takes long on a
	// loaded machine. A probe Secret gets a new finalizer on every attempt, so each attempt is a
	// finalizer change whatever the previous attempt left (a fixed add-then-remove pair could
	// get its removal refused once enforcement starts in between, and then never change the
	// finalizers again).
	probe := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "probe"}}
	if err := e.Client.Create(ctx, probe); err != nil {
		t.Fatal(err)
	}
	// changeProbe tries a finalizer change of the probe: nil when admitted.
	changeProbe := func(finalizers []string) error {
		var cur corev1.Secret
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(probe), &cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Finalizers = finalizers
		return e.Client.Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	testenv.Eventually(t, 2*time.Minute, func() (bool, string) {
		err := changeProbe([]string{"test.flare.dev/probe-" + testenv.RandomHex(4)})
		if err == nil {
			return false, "policy not enforced yet"
		}
		return strings.Contains(err.Error(), "frozen"), err.Error()
	})

	e.CreateAccount(t, ns, "stuck", testenv.AccountOptions{})
	testenv.Eventually(t, 90*time.Second, func() (bool, string) {
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stuck"}, &acct); err != nil {
			return false, err.Error()
		}
		r := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		ok := r != nil && r.Status == metav1.ConditionTrue && s != nil && s.Status == metav1.ConditionFalse &&
			s.Reason == account.ReasonTokenSecretUpdateFailed && strings.Contains(s.Message, "frozen")
		return ok, "Ready " + condString(r) + ", Synced " + condString(s)
	})

	if err := e.Client.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	// Wait until the policy is no longer enforced (also asynchronous), and drop the probe's
	// finalizer on the way.
	testenv.Eventually(t, 2*time.Minute, func() (bool, string) {
		if err := changeProbe(nil); err != nil {
			return false, "policy still enforced: " + err.Error()
		}
		return true, ""
	})
	// The failed Secret updates put the account on error backoff, which grows to tens of seconds
	// while the policy was enforced; a touch of the Secret (a watched event) retries it now.
	touch := fmt.Sprintf(`{"metadata":{"labels":{"test.flare.dev/touch":%q}}}`, testenv.RandomHex(4))
	if err := e.Client.Patch(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stuck-token"}},
		client.RawPatch(types.MergePatchType, []byte(touch))); err != nil {
		t.Fatal(err)
	}
	waitSecretFinalizer(t, e, ns, "stuck-token", true)
	e.WaitAccountCondition(t, ns, "stuck", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	testenv.Eventually(t, 90*time.Second, func() (bool, string) {
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stuck"}, &acct); err != nil {
			return false, err.Error()
		}
		s := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
		return s != nil && s.Status == metav1.ConditionTrue, "Synced " + condString(s)
	})
}

// A status write that fails after a verify changed Ready must not leave the account waiting for
// the next scheduled verify (VerifyInterval, 10 min here): once the API server accepts status
// writes again, the account becomes Ready. An admission policy refuses the status writes at first.
func TestStatusWriteFailureRetried(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 8*time.Minute)

	name := "deny-account-status-" + ns
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			MatchConstraints: &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
					Rule: admissionregistrationv1.Rule{APIGroups: []string{cloudflarev1alpha1.GroupVersion.Group},
						APIVersions: []string{"*"}, Resources: []string{"cloudflareaccounts/status"}},
				},
			}}},
			Validations: []admissionregistrationv1.Validation{{Expression: "false", Message: "test policy: account status is frozen"}},
		},
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}},
		},
	}
	for _, o := range []client.Object{policy, binding} {
		if err := e.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Client.Delete(testenv.Context(t, 10*time.Second), o) })
	}
	// Wait until the policy is enforced (it is loaded asynchronously, which takes long on a
	// loaded machine), before any controller runs.
	probe := e.CreateAccount(t, ns, "probe", testenv.AccountOptions{NoRegister: true, NoSecret: true})
	patchProbe := func() error {
		var got cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, client.ObjectKeyFromObject(probe.CloudflareAccount), &got); err != nil {
			return err
		}
		base := got.DeepCopy()
		got.Status.TokenStatus = "probe-" + testenv.RandomHex(4)
		return e.Client.Status().Patch(ctx, &got, client.MergeFrom(base))
	}
	testenv.Eventually(t, 2*time.Minute, func() (bool, string) {
		err := patchProbe()
		if err == nil {
			return false, "policy not enforced yet"
		}
		return strings.Contains(err.Error(), "frozen"), err.Error()
	})

	e.StartManager(t, testenv.ManagerOptions{}) // default VerifyInterval (10 min)
	a := e.CreateAccount(t, ns, "unwritten", testenv.AccountOptions{})
	testenv.Eventually(t, 60*time.Second, func() (bool, string) {
		n := verifies(t, e, a.AccountID)
		return n >= 1, fmt.Sprintf("%d verifies", n)
	})
	if err := e.Client.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	// Wait until status writes are admitted again (also asynchronous). The refused writes put
	// the account on error backoff, which grows while the policy lingers; a touch of its token
	// Secret (a watched event that changes neither spec nor token) retries it now. That retry
	// must verify again and write Ready: the failed write must not count as done.
	testenv.Eventually(t, 2*time.Minute, func() (bool, string) {
		if err := patchProbe(); err != nil {
			return false, "policy still enforced: " + err.Error()
		}
		return true, ""
	})
	touch := fmt.Sprintf(`{"metadata":{"labels":{"test.flare.dev/touch":%q}}}`, testenv.RandomHex(4))
	if err := e.Client.Patch(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: a.Spec.TokenSecretRef.Name}},
		client.RawPatch(types.MergePatchType, []byte(touch))); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 90*time.Second, func() (bool, string) {
		var acct cloudflarev1alpha1.CloudflareAccount
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "unwritten"}, &acct); err != nil {
			return false, err.Error()
		}
		r := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		return r != nil && r.Status == metav1.ConditionTrue,
			fmt.Sprintf("Ready %s after %d verifies", condString(r), verifies(t, e, a.AccountID))
	})
}
