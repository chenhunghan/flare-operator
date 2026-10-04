package reconcile

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// MaxConditionMessage bounds condition messages; they often quote API error text.
const MaxConditionMessage = 1024

// ReasonNameConflict marks a managing object whose Cloudflare name is taken by a resource it
// cannot prove it owns (no ownership tag is possible): it is adopted only through the
// external-id annotation.
const ReasonNameConflict = "NameConflict"

// ReasonDeleteFailed marks an object being deleted whose Cloudflare DELETE the API refused with
// a 4xx other than 404, 408 and 429 (Synced=False; e.g. an R2 bucket that is not empty). The finalizer stays and retries.
const ReasonDeleteFailed = "DeleteFailed"

// SetCondition sets (or updates) a condition on mg, stamped with mg's generation. The
// transition time only changes when the status changes. The message is sanitized (control
// characters removed) and cut to MaxConditionMessage bytes.
func SetCondition(mg commonv1alpha1.Managed, typ string, status metav1.ConditionStatus, reason, msg string) {
	st := mg.GetResourceStatus()
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            cfclient.Sanitize(msg, MaxConditionMessage),
		ObservedGeneration: mg.GetGeneration(),
	})
}

// GetCondition returns the condition of type typ, or nil.
func GetCondition(mg commonv1alpha1.Managed, typ string) *metav1.Condition {
	return meta.FindStatusCondition(mg.GetResourceStatus().Conditions, typ)
}

// SetReady sets the Ready condition.
func SetReady(mg commonv1alpha1.Managed, status metav1.ConditionStatus, reason, msg string) {
	SetCondition(mg, commonv1alpha1.ConditionReady, status, reason, msg)
}

// SetSynced sets the Synced condition. Synced=False also counts one sync failure of mg's kind
// (MetricSyncFailures).
func SetSynced(mg commonv1alpha1.Managed, status metav1.ConditionStatus, reason, msg string) {
	if status == metav1.ConditionFalse {
		countSyncFailure(mg, reason)
	}
	SetCondition(mg, commonv1alpha1.ConditionSynced, status, reason, msg)
}

// MarkAvailable: Ready=True, reason Available.
func MarkAvailable(mg commonv1alpha1.Managed) {
	SetReady(mg, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable, "")
}

// MarkCreating: Ready=False, reason Creating.
func MarkCreating(mg commonv1alpha1.Managed, msg string) {
	SetReady(mg, metav1.ConditionFalse, commonv1alpha1.ReasonCreating, msg)
}

// MarkDeleting: Ready=False, reason Deleting.
func MarkDeleting(mg commonv1alpha1.Managed, msg string) {
	SetReady(mg, metav1.ConditionFalse, commonv1alpha1.ReasonDeleting, msg)
}

// MarkUnavailable: Ready=False, reason Unavailable.
func MarkUnavailable(mg commonv1alpha1.Managed, msg string) {
	SetReady(mg, metav1.ConditionFalse, commonv1alpha1.ReasonUnavailable, msg)
}

// MarkNotFound: Ready=False, reason ExternalNotFound (e.g. an Observe-only object whose
// external resource does not exist).
func MarkNotFound(mg commonv1alpha1.Managed, msg string) {
	SetReady(mg, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
}

// MarkSynced: Synced=True, reason ReconcileSuccess (or ObserveOnly for read-only objects).
func MarkSynced(mg commonv1alpha1.Managed) {
	reason := commonv1alpha1.ReasonReconcileOK
	if PoliciesOf(mg).ObserveOnly() {
		reason = commonv1alpha1.ReasonObserveOnly
	}
	SetSynced(mg, metav1.ConditionTrue, reason, "")
}

// MarkSyncError: Synced=False with reason (ReconcileError when empty; RateLimited for a
// Cloudflare 429, see Throttled) and err's message.
func MarkSyncError(mg commonv1alpha1.Managed, reason string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if wait, ok := Throttled(err); ok && reason == "" {
		reason, msg = ReasonRateLimited, throttleMessage(err, wait)
	}
	if reason == "" {
		reason = commonv1alpha1.ReasonReconcileError
	}
	SetSynced(mg, metav1.ConditionFalse, reason, msg)
}

// MarkImmutable: Synced=False, reason Immutable.
func MarkImmutable(mg commonv1alpha1.Managed, msg string) {
	SetSynced(mg, metav1.ConditionFalse, commonv1alpha1.ReasonImmutable, msg)
}

// MarkAccountNotReady: Ready=False and Synced=False, reason AccountNotReady.
func MarkAccountNotReady(mg commonv1alpha1.Managed, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	SetReady(mg, metav1.ConditionFalse, commonv1alpha1.ReasonAccountNotReady, msg)
	SetSynced(mg, metav1.ConditionFalse, commonv1alpha1.ReasonAccountNotReady, msg)
}

// IsReady reports Ready=True for the current generation.
func IsReady(mg commonv1alpha1.Managed) bool {
	c := GetCondition(mg, commonv1alpha1.ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == mg.GetGeneration()
}

// SetObservedGeneration records that the current generation has been reconciled.
func SetObservedGeneration(mg commonv1alpha1.Managed) {
	mg.GetResourceStatus().ObservedGeneration = mg.GetGeneration()
}
