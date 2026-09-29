package tunnelnet

import (
	"reflect"
	"testing"

	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
)

func TestParseServiceFQDN(t *testing.T) {
	for _, tc := range []struct {
		host, domain string
		ns, svc      string
		ok           bool
	}{
		{"marker.flare-spike.svc.cluster.local", "cluster.local", "flare-spike", "marker", true},
		{"marker.flare-spike.svc.cluster.local.", "cluster.local", "flare-spike", "marker", true},
		{"Marker.NS.svc.cluster.local", "cluster.local", "ns", "marker", true},
		{"pod-0.db.ns.svc.cluster.local", "cluster.local", "ns", "db", true},
		{"marker.ns.svc.k8s.example", "k8s.example", "ns", "marker", true},
		{"marker.ns.svc.k8s.example", "cluster.local", "", "", false},
		{"marker.flare-spike", "cluster.local", "", "", false},
		{"api.example.com", "cluster.local", "", "", false},
		{"a.b.c.d.svc.cluster.local", "cluster.local", "", "", false},
		{".ns.svc.cluster.local", "cluster.local", "", "", false},
	} {
		ns, svc, ok := ParseServiceFQDN(tc.host, tc.domain)
		if ns != tc.ns || svc != tc.svc || ok != tc.ok {
			t.Errorf("%s (%s): got %q %q %v", tc.host, tc.domain, ns, svc, ok)
		}
	}
}

func TestBackendKeys(t *testing.T) {
	s := func(v string) *string { return &v }
	vs := &workersvpcv1alpha1.VPCService{Spec: workersvpcv1alpha1.VPCServiceSpec{ForProvider: &workersvpcv1alpha1.VPCServiceParameters{
		Host: workersvpcv1alpha1.VPCServiceHost{Hostname: s("web.app.svc.cluster.local"),
			ResolverNetwork: &workersvpcv1alpha1.VPCServiceResolverNetwork{ResolverIPs: []string{"10.96.0.10"}}},
	}}}
	got := BackendKeys("cluster.local")(vs)
	if want := []string{"svc:app/web", "ip:10.96.0.10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := BackendKeys("cluster.local")(&workersvpcv1alpha1.VPCService{}); got != nil {
		t.Errorf("no forProvider: %v", got)
	}
	if (ClusterDNS{}).WithDefaults() != (ClusterDNS{Namespace: "kube-system", Name: "kube-dns", Domain: "cluster.local"}) {
		t.Error("defaults")
	}
}
