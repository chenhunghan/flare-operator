package reconcile_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// TestAccountReadyAcrossDeletionBump: the API server bumps metadata.generation once when it
// sets deletionTimestamp. A Ready condition verified before the deletion, at the generation
// just before that bump, still counts for a deleting account (users keep resolving it until
// the controller re-stamps the condition); any other generation mismatch does not. In
// particular, a condition re-stamped during the deletion (verified after deletionTimestamp)
// does not count once a spec change made during the deletion bumps the generation again.
func TestAccountReadyAcrossDeletionBump(t *testing.T) {
	deleted := metav1.NewTime(metav1.Now().Truncate(time.Second))
	before := metav1.NewTime(deleted.Add(-time.Minute))
	same := metav1.NewTime(deleted.Time)
	after := metav1.NewTime(deleted.Add(time.Minute))
	for _, tc := range []struct {
		name          string
		gen, observed int64
		status        metav1.ConditionStatus
		deleting      bool
		verified      *metav1.Time
		want          bool
	}{
		{"current", 3, 3, metav1.ConditionTrue, false, &before, true},
		{"stale spec", 3, 2, metav1.ConditionTrue, false, &before, false},
		{"deleting, re-stamped", 3, 3, metav1.ConditionTrue, true, &after, true},
		{"deleting, observed before the deletion bump", 3, 2, metav1.ConditionTrue, true, &before, true},
		{"deleting, spec changed too", 4, 2, metav1.ConditionTrue, true, &before, false},
		{"deleting, re-stamped, then a spec change", 4, 3, metav1.ConditionTrue, true, &after, false},
		// Stored timestamps have second precision: a verification stamped in the deletion's
		// second came before it (VerifiedAt stamps later ones at least a second after).
		{"deleting, verified in the second of the deletion", 3, 2, metav1.ConditionTrue, true, &same, true},
		{"deleting, never verified", 3, 2, metav1.ConditionTrue, true, nil, false},
		{"deleting, not Ready", 3, 2, metav1.ConditionFalse, true, &before, false},
		{"deleting, never observed", 1, 0, metav1.ConditionTrue, true, &before, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &cloudflarev1alpha1.CloudflareAccount{}
			acct.Generation = tc.gen
			if tc.deleting {
				acct.DeletionTimestamp = &deleted
			}
			acct.Status.LastVerifiedTime = tc.verified
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

// TestVerifiedAt: a verification of a deleting account is stamped strictly after its
// deletionTimestamp (at second precision), whatever the operator's clock says, so a condition
// re-stamped during the deletion never passes for one verified before it; otherwise it is now.
func TestVerifiedAt(t *testing.T) {
	deleted := metav1.NewTime(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	acct := &cloudflarev1alpha1.CloudflareAccount{}
	now := deleted.Add(400 * time.Millisecond)
	if got := reconcile.VerifiedAt(acct, now); !got.Time.Equal(now) {
		t.Errorf("not deleting: %v, want now %v", got, now)
	}
	acct.DeletionTimestamp = &deleted
	for name, now := range map[string]time.Time{
		"same second":       deleted.Add(400 * time.Millisecond),
		"clock behind":      deleted.Add(-3 * time.Second),
		"exactly deletion":  deleted.Time,
		"one second later":  deleted.Add(time.Second + 200*time.Millisecond),
		"a minute later":    deleted.Add(time.Minute),
		"far behind (skew)": deleted.Add(-time.Hour),
	} {
		got := reconcile.VerifiedAt(acct, now)
		if !got.Truncate(time.Second).After(deleted.Time) {
			t.Errorf("%s: %v is not after the deletion %v at second precision", name, got, deleted)
		}
		acct.Status.LastVerifiedTime = &got
		acct.Generation = 5
		acct.Status.Conditions = []metav1.Condition{{Type: commonv1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 4}}
		if reconcile.AccountReady(acct) {
			t.Errorf("%s: a condition verified during the deletion passes for one verified before it", name)
		}
	}
}
