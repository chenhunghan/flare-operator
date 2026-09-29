//go:build live

// Live smoke test: the manager's real controllers, in an envtest API server (no cluster, so the
// token never lands in a real one), pointed at the Cloudflare API. Every request goes through a
// recording RoundTripper (recorder.go) that writes RAW cassettes for later sanitizing.
//
//	FLARE_LIVE=1                       required, otherwise every test skips
//	CLOUDFLARE_ACCOUNT_ID              the account (32 hex)
//	FLARE_LIVE_TOKEN_FILE              file holding the API token (preferred), or FLARE_LIVE_TOKEN
//	FLARE_LIVE_NOTAG_TOKEN_FILE        optional: a token WITHOUT Resource Tagging permission
//	FLARE_LIVE_RAW_DIR                 RAW cassette directory, must be outside the repo
//	                                   (default: a new temp dir, logged)
//	FLARE_LIVE_LEDGER                  optional: markdown ledger file; every create/delete is appended
//	FLARE_LIVE_BASE_URL                optional API root (default https://api.cloudflare.com/client/v4)
//	FLARE_LIVE_FAKE=1                  dry run against the in-process flarefake instead (no
//	                                   Cloudflare, no credentials): checks the test itself
//
// Everything created is named flare-spike-live-<random>-*. Each test deletes what it created
// and, on failure, a cleanup pass deletes whatever is still tracked. Run with
// `make live` (see the Makefile), then sanitize: hack/sanitize_recordings.py RAW_DIR
// test/recordings/<date>-live.
package live

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/controller/tunnel"
	"flare.dev/operator/internal/controller/vpcservice"
	"flare.dev/operator/internal/fake"
	_ "flare.dev/operator/internal/generic/kinds" // KVNamespace, Queue, D1Database controllers
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

const (
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
	clusterName    = "flare-spike-live"
	waitTimeout    = 3 * time.Minute
)

var (
	env *testenv.Env
	lv  *liveEnv
)

// liveEnv is the configuration shared by the tests.
type liveEnv struct {
	fakeMode  bool
	accountID string
	token     string
	noTag     string // optional token without tagging permission
	baseURL   string // API root
	specURL   string // CloudflareAccount spec.baseURL ("" = operator default)
	rawDir    string
	ledger    string
	prefix    string // flare-spike-live-<rand>
	rec       *Recorder
	hc        *http.Client

	mu      sync.Mutex
	tracked []*tracked
}

// tracked is a Cloudflare resource this run created and must delete.
type tracked struct {
	kind, name, id, deletePath string
	deleted                    bool
}

func TestMain(m *testing.M) {
	if os.Getenv("FLARE_LIVE") != "1" {
		fmt.Fprintln(os.Stderr, "live: FLARE_LIVE is not 1; skipping the live smoke test")
		os.Exit(m.Run())
	}
	cfg, err := loadLive()
	if err != nil {
		fmt.Fprintln(os.Stderr, "live: "+err.Error())
		os.Exit(1)
	}
	lv = cfg
	testenv.Main(m, &env, testenv.Options{})
}

