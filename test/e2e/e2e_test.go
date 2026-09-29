//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
)

const pokeAnnotation = "e2e.flare.dev/poke"

// objects are the managed objects of one e2e run.
type objects struct {
	secret  *corev1.Secret
	account *cloudflarev1alpha1.CloudflareAccount
	kv      *kvv1alpha1.KVNamespace
	queue   *queuesv1alpha1.Queue
	d1      *d1v1alpha1.D1Database
	web     *corev1.Service
	tunnel  *tunnelsv1alpha1.Tunnel
	vpc     *workersvpcv1alpha1.VPCService
	// tunnel2/vpc2 stay until the namespace is deleted (teardown ordering).
	tunnel2 *tunnelsv1alpha1.Tunnel
	vpc2    *workersvpcv1alpha1.VPCService
}

func (o *objects) managed() []commonv1alpha1.Managed {
	return []commonv1alpha1.Managed{o.kv, o.queue, o.d1, o.tunnel, o.vpc, o.tunnel2, o.vpc2}
}

// TestEndToEnd runs the whole flow against the installed chart. The steps share state and
// run in order; the first failing step stops the run (the namespace is still torn down).
func TestEndToEnd(t *testing.T) {
	s := newSuite(t)
	s.ns = "flare-e2e-" + randomHex(3)
	t.Logf("namespace %s, account %s, flarefake %s", s.ns, s.accountID, s.cfg.baseURL())
	o := &objects{}
	t.Cleanup(func() { s.cleanup() })

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"RBAC", s.testRBAC},
		{"Account", func(t *testing.T) { s.testAccount(t, o) }},
		{"CreateResources", func(t *testing.T) { s.testCreate(t, o) }},
		{"Idempotency", func(t *testing.T) { s.testIdempotency(t, o) }},
		{"Update", func(t *testing.T) { s.testUpdate(t, o) }},
		{"ForeignOwnerKept", s.testForeignOwner},
		{"DeleteOrder", func(t *testing.T) { s.testDeleteOrder(t, o) }},
		{"AccountProtection", func(t *testing.T) { s.testAccountProtection(t, o) }},
		{"NamespaceTeardown", func(t *testing.T) { s.testNamespaceTeardown(t, o) }},
		{"ManagerHealth", s.testManagerHealth},
	}
	for _, st := range steps {
		if !t.Run(st.name, func(t *testing.T) { s.t = t; st.run(t) }) {
			s.t = t
			t.Fatalf("step %s failed; stopping", st.name)
		}
	}
	s.t = t
}

// cleanup deletes the test namespace if a step left it behind. Stuck finalizers are reported
// (and then removed so the cluster is left clean).
func (s *suite) cleanup() {
	t := s.t
	var ns corev1.Namespace
	if err := s.c.Get(s.ctx(), client.ObjectKey{Name: s.ns}, &ns); apierrors.IsNotFound(err) {
		return
	}
	t.Logf("cleanup: deleting namespace %s", s.ns)
	_ = s.c.Delete(s.ctx(), &ns)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := s.c.Get(s.ctx(), client.ObjectKey{Name: s.ns}, &ns); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("namespace %s still exists after 3m; removing finalizers", s.ns)
	lists := []client.ObjectList{&tunnelsv1alpha1.TunnelList{}, &workersvpcv1alpha1.VPCServiceList{}, &kvv1alpha1.KVNamespaceList{},
		&queuesv1alpha1.QueueList{}, &d1v1alpha1.D1DatabaseList{}, &cloudflarev1alpha1.CloudflareAccountList{}, &corev1.SecretList{}}
	for _, l := range lists {
		if err := s.c.List(s.ctx(), l, client.InNamespace(s.ns)); err != nil {
			continue
		}
		items, _ := metaItems(l)
		for _, it := range items {
			if len(it.GetFinalizers()) > 0 {
				t.Logf("stuck: %T %s finalizers %v", it, it.GetName(), it.GetFinalizers())
				_ = s.c.Patch(s.ctx(), it, client.RawPatch("application/merge-patch+json", []byte(`{"metadata":{"finalizers":null}}`)))
			}
		}
	}
}

