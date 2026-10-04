//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	d1v1alpha1 "github.com/chenhunghan/flare-operator/api/d1/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
	vectorizev1alpha1 "github.com/chenhunghan/flare-operator/api/vectorize/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "github.com/chenhunghan/flare-operator/api/workersvpc/v1alpha1"
)

// config is the installation under test, from the environment (defaults match
// `make e2e-install`).
type config struct {
	OperatorNS    string // E2E_OPERATOR_NAMESPACE (flare-system)
	Release       string // E2E_RELEASE (flare-operator): Deployment, ServiceAccount, <release>-flarefake Service
	FakePort      int    // flarefake Service port (8787)
	ClusterDomain string // E2E_CLUSTER_DOMAIN (cluster.local)
	StubImage     string // E2E_STUB_IMAGE (cloudflared-stub:e2e)
	StubPull      corev1.PullPolicy
	ExcludeCIDRs  []string // E2E_EXCLUDE_CIDRS (k0s defaults: pods 10.244.0.0/16, services 10.96.0.0/12)
	Timeout       time.Duration
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	return config{
		OperatorNS:    getenv("E2E_OPERATOR_NAMESPACE", "flare-system"),
		Release:       getenv("E2E_RELEASE", "flare-operator"),
		FakePort:      8787,
		ClusterDomain: getenv("E2E_CLUSTER_DOMAIN", "cluster.local"),
		StubImage:     getenv("E2E_STUB_IMAGE", "cloudflared-stub:e2e"),
		StubPull:      corev1.PullPolicy(getenv("E2E_STUB_PULL_POLICY", string(corev1.PullNever))),
		ExcludeCIDRs:  strings.Split(getenv("E2E_EXCLUDE_CIDRS", "10.244.0.0/16,10.96.0.0/12"), ","),
		Timeout:       2 * time.Minute,
	}
}

func (c config) fakeService() string { return c.Release + "-flarefake" }

// baseURL is the CloudflareAccount spec.baseURL the chart allows for its flarefake.
func (c config) baseURL() string {
	return fmt.Sprintf("http://%s.%s.svc:%d/client/v4", c.fakeService(), c.OperatorNS, c.FakePort)
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, cloudflarev1alpha1.AddToScheme, kvv1alpha1.AddToScheme, queuesv1alpha1.AddToScheme,
		d1v1alpha1.AddToScheme, tunnelsv1alpha1.AddToScheme, workersvpcv1alpha1.AddToScheme, workersv1alpha1.AddToScheme, vectorizev1alpha1.AddToScheme,
		pagesv1alpha1.AddToScheme, r2v1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}

// suite is the shared state of one e2e run.
type suite struct {
	t         *testing.T
	cfg       config
	c         client.Client
	cs        *kubernetes.Clientset
	ns        string
	accountID string
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		t.Skip("KUBECONFIG is not set; the e2e tests need a cluster with the chart installed (make e2e-install)")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", kc)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	rc.QPS, rc.Burst = 50, 100
	c, err := client.New(rc, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	return &suite{t: t, cfg: loadConfig(), c: c, cs: cs, accountID: randomHex(16)}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *suite) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	s.t.Cleanup(cancel)
	return ctx
}

// eventually polls f every second until it returns true or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, msg := f()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %s", timeout, what, last)
		}
		time.Sleep(time.Second)
	}
}

// consistently checks f every second for d; f returning false fails the test.
func consistently(t *testing.T, d time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok, msg := f(); !ok {
			t.Fatalf("%s: %s", what, msg)
		}
		time.Sleep(time.Second)
	}
}

// --- flarefake through the API server's service proxy -------------------------------------

func (s *suite) fakeProxyPath(p string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:%d/proxy%s", s.cfg.OperatorNS, s.cfg.fakeService(), s.cfg.FakePort, p)
}

