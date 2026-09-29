package workerscript

import (
	"fmt"
	"strings"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// Crash consistency of the first upload (docs/resilience.md). Before uploading a script that
// does not exist yet, the controller records a create-pending annotation
// (reconcile.MarkCreatePending) whose key is the script name and the hashes of what it is about
// to upload. If the manager dies between the upload and RecordCreated, the next reconcile finds
// a script it has no record of. Without the annotation that is a NameConflict (the object's own
// script looks like someone else's); with it, the script is adopted as this object's own lost
// create, and the recorded hashes stand in for the lost status, so the same content is not
// uploaded a second time (every upload creates a new version and deployment).

// pendingKey is the create-pending key of an upload of d to script name.
func pendingKey(name string, d *desired) string {
	return fmt.Sprintf("%s;c=%s;s=%s;w=%s", name, d.contentHash, d.settingsHash, d.secretsHash)
}

// pendingScript is the script name of ws's create-pending record ("" without one).
func pendingScript(ws *workersv1alpha1.WorkerScript) string {
	key, ok := reconcile.PendingCreate(ws)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(key, ";")
	return name
}

// pendingUpload returns what ws's create-pending record says it was about to upload to name.
func pendingUpload(ws *workersv1alpha1.WorkerScript, name string) (appliedState, bool) {
	key, ok := reconcile.PendingCreate(ws)
	if !ok {
		return appliedState{}, false
	}
	parts := strings.Split(key, ";")
	if len(parts) != 4 || parts[0] != name {
		return appliedState{}, false
	}
	a := appliedState{name: name}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "c":
			a.content = v
		case "s":
			a.settings = v
		case "w":
			a.secrets = v
		default:
			return appliedState{}, false
		}
	}
	return a, a.content != ""
}
