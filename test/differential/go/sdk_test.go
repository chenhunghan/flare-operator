//go:build differential

package sdkdiff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"testing"
	"time"

	cloudflare "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/connectivity"
	"github.com/cloudflare/cloudflare-go/v7/d1"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/queues"
	"github.com/cloudflare/cloudflare-go/v7/r2"
	"github.com/cloudflare/cloudflare-go/v7/resource_tagging"
	"github.com/cloudflare/cloudflare-go/v7/user"
	"github.com/cloudflare/cloudflare-go/v7/workers"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"

	"flare.dev/operator/test/differential/harness"
)

// SDKVersion is the cloudflare-go version pinned in go.mod (keep them in sync).
const SDKVersion = "cloudflare-go@v7.11.0"

var acct = cloudflare.F(harness.AccountID)

// sdkSrc prefixes evidence citations into cloudflare-go at the pinned release (tag v7.11.0 =
// commit 3da6607060703d3f757e22988e96f56f8cd9c04d; also in the module cache).
const sdkSrc = "cloudflare/cloudflare-go@3da6607:"

var (
	dSDKWorkersUploadForm = harness.Discrepancy{
		ID: "SDK-WORKERS-UPLOAD-FORM", Client: SDKVersion,
		Summary: "cloudflare-go's Workers.Scripts.Update sends the upload as flattened form fields (metadata.main_module, metadata.bindings.0.name, files.0) instead of one JSON 'metadata' part; flarefake answers 400/10021 'Missing metadata part.'. " +
			"flarefake follows the pinned spec, the recorded upload 0036 and what wrangler sends (a JSON metadata part), so this may be a cloudflare-go defect; whether the live API accepts the flattened form is UNVERIFIED.",
		Evidence: sdkSrc + "workers/script.go#L4154-L4167 (apiform.MarshalRoot); " + sdkSrc + "internal/apiform/encoder.go#L26",
	}
)

// dSDKSettingsPlacementPanic is a cloudflare-go defect, not a flarefake one: flarefake sends
// what the live API sent (recordings 0065 and 0091 both carry "placement": {}). It stays a
// Discrepancy so that bumping the SDK past the fix un-skips it.
var dSDKSettingsPlacementPanic = harness.Discrepancy{
	ID: "SDK-SETTINGS-PLACEMENT-PANIC", Client: SDKVersion,
	Summary: "client defect: Workers.Scripts.ScriptAndVersionSettings.Get panics (reflect: call of reflect.Value.SetString on struct Value) decoding \"placement\": {}, " +
		"which flarefake and the live API (recordings 0065, 0091) both send for a script without placement. Operator code must not use this SDK call.",
	Evidence: sdkSrc + "workers/scriptscriptandversionsetting.go#L8368-L8375; " + sdkSrc + "internal/apijson/port.go#L84",
}

// noPanic runs fn and turns a panic into an error.
func noPanic(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// knownSDKDecodeGaps are decode problems that are the SDK's (or the pinned spec's), not
// flarefake's: the live API sends the same thing flarefake does.
var knownSDKDecodeGaps = []struct {
	match  *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`^Tunnels\.Cloudflared\.Configurations\.(Update|Get): required but missing/null: config\.ingress\[\d+\]\.hostname$`),
		"cloudflare-go marks ingress[].hostname required, but the live API returns the catch-all rule without one (recordings 0045, 0046)"},
}

func newClient(f *harness.Fake) *cloudflare.Client {
	return cloudflare.NewClient(
		option.WithBaseURL(f.API),
		option.WithAPIToken(harness.Token),
		option.WithMaxRetries(0), // show the emulator's first answer, not a retried one
		option.WithRequestTimeout(30*time.Second),
	)
}

// decoded checks one SDK call: no error, and a response whose SDK-required fields are all
// present and every present field decodes into the SDK's type. It logs the optional fields
// flarefake leaves out and the fields the SDK does not know, for the report. It returns false
// when the step failed (and has already reported it).
func decoded(t *testing.T, what string, v any, err error) bool {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return false
	}
	r := Inspect(v)
	bad := 0