func loadLive() (*liveEnv, error) {
	l := &liveEnv{fakeMode: os.Getenv("FLARE_LIVE_FAKE") == "1", prefix: "flare-spike-live-" + randHex(3),
		ledger: os.Getenv("FLARE_LIVE_LEDGER")}
	if l.fakeMode {
		l.accountID = testenv.RandomAccountID()
		l.token = "flare-spike-fake-" + randHex(8)
	} else {
		l.accountID = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
		if len(l.accountID) != 32 {
			return nil, fmt.Errorf("CLOUDFLARE_ACCOUNT_ID must be the 32-character account ID")
		}
		tok, err := readToken("FLARE_LIVE_TOKEN_FILE", "FLARE_LIVE_TOKEN")
		if err != nil || tok == "" {
			return nil, fmt.Errorf("set FLARE_LIVE_TOKEN_FILE (or FLARE_LIVE_TOKEN): %v", err)
		}
		l.token = tok
		l.noTag, _ = readToken("FLARE_LIVE_NOTAG_TOKEN_FILE", "")
		l.baseURL = os.Getenv("FLARE_LIVE_BASE_URL")
		l.specURL = l.baseURL
		if l.baseURL == "" {
			l.baseURL = defaultBaseURL
		}
	}
	l.rawDir = os.Getenv("FLARE_LIVE_RAW_DIR")
	if l.rawDir == "" {
		d, err := os.MkdirTemp("", "flare-live-raw-")
		if err != nil {
			return nil, err
		}
		l.rawDir = d
	}
	abs, err := filepath.Abs(l.rawDir)
	if err != nil {
		return nil, err
	}
	if repo := testenv.RepoRoot(); abs == repo || strings.HasPrefix(abs, repo+string(filepath.Separator)) ||
		strings.Contains(abs, string(filepath.Separator)+filepath.Join("test", "recordings")) {
		return nil, fmt.Errorf("FLARE_LIVE_RAW_DIR %s is inside the repository: raw cassettes hold unsanitized IDs and tokens", abs)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	l.rawDir = abs
	l.rec = &Recorder{Base: http.DefaultTransport, Dir: l.rawDir, PathPrefix: "/client/v4"}
	l.hc = &http.Client{Transport: l.rec, Timeout: 60 * time.Second}
	fmt.Fprintf(os.Stderr, "live: fake=%v prefix=%s raw cassettes in %s\n", l.fakeMode, l.prefix, l.rawDir)
	return l, nil
}

func readToken(fileVar, valueVar string) (string, error) {
	if f := os.Getenv(fileVar); f != "" {
		b, err := os.ReadFile(f)
		return strings.TrimSpace(string(b)), err
	}
	if valueVar != "" {
		return strings.TrimSpace(os.Getenv(valueVar)), nil
	}
	return "", nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requireLive skips t unless FLARE_LIVE=1 and envtest runs; in fake mode it points the base URL
// at the in-process flarefake and registers the token there.
func requireLive(t *testing.T) *testenv.Env {
	t.Helper()
	if lv == nil {
		t.Skip("FLARE_LIVE is not 1")
	}
	e := testenv.Require(t, env)
	if lv.fakeMode && lv.baseURL == "" {
		lv.baseURL = e.BaseURL
		lv.specURL = e.BaseURL
		e.Fake.AddToken(fake.Token{Value: lv.token, AccountID: lv.accountID})
	}
	t.Cleanup(func() { lv.cleanup(t) })
	return e
}

// ---- ledger and cleanup ----

func (l *liveEnv) logLedger(t *testing.T, row string) {
	t.Helper()
	t.Log("LEDGER " + row)
	if l.ledger == "" {
		return
	}
	f, err := os.OpenFile(l.ledger, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Errorf("ledger: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "| %s | %s |\n", time.Now().Format("15:04:05"), row)
}

func (l *liveEnv) track(t *testing.T, kind, name, id, deletePath string) *tracked {
	t.Helper()
	tr := &tracked{kind: kind, name: name, id: id, deletePath: deletePath}
	l.mu.Lock()
	l.tracked = append(l.tracked, tr)
	l.mu.Unlock()
	l.logLedger(t, fmt.Sprintf("%s | %s | %s | created (fake=%v)", kind, name, id, l.fakeMode))
	return tr
}

func (l *liveEnv) markDeleted(t *testing.T, tr *tracked, how string) {
	t.Helper()
	if tr == nil || tr.deleted {
		return
	}
	tr.deleted = true
	l.logLedger(t, fmt.Sprintf("%s | %s | %s | deleted: %s", tr.kind, tr.name, tr.id, how))
}

// cleanup deletes every tracked resource not yet confirmed deleted.
func (l *liveEnv) cleanup(t *testing.T) {
	l.mu.Lock()
	todo := slices.Clone(l.tracked)
	l.mu.Unlock()
	for i := len(todo) - 1; i >= 0; i-- {
		tr := todo[i]
		if tr.deleted {
			continue
		}
		st, _, err := l.do("cleanup", http.MethodDelete, tr.deletePath, nil, nil, l.token)
		how := fmt.Sprintf("cleanup DELETE -> %d %v", st, err)
		if st == http.StatusNotFound {
			how += " (already gone)"
		}
		l.markDeleted(t, tr, how)
		if err != nil || (st >= 300 && st != http.StatusNotFound) {
			t.Errorf("CLEANUP FAILED for %s %s (%s): delete it by hand", tr.kind, tr.name, tr.id)
		}
	}
	l.sweep(t)
}

// sweep lists every kind this test creates and deletes anything named with this run's prefix
// (random per run, so pre-existing resources never match) that is still there, e.g. a
// resource the operator created but whose ID the test never saw.
func (l *liveEnv) sweep(t *testing.T) {
	type item struct {
		ID        string `json:"id"`
		UUID      string `json:"uuid"`
		QueueID   string `json:"queue_id"`
		ServiceID string `json:"service_id"`
		Name      string `json:"name"`
		Title     string `json:"title"`
		QueueName string `json:"queue_name"`
	}
	for _, k := range []struct{ kind, list, del string }{
		{"VPC service", "/connectivity/directory/services?per_page=100", "/connectivity/directory/services/"},
		{"Tunnel", "/cfd_tunnel?is_deleted=false&per_page=100", "/cfd_tunnel/"},
		{"KV namespace", "/storage/kv/namespaces?per_page=100", "/storage/kv/namespaces/"},
		{"Queue", "/queues", "/queues/"},
		{"D1 database", "/d1/database?per_page=100", "/d1/database/"},
		{"Workers script", "/workers/scripts", "/workers/scripts/"},
	} {
		st, env, err := l.do("sweep-list", http.MethodGet, l.acct(k.list), nil, nil, l.token)
		if err != nil || st != http.StatusOK {
			t.Errorf("sweep: list %s: %d %v %s", k.kind, st, err, env)
			continue
		}
		var items []item
		_ = json.Unmarshal(env.Result, &items)
		for _, it := range items {
			name := it.Name + it.Title + it.QueueName
			id := it.ServiceID + it.QueueID + it.UUID
			if id == "" {
				id = it.ID
			}
			if k.kind == "Workers script" {
				name = it.ID
			}
			if !strings.HasPrefix(name, l.prefix) {
				continue
			}
			st, _, err := l.do("sweep-delete", http.MethodDelete, l.acct(k.del+id), nil, nil, l.token)
			l.logLedger(t, fmt.Sprintf("%s | %s | %s | deleted: sweep DELETE -> %d %v", k.kind, name, id, st, err))
			if err != nil || st >= 300 {
				t.Errorf("SWEEP FAILED for %s %s (%s): delete it by hand", k.kind, name, id)
			}
		}
	}
}

// ---- raw API ----

type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo json.RawMessage `json:"result_info"`
}

func (e envelope) String() string {
	var parts []string
	for _, x := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", x.Code, x.Message))
	}
	return fmt.Sprintf("success=%v errors=[%s]", e.Success, strings.Join(parts, "; "))
}

func (l *liveEnv) acct(p string) string { return "/accounts/" + l.accountID + p }

// do sends one request; body is JSON-encoded unless it is a *multipartBody.
func (l *liveEnv) do(label, method, path string, body any, hdr map[string]string, token string) (int, envelope, error) {
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		rd, ct = bytes.NewReader(b.data), b.contentType
	case string:
		rd, ct = strings.NewReader(b), "application/json"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return 0, envelope{}, err
		}
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, l.baseURL+path, rd)
	if err != nil {
		return 0, envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "flare-operator-live-test")
	req.Header.Set(LabelHeader, label)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return 0, envelope{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env, nil
}

// probe is do under a phase label, logging the outcome (never the result body).
func (l *liveEnv) probe(t *testing.T, phase, method, path string, body any, hdr map[string]string) (int, envelope) {
	t.Helper()
	st, env, err := l.do(phase, method, path, body, hdr, l.token)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", phase, method, path, err)
	}
	t.Logf("PROBE %-40s %s %s -> %d %s", phase, method, redactPath(path), st, env)
	return st, env
}

