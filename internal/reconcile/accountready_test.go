package reconcile_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// TestAccountReadyAcrossDeletionBump: the API server bumps metadata.generation once when it
// sets deletionTimestamp. A Ready condition observed just before that bump still counts for a
// deleting account (users keep resolving it until the controller re-stamps the condition); any
// other generation mismatch does not.
func TestAccountReadyAcrossDeletionBump(t *testing.T) {
	now := metav1.Now()
	for _, tc := range []struct {
		name          string
		gen, observed int64
		status        metav1.ConditionStatus
		deleting      bool
		want          bool
	}{
		{"current", 3, 3, metav1.ConditionTrue, false, true},
		{"stale spec", 3, 2, metav1.ConditionTrue, false, false},
		{"deleting, re-stamped", 3, 3, metav1.ConditionTrue, true, true},
		{"deleting, observed before the deletion bump", 3, 2, metav1.ConditionTrue, true, true},
		{"deleting, spec changed too", 4, 2, metav1.ConditionTrue, true, false},
		{"deleting, not Ready", 3, 2, metav1.ConditionFalse, true, false},
		{"deleting, never observed", 1, 0, metav1.ConditionTrue, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &cloudflarev1alpha1.CloudflareAccount{}
			acct.Generation = tc.gen
			if tc.deleting {
				acct.DeletionTimestamp = &now
			}
			acct.Status.Conditions = []metav1.Condition{{Type: commonv1alpha1.ConditionReady, Status: tc.status, ObservedGeneration: tc.observed}}
			if got := reconcile.AccountReady(acct); got != tc.want {
				t.Fatalf("AccountReady = %v, want %v", got, tc.want)
			}
		})
	}
	if reconcile.AccountReady(&cloudflarev1alpha1.CloudflareAccount{}) {
		t.Fatal("an account without a Ready condition is Ready")
	}
}
