package cfclient

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/chenhunghan/flare-operator/internal/cfclient/internal/routespec"
)

func TestRouteTemplate(t *testing.T) {
	acct := "0123456789abcdef0123456789abcdef"
	for _, c := range []struct{ path, want string }{
		{"/accounts/" + acct + "/storage/kv/namespaces", "/accounts/{account_id}/storage/kv/namespaces"},
		{"/accounts/" + acct + "/storage/kv/namespaces/ffff0000", "/accounts/{account_id}/storage/kv/namespaces/{namespace_id}"},
		{"/accounts/" + acct + "/queues/q1/", "/accounts/{account_id}/queues/{queue_id}"},
		{"/accounts/" + acct + "/d1/database/11111111-2222-3333-4444-555555555555", "/accounts/{account_id}/d1/database/{database_id}"},
		{"/accounts/" + acct + "/cfd_tunnel/t1/token", "/accounts/{account_id}/cfd_tunnel/{tunnel_id}/token"},
		{"/accounts/" + acct + "/connectivity/directory/services/s1", "/accounts/{account_id}/connectivity/directory/services/{service_id}"},
		{"/accounts/" + acct + "/workers/scripts/hello", "/accounts/{account_id}/workers/scripts/{script_name}"},
		{"/accounts/" + acct + "/workers/scripts/hello/settings", "/accounts/{account_id}/workers/scripts/{script_name}/settings"},
		{"/accounts/" + acct + "/workers/scripts/hello/deployments", "/accounts/{account_id}/workers/scripts/{script_name}/deployments"},
		{"/accounts/" + acct + "/workers/scripts/hello/subdomain", "/accounts/{account_id}/workers/scripts/{script_name}/subdomain"},
		{"/accounts/" + acct + "/workers/subdomain", "/accounts/{account_id}/workers/subdomain"},
		{"/accounts/" + acct + "/tags", "/accounts/{account_id}/tags"},
		{"/accounts/" + acct + "/tags/resources", "/accounts/{account_id}/tags/resources"},
		{"/accounts/" + acct + "/tokens/verify", "/accounts/{account_id}/tokens/verify"},
		{"/user/tokens/verify", "/user/tokens/verify"},
		{"/accounts/" + acct + "/vectorize/v2/indexes/my-index", "/accounts/{account_id}/vectorize/v2/indexes/{index_name}"},
		{"/accounts/" + acct + "/ai-gateway/gateways/gw", "/accounts/{account_id}/ai-gateway/gateways/{id}"},
		{"/accounts/" + acct + "/secrets_store/stores/s", "/accounts/{account_id}/secrets_store/stores/{store_id}"},
		{"/accounts/" + acct + "/storage/kv/namespaces/ns/values/a%2Fb", "/accounts/{account_id}/storage/kv/namespaces/{namespace_id}/values/{key_name}"},
		{"/accounts/" + acct, "/accounts/{account_id}"},
		{"/", OtherRoute},
		{"", OtherRoute},
		{"/no/such/" + acct, OtherRoute},
		{"/accounts//queues", OtherRoute},
	} {
		if got := RouteTemplate(c.path); got != c.want {
			t.Errorf("RouteTemplate(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestRouteTemplateRoundTrip instantiates every spec template with values that cannot collide
// with a static segment and checks that it maps back to a template of the same shape and never
// leaks a value.
func TestRouteTemplateRoundTrip(t *testing.T) {
	param := regexp.MustCompile(`\{[^}]*\}`)
	for _, tmpl := range specRoutes {
		concrete := param.ReplaceAllString(tmpl, "Zq9-value")
		got := RouteTemplate(concrete)
		if strings.Contains(got, "Zq9-value") {
			t.Fatalf("RouteTemplate(%q) = %q leaks a value", concrete, got)
		}
		if param.ReplaceAllString(got, "{}") != param.ReplaceAllString(tmpl, "{}") {
			t.Errorf("RouteTemplate(%q) = %q, want the shape of %q", concrete, got, tmpl)
		}
	}
}

// TestGeneratedRoutesMatchSpec keeps zz_generated_routes.go in step with spec/openapi.json.gz
// (regenerate with `go generate ./internal/cfclient/`).
func TestGeneratedRoutesMatchSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	root := filepath.Join("..", "..")
	paths, err := routespec.SpecPaths(filepath.Join(root, "spec", "openapi.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := routespec.Render(paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("zz_generated_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("zz_generated_routes.go is out of date: run go generate ./internal/cfclient/")
	}
}