func redactPath(p string) string { return strings.ReplaceAll(p, lv.accountID, "ACCOUNT_ID") }

type multipartBody struct {
	data        []byte
	contentType string
}

type part struct{ name, filename, contentType, content string }

func multipartOf(parts ...part) *multipartBody {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, p.filename))
		h.Set("Content-Type", p.contentType)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte(p.content))
	}
	_ = w.Close()
	return &multipartBody{data: buf.Bytes(), contentType: w.FormDataContentType()}
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return v
}

// ---- Kubernetes helpers ----

func ctx(t *testing.T) context.Context { return testenv.Context(t, 30*time.Second) }

func cond(cs []metav1.Condition, typ string) *metav1.Condition {
	return meta.FindStatusCondition(cs, typ)
}

func condString(cs []metav1.Condition) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s=%s(%s: %s)", c.Type, c.Status, c.Reason, c.Message))
	}
	return strings.Join(parts, "; ")
}

func waitManaged(t *testing.T, e *testenv.Env, obj commonv1alpha1.Managed) {
	t.Helper()
	key := client.ObjectKeyFromObject(obj)
	testenv.Eventually(t, waitTimeout, func() (bool, string) {
		if err := e.Client.Get(ctx(t), key, obj); err != nil {
			return false, err.Error()
		}
		st := obj.GetResourceStatus()
		r, s := cond(st.Conditions, commonv1alpha1.ConditionReady), cond(st.Conditions, commonv1alpha1.ConditionSynced)
		ok := r != nil && s != nil && r.Status == metav1.ConditionTrue && s.Status == metav1.ConditionTrue &&
			st.ObservedGeneration == obj.GetGeneration()
		return ok, fmt.Sprintf("%T %s generation %d observed %d: %s", obj, key, obj.GetGeneration(), st.ObservedGeneration, condString(st.Conditions))
	})
}

func waitGone(t *testing.T, e *testenv.Env, obj client.Object) {
	t.Helper()
	key := client.ObjectKeyFromObject(obj)
	testenv.Eventually(t, waitTimeout, func() (bool, string) {
		err := e.Client.Get(ctx(t), key, obj)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		msg := fmt.Sprintf("%T %s finalizers %v", obj, key, obj.GetFinalizers())
		if mg, ok := obj.(commonv1alpha1.Managed); ok {
			msg += ": " + condString(mg.GetResourceStatus().Conditions)
		}
		return false, msg
	})
}

func update(t *testing.T, e *testenv.Env, obj client.Object, mut func()) {
	t.Helper()
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(ctx(t), client.ObjectKeyFromObject(obj), obj); err != nil {
			return false, err.Error()
		}
		mut()
		if err := e.Client.Update(ctx(t), obj); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
}

func ensureKubeDNS(t *testing.T, e *testenv.Env) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"k8s-app": "kube-dns"}, Ports: []corev1.ServicePort{
			{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
			{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)},
		}},
	}
	if err := e.Client.Create(ctx(t), svc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create kube-dns: %v", err)
	}
}

// settleDeployment simulates the Deployment controller (envtest runs none) for the Tunnel's
// cloudflared Deployment: it has observed its generation and runs zero replicas.
func settleDeployment(t *testing.T, e *testenv.Env, tun *tunnelsv1alpha1.Tunnel) {
	t.Helper()
	testenv.Eventually(t, time.Minute, func() (bool, string) {
		if err := e.Client.Get(ctx(t), client.ObjectKeyFromObject(tun), tun); err != nil {
			return false, err.Error()
		}
		if tun.Status.Connector.DeploymentName == "" {
			return false, "no status.connector.deploymentName"
		}
		var dep appsv1.Deployment
		if err := e.Client.Get(ctx(t), client.ObjectKey{Namespace: tun.Namespace, Name: tun.Status.Connector.DeploymentName}, &dep); err != nil {
			return false, err.Error()
		}
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
			return false, "cloudflared Deployment does not have replicas 0"
		}
		dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation}
		if err := e.Client.Status().Update(ctx(t), &dep); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
}

// ownerTag reads the resource's tags directly and returns the owner tag.
func ownerTag(t *testing.T, phase, typ, id string) (string, int) {
	t.Helper()
	q := url.Values{"resource_type": {typ}, "resource_id": {id}}
	st, env := lv.probe(t, phase, http.MethodGet, lv.acct("/tags?"+q.Encode()), nil, nil)
	if st != http.StatusOK {
		return "", st
	}
	r := decode[struct {
		Tags map[string]string `json:"tags"`
	}](t, env.Result)
	return r.Tags[reconcile.OwnerTagKey], st
}

// ---- the operator flow ----

