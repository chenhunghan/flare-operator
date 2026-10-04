package vpcservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// The observed shape is recording 0176's response (nulls included).
const observed0176 = `{
  "service_id": "01a0ec3a-0c1a-7893-aa05-d5d555c43551", "type": "http", "name": "flare-spike-k8s-2",
  "http_port": 80, "https_port": null,
  "host": {"ipv4": null, "ipv6": null, "hostname": "marker.flare-spike.svc.cluster.local", "network": null,
           "resolver_network": {"tunnel_id": "2c2ebad5-acd8-472f-8d20-bd5b78b34083", "resolver_ips": ["10.96.0.10"]}},
  "created_at": "2026-09-29T08:13:42Z", "updated_at": "2026-09-29T08:13:42Z"}`

func TestMatches(t *testing.T) {
	var have apiService
	if err := json.Unmarshal([]byte(observed0176), &have); err != nil {
		t.Fatal(err)
	}
	s := func(v string) *string { return &v }
	p := func(v int32) *int32 { return &v }
	base := func() *apiService {
		return &apiService{Name: "flare-spike-k8s-2", Type: "http", HTTPPort: p(80),
			Host: apiHost{Hostname: s("marker.flare-spike.svc.cluster.local"),
				ResolverNetwork: &apiNetwork{TunnelID: "2c2ebad5-acd8-472f-8d20-bd5b78b34083", ResolverIPs: []string{"10.96.0.10"}}}}
	}
	if !matches(base(), &have) {
		t.Fatal("identical desired body does not match 0176")
	}
	// The request body of 0176 serializes like the recording's request.
	b, _ := json.Marshal(base())
	want := `{"name":"flare-spike-k8s-2","type":"http","host":{"hostname":"marker.flare-spike.svc.cluster.local","resolver_network":{"tunnel_id":"2c2ebad5-acd8-472f-8d20-bd5b78b34083","resolver_ips":["10.96.0.10"]}},"http_port":80}`
	if string(b) != want {
		t.Errorf("body\n got %s\nwant %s", b, want)
	}
	for name, mut := range map[string]func(*apiService){
		"name":         func(a *apiService) { a.Name = "other" },
		"port":         func(a *apiService) { a.HTTPPort = p(81) },
		"https":        func(a *apiService) { a.HTTPSPort = p(443) },
		"tunnel":       func(a *apiService) { a.Host.ResolverNetwork.TunnelID = "x" },
		"resolver":     func(a *apiService) { a.Host.ResolverNetwork.ResolverIPs = nil },
		"tls":          func(a *apiService) { a.TLSSettings = &apiTLS{CertVerificationMode: "disabled"} },
		"to-ipv4-host": func(a *apiService) { a.Host = apiHost{IPv4: s("10.0.0.1"), Network: &apiNetwork{TunnelID: "x"}} },
	} {
		d := base()
		mut(d)
		if matches(d, &have) {
			t.Errorf("%s: change not detected", name)
		}
	}
	// Unset desired ports do not count as drift.
	d := base()
	d.HTTPPort = nil
	if !matches(d, &have) {
		t.Error("unset http_port treated as drift")
	}
}

func TestIsDuplicateName(t *testing.T) {
	dup := &cfclient.APIError{Status: 400, Errors: []cfclient.ErrorDetail{{Code: 5101,
		Message: "request contained invalid parameters: Service name 'flare-spike-vpc-3' already exists"}}} // 0059
	other := &cfclient.APIError{Status: 400, Errors: []cfclient.ErrorDetail{{Code: 5101,
		Message: "request contained invalid parameters: Tunnel ID Not Found"}}}
	for _, c := range []struct {
		err  error
		want bool
	}{{dup, true}, {fmt.Errorf("create: %w", dup), true}, {other, false}, {errors.New("already exists"), false}} {
		if got := isDuplicateName(c.err); got != c.want {
			t.Errorf("isDuplicateName(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
