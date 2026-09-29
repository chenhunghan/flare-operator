// Package e2e drives an installed flare-operator (charts/flare-operator with
// flarefake.enabled=true) on a real cluster. The tests carry the e2e build tag and skip unless
// KUBECONFIG is set:
//
//	make e2e-images e2e-install   # build images, load them into the cluster, helm install
//	make e2e                      # go test -tags e2e ./test/e2e
//	make e2e-uninstall            # helm uninstall, delete the CRDs and flare-system, and
//	                              # remove the loaded images (E2E_IMAGE_REMOVE)
//
// The tests talk to the in-cluster flarefake through the API server's service proxy
// (/api/v1/namespaces/<ns>/services/http:<svc>:<port>/proxy/...), so no port-forward is
// needed. They read its request journal to assert which Cloudflare writes the operator made.
//
// cloudflared cannot connect to flarefake (it has no tunnel edge), so Tunnels run the
// connector image test/e2e/cloudflared-stub (spec.connector.image): it serves /ready on :2000
// once TUNNEL_TOKEN decodes as a tunnel token, which is what the Tunnel controller's readiness
// (a ready cloudflared replica) needs. Tunnel connections are simulated with flarefake's
// /_fake/accounts/{a}/tunnels/{id}/connect|disconnect control endpoints.
package e2e