func metaItems(l client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	switch l := l.(type) {
	case *tunnelsv1alpha1.TunnelList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *workersvpcv1alpha1.VPCServiceList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *kvv1alpha1.KVNamespaceList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *queuesv1alpha1.QueueList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *d1v1alpha1.D1DatabaseList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *cloudflarev1alpha1.CloudflareAccountList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *corev1.SecretList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	}
	return out, nil
}

// testRBAC checks, with SubjectAccessReviews for the manager's ServiceAccount, every rule of
// the generated ClusterRole (config/rbac/role.yaml) and the leader-election lease.
func (s *suite) testRBAC(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(b, &role); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("system:serviceaccount:%s:%s", s.cfg.OperatorNS, s.cfg.Release)
	check := func(ns, group, resource, verb string) {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + s.cfg.OperatorNS, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns, Group: group, Resource: strings.Split(resource, "/")[0], Subresource: subresource(resource), Verb: verb,
			},
		}}
		if err := s.c.Create(s.ctx(), sar); err != nil {
			t.Fatalf("SubjectAccessReview: %v", err)
		}
		if !sar.Status.Allowed {
			t.Errorf("%s may not %s %s.%s in %q: %s", user, verb, resource, group, ns, sar.Status.Reason)
		}
	}
	n := 0
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					check("", g, res, v) // cluster-wide (the manager watches every namespace)
					n++
				}
			}
		}
	}
	for _, v := range []string{"get", "create", "update"} {
		check(s.cfg.OperatorNS, "coordination.k8s.io", "leases", v)
	}
	t.Logf("%d ClusterRole permissions allowed for %s", n, user)
}

func subresource(r string) string {
	if _, sub, ok := strings.Cut(r, "/"); ok {
		return sub
	}
	return ""
}

// testAccount creates the namespace, the token Secret and a CloudflareAccount pointing at the
// in-cluster flarefake, and waits for it to be Ready.
func (s *suite) testAccount(t *testing.T, o *objects) {
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns, Labels: map[string]string{"flare.dev/e2e": "true"}}})
	o.secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "cf-token"},
		StringData: map[string]string{"token": "e2e-token-" + randomHex(8)}}
	s.create(o.secret)
	o.account = &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "acct"},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID:      s.accountID,
			TokenSecretRef: cloudflarev1alpha1.SecretKeySelector{Name: o.secret.Name, Key: "token"},
			BaseURL:        s.cfg.baseURL(),
		},
	}
	s.create(o.account)
	eventually(t, s.cfg.Timeout, "CloudflareAccount Ready", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.account), o.account); err != nil {
			return false, err.Error()
		}
		r := cond(o.account.Status.Conditions, commonv1alpha1.ConditionReady)
		return r != nil && r.Status == metav1.ConditionTrue, condString(o.account.Status.Conditions)
	})
	if o.account.Status.TokenStatus != "active" {
		t.Errorf("tokenStatus = %q, want active", o.account.Status.TokenStatus)
	}
	// The account holds its token Secret (account-token finalizer).
	eventually(t, 30*time.Second, "token Secret finalizer", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.secret), o.secret); err != nil {
			return false, err.Error()
		}
		return controllerutil.ContainsFinalizer(o.secret, cloudflarev1alpha1.AccountTokenFinalizer), fmt.Sprint(o.secret.Finalizers)
	})
}

func (s *suite) tunnelSpec(name string) *tunnelsv1alpha1.Tunnel {
	return &tunnelsv1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: name},
		Spec: tunnelsv1alpha1.TunnelSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider:  tunnelsv1alpha1.TunnelParameters{Name: "flare-e2e-" + s.ns + "-" + name},
			Connector: tunnelsv1alpha1.ConnectorSpec{
				Image: s.cfg.StubImage, ImagePullPolicy: s.cfg.StubPull, Replicas: ptr.To[int32](1),
			},
			NetworkPolicy: tunnelsv1alpha1.TunnelNetworkPolicy{ExcludeCIDRs: s.cfg.ExcludeCIDRs},
		},
	}
}

