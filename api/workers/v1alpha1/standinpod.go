package v1alpha1

// Stand-in Pods: with the Workers logs feature on (chart value workersLogs.enabled; design in
// docs/workers-logs-design.md), every WorkerScript gets one Pod bound to the virtual node
// (default cf-workers) so that `kubectl logs` reads the Worker's logs from Cloudflare. The
// names below are part of the user-facing contract: users select stand-in Pods by these
// labels (kubectl logs -l flare.dev/workerscript=api) and opt WorkerScripts out with the
// annotation. Frozen contract; change deliberately.
const (
	// StandInLabelWorkerScript is set on a stand-in Pod to the name of its WorkerScript (the
	// same namespace), or, when the name is not a valid label value (longer than 63
	// characters), to "h-" plus 40 hex digits of its SHA-256 (the scheme of
	// reconcile.AccountLabelValue).
	StandInLabelWorkerScript = "flare.dev/workerscript"
	// StandInLabelWorkerScriptUID is set on a stand-in Pod to its WorkerScript's metadata.uid.
	// A Pod whose controller ownerReference, StandInLabelWorkerScriptUID and the WorkerScript's
	// current UID do not all agree is not served (it is not a stand-in Pod of that object).
	StandInLabelWorkerScriptUID = "flare.dev/workerscript-uid"
	// StandInLabelRole marks every stand-in Pod; its value is StandInRoleWorker.
	StandInLabelRole = "flare.dev/stand-in"
	// StandInRoleWorker is the StandInLabelRole value of a WorkerScript's stand-in Pod.
	StandInRoleWorker = "worker"

	// StandInAnnotationScriptName is informational: the Cloudflare script name the stand-in
	// Pod showed logs for when it was last written (WorkerScript.ScriptName()). The virtual
	// kubelet never trusts it; it resolves the script from the WorkerScript itself.
	StandInAnnotationScriptName = "flare.dev/script-name"

	// AnnotationStandInPod on a WorkerScript set to "false" opts it out: no stand-in Pod is
	// created, and an existing one is deleted. Any other value, or no annotation, opts in
	// (while the feature is enabled and the namespace matches workersLogs.namespaceSelector).
	AnnotationStandInPod = "flare.dev/stand-in-pod"

	// StandInContainerName is the only container of a stand-in Pod (kubectl logs -c worker).
	StandInContainerName = "worker"
	// StandInPodNameSuffix is appended to the WorkerScript name to form the stand-in Pod name:
	// <name>-worker when that is at most 63 characters, else the first 46 characters of the
	// name with trailing "-" and "." trimmed, "-", 8 hex digits of the SHA-256 of the full
	// name, and the suffix (at most 62 characters, a valid DNS-1123 subdomain). Two
	// WorkerScripts can still collide only through a hash collision; the stand-in controller
	// never touches a Pod it does not control.
	StandInPodNameSuffix = "-worker"
)
