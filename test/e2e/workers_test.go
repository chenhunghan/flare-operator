//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
)

// workerCode is a fetch handler that answers version.
func workerCode(version string) string {
	return fmt.Sprintf("export default {\n  async fetch(req, env) {\n    return new Response(%q);\n  }\n};\n", version)
}

// workerSpec is a WorkerScript that binds the run's KVNamespace (kv), D1Database (db) and
// VPCService (web) by reference.
func (s *suite) workerSpec(name, code string) *workersv1alpha1.WorkerScript {
	return &workersv1alpha1.WorkerScript{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: name},
		Spec: workersv1alpha1.WorkerScriptSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete},
			ForProvider: &workersv1alpha1.WorkerScriptParameters{
				ScriptName:        s.ns + "-" + name,
				MainModule:        "index.js",
				CompatibilityDate: "2026-09-01",
				Modules:           map[string]workersv1alpha1.WorkerModule{"index.js": {Type: "esm", Content: code}},
				Bindings: []workersv1alpha1.WorkerBinding{
					{Name: "CACHE", Type: workersv1alpha1.BindingKVNamespace, KVNamespaceRef: &commonv1alpha1.LocalRef{Name: "kv"}},
					{Name: "DB", Type: workersv1alpha1.BindingD1, D1DatabaseRef: &commonv1alpha1.LocalRef{Name: "db"}},
					{Name: "WEB", Type: workersv1alpha1.BindingVPCService, VPCServiceRef: &commonv1alpha1.LocalRef{Name: "web"}},
				},
			},
		},
	}
}

// checkWorkerBindings reads the script's settings from flarefake and checks that every binding
// carries the Cloudflare ID of the object it references.
func (s *suite) checkWorkerBindings(t *testing.T, o *objects) {
	var st struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := s.cfGet("/workers/scripts/"+o.worker.ScriptName()+"/settings", &st); err != nil {
		t.Errorf("Worker settings: %v", err)
		return
	}
	want := map[string]string{"CACHE": o.kv.Status.ID, "DB": o.d1.Status.ID, "WEB": o.vpc.Status.ID}
	for _, b := range st.Bindings {
		name, _ := b["name"].(string)
		id, ok := want[name]
		if !ok {
			continue
		}
		found := false
		for k, v := range b {
			if k != "name" && k != "type" && v == id {
				found = true
			}
		}
		if !found {
			t.Errorf("binding %s = %v, want it to carry %s", name, b, id)
		}
		delete(want, name)
	}
	if len(want) > 0 {
		t.Errorf("bindings missing from the script's settings: %v (got %v)", want, st.Bindings)
	}
	if o.worker.Status.ID != o.worker.ScriptName() {
		t.Errorf("WorkerScript status.id = %q, want %q", o.worker.Status.ID, o.worker.ScriptName())
	}
}