// TestLiveOperator runs CloudflareAccount, KVNamespace, Queue, D1Database, Tunnel and VPCService
// through create, steady state (no writes), update and delete against the API.
func TestLiveOperator(t *testing.T) {
	e := requireLive(t)
	lv.rec.SetPhase("op")
	defer lv.rec.SetPhase("")
	e.StartManager(t, testenv.ManagerOptions{
		Controllers:     []string{"kvnamespace", "queue", "d1database", tunnel.Name, vpcservice.Name},
		ClusterName:     clusterName,
		AccountsOptions: []reconcile.AccountsOption{reconcile.WithHTTPClient(lv.hc), reconcile.WithUserAgent("flare-operator-live-test")},
	})
	ensureKubeDNS(t, e)
	ns := e.Namespace(t)
	owner := func(name string) string { return reconcile.OwnerValue(clusterName, ns, name) }

	// CloudflareAccount Ready (tokens/verify).
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cf-token"}, Data: map[string][]byte{"token": []byte(lv.token)}}
	if err := e.Client.Create(ctx(t), sec); err != nil {
		t.Fatal(err)
	}
	acct := &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "acct"},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{AccountID: lv.accountID, BaseURL: lv.specURL,
			TokenSecretRef: cloudflarev1alpha1.SecretKeySelector{Name: "cf-token", Key: "token"}},
	}
	if err := e.Client.Create(ctx(t), acct); err != nil {
		t.Fatal(err)
	}
	acct = e.WaitAccountCondition(t, ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	t.Logf("CloudflareAccount Ready: tokenType=%s atProvider.status=%s", acct.Status.TokenType, acct.Status.AtProvider.Status)
	if acct.Status.AtProvider.Status != "active" {
		t.Errorf("atProvider.status = %q, want active", acct.Status.AtProvider.Status)
	}

	// KVNamespace, Queue, D1Database (deletionPolicy Delete).
	rs := commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete}
	kv := &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kv"},
		Spec: kvv1alpha1.KVNamespaceSpec{ResourceSpec: rs, ForProvider: kvv1alpha1.KVNamespaceParameters{Title: ptr.To(lv.prefix + "-kv")}}}
	q := &queuesv1alpha1.Queue{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "queue"},
		Spec: queuesv1alpha1.QueueSpec{ResourceSpec: rs, ForProvider: queuesv1alpha1.QueueParameters{QueueName: ptr.To(lv.prefix + "-q"),
			Settings: &queuesv1alpha1.QueueSettingsParameters{DeliveryDelay: ptr.To[int64](5), MessageRetentionPeriod: ptr.To[int64](7200)}}}}
	db := &d1v1alpha1.D1Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
		Spec: d1v1alpha1.D1DatabaseSpec{ResourceSpec: rs, ForProvider: d1v1alpha1.D1DatabaseParameters{Name: ptr.To(lv.prefix + "-db"),
			ReadReplication: &d1v1alpha1.D1DatabaseReadReplicationParameters{Mode: "disabled"}}}}
	mark := lv.rec.Len()
	for _, o := range []client.Object{kv, q, db} {
		if err := e.Client.Create(ctx(t), o); err != nil {
			t.Fatal(err)
		}
	}
	type res struct {
		obj        commonv1alpha1.Managed
		kind, name string
		tagType    string
		getPath    func(id string) string
		tr         *tracked
	}
	all := []*res{
		{obj: kv, kind: "KV namespace", name: *kv.Spec.ForProvider.Title, tagType: "kv_namespace",
			getPath: func(id string) string { return lv.acct("/storage/kv/namespaces/" + id) }},
		{obj: q, kind: "Queue", name: *q.Spec.ForProvider.QueueName, tagType: "queue",
			getPath: func(id string) string { return lv.acct("/queues/" + id) }},
		{obj: db, kind: "D1 database", name: *db.Spec.ForProvider.Name, tagType: "d1_database",
			getPath: func(id string) string { return lv.acct("/d1/database/" + id) }},
	}
	for _, r := range all {
		// Track as soon as the ID is known, so a failing wait still cleans up.
		key := client.ObjectKeyFromObject(r.obj)
		testenv.Eventually(t, waitTimeout, func() (bool, string) {
			if err := e.Client.Get(ctx(t), key, r.obj); err != nil {
				return false, err.Error()
			}
			return r.obj.GetResourceStatus().ID != "", condString(r.obj.GetResourceStatus().Conditions)
		})
		id := r.obj.GetResourceStatus().ID
		r.tr = lv.track(t, r.kind, r.name, id, r.getPath(id))
		waitManaged(t, e, r.obj)
	}
	time.Sleep(3 * time.Second)
	t.Logf("create writes: %v", redactAll(Writes(lv.rec.Since(mark))))

	// Ownership tags (Resource Tagging) on each.
	for _, r := range all {
		got, st := ownerTag(t, "op-check-owner-tag", r.tagType, r.obj.GetResourceStatus().ID)
		if got != owner(r.obj.GetName()) {
			t.Errorf("%s owner tag = %q (HTTP %d), want %q", r.kind, got, st, owner(r.obj.GetName()))
		}
	}

	// Tunnel (no connector) and a VPCService on it (FQDN hostname).
	tun := &tunnelsv1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "tun"},
		Spec: tunnelsv1alpha1.TunnelSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider: tunnelsv1alpha1.TunnelParameters{Name: lv.prefix + "-tun"},
			Connector:   tunnelsv1alpha1.ConnectorSpec{Replicas: ptr.To[int32](0)}}}
	if err := e.Client.Create(ctx(t), tun); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, waitTimeout, func() (bool, string) {
		_ = e.Client.Get(ctx(t), client.ObjectKeyFromObject(tun), tun)
		return tun.Status.ID != "", condString(tun.Status.Conditions)
	})
	tunTr := lv.track(t, "Tunnel", tun.Spec.ForProvider.Name, tun.Status.ID, lv.acct("/cfd_tunnel/"+tun.Status.ID))
	settleDeployment(t, e, tun)
	waitManaged(t, e, tun)
	testenv.Eventually(t, time.Minute, func() (bool, string) {
		_ = e.Client.Get(ctx(t), client.ObjectKeyFromObject(tun), tun)
		name := tun.Status.Connector.TokenSecretName
		if name == "" {
			return false, "no status.connector.tokenSecretName"
		}
		var s corev1.Secret
		if err := e.Client.Get(ctx(t), client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
			return false, err.Error()
		}
		return len(s.Data["token"]) > 20 && metav1.IsControlledBy(&s, tun), fmt.Sprintf("token %d bytes", len(s.Data["token"]))
	})
	if got, st := ownerTag(t, "op-check-owner-tag", "cloudflared_tunnel", tun.Status.ID); got != owner("tun") {
		t.Errorf("Tunnel owner tag = %q (HTTP %d), want %q", got, st, owner("tun"))
	}

	host := fmt.Sprintf("flare-spike-live.%s.svc.cluster.local", ns)
	vpc := &workersvpcv1alpha1.VPCService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: workersvpcv1alpha1.VPCServiceSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider: &workersvpcv1alpha1.VPCServiceParameters{Name: lv.prefix + "-vpc", Type: "http",
				Host: workersvpcv1alpha1.VPCServiceHost{Hostname: ptr.To(host)}, HTTPPort: ptr.To[int32](80),
				TunnelRef: &commonv1alpha1.LocalRef{Name: "tun"}}}}
	if err := e.Client.Create(ctx(t), vpc); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, waitTimeout, func() (bool, string) {
		_ = e.Client.Get(ctx(t), client.ObjectKeyFromObject(vpc), vpc)
		return vpc.Status.ID != "", condString(vpc.Status.Conditions)
	})
	vpcTr := lv.track(t, "VPC service", vpc.Spec.ForProvider.Name, vpc.Status.ID, lv.acct("/connectivity/directory/services/"+vpc.Status.ID))
	waitManaged(t, e, vpc)
	{
		st, env := lv.probe(t, "op-check-vpc", http.MethodGet, lv.acct("/connectivity/directory/services/"+vpc.Status.ID), nil, nil)
		if st != http.StatusOK {
			t.Fatalf("GET VPC service: %d %s", st, env)
		}
		got := decode[struct {
			Host struct {
				Hostname        string `json:"hostname"`
				ResolverNetwork struct {
					TunnelID string `json:"tunnel_id"`
				} `json:"resolver_network"`
			} `json:"host"`
		}](t, env.Result)
		if got.Host.Hostname != host || got.Host.ResolverNetwork.TunnelID != tun.Status.ID {
			t.Errorf("VPC service host = %+v, want %s via tunnel %s", got.Host, host, tun.Status.ID)
		}
	}

	// Steady state: re-reconcile every object; zero writes.
	managed := []commonv1alpha1.Managed{kv, q, db, tun, vpc}
	time.Sleep(3 * time.Second)
	mark = lv.rec.Len()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, mg := range managed {
		obj := mg.(client.Object)
		update(t, e, obj, func() {
			a := obj.GetAnnotations()
			if a == nil {
				a = map[string]string{}
			}
			a["live.flare.dev/poke"] = stamp
			obj.SetAnnotations(a)
		})
	}
	testenv.Eventually(t, waitTimeout, func() (bool, string) {
		es := lv.rec.Since(mark)
		var missing []string
		for _, mg := range managed {
			id := mg.GetResourceStatus().ID
			if !slices.ContainsFunc(es, func(c Cassette) bool { return c.Method == http.MethodGet && strings.Contains(c.Path, id) }) {
				missing = append(missing, fmt.Sprintf("%T", mg))
			}
		}
		return len(missing) == 0, "not re-read yet: " + strings.Join(missing, ", ")
	})
	time.Sleep(5 * time.Second)
	if w := Writes(lv.rec.Since(mark)); len(w) != 0 {
		t.Errorf("steady state: %d write(s) on re-reconcile: %v", len(w), redactAll(w))
	} else {
		t.Logf("steady state: %d requests after re-reconcile, zero writes", len(lv.rec.Since(mark)))
	}

	// Update KV (rename, PUT), Queue settings (PATCH), D1 read replication (PATCH).
	mark = lv.rec.Len()
	newTitle := *kv.Spec.ForProvider.Title + "-renamed"
	update(t, e, kv, func() { kv.Spec.ForProvider.Title = ptr.To(newTitle) })
	update(t, e, q, func() { q.Spec.ForProvider.Settings.DeliveryDelay = ptr.To[int64](10) })
	update(t, e, db, func() {
		db.Spec.ForProvider.ReadReplication = &d1v1alpha1.D1DatabaseReadReplicationParameters{Mode: "auto"}
	})
	for _, mg := range []commonv1alpha1.Managed{kv, q, db} {
		waitManaged(t, e, mg)
	}
	time.Sleep(3 * time.Second)
	wantW := []string{
		"PUT " + lv.acct("/storage/kv/namespaces/"+kv.Status.ID),
		"PATCH " + lv.acct("/queues/"+q.Status.ID),
		"PATCH " + lv.acct("/d1/database/"+db.Status.ID),
	}
	gotW := Writes(lv.rec.Since(mark))
	slices.Sort(gotW)
	slices.Sort(wantW)
	if !slices.Equal(gotW, wantW) {
		t.Errorf("update writes = %v, want %v", redactAll(gotW), redactAll(wantW))
	}
	{
		_, env := lv.probe(t, "op-check-kv", http.MethodGet, lv.acct("/storage/kv/namespaces/"+kv.Status.ID), nil, nil)
		if got := decode[struct{ Title string }](t, env.Result); got.Title != newTitle {
			t.Errorf("KV title after update = %q, want %q", got.Title, newTitle)
		}
		all[0].tr.name = newTitle
		_, env = lv.probe(t, "op-check-queue", http.MethodGet, lv.acct("/queues/"+q.Status.ID), nil, nil)
		got := decode[struct {
			Settings struct {
				DeliveryDelay          int64 `json:"delivery_delay"`
				MessageRetentionPeriod int64 `json:"message_retention_period"`
			} `json:"settings"`
		}](t, env.Result)
		if got.Settings.DeliveryDelay != 10 || got.Settings.MessageRetentionPeriod != 7200 {
			t.Errorf("queue settings after update = %+v, want 10/7200", got.Settings)
		}
		_, env = lv.probe(t, "op-check-d1", http.MethodGet, lv.acct("/d1/database/"+db.Status.ID), nil, nil)
		gd := decode[struct {
			ReadReplication struct{ Mode string } `json:"read_replication"`
		}](t, env.Result)
		if gd.ReadReplication.Mode != "auto" {
			t.Errorf("D1 read_replication after update = %q, want auto", gd.ReadReplication.Mode)
		}
	}

	// Delete the Tunnel and its VPCService together: the service must go first.
	settleDeployment(t, e, tun)
	mark = lv.rec.Len()
	tunID, vpcID := tun.Status.ID, vpc.Status.ID
	if err := e.Client.Delete(ctx(t), tun); err != nil {
		t.Fatal(err)
	}
	if err := e.Client.Delete(ctx(t), vpc); err != nil {
		t.Fatal(err)
	}
	waitGone(t, e, vpc)
	waitGone(t, e, tun)
	es := lv.rec.Since(mark)
	iv := slices.IndexFunc(es, func(c Cassette) bool {
		return c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/connectivity/directory/services/"+vpcID)
	})
	it := slices.IndexFunc(es, func(c Cassette) bool {
		return c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/cfd_tunnel/"+tunID)
	})
	if iv < 0 || it < 0 || iv > it {
		t.Errorf("delete order: VPC service DELETE at %d, tunnel DELETE at %d; want the service first", iv, it)
	}
	if st, env := lv.probe(t, "op-vpc-get-after-delete", http.MethodGet, lv.acct("/connectivity/directory/services/"+vpcID), nil, nil); st == http.StatusNotFound {
		lv.markDeleted(t, vpcTr, "operator (GET 404)")
	} else {
		t.Errorf("VPC service after delete: %d %s", st, env)
	}
	if st, env := lv.probe(t, "op-tunnel-get-after-delete", http.MethodGet, lv.acct("/cfd_tunnel/"+tunID), nil, nil); st == http.StatusOK {
		got := decode[struct {
			DeletedAt *string `json:"deleted_at"`
		}](t, env.Result)
		if got.DeletedAt == nil {
			t.Errorf("tunnel after delete has deleted_at null")
		} else {
			lv.markDeleted(t, tunTr, "operator (soft delete, deleted_at set)")
		}
	} else if st == http.StatusNotFound {
		lv.markDeleted(t, tunTr, "operator (GET 404)")
	} else {
		t.Errorf("tunnel after delete: %d %s", st, env)
	}

	// Delete KV, Queue, D1 (deletionPolicy Delete).
	for _, r := range all {
		if err := e.Client.Delete(ctx(t), r.obj.(client.Object)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range all {
		waitGone(t, e, r.obj.(client.Object))
		id := r.tr.id
		st, env := lv.probe(t, "op-get-after-delete", http.MethodGet, r.getPath(id), nil, nil)
		if st == http.StatusNotFound {
			lv.markDeleted(t, r.tr, "operator (GET 404)")
		} else {
			t.Errorf("%s after delete: %d %s", r.kind, st, env)
		}
		_, _ = ownerTag(t, "op-tags-get-deleted-"+strings.ReplaceAll(r.tagType, "_", "-"), r.tagType, id)
	}
	lv.probe(t, "op-tags-index-after-delete", http.MethodGet, lv.acct("/tags/resources"), nil, nil)

	if err := e.Client.Delete(ctx(t), acct); err != nil {
		t.Fatal(err)
	}
	waitGone(t, e, acct)
	if w := lv.rec.Errors(); len(w) > 0 {
		t.Errorf("cassette write errors: %v", w)
	}
}

func redactAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = redactPath(s)
	}
	return out
}

