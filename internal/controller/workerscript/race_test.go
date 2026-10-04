package workerscript_test

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// TestConcurrentCreateSameName: two objects with the same script_name reconciled at once by
// several workers. The upload is a PUT upsert, so both seeing "no script" and both uploading
// would replace the first object's live code and give both an ownership record. Creates of one
// name are serialized and existence is re-checked: one upload, one owner, one NameConflict.
func TestConcurrentCreateSameName(t *testing.T) {
	h := startWith(t, testenv.ManagerOptions{MaxConcurrentReconciles: 4})
	name := "racey-" + testenv.RandomHex(4)
	// Hold back the answers to both objects' first existence check, so both reconciles are
	// past it before either uploads (a real GET takes hundreds of milliseconds anyway).
	if err := h.e.Control.InjectFault(h.ctx(), fake.Fault{Method: http.MethodGet,
		PathRegex: "^/accounts/" + h.acct.AccountID + "/workers/scripts/" + name + "/settings$",
		Times:     2, FaultShape: fake.FaultShape{DelayMs: 800, Passthrough: true}}); err != nil {
		t.Fatal(err)
	}
	m := h.mark()
	fp := func(code string) *workersv1alpha1.WorkerScriptParameters {
		p := params(code)
		p.ScriptName = name
		return p
	}
	a := h.newScript("a", fp(`export default { fetch() { return new Response("a") } }`), nil)
	b := h.newScript("b", fp(`export default { fetch() { return new Response("b") } }`), nil)
	conflicted := func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Ready", metav1.ConditionFalse, reconcile.ReasonNameConflict)
	}
	testenv.Eventually(t, 30e9, func() (bool, string) {
		if err := h.get(a); err != nil {
			return false, err.Error()
		}
		if err := h.get(b); err != nil {
			return false, err.Error()
		}
		return (ready(a) && conflicted(b)) || (ready(b) && conflicted(a)),
			"a: " + condOf(a.Status.Conditions, "Ready") + "; b: " + condOf(b.Status.Conditions, "Ready")
	})
	winner, loser := a, b
	if ready(b) {
		winner, loser = b, a
	}
	if n := h.uploads(h.since(m), name); n != 1 {
		t.Errorf("%d uploads of %s, want 1 (the loser overwrote the winner's code):\n%s", n, name, testenv.Summary(h.since(m)))
	}
	if !reconcile.HasOwnershipProof(winner, name) {
		t.Errorf("winner %s has no ownership record", winner.Name)
	}
	if reconcile.HasOwnershipProof(loser, name) {
		t.Errorf("loser %s recorded ownership of %s", loser.Name, name)
	}
}