// fake sends a request to flarefake (p starts with /_fake or /client/v4). The API server's
// proxy consumes Authorization, so API calls authenticate with X-Auth-Key (flarefake accepts
// any credential; only /tokens/verify checks them).
func (s *suite) fake(method, p string, body any) ([]byte, error) {
	path, query, _ := strings.Cut(p, "?")
	req := s.cs.CoreV1().RESTClient().Verb(method).AbsPath(s.fakeProxyPath(path)).SetHeader("X-Auth-Key", "e2e")
	if query != "" {
		vals, err := url.ParseQuery(query)
		if err != nil {
			return nil, err
		}
		for k, vs := range vals {
			for _, v := range vs {
				req = req.Param(k, v)
			}
		}
	}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req = req.Body(bytes.NewReader(b)).SetHeader("Content-Type", "application/json")
	}
	return req.DoRaw(s.ctx())
}

type journalEntry struct {
	Time            time.Time `json:"time"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	Query           string    `json:"query"`
	Status          int       `json:"status"`
	SchemaViolation string    `json:"schema_violation"`
	Fault           bool      `json:"fault"`
}

func (e journalEntry) String() string {
	s := fmt.Sprintf("%s %s", e.Method, e.Path)
	if e.Query != "" {
		s += "?" + e.Query
	}
	return fmt.Sprintf("%s -> %d", s, e.Status)
}

func (e journalEntry) write() bool { return e.Method != "GET" && e.Method != "HEAD" }

// journal returns flarefake's journal entries for this suite's account (the API calls the
// operator made for it, plus the suite's own X-Auth-Key GETs).
func (s *suite) journal() []journalEntry {
	s.t.Helper()
	return s.journalFor(s.accountID)
}

// journalFor returns flarefake's journal entries for the account accountID.
func (s *suite) journalFor(accountID string) []journalEntry {
	s.t.Helper()
	b, err := s.fake("GET", "/_fake/journal", nil)
	if err != nil {
		s.t.Fatalf("read flarefake journal: %v", err)
	}
	var all []journalEntry
	if err := json.Unmarshal(b, &all); err != nil {
		s.t.Fatalf("decode journal: %v", err)
	}
	prefix := "/accounts/" + accountID
	var out []journalEntry
	for _, e := range all {
		if e.Path == prefix || strings.HasPrefix(e.Path, prefix+"/") {
			out = append(out, e)
		}
	}
	return out
}

func (s *suite) clearJournal() {
	s.t.Helper()
	if _, err := s.fake("DELETE", "/_fake/journal", nil); err != nil {
		s.t.Fatalf("clear flarefake journal: %v", err)
	}
}

func writes(j []journalEntry) []string {
	var out []string
	for _, e := range j {
		if e.write() {
			out = append(out, e.Method+" "+e.Path)
		}
	}
	return out
}

// knownSpecDefects are requests the real API accepts but the pinned spec rejects; the same list
// as internal/generic/descriptors/emulator_test.go.
var knownSpecDefects = []struct{ method, pathPart, contains, why string }{
	{"POST", "/d1/database", `"/primary_location_hint": value is not one of the allowed values`, "spec enum is lower-case; the API wants upper-case (0019)"},
	{"", "/d1/database/", `parameter "database_id" in path has an error: input matches more than one oneOf schemas`, "UUID matches the oneOf twice (0020)"},
	{"DELETE", "/storage/kv/namespaces/", "request body has an error: value is required but missing", "the API deletes without a body (0013)"},
	{"POST", "/pages/projects/", "path manifest: not matching content types",
		"wrangler sends the deployment manifest as a plain form field; the spec's encoding says application/json (internal/controller/pagesdeployment/spec_test.go)"},
}

func knownDefect(e journalEntry) bool {
	for _, k := range knownSpecDefects {
		if (k.method == "" || k.method == e.Method) && strings.Contains(e.Path, k.pathPart) && strings.Contains(e.SchemaViolation, k.contains) {
			return true
		}
	}
	return false
}

// checkJournalClean fails on schema violations (other than knownSpecDefects) and server errors
// in j.
func (s *suite) checkJournalClean(j []journalEntry) {
	s.t.Helper()
	for _, e := range j {
		if e.SchemaViolation != "" && !knownDefect(e) {
			s.t.Errorf("request violates the pinned spec: %s: %s", e, e.SchemaViolation)
		}
		if e.Status >= 500 && !e.Fault && !isTagRead(e) {
			s.t.Errorf("flarefake answered %s", e)
		}
	}
}

// isTagRead: GET …/tags answers 500 for a never-tagged resource (UNVERIFIED emulation of the
// live API, internal/fake/tags.go); the tagger handles it.
func isTagRead(e journalEntry) bool { return e.Method == "GET" && strings.HasSuffix(e.Path, "/tags") }

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
}

// cfGet reads an API path (relative to /client/v4/accounts/<account>) from flarefake.
func (s *suite) cfGet(p string, into any) error {
	b, err := s.fake("GET", "/client/v4/accounts/"+s.accountID+p, nil)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("decode %s: %w", p, err)
	}
	return json.Unmarshal(env.Result, into)
}

// cfDo sends a write to an API path (relative to /client/v4/accounts/<account>) and decodes
// the result into into (when non-nil).
func (s *suite) cfDo(method, p string, body, into any) error {
	b, err := s.fake(method, "/client/v4/accounts/"+s.accountID+p, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, p, err)
	}
	if into == nil || len(b) == 0 {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("decode %s: %w", p, err)
	}
	return json.Unmarshal(env.Result, into)
}

// --- Kubernetes helpers ---------------------------------------------------------------------

func cond(conds []metav1.Condition, typ string) *metav1.Condition {
	return meta.FindStatusCondition(conds, typ)
}

func condString(conds []metav1.Condition) string {
	var parts []string
	for _, c := range conds {
		parts = append(parts, fmt.Sprintf("%s=%s(%s: %s)", c.Type, c.Status, c.Reason, c.Message))
	}
	return strings.Join(parts, "; ")
}

// waitManaged waits until obj (re-read each second) has Ready and Synced True for its
// current generation.
func (s *suite) waitManaged(obj commonv1alpha1.Managed, timeout time.Duration) {
	s.t.Helper()
	key := client.ObjectKeyFromObject(obj)
	eventually(s.t, timeout, fmt.Sprintf("%T %s Ready and Synced", obj, key), func() (bool, string) {
		if err := s.c.Get(s.ctx(), key, obj); err != nil {
			return false, err.Error()
		}
		st := obj.GetResourceStatus()
		r, sy := cond(st.Conditions, commonv1alpha1.ConditionReady), cond(st.Conditions, commonv1alpha1.ConditionSynced)
		ok := r != nil && sy != nil && r.Status == metav1.ConditionTrue && sy.Status == metav1.ConditionTrue &&
			st.ObservedGeneration == obj.GetGeneration()
		return ok, fmt.Sprintf("generation %d observed %d: %s", obj.GetGeneration(), st.ObservedGeneration, condString(st.Conditions))
	})
}

// waitGone waits until obj no longer exists.
func (s *suite) waitGone(obj client.Object, timeout time.Duration) {
	s.t.Helper()
	key := client.ObjectKeyFromObject(obj)
	eventually(s.t, timeout, fmt.Sprintf("%T %s to be deleted", obj, key), func() (bool, string) {
		err := s.c.Get(s.ctx(), key, obj)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("finalizers %v, deletionTimestamp %v", obj.GetFinalizers(), obj.GetDeletionTimestamp())
	})
}

func (s *suite) create(obj client.Object) {
	s.t.Helper()
	if err := s.c.Create(s.ctx(), obj); err != nil {
		s.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

// patch applies mutate to a fresh copy of obj with a merge patch.
func (s *suite) patch(obj client.Object, mutate func()) {
	s.t.Helper()
	if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(obj), obj); err != nil {
		s.t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	mutate()
	if err := s.c.Patch(s.ctx(), obj, client.MergeFrom(base)); err != nil {
		s.t.Fatalf("patch %T %s: %v", obj, obj.GetName(), err)
	}
}

// managerPods lists the manager's pods.
func (s *suite) managerPods() []corev1.Pod {
	s.t.Helper()
	var pl corev1.PodList
	if err := s.c.List(s.ctx(), &pl, client.InNamespace(s.cfg.OperatorNS), client.MatchingLabels{
		"app.kubernetes.io/instance": s.cfg.Release, "app.kubernetes.io/component": "manager",
	}); err != nil {
		s.t.Fatal(err)
	}
	sort.Slice(pl.Items, func(i, j int) bool { return pl.Items[i].Name < pl.Items[j].Name })
	return pl.Items
}

// leaseName is the manager's leader-election lease (cmd/manager LeaderElectionID).
const leaseName = "flare-operator.cloudflare.flare.dev"

// restartManager checks the logs of the running manager pods, deletes them and waits until a
// new pod is Ready and holds the leader lease.
func (s *suite) restartManager() {
	s.t.Helper()
	pods := s.managerPods()
	if len(pods) == 0 {
		s.t.Fatal("no manager pod")
	}
	old := map[string]bool{}
	for i := range pods {
		s.checkLogs(pods[i].Name, s.managerLogs(pods[i].Name))
		old[pods[i].Name] = true
		if err := s.c.Delete(s.ctx(), &pods[i]); err != nil {
			s.t.Fatal(err)
		}
	}
	eventually(s.t, 2*time.Minute, "a new manager pod to be Ready and lead", func() (bool, string) {
		var lease coordinationv1.Lease
		err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.cfg.OperatorNS, Name: leaseName}, &lease)
		if err != nil && !apierrors.IsNotFound(err) { // no lease: leader election is off
			return false, err.Error()
		}
		holder := ptr.Deref(lease.Spec.HolderIdentity, "")
		for _, p := range s.managerPods() {
			if !old[p.Name] && podReady(&p) && (apierrors.IsNotFound(err) || strings.HasPrefix(holder, p.Name+"_")) {
				return true, ""
			}
		}
		return false, "lease holder " + holder
	})
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// managerLogs returns the manager container log of pod name.
func (s *suite) managerLogs(name string) string {
	s.t.Helper()
	rc, err := s.cs.CoreV1().Pods(s.cfg.OperatorNS).GetLogs(name, &corev1.PodLogOptions{Container: "manager"}).Stream(s.ctx())
	if err != nil {
		s.t.Errorf("logs of %s: %v", name, err)
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

var (
	rePanic = regexp.MustCompile(`(?i)panic|goroutine \d+ \[`)
	// An RBAC denial reads `… is forbidden: User "system:serviceaccount:…" cannot <verb> resource …`.
	// Not an RBAC denial: "forbidden: unable to create new content in namespace … because it is
	// being terminated" (the NamespaceLifecycle admission plugin during namespace teardown).
	reForbidden = regexp.MustCompile(`forbidden: User |cannot (list|get|watch|create|update|patch|delete|deletecollection) resource`)
)

// checkLogs fails on panics and RBAC denials in a manager log and returns its error lines.
func (s *suite) checkLogs(pod, logs string) []string {
	s.t.Helper()
	var errs []string
	for _, line := range strings.Split(logs, "\n") {
		switch {
		case rePanic.MatchString(line):
			s.t.Errorf("manager %s logged a panic: %s", pod, line)
		case reForbidden.MatchString(line):
			s.t.Errorf("manager %s logged an RBAC denial: %s", pod, line)
		case strings.Contains(line, `"level":"error"`):
			errs = append(errs, line)
		}
	}
	return errs
}