// ---- direct probes of UNVERIFIED behaviours ----

// TestLiveProbes records behaviours flarefake models without recordings (docs/STATUS.md
// UNVERIFIED): tokens/verify, Resource Tagging (never tagged, PUT, If-Match, DELETE, index,
// deleted resources), tunnel token and duplicate names, VPC services without ports, and Workers
// script settings PATCH (multipart vs JSON). It asserts only what the operator relies on; the
// rest is logged and recorded for the flarefake fidelity follow-up.
func TestLiveProbes(t *testing.T) {
	requireLive(t)

	// tokens/verify: account and user endpoints, valid and invalid token.
	lv.probe(t, "tokens-verify-account", http.MethodGet, lv.acct("/tokens/verify"), nil, nil)
	lv.probe(t, "tokens-verify-user", http.MethodGet, "/user/tokens/verify", nil, nil)
	st, env, err := lv.do("tokens-verify-account-invalid", http.MethodGet, lv.acct("/tokens/verify"), nil, nil, "flare-spike-invalid-"+randHex(16))
	t.Logf("PROBE tokens-verify-account-invalid -> %d %s %v", st, env, err)
	st, env, err = lv.do("tokens-verify-user-invalid", http.MethodGet, "/user/tokens/verify", nil, nil, "flare-spike-invalid-"+randHex(16))
	t.Logf("PROBE tokens-verify-user-invalid -> %d %s %v", st, env, err)

	t.Run("Tags", probeTags)
	t.Run("TunnelVPC", probeTunnelVPC)
	t.Run("WorkersSettings", probeWorkersSettings)
	if w := lv.rec.Errors(); len(w) > 0 {
		t.Errorf("cassette write errors: %v", w)
	}
}

