package apivalidation

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) { testenv.Main(m, &env, testenv.Options{}) }

const accountID = "0123456789abcdef0123456789abcdef"

// object parses a manifest (apiVersion, kind, spec; name and namespace are filled in).
func object(t *testing.T, ns, name, manifest string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &u.Object); err != nil {
		t.Fatalf("manifest: %v\n%s", err, manifest)
	}
	u.SetNamespace(ns)
	if u.GetName() == "" {
		u.SetName(name)
	}
	return u
}

// check fails t unless err matches want: "" means accepted, anything else is a substring of
// the API server's Invalid error.
func check(t *testing.T, what string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: want accepted, got %v", what, err)
	case want != "" && err == nil:
		t.Errorf("%s: want rejected (%q), but it was accepted", what, want)
	case want != "" && !apierrors.IsInvalid(err):
		t.Errorf("%s: want an Invalid error (%q), got %v", what, want, err)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("%s: want an error containing %q, got %v", what, want, err)
	}
}

// update re-reads u, applies mutate to the fresh object and updates it; it returns the error.
func update(t *testing.T, u *unstructured.Unstructured, mutate func(obj map[string]any)) error {
	t.Helper()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := env.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(u), u); err != nil {
			return err
		}
		mutate(u.Object)
		return env.Client.Update(testenv.Context(t, 10*time.Second), u)
	})
}

// setStatus writes status through the status subresource (as the controller would).
func setStatus(t *testing.T, u *unstructured.Unstructured, status map[string]any) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := env.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(u), u); err != nil {
			return err
		}
		u.Object["status"] = status
		return env.Client.Status().Update(testenv.Context(t, 10*time.Second), u)
	})
	if err != nil {
		t.Fatalf("status update: %v", err)
	}
}

// forProvider returns spec.forProvider of obj, creating it when missing.
func forProvider(obj map[string]any) map[string]any {
	spec := obj["spec"].(map[string]any)
	fp, ok := spec["forProvider"].(map[string]any)
	if !ok {
		fp = map[string]any{}
		spec["forProvider"] = fp
	}
	return fp
}

// Minimal valid objects of every kind.
const (
	kvNamespace = `apiVersion: kv.cloudflare.flare.dev/v1alpha1
kind: KVNamespace
spec:
  accountRef: {name: acct}
  forProvider: {title: t}`
	queue = `apiVersion: queues.cloudflare.flare.dev/v1alpha1
kind: Queue
spec:
  accountRef: {name: acct}
  forProvider: {queue_name: q}`
	d1Database = `apiVersion: d1.cloudflare.flare.dev/v1alpha1
kind: D1Database
spec:
  accountRef: {name: acct}
  forProvider: {name: db}`
	vectorizeIndex = `apiVersion: vectorize.cloudflare.flare.dev/v1alpha1
kind: VectorizeIndex
spec:
  accountRef: {name: acct}
  forProvider: {name: idx, config: {dimensions: 3, metric: cosine}}`
	secretsStore = `apiVersion: secretsstore.cloudflare.flare.dev/v1alpha1
kind: SecretsStore
spec:
  accountRef: {name: acct}
  forProvider: {name: store}`
	aiGateway = `apiVersion: aigateway.cloudflare.flare.dev/v1alpha1
kind: AIGateway
spec:
  accountRef: {name: acct}
  forProvider: {id: gw, cache_invalidate_on_update: false, cache_ttl: 0, collect_logs: true, rate_limiting_interval: 0, rate_limiting_limit: 0}`
	tunnel = `apiVersion: tunnels.cloudflare.flare.dev/v1alpha1
kind: Tunnel
spec:
  accountRef: {name: acct}`
	vpcService = `apiVersion: workersvpc.cloudflare.flare.dev/v1alpha1
kind: VPCService
spec:
  accountRef: {name: acct}
  forProvider:
    type: tcp
    tcp_port: 5432
    host: {ipv4: 10.0.0.1}
    tunnelRef: {name: tun}`
	workerScript = `apiVersion: workers.cloudflare.flare.dev/v1alpha1
kind: WorkerScript
spec:
  accountRef: {name: acct}
  forProvider:
    main_module: index.js
    modules:
      index.js: {content: "export default {}"}`
	cloudflareAccount = `apiVersion: cloudflare.flare.dev/v1alpha1
kind: CloudflareAccount
spec:
  accountID: ` + accountID + `
  tokenSecretRef: {name: tok}`
)

