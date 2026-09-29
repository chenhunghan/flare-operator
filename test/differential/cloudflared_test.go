//go:build differential

package differential

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flare.dev/operator/test/differential/harness"
)

// Evidence citations into cloudflared's source use the pinned release, tag 2026.9.3
// ("cloudflare/cloudflared@2026.9.3:<path>#L<n>").

// knownCloudflaredSpecViolation: `route ip add` must send tunnel_id, which the spec both
// requires and marks readOnly in the request body (an unsatisfiable request schema), so the
// live API accepts it. SOURCED (relies): cloudflare/cloudflared@2026.9.3:cfapi/ip_route.go#L75-L88,L160-L175.
const knownCloudflaredSpecViolation = `POST /accounts/` + harness.AccountID + `/teamnet/routes: request body has an error: doesn't match schema: readOnly property "tunnel_id" in request`

// fakeZoneID fills the origin cert's zoneID, which cloudflared requires (decodeOriginCert,
// cloudflared@2026.9.3:credentials/origin_cert.go#L106-L108) but uses only for DNS routes.
const fakeZoneID = "0000000000000000000000000000f00d"

// originCert writes a cloudflared origin certificate (cert.pem) that carries the fake token,
// in the format credentials.OriginCert encodes: a PEM block "ARGO TUNNEL TOKEN" around JSON
// zoneID/accountID/apiToken (cloudflared@2026.9.3:credentials/origin_cert.go#L22-L27,L62-L81).
// It holds no real credential.
func originCert(t *testing.T, dir string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"zoneID": fakeZoneID, "accountID": harness.AccountID, "apiToken": harness.Token})
	file := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "ARGO TUNNEL TOKEN", Bytes: b}), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestCloudflared drives the pinned cloudflared's management-API commands (tunnel create,