type tagsResult struct {
	Tags map[string]string `json:"tags"`
	ETag string            `json:"etag"`
}

func probeTags(t *testing.T) {
	st, env := lv.probe(t, "tagprobe-kv-create", http.MethodPost, lv.acct("/storage/kv/namespaces"), map[string]any{"title": lv.prefix + "-tagprobe"}, nil)
	if st != http.StatusOK {
		t.Fatalf("create KV namespace: %d %s", st, env)
	}
	id := decode[struct{ ID string }](t, env.Result).ID
	tr := lv.track(t, "KV namespace", lv.prefix+"-tagprobe", id, lv.acct("/storage/kv/namespaces/"+id))

	target := map[string]any{"resource_type": "kv_namespace", "resource_id": id}
	with := func(tags map[string]string) map[string]any {
		m := map[string]any{"resource_type": "kv_namespace", "resource_id": id, "tags": tags}
		return m
	}
	getPath := lv.acct("/tags?" + url.Values{"resource_type": {"kv_namespace"}, "resource_id": {id}}.Encode())
	get := func(phase string) (int, tagsResult) {
		st, env := lv.probe(t, phase, http.MethodGet, getPath, nil, nil)
		var r tagsResult
		if st == http.StatusOK {
			r = decode[tagsResult](t, env.Result)
		}
		t.Logf("  tags=%v etag=%q", r.Tags, r.ETag)
		return st, r
	}
	indexOne := lv.acct("/tags/resources?" + url.Values{"type": {"kv_namespace"}, "id": {id}}.Encode())

	get("tags-get-never-tagged")
	get("tags-get-never-tagged-again")
	lv.probe(t, "tags-index-never-tagged", http.MethodGet, indexOne, nil, nil)
	lv.probe(t, "tags-index-all-before", http.MethodGet, lv.acct("/tags/resources"), nil, nil)

	owner := reconcile.OwnerValue(clusterName, "probe", "kv") // '/' in the value
	if st, _ := lv.probe(t, "tags-put-first", http.MethodPut, lv.acct("/tags"), with(map[string]string{reconcile.OwnerTagKey: owner, "flare-spike": "probe"}), nil); st != http.StatusOK {
		t.Errorf("first tags PUT (owner key with '/', value with '/') = %d", st)
	}
	_, r := get("tags-get-after-put")
	if r.Tags[reconcile.OwnerTagKey] != owner {
		t.Errorf("owner tag after PUT = %q, want %q", r.Tags[reconcile.OwnerTagKey], owner)
	}
	lv.probe(t, "tags-put-ifmatch-mismatch", http.MethodPut, lv.acct("/tags"), with(map[string]string{"flare-spike": "mismatch"}), map[string]string{"If-Match": "v1:0000000000000000"})
	_, r = get("tags-get-after-mismatch")
	if r.ETag != "" {
		lv.probe(t, "tags-put-ifmatch-unquoted", http.MethodPut, lv.acct("/tags"), with(map[string]string{reconcile.OwnerTagKey: owner, "flare-spike": "unquoted"}), map[string]string{"If-Match": r.ETag})
		_, r = get("tags-get-after-unquoted")
	}
	if r.ETag != "" {
		lv.probe(t, "tags-put-ifmatch-quoted", http.MethodPut, lv.acct("/tags"), with(map[string]string{reconcile.OwnerTagKey: owner, "flare-spike": "quoted"}), map[string]string{"If-Match": `"` + strings.Trim(r.ETag, `"`) + `"`})
	}
	lv.probe(t, "tags-index-filtered-after-put", http.MethodGet, indexOne, nil, nil)
	lv.probe(t, "tags-index-type-after-put", http.MethodGet, lv.acct("/tags/resources?type=kv_namespace"), nil, nil)
	_, r = get("tags-get-before-delete")
	hdr := map[string]string{}
	if r.ETag != "" {
		hdr["If-Match"] = r.ETag
	}
	lv.probe(t, "tags-delete", http.MethodDelete, lv.acct("/tags"), target, hdr)
	get("tags-get-after-delete")
	lv.probe(t, "tags-index-after-tags-delete", http.MethodGet, indexOne, nil, nil)
	lv.probe(t, "tags-delete-again", http.MethodDelete, lv.acct("/tags"), target, nil)

	if lv.noTag != "" {
		st, env, err := lv.do("tags-get-notag-token", http.MethodGet, getPath, nil, nil, lv.noTag)
		t.Logf("PROBE tags-get-notag-token -> %d %s %v", st, env, err)
	} else {
		t.Log("no FLARE_LIVE_NOTAG_TOKEN_FILE: skipping the tags read with a token lacking tagging permission")
	}

	// Tags of a deleted resource, and the index after the resource is gone.
	lv.probe(t, "tags-put-before-resource-delete", http.MethodPut, lv.acct("/tags"), with(map[string]string{reconcile.OwnerTagKey: owner}), nil)
	if st, env := lv.probe(t, "tagprobe-kv-delete", http.MethodDelete, lv.acct("/storage/kv/namespaces/"+id), nil, nil); st == http.StatusOK {
		lv.markDeleted(t, tr, "probe DELETE 200")
	} else {
		t.Errorf("delete KV namespace: %d %s", st, env)
	}
	get("tags-get-deleted-resource")
	lv.probe(t, "tags-index-filtered-after-resource-delete", http.MethodGet, indexOne, nil, nil)
	lv.probe(t, "tags-index-all-after-resource-delete", http.MethodGet, lv.acct("/tags/resources"), nil, nil)
	lv.probe(t, "tags-delete-deleted-resource", http.MethodDelete, lv.acct("/tags"), target, nil)
	lv.probe(t, "tags-index-all-final", http.MethodGet, lv.acct("/tags/resources"), nil, nil)
}

