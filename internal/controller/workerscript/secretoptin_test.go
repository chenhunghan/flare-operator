package workerscript_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/workerscript"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// TestSecretBindingOptIn: a secret_text binding reads only a Secret labelled
// cloudflare.flare.dev/worker-binding=true. Other Secrets of the namespace (a CloudflareAccount's
// token, a service account token) are refused with the same message as a missing one, and
// nothing is uploaded, so a WorkerScript author cannot read them through the Worker nor probe
// which exist.
func TestSecretBindingOptIn(t *testing.T) {
	h := start(t)
	plain := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "not-opted-in"}, StringData: map[string]string{"v": "s3cr3t"}}
	h.create(plain)
	sa := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sa-token", Labels: map[string]string{workerscript.LabelWorkerBinding: "true"},
		Annotations: map[string]string{corev1.ServiceAccountNameKey: "default"}},
		Type: corev1.SecretTypeServiceAccountToken, StringData: map[string]string{"v": "tok"}}
	h.create(sa)

	blocked := func(msg *string) func(*workersv1alpha1.WorkerScript) bool {
		return func(ws *workersv1alpha1.WorkerScript) bool {
			c := meta.FindStatusCondition(ws.Status.Conditions, "Ready")
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != commonv1alpha1.ReasonDependency {
				return false
			}
			*msg = c.Message
			return true
		}
	}
	m := h.mark()
	for _, c := range []struct{ script, secret string }{{"reads-plain", "not-opted-in"}, {"reads-sa", "sa-token"}, {"reads-missing", "missing"}} {
		fp := params(fetchModule)
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "X", Type: "secret_text",
			SecretKeyRef: &workersv1alpha1.SecretKeyRef{Name: c.secret, Key: "v"}}}
		h.newScript(c.script, fp, nil)
		var msg string
		h.waitScript(c.script, blocked(&msg))
		if !strings.Contains(msg, workerscript.LabelWorkerBinding+"=true") || strings.Contains(msg, "not found") {
			t.Errorf("%s: message %q", c.script, msg)
		}
		if n := h.uploads(h.since(m), c.script); n != 0 {
			t.Errorf("%s: %d uploads with a Secret that is not opted in", c.script, n)
		}
	}
	if h.scriptExists("reads-plain") {
		t.Error("the Worker was uploaded with a Secret that is not opted in")
	}

	// Opting the Secret in unblocks the binding.
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "not-opted-in"}, plain); err != nil {
		t.Fatal(err)
	}
	plain.Labels = map[string]string{workerscript.LabelWorkerBinding: "true"}
	if err := h.e.Client.Update(h.ctx(), plain); err != nil {
		t.Fatal(err)
	}
	h.waitScript("reads-plain", scriptReady)
	if n := h.uploads(h.since(m), "reads-plain"); n != 1 {
		t.Errorf("uploads after the opt-in: %d, want 1\n%s", n, testenv.Summary(h.since(m)))
	}
}