// list, info, token, cleanup, delete; vnet add/list/delete; route ip add/show/get/delete)
// against flarefake. It never runs a
// connector (`tunnel run`), so nothing contacts the Cloudflare edge.
func TestCloudflared(t *testing.T) {
	bin := findClient(t, "FLARE_DIFF_CLOUDFLARED",
		filepath.Join(cacheDir(), "cloudflared-"+CloudflaredVersion, "cloudflared"), "cloudflared", true)
	f := harness.Start(t, harness.Options{})
	defer harness.WriteCapture(t, f, "cloudflared-"+CloudflaredVersion)
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		// The hidden --api-url flag (cloudflared@2026.9.3:cmd/cloudflared/tunnel/cmd.go#L737-L743),
		// used for every management call (subcommand_context.go#L68-L90).
		"TUNNEL_API_URL=" + f.API,
		"TUNNEL_ORIGIN_CERT=" + originCert(t, home),
		"NO_AUTOUPDATE=true",
	}
	c := func(t *testing.T, args ...string) result {
		t.Helper()
		mark := f.Mark()
		r := run(t, home, env, bin, append([]string{"tunnel", "--no-autoupdate"}, args...)...)
		logRequests(t, f, mark)
		if r.err != nil {
			t.Errorf("%v", r.failed())
		}
		return r
	}
	if r := run(t, home, env, bin, "--version"); !strings.Contains(r.stdout, CloudflaredVersion) {
		t.Logf("not the pinned cloudflared %s: %s", CloudflaredVersion, strings.TrimSpace(r.stdout))
	}

	const name = "flare-diff-tunnel"
	creds := filepath.Join(home, "creds.json")
	var tunnelID string
	t.Run("create", func(t *testing.T) {
		c(t, "create", "--credentials-file", creds, name)
		var cr struct{ AccountTag, TunnelSecret, TunnelID string }
		b, err := os.ReadFile(creds)
		if err != nil || json.Unmarshal(b, &cr) != nil || cr.AccountTag != harness.AccountID || cr.TunnelID == "" || cr.TunnelSecret == "" {
			t.Fatalf("credentials file %s: %s (%v)", creds, b, err)
		}
		tunnelID = cr.TunnelID
		var tun struct{ Name string }
		if get(t, f, acctPath("/cfd_tunnel/"+tunnelID), &tun) != http.StatusOK || tun.Name != name {
			t.Errorf("flarefake tunnel %s: %+v", tunnelID, tun)
		}
	})
	if tunnelID == "" {
		t.FailNow()
	}
	t.Run("list", func(t *testing.T) {
		var list []struct{ ID, Name string }
		jsonOut(t, c(t, "list", "--output", "json"), &list)
		if len(list) != 1 || list[0].ID != tunnelID || list[0].Name != name {
			t.Errorf("tunnel list %+v", list)
		}
	})
	t.Run("info-with-connector", func(t *testing.T) {
		if st, body := f.Control(http.MethodPost, "/accounts/"+harness.AccountID+"/tunnels/"+tunnelID+"/connect", `{"replicas":1,"connections":2}`); st != http.StatusOK {
			t.Fatalf("connect: %d %s", st, body)
		}
		var info struct {
			ID    string
			Conns []struct {
				ID    string
				Conns []struct {
					ColoName string `json:"colo_name"`
					ID       string
				}
			}
		}
		jsonOut(t, c(t, "info", "--output", "json", name), &info)
		if info.ID != tunnelID || len(info.Conns) != 1 || len(info.Conns[0].Conns) != 2 || info.Conns[0].Conns[0].ColoName == "" {
			t.Errorf("tunnel info %+v, want 1 connector with 2 connections", info)
		}
	})
	t.Run("token", func(t *testing.T) {
		r := c(t, "token", name)
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.stdout))
		var tok struct{ A, T, S string }
		if err != nil || json.Unmarshal(raw, &tok) != nil || tok.A != harness.AccountID || tok.T != tunnelID || tok.S == "" {
			t.Errorf("token %q does not decode to {a: account, t: tunnel, s: secret}: %s %v", r.stdout, raw, err)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		c(t, "cleanup", name)
		var conns []any
		if get(t, f, acctPath("/cfd_tunnel/"+tunnelID+"/connections"), &conns); len(conns) != 0 {
			t.Errorf("connections after cleanup: %v", conns)
		}
	})
	t.Run("vnet", func(t *testing.T) {
		c(t, "vnet", "add", "flare-diff-vnet")
		var list []struct {
			Name      string
			IsDefault bool `json:"is_default_network"`
		}
		jsonOut(t, c(t, "vnet", "list", "--output", "json"), &list)
		names := fmt.Sprint(list)
		if len(list) != 2 || !strings.Contains(names, "flare-diff-vnet") || !strings.Contains(names, "default") {
			t.Errorf("vnet list %+v, want the default network and flare-diff-vnet", list)
		}
		c(t, "vnet", "delete", "flare-diff-vnet")
	})
	t.Run("route-ip", func(t *testing.T) {
		const network = "10.9.0.0/16"
		c(t, "route", "ip", "add", network, name, "flare-diff route")
		var routes []struct {
			ID         string
			Network    string
			TunnelID   string `json:"tunnel_id"`
			TunnelName string `json:"tunnel_name"`
			Comment    string
		}
		jsonOut(t, c(t, "route", "ip", "show", "--output", "json"), &routes)
		if len(routes) != 1 || routes[0].Network != network || routes[0].TunnelID != tunnelID || routes[0].TunnelName != name || routes[0].Comment != "flare-diff route" {
			t.Fatalf("route ip show %+v, want %s over %s", routes, network, name)
		}
		if r := c(t, "route", "ip", "get", "10.9.1.2"); !strings.Contains(r.stdout, network) {
			t.Errorf("route ip get 10.9.1.2 does not name %s:\n%s", network, r.stdout)
		}
		c(t, "route", "ip", "delete", network) // by network: cloudflared looks the route ID up first
		var live []any
		if get(t, f, acctPath("/teamnet/routes?is_deleted=false"), &live); len(live) != 0 {
			t.Errorf("routes after delete: %v", live)
		}
	})
	t.Run("delete", func(t *testing.T) {
		c(t, "delete", name)
		var list []any
		if get(t, f, acctPath("/cfd_tunnel?is_deleted=false"), &list); len(list) != 0 {
			t.Errorf("tunnels after delete: %v", list)
		}
	})
	t.Run("spec-and-routes", func(t *testing.T) {
		for _, v := range f.UnexplainedSchemaViolations(knownCloudflaredSpecViolation) {
			t.Errorf("cloudflared request broke the pinned spec (new discrepancy?): %s", v)
		}
		for _, u := range f.Unanswered() {
			t.Errorf("cloudflared called a route flarefake does not emulate (new discrepancy?): %s", u)
		}
	})
}