func probeTunnelVPC(t *testing.T) {
	name := lv.prefix + "-probe-tun"
	st, env := lv.probe(t, "tunnel-create", http.MethodPost, lv.acct("/cfd_tunnel"), map[string]any{"name": name, "config_src": "cloudflare"}, nil)
	if st != http.StatusOK {
		t.Fatalf("create tunnel: %d %s", st, env)
	}
	id := decode[struct{ ID string }](t, env.Result).ID
	tr := lv.track(t, "Tunnel", name, id, lv.acct("/cfd_tunnel/"+id))
	if st, _ := lv.probe(t, "tunnel-token-get", http.MethodGet, lv.acct("/cfd_tunnel/"+id+"/token"), nil, nil); st != http.StatusOK {
		t.Errorf("tunnel token GET = %d", st)
	}
	var dupTr *tracked
	if st, env := lv.probe(t, "tunnel-create-duplicate-name", http.MethodPost, lv.acct("/cfd_tunnel"), map[string]any{"name": name, "config_src": "cloudflare"}, nil); st == http.StatusOK {
		dup := decode[struct{ ID string }](t, env.Result).ID
		dupTr = lv.track(t, "Tunnel (duplicate name)", name, dup, lv.acct("/cfd_tunnel/"+dup))
	}
	lv.probe(t, "tunnel-list-by-name", http.MethodGet, lv.acct("/cfd_tunnel?"+url.Values{"name": {name}, "is_deleted": {"false"}}.Encode()), nil, nil)

	// VPC services with ports / app_protocol omitted.
	var vpcs []*tracked
	for _, c := range []struct {
		phase string
		body  map[string]any
	}{
		{"vpc-create-http-noports", map[string]any{"name": lv.prefix + "-probe-vpc-http", "type": "http",
			"host": map[string]any{"hostname": "flare-spike-probe.default.svc.cluster.local", "resolver_network": map[string]any{"tunnel_id": id}}}},
		{"vpc-create-tcp-noport-noappproto", map[string]any{"name": lv.prefix + "-probe-vpc-tcp", "type": "tcp",
			"host": map[string]any{"ipv4": "192.0.2.10", "network": map[string]any{"tunnel_id": id}}}},
		{"vpc-create-tcp-port-noappproto", map[string]any{"name": lv.prefix + "-probe-vpc-tcp2", "type": "tcp", "tcp_port": 5432,
			"host": map[string]any{"ipv4": "192.0.2.11", "network": map[string]any{"tunnel_id": id}}}},
	} {
		st, env := lv.probe(t, c.phase, http.MethodPost, lv.acct("/connectivity/directory/services"), c.body, nil)
		if st != http.StatusOK {
			continue
		}
		sid := decode[struct {
			ServiceID string `json:"service_id"`
		}](t, env.Result).ServiceID
		vtr := lv.track(t, "VPC service", c.body["name"].(string), sid, lv.acct("/connectivity/directory/services/"+sid))
		vpcs = append(vpcs, vtr)
		lv.probe(t, strings.Replace(c.phase, "create", "get", 1), http.MethodGet, lv.acct("/connectivity/directory/services/"+sid), nil, nil)
	}
	for _, v := range vpcs {
		if st, env := lv.probe(t, "vpc-delete", http.MethodDelete, v.deletePath, nil, nil); st < 300 {
			lv.markDeleted(t, v, fmt.Sprintf("probe DELETE %d", st))
		} else {
			t.Errorf("delete VPC service: %d %s", st, env)
		}
	}
	for _, x := range []*tracked{dupTr, tr} {
		if x == nil {
			continue
		}
		if st, env := lv.probe(t, "tunnel-delete", http.MethodDelete, x.deletePath, nil, nil); st < 300 {
			lv.markDeleted(t, x, fmt.Sprintf("probe DELETE %d (soft delete)", st))
		} else {
			t.Errorf("delete tunnel: %d %s", st, env)
		}
	}
	lv.probe(t, "tunnel-get-after-delete", http.MethodGet, lv.acct("/cfd_tunnel/"+id), nil, nil)
	lv.probe(t, "tunnel-token-get-after-delete", http.MethodGet, lv.acct("/cfd_tunnel/"+id+"/token"), nil, nil)
}

