// Package vk is the Workers virtual kubelet: one virtual node (default cf-workers) whose kubelet
// API serves `kubectl logs` for the stand-in Pods of WorkerScripts, reading the logs from
// Cloudflare through internal/workerlogs. It runs as its own Deployment (cmd/workers-vk), not in
// the manager. Design and evidence: docs/workers-logs-design.md.
//
// Frozen contract (contract.go); change deliberately. Workstream B implements the package and
// cmd/workers-vk; workstream C implements internal/vk/standin (stand-in Pods and their status).
//
// What runs in the process (all on the leader of the Lease flare-operator-workers-vk.flare.dev):
//
//   - The node: virtual-kubelet's node.NodeController with the v1 node Lease in
//     kube-node-lease. The Node carries an ownerReference to the release's ClusterRole
//     (Config.OwnerClusterRole), so deleting the release (or disabling the feature) garbage-
//     collects the Node, and the pod GC controller then deletes the Pods bound to it.
//   - The pod controller: virtual-kubelet's node.PodController, built directly (not with
//     nodeutil.NewNode, whose ConfigMap, Secret and Service informers watch the whole cluster)
//     with SkipDownwardAPIResolution and informers filtered to nothing. The provider reports the
//     status of stand-in Pods from their WorkerScripts (standin.StatusMapper) and fails any
//     other Pod bound to the node.
//   - The kubelet API: HTTPS on Config.ListenAddr with the serving certificate of
//     Config.TLS, client certificates verified against the cluster's client CA (ConfigMap
//     kube-system/extension-apiserver-authentication, key client-ca-file) or bearer tokens
//     (TokenReview), each request authorized with a SubjectAccessReview on nodes/<sub> of this
//     node (nodeutil.WebhookAuth). /containerLogs/{ns}/{pod}/{container} is served by
//     workerlogs.Streamer; exec, attach, port-forward and run answer 501 with ErrUnsupported's
//     message; /pods, /stats/summary and /metrics/resource answer with empty data so
//     metrics-server and scrapers see a healthy, empty node.
//   - The stand-in controller (internal/vk/standin): a controller-runtime reconciler that keeps
//     one Pod per WorkerScript.
package vk