func (s *suite) vpcSpec(name, tunnel, host string, port int32) *workersvpcv1alpha1.VPCService {
	return &workersvpcv1alpha1.VPCService{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: name},
		Spec: workersvpcv1alpha1.VPCServiceSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider: &workersvpcv1alpha1.VPCServiceParameters{
				Name:      "flare-e2e-" + s.ns + "-" + name,
				Type:      "http",
				Host:      workersvpcv1alpha1.VPCServiceHost{Hostname: ptr.To(host)},
				HTTPPort:  ptr.To(port),
				TunnelRef: &commonv1alpha1.LocalRef{Name: tunnel},
			},
		},
	}
}

// testCreate creates one object of every kind and checks the Cloudflare and Kubernetes side.
func (s *suite) testCreate(t *testing.T, o *objects) {
	acct := commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete}
	o.kv = &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "kv"},
		Spec: kvv1alpha1.KVNamespaceSpec{ResourceSpec: acct, ForProvider: kvv1alpha1.KVNamespaceParameters{Title: ptr.To("flare-e2e-" + s.ns + "-kv")}}}
	o.queue = &queuesv1alpha1.Queue{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "queue"},
		Spec: queuesv1alpha1.QueueSpec{ResourceSpec: acct, ForProvider: queuesv1alpha1.QueueParameters{
			QueueName: ptr.To("flare-e2e-" + s.ns + "-queue"),
			// delivery_paused is write-only (never read back, 0028/0031/0032): the idempotency
			// step checks that its hash (status.writeOnlyHash) survives a manager restart.
			Settings: &queuesv1alpha1.QueueSettingsParameters{DeliveryDelay: ptr.To[int64](5), MessageRetentionPeriod: ptr.To[int64](7200),
				DeliveryPaused: ptr.To(false)},
		}}}
	o.d1 = &d1v1alpha1.D1Database{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "db"},
		Spec: d1v1alpha1.D1DatabaseSpec{ResourceSpec: acct, ForProvider: d1v1alpha1.D1DatabaseParameters{
			Name: ptr.To("flare-e2e-" + s.ns + "-db"), PrimaryLocationHint: ptr.To("WEUR"),
		}}}
	// A backend Service for the VPC services (no pods needed: the policy selects by labels).
	o.web = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "web"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}}}}
	o.tunnel, o.tunnel2 = s.tunnelSpec("tun"), s.tunnelSpec("tun2")
	host := fmt.Sprintf("web.%s.svc.%s", s.ns, s.cfg.ClusterDomain)
	o.vpc, o.vpc2 = s.vpcSpec("web", "tun", host, 80), s.vpcSpec("web2", "tun2", host, 80)
	s.clearJournal()
	for _, obj := range []client.Object{o.kv, o.queue, o.d1, o.web, o.tunnel, o.tunnel2, o.vpc, o.vpc2} {
		s.create(obj)
	}
	for _, mg := range o.managed() {
		s.waitManaged(mg, 3*time.Minute)
	}
	// Exactly the expected creates: one POST per resource, the queue's settings PATCH right
	// after its create (settings are not part of the create body, 0028), and one owner-tag PUT
	// per taggable resource (VPC services have no Resource Tagging type).
	time.Sleep(3 * time.Second)
	a := "/accounts/" + s.accountID
	want := []string{
		"POST " + a + "/storage/kv/namespaces", "POST " + a + "/queues", "PATCH " + a + "/queues/" + o.queue.Status.ID,
		"POST " + a + "/d1/database", "POST " + a + "/cfd_tunnel", "POST " + a + "/cfd_tunnel",
		"POST " + a + "/connectivity/directory/services", "POST " + a + "/connectivity/directory/services",
	}
	for range 5 {
		want = append(want, "PUT "+a+"/tags")
	}
	got := writes(s.journal())
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("writes while creating = %v\nwant exactly %v", got, want)
	}

	// Cloudflare side.
	var kv struct{ ID, Title string }
	if err := s.cfGet("/storage/kv/namespaces/"+o.kv.Status.ID, &kv); err != nil || kv.Title != *o.kv.Spec.ForProvider.Title {
		t.Errorf("KV namespace in flarefake = %+v, %v", kv, err)
	}
	var q struct {
		QueueName string `json:"queue_name"`
		Settings  struct {
			DeliveryDelay          int64 `json:"delivery_delay"`
			MessageRetentionPeriod int64 `json:"message_retention_period"`
		} `json:"settings"`
	}
	if err := s.cfGet("/queues/"+o.queue.Status.ID, &q); err != nil || q.Settings.DeliveryDelay != 5 || q.Settings.MessageRetentionPeriod != 7200 {
		t.Errorf("queue in flarefake = %+v, %v; want settings 5/7200 (sent after create)", q, err)
	}
	var db struct{ Name, UUID string }
	if err := s.cfGet("/d1/database/"+o.d1.Status.ID, &db); err != nil || db.Name != *o.d1.Spec.ForProvider.Name {
		t.Errorf("D1 database in flarefake = %+v, %v", db, err)
	}
	var kubeDNS corev1.Service
	if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: "kube-system", Name: "kube-dns"}, &kubeDNS); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		vpc *workersvpcv1alpha1.VPCService
		tun *tunnelsv1alpha1.Tunnel
	}{{o.vpc, o.tunnel}, {o.vpc2, o.tunnel2}} {
		var vs struct {
			Host struct {
				Hostname        string `json:"hostname"`
				ResolverNetwork struct {
					TunnelID    string   `json:"tunnel_id"`
					ResolverIPs []string `json:"resolver_ips"`
				} `json:"resolver_network"`
			} `json:"host"`
			HTTPPort int32 `json:"http_port"`
		}
		if err := s.cfGet("/connectivity/directory/services/"+p.vpc.Status.ID, &vs); err != nil {
			t.Fatal(err)
		}
		if vs.Host.ResolverNetwork.TunnelID != p.tun.Status.ID || vs.Host.Hostname != host ||
			!slices.Equal(vs.Host.ResolverNetwork.ResolverIPs, []string{kubeDNS.Spec.ClusterIP}) {
			t.Errorf("VPC service %s in flarefake = %+v; want tunnel %s, hostname %s, resolver %s", p.vpc.Name, vs, p.tun.Status.ID, host, kubeDNS.Spec.ClusterIP)
		}
		if p.vpc.Status.TunnelID != p.tun.Status.ID {
			t.Errorf("VPCService %s status.tunnelID = %q, want %q", p.vpc.Name, p.vpc.Status.TunnelID, p.tun.Status.ID)
		}
	}

	// Tunnel connector objects: token Secret, cloudflared Deployment (stub, ready), egress policy.
	for _, tun := range []*tunnelsv1alpha1.Tunnel{o.tunnel, o.tunnel2} {
		var sec corev1.Secret
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: tun.Status.Connector.TokenSecretName}, &sec); err != nil {
			t.Fatalf("token Secret: %v", err)
		}
		if len(sec.Data["token"]) == 0 || !metav1.IsControlledBy(&sec, tun) {
			t.Errorf("token Secret %s: token %d bytes, controlled %v", sec.Name, len(sec.Data["token"]), metav1.IsControlledBy(&sec, tun))
		}
		var dep appsv1.Deployment
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: tun.Status.Connector.DeploymentName}, &dep); err != nil {
			t.Fatalf("cloudflared Deployment: %v", err)
		}
		if !metav1.IsControlledBy(&dep, tun) || dep.Status.ReadyReplicas != 1 || tun.Status.Connector.ReadyReplicas != 1 {
			t.Errorf("Deployment %s: controlled %v, ready %d (status.connector %+v)", dep.Name, metav1.IsControlledBy(&dep, tun), dep.Status.ReadyReplicas, tun.Status.Connector)
		}
		var np networkingv1.NetworkPolicy
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: tun.Status.NetworkPolicy.Name}, &np); err != nil {
			t.Fatalf("NetworkPolicy: %v", err)
		}
		if !metav1.IsControlledBy(&np, tun) || !allowsPods(&np, s.ns, map[string]string{"app": "web"}, 8080) {
			t.Errorf("NetworkPolicy %s does not allow the web pods on 8080: %+v", np.Name, np.Spec.Egress)
		}
		if len(tun.Status.NetworkPolicy.Backends) != 1 || len(tun.Status.NetworkPolicy.Warnings) != 0 {
			t.Errorf("Tunnel %s status.networkPolicy = %+v", tun.Name, tun.Status.NetworkPolicy)
		}
	}

	// The account is in use.
	if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.account), o.account); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "account-in-use finalizer", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.account), o.account); err != nil {
			return false, err.Error()
		}
		return controllerutil.ContainsFinalizer(o.account, cloudflarev1alpha1.AccountInUseFinalizer), fmt.Sprint(o.account.Finalizers)
	})
	s.checkJournalClean(s.journal())
}