func probeWorkersSettings(t *testing.T) {
	name := lv.prefix + "-w"
	path := lv.acct("/workers/scripts/" + name)
	up := multipartOf(
		part{"metadata", "blob", "application/json", `{"main_module":"worker.mjs","compatibility_date":"2026-09-01"}`},
		part{"worker.mjs", "worker.mjs", "application/javascript+module", "export default {\n  async fetch() {\n    return new Response(\"flare-spike\");\n  }\n};\n"},
	)
	st, env := lv.probe(t, "workers-upload", http.MethodPut, path, up, nil)
	if st != http.StatusOK {
		t.Fatalf("upload worker: %d %s", st, env)
	}
	tr := lv.track(t, "Workers script", name, name, path)
	lv.probe(t, "workers-settings-get", http.MethodGet, path+"/settings", nil, nil)
	lv.probe(t, "workers-settings-patch-multipart", http.MethodPatch, path+"/settings",
		multipartOf(part{"settings", "blob", "application/json", `{"tags":["flare-spike-live"]}`}), nil)
	lv.probe(t, "workers-settings-get-after-multipart", http.MethodGet, path+"/settings", nil, nil)
	lv.probe(t, "workers-settings-patch-json", http.MethodPatch, path+"/settings", `{"logpush":false}`, nil)
	lv.probe(t, "workers-settings-patch-multipart-nosettings", http.MethodPatch, path+"/settings",
		multipartOf(part{"other", "blob", "application/json", `{}`}), nil)
	lv.probe(t, "workers-settings-get-final", http.MethodGet, path+"/settings", nil, nil)
	lv.probe(t, "workers-versions-list", http.MethodGet, path+"/versions", nil, nil)
	lv.probe(t, "workers-deployments-list", http.MethodGet, path+"/deployments", nil, nil)
	if st, env := lv.probe(t, "workers-delete", http.MethodDelete, path, nil, nil); st == http.StatusOK {
		lv.markDeleted(t, tr, "probe DELETE 200")
	} else {
		t.Errorf("delete worker: %d %s", st, env)
	}
	lv.probe(t, "workers-settings-get-after-delete", http.MethodGet, path+"/settings", nil, nil)
	lv.probe(t, "workers-settings-patch-after-delete", http.MethodPatch, path+"/settings",
		multipartOf(part{"settings", "blob", "application/json", `{"logpush":false}`}), nil)
}