problems:
	for _, p := range r.Problems() {
		for _, g := range knownSDKDecodeGaps {
			if g.match.MatchString(what + ": " + p) {
				t.Logf("%s: %s (known SDK gap: %s)", what, p, g.reason)
				continue problems
			}
		}
		t.Errorf("%s: %s", what, p)
		bad++
	}
	if len(r.OptionalMissing) > 0 || len(r.Extra) > 0 {
		t.Logf("%s: optional fields not sent: %v; fields unknown to the SDK: %v", what, r.OptionalMissing, r.Extra)
	}
	return bad == 0
}

// autoPager is what every cloudflare-go ListAutoPaging returns.
type autoPager[T any] interface {
	Next() bool
	Current() T
	Err() error
}

// maxListItems bounds an auto-pager: the tests create at most a few resources, so more items
// mean the SDK's pagination never saw a last page.
const maxListItems = 25

// listItems drains it (at most maxListItems items) and returns the items, or an error when
// the pager failed or never ended.
func listItems[T any](it autoPager[T]) ([]T, error) {
	var items []T
	for it.Next() {
		items = append(items, it.Current())
		if len(items) > maxListItems {
			return items, fmt.Errorf("pagination never ended: more than %d items (the same page repeated?)", maxListItems)
		}
	}
	return items, it.Err()
}

// list checks a ListAutoPaging call: it ends, every item decodes (see decoded), and, when
// want >= 0, it yields want items.
func list[T any](t *testing.T, what string, it autoPager[T], want int) []T {
	t.Helper()
	items, err := listItems(it)
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return items
	}
	for i, v := range items {
		decoded(t, fmt.Sprintf("%s[%d]", what, i), v, nil)
	}
	if want >= 0 && len(items) != want {
		t.Errorf("%s: %d items, want %d", what, len(items), want)
	}
	return items
}

// specUpload builds a Workers script upload the way the pinned spec, recording 0036 and
// wrangler shape it: a JSON "metadata" part and one part per module, named after it.
func specUpload(t *testing.T, metadata map[string]any, module, src string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	meta, _ := json.Marshal(metadata)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(meta)
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, module, module))
	h.Set("Content-Type", "application/javascript+module")
	pw, _ = mw.CreatePart(h)
	_, _ = pw.Write([]byte(src))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// apiStatus returns the HTTP status of a cloudflare-go API error (0 if err is not one).
func apiStatus(err error) int {
	var e *cloudflare.Error
	if errors.As(err, &e) {
		return e.StatusCode
	}
	return 0
}