// allowsPods reports whether np has an egress rule to pods sel in namespace ns on TCP port.
func allowsPods(np *networkingv1.NetworkPolicy, ns string, sel map[string]string, port int32) bool {
	for _, r := range np.Spec.Egress {
		portOK := false
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntValue() == int(port) {
				portOK = true
			}
		}
		if !portOK {
			continue
		}
		for _, peer := range r.To {
			if peer.PodSelector == nil || peer.NamespaceSelector == nil {
				continue
			}
			if peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != ns {
				continue
			}
			match := true
			for k, v := range sel {
				if peer.PodSelector.MatchLabels[k] != v {
					match = false
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// testIdempotency asserts that re-reconciling objects in steady state makes no Cloudflare
// writes: first after annotating every object, then after restarting the manager.
func (s *suite) testIdempotency(t *testing.T, o *objects) {
	ids := map[string]string{}
	for _, mg := range o.managed() {
		ids[fmt.Sprintf("%T/%s", mg, mg.GetName())] = mg.GetResourceStatus().ID
	}
	// sawReads waits until every object's resource has been read since the journal was cleared.
	sawReads := func(what string) {
		eventually(t, 3*time.Minute, "a GET of every resource ("+what+")", func() (bool, string) {
			j := s.journal()
			var missing []string
			for k, id := range ids {
				if !slices.ContainsFunc(j, func(e journalEntry) bool { return e.Method == "GET" && strings.Contains(e.Path, id) }) {
					missing = append(missing, k)
				}
			}
			return len(missing) == 0, "not yet read: " + strings.Join(missing, ", ")
		})
		time.Sleep(5 * time.Second) // let any follow-up write of those reconciles land
		j := s.journal()
		if w := writes(j); len(w) > 0 {
			t.Errorf("%s: %d Cloudflare write(s) in steady state: %v", what, len(w), w)
		}
		s.checkJournalClean(j)
		t.Logf("%s: %d requests, all reads", what, len(j))
	}

	time.Sleep(3 * time.Second) // settle
	s.clearJournal()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, mg := range o.managed() {
		obj := mg.(client.Object)
		s.patch(obj, func() {
			a := obj.GetAnnotations()
			if a == nil {
				a = map[string]string{}
			}
			a[pokeAnnotation] = stamp
			obj.SetAnnotations(a)
		})
	}
	sawReads("after annotating every object")

	// Restart the manager: every object is reconciled from scratch (empty caches, no in-memory
	// clients).
	s.clearJournal()
	s.restartManager()
	sawReads("after restarting the manager")
	for _, mg := range o.managed() {
		s.waitManaged(mg, 30*time.Second)
	}
}

// testUpdate changes one field of the Queue, KVNamespace and VPCService and expects exactly one
// write each: PATCH queue (settings merge), PUT KV (rename), PUT VPC service (full replace).
func (s *suite) testUpdate(t *testing.T, o *objects) {
	s.clearJournal()
	s.patch(o.queue, func() { o.queue.Spec.ForProvider.Settings.DeliveryDelay = ptr.To[int64](10) })
	newTitle := *o.kv.Spec.ForProvider.Title + "-renamed"
	s.patch(o.kv, func() { o.kv.Spec.ForProvider.Title = ptr.To(newTitle) })
	s.patch(o.vpc, func() { o.vpc.Spec.ForProvider.HTTPPort = ptr.To[int32](8080) })
	// A connector change is Kubernetes-only: no Cloudflare write.
	s.patch(o.tunnel2, func() { o.tunnel2.Spec.Connector.Replicas = ptr.To[int32](2) })
	for _, mg := range []commonv1alpha1.Managed{o.queue, o.kv, o.vpc, o.tunnel2} {
		s.waitManaged(mg, time.Minute)
	}
	eventually(t, 2*time.Minute, "Tunnel tun2 to report 2 ready connectors", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.tunnel2), o.tunnel2); err != nil {
			return false, err.Error()
		}
		c := o.tunnel2.Status.Connector
		return c.Replicas == 2 && c.ReadyReplicas == 2, fmt.Sprintf("%+v", c)
	})
	time.Sleep(5 * time.Second)
	a := "/accounts/" + s.accountID
	want := []string{
		"PATCH " + a + "/queues/" + o.queue.Status.ID,
		"PUT " + a + "/storage/kv/namespaces/" + o.kv.Status.ID,
		"PUT " + a + "/connectivity/directory/services/" + o.vpc.Status.ID,
	}
	j := s.journal()
	got := writes(j)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("writes after the update = %v, want exactly %v", got, want)
	}
	s.checkJournalClean(j)

	var q struct {
		Settings struct {
			DeliveryDelay          int64 `json:"delivery_delay"`
			MessageRetentionPeriod int64 `json:"message_retention_period"`
		} `json:"settings"`
	}
	if err := s.cfGet("/queues/"+o.queue.Status.ID, &q); err != nil || q.Settings.DeliveryDelay != 10 || q.Settings.MessageRetentionPeriod != 7200 {
		t.Errorf("queue settings after PATCH = %+v, %v; want 10/7200", q.Settings, err)
	}
	var kv struct{ Title string }
	if err := s.cfGet("/storage/kv/namespaces/"+o.kv.Status.ID, &kv); err != nil || kv.Title != newTitle {
		t.Errorf("KV title after rename = %q, %v", kv.Title, err)
	}
	var vs struct {
		HTTPPort int32 `json:"http_port"`
	}
	if err := s.cfGet("/connectivity/directory/services/"+o.vpc.Status.ID, &vs); err != nil || vs.HTTPPort != 8080 {
		t.Errorf("VPC service http_port = %d, %v", vs.HTTPPort, err)
	}
	// The Tunnel's policy follows the VPC service's new port (Service port 8080 has no target
	// mapping, so the port itself is used).
	eventually(t, time.Minute, "tunnel policy status to list the new port", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.tunnel), o.tunnel); err != nil {
			return false, err.Error()
		}
		return strings.Contains(strings.Join(o.tunnel.Status.NetworkPolicy.Backends, ","), "8080"), fmt.Sprint(o.tunnel.Status.NetworkPolicy.Backends)
	})
}