// managed are the managed kinds (everything but CloudflareAccount).
var managed = map[string]string{
	"KVNamespace": kvNamespace, "Queue": queue, "D1Database": d1Database, "VectorizeIndex": vectorizeIndex,
	"SecretsStore": secretsStore, "AIGateway": aiGateway, "Tunnel": tunnel, "VPCService": vpcService,
	"WorkerScript": workerScript,
}

// TestCreate creates valid and invalid objects of every kind.
func TestCreate(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	type tc struct {
		manifest string
		mutate   func(obj map[string]any)
		want     string
	}
	set := func(path string, v any) func(map[string]any) {
		return func(obj map[string]any) {
			parts := strings.Split(path, ".")
			m := obj
			for _, p := range parts[:len(parts)-1] {
				next, ok := m[p].(map[string]any)
				if !ok {
					next = map[string]any{}
					m[p] = next
				}
				m = next
			}
			if v == nil {
				delete(m, parts[len(parts)-1])
			} else {
				m[parts[len(parts)-1]] = v
			}
		}
	}
	both := func(fs ...func(map[string]any)) func(map[string]any) {
		return func(obj map[string]any) {
			for _, f := range fs {
				f(obj)
			}
		}
	}
	cases := map[string]tc{
		"CloudflareAccount valid":             {manifest: cloudflareAccount},
		"CloudflareAccount bad accountID":     {cloudflareAccount, set("spec.accountID", "ABC"), "spec.accountID"},
		"CloudflareAccount empty secret name": {cloudflareAccount, set("spec.tokenSecretRef.name", ""), "spec.tokenSecretRef.name"},
		"CloudflareAccount non-http baseURL":  {cloudflareAccount, set("spec.baseURL", "ftp://x"), "spec.baseURL"},

		"KVNamespace missing title":         {kvNamespace, set("spec.forProvider.title", nil), "forProvider.title is required"},
		"KVNamespace observe without title": {kvNamespace, both(set("spec.forProvider.title", nil), set("spec.managementPolicies", []any{"Observe"})), ""},
		"KVNamespace bad jurisdiction":      {kvNamespace, set("spec.forProvider.jurisdiction", "mars"), "spec.forProvider.jurisdiction"},
		"KVNamespace title too long":        {kvNamespace, set("spec.forProvider.title", strings.Repeat("x", 513)), "spec.forProvider.title"},
		"Queue bad jurisdiction":            {queue, set("spec.forProvider.jurisdiction", "mars"), "spec.forProvider.jurisdiction"},
		"Queue missing queue_name":          {queue, set("spec.forProvider.queue_name", nil), "forProvider.queue_name is required"},
		"D1Database bad name":               {d1Database, set("spec.forProvider.name", "bad name!"), "spec.forProvider.name"},
		"D1Database lower-case hint":        {d1Database, set("spec.forProvider.primary_location_hint", "wnam"), "spec.forProvider.primary_location_hint"},
		"D1Database hint":                   {d1Database, set("spec.forProvider.primary_location_hint", "WNAM"), ""},
		"VectorizeIndex zero dimensions":    {vectorizeIndex, set("spec.forProvider.config.dimensions", 0), "spec.forProvider.config.dimensions"},
		"VectorizeIndex missing config":     {vectorizeIndex, set("spec.forProvider.config", nil), "forProvider.config is required"},
		"VectorizeIndex bad name":           {vectorizeIndex, set("spec.forProvider.name", "Idx"), "spec.forProvider.name"},
		"SecretsStore missing name":         {secretsStore, set("spec.forProvider.name", nil), "forProvider.name is required"},
		"AIGateway bad id":                  {aiGateway, set("spec.forProvider.id", "Bad ID"), "spec.forProvider.id"},
		"AIGateway missing collect_logs":    {aiGateway, set("spec.forProvider.collect_logs", nil), "forProvider.collect_logs is required"},

		"Tunnel name over 63":            {tunnel, set("metadata.name", strings.Repeat("t", 64)), "metadata.name must be at most 63"},
		"Tunnel bad excludeCIDRs":        {tunnel, set("spec.networkPolicy.excludeCIDRs", []any{"10.0.0.1"}), "must be an IPv4 CIDR"},
		"VPCService hostname and ipv4":   {vpcService, set("spec.forProvider.host.hostname", "db.ns.svc.cluster.local"), "set either hostname or ipv4/ipv6"},
		"VPCService tunnelRef and id":    {vpcService, set("spec.forProvider.host.network", map[string]any{"tunnel_id": "x"}), "exactly one of tunnelRef"},
		"VPCService http_port on tcp":    {vpcService, set("spec.forProvider.http_port", 80), "only valid for type http"},
		"VPCService empty tunnelRef":     {vpcService, set("spec.forProvider.tunnelRef.name", ""), "tunnelRef.name must not be empty"},
		"VPCService without forProvider": {vpcService, set("spec.forProvider", nil), "forProvider is required unless"},
		"VPCService observe only":        {vpcService, both(set("spec.forProvider", nil), set("spec.managementPolicies", []any{"Observe"})), ""},
		"VPCService short hostname": {vpcService, both(set("spec.forProvider.host", map[string]any{"hostname": "db.ns.svc"})),
			"must be fully qualified"},
		"WorkerScript modules and sourceRef": {workerScript, set("spec.forProvider.sourceRef", map[string]any{"name": "cm"}), "exactly one of modules, sourceRef or moduleSource"},
		"WorkerScript neither":               {workerScript, set("spec.forProvider.modules", nil), "exactly one of modules, sourceRef or moduleSource"},
		"WorkerScript moduleSource": {workerScript, both(set("spec.forProvider.modules", nil), set("spec.forProvider.moduleSource",
			map[string]any{"url": map[string]any{"url": "https://example.com/w.tgz", "sha256": strings.Repeat("a", 64)}})), ""},
		"WorkerScript assets-only": {workerScript, both(both(set("spec.forProvider.modules", nil), set("spec.forProvider.main_module", nil)),
			set("spec.forProvider.assets", map[string]any{"source": map[string]any{"configMapRef": map[string]any{"configMaps": []any{map[string]any{"name": "site"}}}}})), ""},
		"WorkerScript sourceRef":           {workerScript, both(set("spec.forProvider.modules", nil), set("spec.forProvider.sourceRef", map[string]any{"name": "cm"})), ""},
		"WorkerScript unknown main_module": {workerScript, set("spec.forProvider.main_module", "main.js"), "main_module must name one of modules"},
		"WorkerScript bad script_name":     {workerScript, set("spec.forProvider.script_name", "Bad"), "spec.forProvider.script_name"},
		"WorkerScript kv binding without id": {workerScript, set("spec.forProvider.bindings", []any{map[string]any{"name": "KV", "type": "kv_namespace"}}),
			"type kv_namespace needs exactly one of"},
		"WorkerScript kv binding empty ref": {workerScript, set("spec.forProvider.bindings", []any{map[string]any{"name": "KV", "type": "kv_namespace",
			"kvNamespaceRef": map[string]any{"name": ""}}}), "needs a non-empty name"},
		"WorkerScript kv binding by ref": {workerScript, set("spec.forProvider.bindings", []any{map[string]any{"name": "KV", "type": "kv_namespace",
			"kvNamespaceRef": map[string]any{"name": "kv"}}}), ""},
	}
	// Every managed kind: valid as is; the common fields are validated.
	for kind, m := range managed {
		cases[kind+" valid"] = tc{manifest: m}
		cases[kind+" empty accountRef"] = tc{m, set("spec.accountRef.name", ""), "accountRef.name must name a CloudflareAccount"}
		cases[kind+" no accountRef"] = tc{m, set("spec.accountRef", nil), "spec.accountRef"}
		cases[kind+" bad deletionPolicy"] = tc{m, set("spec.deletionPolicy", "Keep"), "spec.deletionPolicy"}
		cases[kind+" bad managementPolicy"] = tc{m, set("spec.managementPolicies", []any{"Observe", "Watch"}), "spec.managementPolicies"}
		cases[kind+" Orphan"] = tc{m, set("spec.deletionPolicy", "Orphan"), ""}
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	slices.Sort(names)
	for i, n := range names {
		c := cases[n]
		t.Run(n, func(t *testing.T) {
			u := object(t, ns, fmt.Sprintf("obj-%d", i), c.manifest)
			if c.mutate != nil {
				c.mutate(u.Object)
			}
			check(t, "create", env.Client.Create(testenv.Context(t, 10*time.Second), u), c.want)
		})
	}
}

// TestImmutable changes immutable forProvider fields before and after the resource exists.
func TestImmutable(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	const immutable = "is immutable"
	cases := []struct {
		kind, manifest, field string
		// v1 is the initial value, v2 a different one, v3 the value Cloudflare reports.
		v1, v2, v3 any
		// escape: a change to the value in status.atProvider is allowed.
		escape bool
		// removable: the field is not create-required, so removing it is allowed.
		removable bool
	}{
		// Two-value enums: Cloudflare reports the stored value.
		{"KVNamespace", kvNamespace, "jurisdiction", "eu", "fedramp", "eu", true, true},
		{"Queue", queue, "jurisdiction", "eu", "us", "fedramp", true, true},
		{"D1Database", d1Database, "name", "db-a", "db-b", "db-c", true, false},
		{"D1Database", d1Database, "jurisdiction", "eu", "fedramp", "eu", true, true},
		// primary_location_hint is write-only: never read back, so no atProvider escape.
		{"D1Database", d1Database, "primary_location_hint", "WNAM", "ENAM", "APAC", false, true},
		{"VectorizeIndex", vectorizeIndex, "name", "idx-a", "idx-b", "idx-c", true, false},
		{"VectorizeIndex", vectorizeIndex, "description", "a", "b", "c", true, true},
		// config is an object: compared as a whole, no atProvider escape.
		{"VectorizeIndex", vectorizeIndex, "config", map[string]any{"dimensions": int64(3), "metric": "cosine"},
			map[string]any{"dimensions": int64(4), "metric": "cosine"}, nil, false, false},
		{"SecretsStore", secretsStore, "name", "s-a", "s-b", "s-c", true, false},
		{"AIGateway", aiGateway, "id", "gw-a", "gw-b", "gw-c", true, false},
		{"Tunnel", tunnel, "name", "tun-a", "tun-b", "tun-c", true, true},
	}
	for i, c := range cases {
		t.Run(c.kind+"/"+c.field, func(t *testing.T) {
			u := object(t, ns, fmt.Sprintf("imm-%d", i), c.manifest)
			forProvider(u.Object)[c.field] = c.v1
			check(t, "create", env.Client.Create(testenv.Context(t, 10*time.Second), u), "")
			setField := func(v any) func(map[string]any) {
				return func(obj map[string]any) {
					if v == nil {
						delete(forProvider(obj), c.field)
					} else {
						forProvider(obj)[c.field] = v
					}
				}
			}

			// Before the resource exists (no status.id) the field may change.
			check(t, "change before status.id", update(t, u, setField(c.v2)), "")
			check(t, "change back before status.id", update(t, u, setField(c.v1)), "")

			at := map[string]any{}
			if c.escape {
				at[c.field] = c.v3
			}
			setStatus(t, u, map[string]any{"id": "ext-" + fmt.Sprint(i), "atProvider": at})
			check(t, "change after status.id", update(t, u, setField(c.v2)), immutable)
			check(t, "unchanged value", update(t, u, setField(c.v1)), "")
			check(t, "other field changes", update(t, u, func(obj map[string]any) {
				obj["spec"].(map[string]any)["deletionPolicy"] = "Orphan"
			}), "")
			if c.escape {
				check(t, "change to status.atProvider's value", update(t, u, setField(c.v3)), "")
			}
			if c.removable {
				// Removing is allowed; re-adding another value is left to the controller
				// (Synced=False, reason Immutable).
				check(t, "remove", update(t, u, setField(nil)), "")
				check(t, "re-add", update(t, u, setField(c.v2)), "")
			} else {
				check(t, "remove a create-required field", update(t, u, setField(nil)), "is required unless")
			}
		})
	}

	// WorkerScript's script_name is its Cloudflare ID: immutable from creation on, and it
	// cannot be added or removed.
	t.Run("WorkerScript/script_name", func(t *testing.T) {
		u := object(t, ns, "ws-imm", workerScript)
		forProvider(u.Object)["script_name"] = "a"
		check(t, "create", env.Client.Create(testenv.Context(t, 10*time.Second), u), "")
		check(t, "change", update(t, u, func(obj map[string]any) { forProvider(obj)["script_name"] = "b" }), "script_name is immutable")
		check(t, "remove", update(t, u, func(obj map[string]any) { delete(forProvider(obj), "script_name") }), "cannot be added or removed")
		check(t, "other field changes", update(t, u, func(obj map[string]any) { forProvider(obj)["compatibility_date"] = "2026-09-01" }), "")
	})
}

// TestAccountImmutable: spec.accountRef of every managed kind cannot change once the resource
// exists (status.id is set), and CloudflareAccount spec.accountID never changes. Either change
// would make the controller create a second resource in the new account and leave the first
// one unmanaged in the old one.
func TestAccountImmutable(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	setRef := func(name string) func(map[string]any) {
		return func(obj map[string]any) {
			obj["spec"].(map[string]any)["accountRef"] = map[string]any{"name": name}
		}
	}
	for kind, manifest := range managed {
		t.Run(kind, func(t *testing.T) {
			u := object(t, ns, "acctref-"+strings.ToLower(kind), manifest)
			check(t, "create", env.Client.Create(testenv.Context(t, 10*time.Second), u), "")
			check(t, "switch before status.id", update(t, u, setRef("other")), "")
			check(t, "switch back before status.id", update(t, u, setRef("acct")), "")
			setStatus(t, u, map[string]any{"id": "ext-" + strings.ToLower(kind)})
			check(t, "switch after status.id", update(t, u, setRef("other")), "spec.accountRef is immutable")
			check(t, "other field changes", update(t, u, func(obj map[string]any) {
				obj["spec"].(map[string]any)["deletionPolicy"] = "Orphan"
			}), "")
		})
	}
	t.Run("CloudflareAccount", func(t *testing.T) {
		u := object(t, ns, "acct-imm", cloudflareAccount)
		check(t, "create", env.Client.Create(testenv.Context(t, 10*time.Second), u), "")
		check(t, "change accountID", update(t, u, func(obj map[string]any) {
			obj["spec"].(map[string]any)["accountID"] = strings.Repeat("f", 32)
		}), "accountID is immutable")
		check(t, "change the token Secret", update(t, u, func(obj map[string]any) {
			obj["spec"].(map[string]any)["tokenSecretRef"] = map[string]any{"name": "tok2"}
		}), "")
	})
}

// TestKubectlNames checks what kubectl sees through discovery: every kind is in the cloudflare
// category, managed kinds also in managed, and each has "cf"-prefixed short names.
func TestKubectlNames(t *testing.T) {
	e := testenv.Require(t, env)
	dc, err := discovery.NewDiscoveryClientForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		_, all, err := dc.ServerGroupsAndResources()
		if err != nil {
			return false, err.Error()
		}
		seen := map[string]bool{}
		shortOwner := map[string]string{}
		for _, l := range all {
			if !strings.HasSuffix(l.GroupVersion, "cloudflare.flare.dev/v1alpha1") {
				continue
			}
			for _, r := range l.APIResources {
				if strings.Contains(r.Name, "/") {
					continue
				}
				seen[r.Kind] = true
				if !slices.Contains(r.Categories, "cloudflare") {
					return false, fmt.Sprintf("%s: categories %v lack cloudflare", r.Kind, r.Categories)
				}
				if wantManaged := r.Kind != "CloudflareAccount"; slices.Contains(r.Categories, "managed") != wantManaged {
					return false, fmt.Sprintf("%s: categories %v (managed expected: %v)", r.Kind, r.Categories, wantManaged)
				}
				if len(r.ShortNames) == 0 {
					return false, r.Kind + ": no short names"
				}
				for _, sn := range r.ShortNames {
					if !strings.HasPrefix(sn, "cf") {
						return false, fmt.Sprintf("%s: short name %q lacks the cf prefix", r.Kind, sn)
					}
					if o, dup := shortOwner[sn]; dup && o != r.Kind {
						return false, fmt.Sprintf("short name %q of %s and %s", sn, o, r.Kind)
					}
					shortOwner[sn] = r.Kind
				}
			}
		}
		for kind := range managed {
			if !seen[kind] {
				return false, kind + " not discovered"
			}
		}
		if !seen["CloudflareAccount"] {
			return false, "CloudflareAccount not discovered"
		}
		return true, ""
	})
}