func TestCloudflareGo(t *testing.T) {
	f := harness.Start(t, harness.Options{})
	c := newClient(f)
	ctx := context.Background()

	t.Run("tokens", func(t *testing.T) {
		v, err := c.User.Tokens.Verify(ctx)
		if decoded(t, "user.Tokens.Verify", v, err) && v.Status != user.TokenVerifyResponseStatusActive {
			t.Errorf("token status %q, want active", v.Status)
		}
	})

	t.Run("kv", func(t *testing.T) {
		ns, err := c.KV.Namespaces.New(ctx, kv.NamespaceNewParams{AccountID: acct, Title: cloudflare.F("flare-diff-kv")})
		if !decoded(t, "KV.Namespaces.New", ns, err) {
			return
		}
		if ns.ID == "" || ns.Title != "flare-diff-kv" {
			t.Errorf("created namespace %+v", ns)
		}
		got, err := c.KV.Namespaces.Get(ctx, ns.ID, kv.NamespaceGetParams{AccountID: acct})
		decoded(t, "KV.Namespaces.Get", got, err)
		upd, err := c.KV.Namespaces.Update(ctx, ns.ID, kv.NamespaceUpdateParams{AccountID: acct, Title: cloudflare.F("flare-diff-kv2")})
		if decoded(t, "KV.Namespaces.Update", upd, err) && upd.Title != "flare-diff-kv2" {
			t.Errorf("renamed title %q", upd.Title)
		}
		list(t, "KV.Namespaces.List", c.KV.Namespaces.ListAutoPaging(ctx, kv.NamespaceListParams{AccountID: acct}), 1)
		del, err := c.KV.Namespaces.Delete(ctx, ns.ID, kv.NamespaceDeleteParams{AccountID: acct})
		decoded(t, "KV.Namespaces.Delete", del, err)
		if _, err := c.KV.Namespaces.Get(ctx, ns.ID, kv.NamespaceGetParams{AccountID: acct}); apiStatus(err) != http.StatusNotFound {
			t.Errorf("Get after delete: %v, want a 404 API error", err)
		}
	})

	t.Run("queues", func(t *testing.T) {
		q, err := c.Queues.New(ctx, queues.QueueNewParams{AccountID: acct, QueueName: cloudflare.F("flare-diff-q")})
		if !decoded(t, "Queues.New", q, err) {
			return
		}
		got, err := c.Queues.Get(ctx, q.QueueID, queues.QueueGetParams{AccountID: acct})
		decoded(t, "Queues.Get", got, err)
		ed, err := c.Queues.Edit(ctx, q.QueueID, queues.QueueEditParams{AccountID: acct,
			Queue: queues.QueueParam{Settings: cloudflare.F(queues.QueueSettingsParam{DeliveryDelay: cloudflare.F(5.0)})}})
		if decoded(t, "Queues.Edit", ed, err) && ed.Settings.DeliveryDelay != 5 {
			t.Errorf("edited delivery_delay %v", ed.Settings.DeliveryDelay)
		}
		list(t, "Queues.List", c.Queues.ListAutoPaging(ctx, queues.QueueListParams{AccountID: acct}), 1)
		del, err := c.Queues.Delete(ctx, q.QueueID, queues.QueueDeleteParams{AccountID: acct})
		decoded(t, "Queues.Delete", del, err)
		if _, err := c.Queues.Get(ctx, q.QueueID, queues.QueueGetParams{AccountID: acct}); apiStatus(err) != http.StatusNotFound {
			t.Errorf("Get after delete: %v, want a 404 API error", err)
		}
	})

	t.Run("d1", func(t *testing.T) {
		db, err := c.D1.Database.New(ctx, d1.DatabaseNewParams{AccountID: acct, Name: cloudflare.F("flare-diff-d1")})
		if !decoded(t, "D1.Database.New", db, err) {
			return
		}
		got, err := c.D1.Database.Get(ctx, db.UUID, d1.DatabaseGetParams{AccountID: acct})
		decoded(t, "D1.Database.Get", got, err)
		list(t, "D1.Database.List", c.D1.Database.ListAutoPaging(ctx, d1.DatabaseListParams{AccountID: acct}), 1)
		del, err := c.D1.Database.Delete(ctx, db.UUID, d1.DatabaseDeleteParams{AccountID: acct})
		decoded(t, "D1.Database.Delete", del, err)
		if _, err := c.D1.Database.Get(ctx, db.UUID, d1.DatabaseGetParams{AccountID: acct}); apiStatus(err) != http.StatusNotFound {
			t.Errorf("Get after delete: %v, want a 404 API error", err)
		}
	})

	// R2 buckets in the EU jurisdiction: the SDK sends cf-r2-jurisdiction on every call and the
	// storage class of Edit in cf-r2-storage-class with no body
	// (cloudflare/cloudflare-go@3da6607:r2/bucket.go#L121-L145), as the operator does.
	t.Run("r2", func(t *testing.T) {
		const name = "flare-diff-r2"
		b, err := c.R2.Buckets.New(ctx, r2.BucketNewParams{AccountID: acct, Name: cloudflare.F(name),
			StorageClass:     cloudflare.F(r2.BucketNewParamsStorageClassInfrequentAccess),
			CfR2Jurisdiction: cloudflare.F(r2.BucketNewParamsCfR2JurisdictionEu)})
		if !decoded(t, "R2.Buckets.New", b, err) {
			return
		}
		if b.Name != name || b.StorageClass != r2.BucketStorageClassInfrequentAccess || b.Jurisdiction != r2.BucketJurisdictionEu {
			t.Errorf("created bucket %+v", b)
		}
		got, err := c.R2.Buckets.Get(ctx, name, r2.BucketGetParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketGetParamsCfR2JurisdictionEu)})
		decoded(t, "R2.Buckets.Get", got, err)
		if _, err := c.R2.Buckets.Get(ctx, name, r2.BucketGetParams{AccountID: acct}); apiStatus(err) != http.StatusNotFound {
			t.Errorf("Get without the jurisdiction: %v, want a 404 API error", err)
		}
		ed, err := c.R2.Buckets.Edit(ctx, name, r2.BucketEditParams{AccountID: acct,
			StorageClass:     cloudflare.F(r2.BucketEditParamsCfR2StorageClassStandard),
			CfR2Jurisdiction: cloudflare.F(r2.BucketEditParamsCfR2JurisdictionEu)})
		if decoded(t, "R2.Buckets.Edit", ed, err) && ed.StorageClass != r2.BucketStorageClassStandard {
			t.Errorf("edited storage class %q", ed.StorageClass)
		}
		ls, err := c.R2.Buckets.List(ctx, r2.BucketListParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketListParamsCfR2JurisdictionEu)})
		if decoded(t, "R2.Buckets.List", ls, err) && (len(ls.Buckets) != 1 || ls.Buckets[0].Name != name) {
			t.Errorf("EU buckets %+v", ls.Buckets)
		}
		if _, err := c.R2.Buckets.CORS.Update(ctx, name, r2.BucketCORSUpdateParams{AccountID: acct,
			CfR2Jurisdiction: cloudflare.F(r2.BucketCORSUpdateParamsCfR2JurisdictionEu),
			Rules: cloudflare.F([]r2.BucketCORSUpdateParamsRule{{
				Allowed: cloudflare.F(r2.BucketCORSUpdateParamsRulesAllowed{
					Methods: cloudflare.F([]r2.BucketCORSUpdateParamsRulesAllowedMethod{r2.BucketCORSUpdateParamsRulesAllowedMethodGet}),
					Origins: cloudflare.F([]string{"https://example.com"})}),
				MaxAgeSeconds: cloudflare.F(3600.0)}})}); err != nil {
			t.Errorf("R2.Buckets.CORS.Update: %v", err)
		}
		cors, err := c.R2.Buckets.CORS.Get(ctx, name, r2.BucketCORSGetParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketCORSGetParamsCfR2JurisdictionEu)})
		if decoded(t, "R2.Buckets.CORS.Get", cors, err) && (len(cors.Rules) != 1 || len(cors.Rules[0].Allowed.Origins) != 1 || cors.Rules[0].MaxAgeSeconds != 3600) {
			t.Errorf("CORS rules %+v", cors.Rules)
		}
		if _, err := c.R2.Buckets.CORS.Delete(ctx, name, r2.BucketCORSDeleteParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketCORSDeleteParamsCfR2JurisdictionEu)}); err != nil {
			t.Errorf("R2.Buckets.CORS.Delete: %v", err)
		}
		if _, err := c.R2.Buckets.Delete(ctx, name, r2.BucketDeleteParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketDeleteParamsCfR2JurisdictionEu)}); err != nil {
			t.Errorf("R2.Buckets.Delete: %v", err)
		}
		if _, err := c.R2.Buckets.Get(ctx, name, r2.BucketGetParams{AccountID: acct, CfR2Jurisdiction: cloudflare.F(r2.BucketGetParamsCfR2JurisdictionEu)}); apiStatus(err) != http.StatusNotFound {
			t.Errorf("Get after delete: %v, want a 404 API error", err)
		}
	})

	var tunnelID string
	t.Run("tunnels", func(t *testing.T) {
		tun, err := c.ZeroTrust.Tunnels.Cloudflared.New(ctx, zero_trust.TunnelCloudflaredNewParams{AccountID: acct,
			Name: cloudflare.F("flare-diff-tunnel"), ConfigSrc: cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcCloudflare)})
		if !decoded(t, "Tunnels.Cloudflared.New", tun, err) {
			return
		}
		tunnelID = tun.ID
		got, err := c.ZeroTrust.Tunnels.Cloudflared.Get(ctx, tun.ID, zero_trust.TunnelCloudflaredGetParams{AccountID: acct})
		decoded(t, "Tunnels.Cloudflared.Get", got, err)
		tok, err := c.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tun.ID, zero_trust.TunnelCloudflaredTokenGetParams{AccountID: acct})
		if err != nil || tok == nil || *tok == "" {
			t.Errorf("Tunnels.Cloudflared.Token.Get: %v %v", tok, err)
		}
		list(t, "Tunnels.Cloudflared.List", c.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, zero_trust.TunnelCloudflaredListParams{AccountID: acct, IsDeleted: cloudflare.F(false)}), 1)
		// Connections, with a connector attached through the flarefake control API.
		if st, _ := f.Control(http.MethodPost, "/accounts/"+harness.AccountID+"/tunnels/"+tun.ID+"/connect", `{"replicas":1,"connections":2}`); st != http.StatusOK {
			t.Fatalf("connect: %d", st)
		}
		conns, err := c.ZeroTrust.Tunnels.Cloudflared.Connections.Get(ctx, tun.ID, zero_trust.TunnelCloudflaredConnectionGetParams{AccountID: acct})
		if decoded(t, "Tunnels.Cloudflared.Connections.Get", conns, err) && len(conns.Result) != 1 {
			t.Errorf("connections: %d clients, want 1", len(conns.Result))
		}
		cfg, err := c.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, tun.ID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{AccountID: acct,
			Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
				// The shape of recording 0045: a hostname rule and the required catch-all.
				Ingress: cloudflare.F([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
					{Hostname: cloudflare.F("a.flare-diff.invalid"), Service: cloudflare.F("http://127.0.0.1:18080")},
					{Service: cloudflare.F("http_status:404")},
				}),
			})})
		decoded(t, "Tunnels.Cloudflared.Configurations.Update", cfg, err)
		gcfg, err := c.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, tun.ID, zero_trust.TunnelCloudflaredConfigurationGetParams{AccountID: acct})
		if decoded(t, "Tunnels.Cloudflared.Configurations.Get", gcfg, err) &&
			(len(gcfg.Config.Ingress) != 2 || gcfg.Config.Ingress[0].Hostname != "a.flare-diff.invalid" || gcfg.Version != cfg.Version) {
			t.Errorf("configuration read back: version %v, ingress %+v", gcfg.Version, gcfg.Config.Ingress)
		}
	})

	t.Run("virtual-networks", func(t *testing.T) {
		vn, err := c.ZeroTrust.Networks.VirtualNetworks.New(ctx, zero_trust.NetworkVirtualNetworkNewParams{AccountID: acct, Name: cloudflare.F("flare-diff-vnet")})
		if !decoded(t, "Networks.VirtualNetworks.New", vn, err) {
			return
		}
		list(t, "Networks.VirtualNetworks.List", c.ZeroTrust.Networks.VirtualNetworks.ListAutoPaging(ctx, zero_trust.NetworkVirtualNetworkListParams{AccountID: acct}), -1)
		del, err := c.ZeroTrust.Networks.VirtualNetworks.Delete(ctx, vn.ID, zero_trust.NetworkVirtualNetworkDeleteParams{AccountID: acct})
		decoded(t, "Networks.VirtualNetworks.Delete", del, err)
	})

	t.Run("workers-vpc", func(t *testing.T) {
		if tunnelID == "" {
			t.Skip("needs the tunnel from the tunnels step")
		}
		svc, err := c.Connectivity.Directory.Services.New(ctx, connectivity.DirectoryServiceNewParams{AccountID: acct,
			Body: connectivity.DirectoryServiceNewParamsBody{
				Name: cloudflare.F("flare-diff-vpc"), Type: cloudflare.F(connectivity.DirectoryServiceNewParamsBodyTypeHTTP),
				Host:     cloudflare.F[any](map[string]any{"ipv4": "10.0.0.5", "network": map[string]any{"tunnel_id": tunnelID}}),
				HTTPPort: cloudflare.F[int64](8080),
			}})
		if !decoded(t, "Connectivity.Directory.Services.New", svc, err) {
			return
		}
		got, err := c.Connectivity.Directory.Services.Get(ctx, svc.ServiceID, connectivity.DirectoryServiceGetParams{AccountID: acct})
		decoded(t, "Connectivity.Directory.Services.Get", got, err)
		page, err := c.Connectivity.Directory.Services.List(ctx, connectivity.DirectoryServiceListParams{AccountID: acct})
		if decoded(t, "Connectivity.Directory.Services.List", page, err) && len(page.Result) != 1 {
			t.Errorf("Connectivity.Directory.Services.List: %d items, want 1", len(page.Result))
		}
		// ListAutoPaging asks for page N+1 until a page is empty (no result_info, as in 0112).
		list(t, "Connectivity.Directory.Services.List", c.Connectivity.Directory.Services.ListAutoPaging(ctx, connectivity.DirectoryServiceListParams{AccountID: acct}), 1)
		if err := c.Connectivity.Directory.Services.Delete(ctx, svc.ServiceID, connectivity.DirectoryServiceDeleteParams{AccountID: acct}); err != nil {
			t.Errorf("Connectivity.Directory.Services.Delete: %v", err)
		}
	})

	t.Run("tunnel-delete", func(t *testing.T) {
		if tunnelID == "" {
			t.Skip("needs the tunnel from the tunnels step")
		}
		_, _ = f.Control(http.MethodPost, "/accounts/"+harness.AccountID+"/tunnels/"+tunnelID+"/disconnect", "")
		del, err := c.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{AccountID: acct})
		decoded(t, "Tunnels.Cloudflared.Delete", del, err)
	})

	t.Run("resource-tagging", func(t *testing.T) {
		ns, err := c.KV.Namespaces.New(ctx, kv.NamespaceNewParams{AccountID: acct, Title: cloudflare.F("flare-diff-tagged")})
		if err != nil {
			t.Fatal(err)
		}
		up, err := c.ResourceTagging.AccountTags.Update(ctx, resource_tagging.AccountTagUpdateParams{AccountID: acct,
			Body: resource_tagging.AccountTagUpdateParamsBody{
				ResourceID: cloudflare.F(ns.ID), ResourceType: cloudflare.F(resource_tagging.AccountTagUpdateParamsBodyResourceTypeKVNamespace),
				Tags: cloudflare.F[any](map[string]string{"flare.dev/owner": "diff"}),
			}})
		decoded(t, "ResourceTagging.AccountTags.Update", up, err)
		got, err := c.ResourceTagging.AccountTags.Get(ctx, resource_tagging.AccountTagGetParams{AccountID: acct,
			ResourceID: cloudflare.F(ns.ID), ResourceType: cloudflare.F(resource_tagging.AccountTagGetParamsResourceTypeKVNamespace)})
		decoded(t, "ResourceTagging.AccountTags.Get", got, err)
		list(t, "ResourceTagging.List", c.ResourceTagging.ListAutoPaging(ctx, resource_tagging.ResourceTaggingListParams{AccountID: acct}), 1)
	})

	t.Run("workers", func(t *testing.T) {
		kvns, err := c.KV.Namespaces.New(ctx, kv.NamespaceNewParams{AccountID: acct, Title: cloudflare.F("flare-diff-worker-kv")})
		if err != nil {
			t.Fatal(err)
		}
		const name = "flare-diff-sdk-worker"
		src := "export default {\n  async fetch(req, env) {\n    return new Response('hi');\n  },\n};\n"
		params := workers.ScriptUpdateParams{AccountID: acct,
			Metadata: cloudflare.F(workers.ScriptUpdateParamsMetadata{
				MainModule:        cloudflare.F("index.js"),
				CompatibilityDate: cloudflare.F("2026-09-01"),
				Bindings: cloudflare.F([]workers.ScriptUpdateParamsMetadataBindingUnion{
					workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindKVNamespace{
						Name: cloudflare.F("KV"), NamespaceID: cloudflare.F(kvns.ID),
						Type: cloudflare.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindKVNamespaceTypeKVNamespace),
					},
					workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindPlainText{
						Name: cloudflare.F("GREETING"), Text: cloudflare.F("hi"),
						Type: cloudflare.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindPlainTextTypePlainText),
					},
				}),
			}),
			Files: cloudflare.F([]io.Reader{cloudflare.FileParam(strings.NewReader(src), "index.js", "application/javascript+module").Value}),
		}
		dSDKWorkersUploadForm.Check(t, func() error {
			_, err := c.Workers.Scripts.Update(ctx, name+"-form", params)
			return err
		})
		// Upload through the SDK method, with the body replaced by the multipart shape of the
		// spec and recording 0036 (a JSON "metadata" part plus one part per module), so the
		// response decoding and the rest of the flow are still covered.
		body, ctype := specUpload(t, map[string]any{
			"main_module": "index.js", "compatibility_date": "2026-09-01",
			"bindings": []map[string]any{
				{"type": "kv_namespace", "name": "KV", "namespace_id": kvns.ID},
				{"type": "plain_text", "name": "GREETING", "text": "hi"},
			},
		}, "index.js", src)
		up, err := c.Workers.Scripts.Update(ctx, name, params, option.WithRequestBody(ctype, body))
		if !decoded(t, "Workers.Scripts.Update", up, err) {
			return
		}
		if len(up.Handlers) != 1 || up.Handlers[0] != "fetch" {
			t.Errorf("upload handlers %v, want [fetch]", up.Handlers)
		}
		// A one-line module (the handler is not at the start of a line) reports [fetch] too.
		oneLine := "export default { async fetch(req, env) { return new Response('hi'); } };\n"
		body1, ctype1 := specUpload(t, map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01"}, "index.js", oneLine)
		if up1, err := c.Workers.Scripts.Update(ctx, name+"-oneline", params, option.WithRequestBody(ctype1, body1)); !decoded(t, "Workers.Scripts.Update (one line)", up1, err) {
			return
		} else if len(up1.Handlers) != 1 || up1.Handlers[0] != "fetch" {
			t.Errorf("handlers %v for a one-line fetch module, want [fetch]", up1.Handlers)
		}
		list(t, "Workers.Scripts.List", c.Workers.Scripts.ListAutoPaging(ctx, workers.ScriptListParams{AccountID: acct}), -1)
		dSDKSettingsPlacementPanic.Check(t, func() error {
			return noPanic(func() error {
				st, err := c.Workers.Scripts.ScriptAndVersionSettings.Get(ctx, name, workers.ScriptScriptAndVersionSettingGetParams{AccountID: acct})
				if decoded(t, "Workers.Scripts.ScriptAndVersionSettings.Get", st, err) && len(st.Bindings) != 2 {
					t.Errorf("settings bindings %+v, want KV and GREETING", st.Bindings)
				}
				return nil
			})
		})
		vs, err := c.Workers.Scripts.Versions.List(ctx, name, workers.ScriptVersionListParams{AccountID: acct})
		decoded(t, "Workers.Scripts.Versions.List", vs, err)
		ds, err := c.Workers.Scripts.Deployments.List(ctx, name, workers.ScriptDeploymentListParams{AccountID: acct})
		decoded(t, "Workers.Scripts.Deployments.List", ds, err)
		sub, err := c.Workers.Scripts.Subdomain.New(ctx, name, workers.ScriptSubdomainNewParams{AccountID: acct, Enabled: cloudflare.F(true)})
		decoded(t, "Workers.Scripts.Subdomain.New", sub, err)
		gsub, err := c.Workers.Scripts.Subdomain.Get(ctx, name, workers.ScriptSubdomainGetParams{AccountID: acct})
		if decoded(t, "Workers.Scripts.Subdomain.Get", gsub, err) && !gsub.Enabled {
			t.Errorf("subdomain not enabled after POST enabled=true")
		}
		asub, err := c.Workers.Subdomains.Get(ctx, workers.SubdomainGetParams{AccountID: acct})
		decoded(t, "Workers.Subdomains.Get", asub, err)
		del, err := c.Workers.Scripts.Delete(ctx, name, workers.ScriptDeleteParams{AccountID: acct})
		decoded(t, "Workers.Scripts.Delete", del, err)
	})

	for _, v := range f.UnexplainedSchemaViolations() {
		t.Errorf("cloudflare-go request broke the pinned spec (new discrepancy?): %s", v)
	}
	if u := f.Unanswered(); len(u) > 0 {
		t.Errorf("cloudflare-go called routes flarefake does not emulate: %v", u)
	}
	harness.Log(t, f.Requests())
	harness.WriteCapture(t, f, "cloudflare-go")
}