// testDeleteOrder deletes a Tunnel that a VPCService references: the Tunnel waits; deleting
// the VPCService lets it proceed, and Cloudflare sees the VPC service DELETE before the tunnel
// DELETE. The tunnel is "connected" (flarefake control API) until cloudflared is scaled down,
// so the controller must also wait for the connections to drain.
func (s *suite) testDeleteOrder(t *testing.T, o *objects) {
	tid, vid := o.tunnel.Status.ID, o.vpc.Status.ID
	if _, err := s.fake("POST", fmt.Sprintf("/_fake/accounts/%s/tunnels/%s/connect", s.accountID, tid), map[string]int{"replicas": 1, "connections": 4}); err != nil {
		t.Fatalf("connect tunnel: %v", err)
	}
	s.clearJournal()
	if err := s.c.Delete(s.ctx(), o.tunnel); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Minute, "Tunnel to report DependencyNotReady", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.tunnel), o.tunnel); err != nil {
			return false, err.Error()
		}
		r := cond(o.tunnel.Status.Conditions, commonv1alpha1.ConditionReady)
		return r != nil && r.Reason == commonv1alpha1.ReasonDependency && strings.Contains(r.Message, o.vpc.Name), condString(o.tunnel.Status.Conditions)
	})
	consistently(t, 5*time.Second, "blocked Tunnel", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.tunnel), o.tunnel); err != nil {
			return false, "Tunnel went away while a VPCService references it: " + err.Error()
		}
		var dep appsv1.Deployment
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: o.tunnel.Status.Connector.DeploymentName}, &dep); err != nil {
			return false, err.Error()
		}
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
			return false, "cloudflared was scaled down while the Tunnel is still referenced"
		}
		if w := writes(s.journal()); len(w) > 0 {
			return false, fmt.Sprintf("Cloudflare writes while blocked: %v", w)
		}
		return true, ""
	})

	if err := s.c.Delete(s.ctx(), o.vpc); err != nil {
		t.Fatal(err)
	}
	s.waitGone(o.vpc, time.Minute)
	// cloudflared scales to zero; the tunnel still has connections, so it is not deleted yet.
	eventually(t, 2*time.Minute, "cloudflared scaled to zero", func() (bool, string) {
		var dep appsv1.Deployment
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: o.tunnel.Status.Connector.DeploymentName}, &dep); err != nil {
			return false, err.Error()
		}
		return dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 && dep.Status.Replicas == 0, fmt.Sprintf("spec %v status %d", ptr.Deref(dep.Spec.Replicas, -1), dep.Status.Replicas)
	})
	draining := func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.tunnel), o.tunnel); err != nil {
			return false, "Tunnel deleted while connected: " + err.Error()
		}
		for _, w := range writes(s.journal()) {
			if strings.HasSuffix(w, "/cfd_tunnel/"+tid) {
				return false, "tunnel DELETE sent while connected: " + w
			}
		}
		return true, ""
	}
	consistently(t, 8*time.Second, "Tunnel waiting for connections to drain", draining)
	// A manager restart in the middle of the deletion resumes it from the objects' state.
	s.restartManager()
	consistently(t, 8*time.Second, "Tunnel waiting for connections to drain after a manager restart", draining)
	if _, err := s.fake("POST", fmt.Sprintf("/_fake/accounts/%s/tunnels/%s/disconnect", s.accountID, tid), nil); err != nil {
		t.Fatalf("disconnect tunnel: %v", err)
	}
	s.waitGone(o.tunnel, time.Minute)

	j := s.journal()
	iv := slices.IndexFunc(j, func(e journalEntry) bool {
		return e.Method == "DELETE" && e.Path == "/accounts/"+s.accountID+"/connectivity/directory/services/"+vid
	})
	it := slices.IndexFunc(j, func(e journalEntry) bool {
		return e.Method == "DELETE" && e.Path == "/accounts/"+s.accountID+"/cfd_tunnel/"+tid && e.Status == 200
	})
	if iv < 0 || it < 0 || iv > it {
		t.Errorf("journal order: VPC service DELETE at %d, tunnel DELETE at %d; want both, VPC first. writes: %v", iv, it, writes(j))
	}
	s.checkJournalClean(j)
	var tun struct {
		DeletedAt *string `json:"deleted_at"`
	}
	if err := s.cfGet("/cfd_tunnel/"+tid, &tun); err != nil || tun.DeletedAt == nil {
		t.Errorf("tunnel %s in flarefake: deleted_at %v, %v", tid, tun.DeletedAt, err)
	}
	// Owned objects are garbage-collected.
	eventually(t, time.Minute, "owned Deployment, Secret and NetworkPolicy to go", func() (bool, string) {
		var left []string
		for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Secret{}, &networkingv1.NetworkPolicy{}} {
			name := o.tunnel.Status.Connector.DeploymentName
			if _, ok := obj.(*corev1.Secret); ok {
				name = o.tunnel.Status.Connector.TokenSecretName
			}
			if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: name}, obj); err == nil {
				left = append(left, fmt.Sprintf("%T %s", obj, name))
			}
		}
		return len(left) == 0, strings.Join(left, ", ")
	})
}

// testAccountProtection deletes the CloudflareAccount and its Secret while resources still use
// the account: both must stay (finalizers) and the resources keep working.
func (s *suite) testAccountProtection(t *testing.T, o *objects) {
	s.clearJournal()
	if err := s.c.Delete(s.ctx(), o.account); err != nil {
		t.Fatal(err)
	}
	if err := s.c.Delete(s.ctx(), o.secret); err != nil {
		t.Fatal(err)
	}
	consistently(t, 8*time.Second, "account and Secret held", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.account), o.account); err != nil {
			return false, "account: " + err.Error()
		}
		if !controllerutil.ContainsFinalizer(o.account, cloudflarev1alpha1.AccountInUseFinalizer) {
			return false, fmt.Sprintf("account finalizers %v", o.account.Finalizers)
		}
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(o.secret), o.secret); err != nil {
			return false, "secret: " + err.Error()
		}
		if !controllerutil.ContainsFinalizer(o.secret, cloudflarev1alpha1.AccountTokenFinalizer) {
			return false, fmt.Sprintf("secret finalizers %v", o.secret.Finalizers)
		}
		return true, ""
	})
	if o.account.DeletionTimestamp == nil || o.secret.DeletionTimestamp == nil {
		t.Errorf("account/secret not terminating")
	}
	t.Logf("deleting account: %s", condString(o.account.Status.Conditions))
	// Managed objects are untouched in Cloudflare.
	if w := writes(s.journal()); slices.ContainsFunc(w, func(x string) bool { return strings.HasPrefix(x, "DELETE") }) {
		t.Errorf("DELETE sent after deleting the account only: %v", w)
	}
}

// testNamespaceTeardown deletes the namespace with the (deleting) account, its Secret and the
// remaining resources: everything must go, and flarefake must hold nothing for the account.
func (s *suite) testNamespaceTeardown(t *testing.T, o *objects) {
	tid2, vid2 := o.tunnel2.Status.ID, o.vpc2.Status.ID
	s.clearJournal()
	if err := s.c.Delete(s.ctx(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns}}); err != nil {
		t.Fatal(err)
	}
	s.waitGone(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns}}, 4*time.Minute)

	j := s.journal()
	iv := slices.IndexFunc(j, func(e journalEntry) bool { return e.Method == "DELETE" && strings.HasSuffix(e.Path, "/services/"+vid2) })
	it := slices.IndexFunc(j, func(e journalEntry) bool {
		return e.Method == "DELETE" && strings.HasSuffix(e.Path, "/cfd_tunnel/"+tid2) && e.Status == 200
	})
	if iv < 0 || it < 0 || iv > it {
		t.Errorf("teardown order: VPC service DELETE at %d, tunnel DELETE at %d; writes %v", iv, it, writes(j))
	}
	s.checkJournalClean(j)
	t.Logf("teardown writes: %v", writes(j))

	// Nothing left in flarefake for the account.
	for _, p := range []string{"/storage/kv/namespaces", "/queues", "/d1/database", "/cfd_tunnel?is_deleted=false", "/connectivity/directory/services"} {
		var items []map[string]any
		if err := s.cfGet(p, &items); err != nil {
			t.Errorf("list %s: %v", p, err)
			continue
		}
		if len(items) > 0 {
			t.Errorf("flarefake still holds %d item(s) at %s: %v", len(items), p, items)
		}
	}
	var tagged []map[string]any
	if err := s.cfGet("/tags/resources", &tagged); err != nil {
		t.Errorf("list tagged resources: %v", err)
	} else if len(tagged) > 0 {
		t.Logf("tag index still lists %d resource(s) after deletion: %v", len(tagged), tagged)
	}
}

// testManagerHealth checks the manager's logs for panics and RBAC denials and that neither the
// manager nor flarefake restarted.
func (s *suite) testManagerHealth(t *testing.T) {
	for _, p := range s.managerPods() {
		errs := s.checkLogs(p.Name, s.managerLogs(p.Name))
		for _, e := range errs {
			t.Logf("manager error log: %s", e)
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				t.Errorf("manager container %s restarted %d times", cs.Name, cs.RestartCount)
			}
		}
	}
	var pl corev1.PodList
	if err := s.c.List(s.ctx(), &pl, client.InNamespace(s.cfg.OperatorNS), client.MatchingLabels{"app.kubernetes.io/component": "flarefake"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range pl.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				t.Errorf("flarefake restarted %d times (state is in memory)", cs.RestartCount)
			}
		}
	}
}
