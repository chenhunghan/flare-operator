// Package testenv is the integration-test harness: an envtest API server with every CRD from
// config/crd/bases, an in-process flarefake (httptest over internal/fake), a manager running
// the CloudflareAccount controller (plus any registered controllers a test asks for), and
// helpers to create Ready CloudflareAccounts and to assert on the flarefake request journal.
//
// Typical use in a controller package:
//
//	var env *testenv.Env
//
//	func TestMain(m *testing.M) { testenv.Main(m, &env, testenv.Options{}) }
//
//	func TestSomething(t *testing.T) {
//	    e := testenv.Require(t, env)                 // skips when envtest assets are missing
//	    mgr := e.StartManager(t, testenv.ManagerOptions{Controllers: []string{"kvnamespace"}})
//	    ns := e.Namespace(t)
//	    acct := e.CreateReadyAccount(t, ns, "acct")  // token Secret + Ready CloudflareAccount
//	    _ = mgr; _ = acct
//	    e.ClearJournal(t)
//	    // … create objects, wait, then:
//	    if w := testenv.Writes(e.Journal(t)); len(w) != 0 { t.Fatalf("unexpected writes %v", w) }
//	}
//
// Envtest assets (kube-apiserver, etcd) come from $KUBEBUILDER_ASSETS or, when unset, from
// ~/.cache/flare-operator/envtest/k8s (shared by all worktrees; `make envtest`) or
// ./bin/k8s/<K8sVersion>-<os>-<arch>. When neither exists every
// test that calls Require is skipped with a message saying how to fetch them.
//
// The fake starts in strict token mode (a sentinel token is registered), so only tokens
// registered through CreateAccount/AddToken verify. FakeControl.Reset returns it to open mode.
package testenv

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"flare.dev/operator/internal/controller"
	_ "flare.dev/operator/internal/controller/account" // registers the CloudflareAccount controller + scheme
	"flare.dev/operator/internal/fake"
)

// K8sVersion is the envtest Kubernetes version `make envtest` installs (ENVTEST_K8S_VERSION).
const K8sVersion = "1.37.0"

// SentinelToken is registered in the fake at start so token checks are strict.
const SentinelToken = "testenv-sentinel-token"

// Options configures Start.
type Options struct {
	// ExtraCRDPaths are added to <repo>/config/crd/bases.
	ExtraCRDPaths []string
	// AddToScheme adds API groups beyond client-go's and every controller.Register'ed group.
	AddToScheme []func(*runtime.Scheme) error
	// Fake configures the in-process flarefake.
	Fake fake.Options
}

// Env is a running harness.
type Env struct {
	Config *rest.Config
	Scheme *runtime.Scheme
	// Client talks to the API server directly (no cache).
	Client client.Client

	// Fake is the in-process emulator; FakeServer serves it; BaseURL is its /client/v4 root,
	// which CloudflareAccounts created by CreateAccount point at.
	Fake       *fake.Server
	FakeServer *httptest.Server
	BaseURL    string
	// Control drives flarefake over its HTTP control API (/_fake), like an out-of-process fake.
	Control *FakeControl

	env *envtest.Environment
}

// RepoRoot returns the repository root (derived from this file's location).
func RepoRoot() string {
	_, file, _, _ := goruntime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Assets returns the directory holding the envtest binaries and whether it exists.
func Assets() (string, bool) {
	has := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "kube-apiserver"))
		return err == nil
	}
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d, has(d)
	}
	name := fmt.Sprintf("%s-%s-%s", K8sVersion, goruntime.GOOS, goruntime.GOARCH)
	var bases []string
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, filepath.Join(home, ".cache", "flare-operator", "envtest", "k8s"))
	}
	bases = append(bases, filepath.Join(RepoRoot(), "bin", "k8s"))
	want := filepath.Join(bases[0], name)
	for _, base := range bases {
		if has(filepath.Join(base, name)) {
			return filepath.Join(base, name), true
		}
	}
	for _, base := range bases {
		matches, _ := filepath.Glob(filepath.Join(base, "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, m := range matches {
			if has(m) {
				return m, true
			}
		}
	}
	return want, false
}

// MissingAssetsMessage explains how to get the envtest binaries.
func MissingAssetsMessage() string {
	d, _ := Assets()
	return fmt.Sprintf("envtest assets not found (looked in %s); run `make envtest` or set KUBEBUILDER_ASSETS — envtest tests are skipped", d)
}

// Start boots the API server and flarefake. It fails when the assets are missing.
func Start(opts Options) (*Env, error) {
	assets, ok := Assets()
	if !ok {
		return nil, fmt.Errorf("%s", MissingAssetsMessage())
	}
	if os.Getenv("TESTENV_LOG") != "" {
		logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))
	} else {
		logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := controller.AddToScheme(scheme); err != nil {
		return nil, err
	}
	for _, add := range opts.AddToScheme {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}

	te := &envtest.Environment{
		CRDDirectoryPaths:     append([]string{filepath.Join(RepoRoot(), "config", "crd", "bases")}, opts.ExtraCRDPaths...),
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
		Scheme:                scheme,
	}
	cfg, err := te.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = te.Stop()
		return nil, err
	}
	fs := fake.New(opts.Fake)
	fs.AddToken(fake.Token{Value: SentinelToken, AccountID: "00000000000000000000000000000000"})
	hs := httptest.NewServer(fs)
	return &Env{
		Config:     cfg,
		Scheme:     scheme,
		Client:     c,
		Fake:       fs,
		FakeServer: hs,
		BaseURL:    hs.URL + "/client/v4",
		Control:    NewFakeControl(hs.URL),
		env:        te,
	}, nil
}

// Stop shuts everything down.
func (e *Env) Stop() error {
	if e == nil {
		return nil
	}
	e.FakeServer.Close()
	return e.env.Stop()
}

// Main is a TestMain body: it starts the harness into *out, runs the tests and stops it. When
// the envtest assets are missing it prints why, leaves *out nil and still runs the tests
// (Require skips them), so packages mixing unit and envtest tests keep their unit coverage.
//
// Main also turns on strict response validation (see StrictMain): every flarefake response in
// the package's tests is checked against the pinned spec, and the run fails on a violation that
// internal/fake's responseAllowlist does not cover. opts.Fake.NoStrictResponses opts out.
func Main(m *testing.M, out **Env, opts Options) {
	enableStrictResponses()
	if _, ok := Assets(); !ok {
		fmt.Fprintln(os.Stderr, "testenv: "+MissingAssetsMessage())
		os.Exit(strictExit(m.Run()))
	}
	e, err := Start(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv: "+err.Error())
		os.Exit(1)
	}
	*out = e
	code := m.Run()
	if err := e.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "testenv: stop: "+err.Error())
	}
	os.Exit(strictExit(code))
}

// StrictMain is a TestMain body for packages that run flarefake without the envtest harness:
// it validates every flarefake response against the pinned spec (except under -short, which
// skips loading the 26 MB spec) and fails the run on violations the allowlist does not cover.
func StrictMain(m *testing.M) {
	enableStrictResponses()
	os.Exit(strictExit(m.Run()))
}

func enableStrictResponses() {
	if !flag.Parsed() {
		flag.Parse()
	}
	if testing.Short() {
		return
	}
	spec, err := fake.LoadDefaultSpec()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv: load spec: "+err.Error())
		os.Exit(1)
	}
	fake.EnableStrictResponses(spec)
}

func strictExit(code int) int {
	if err := fake.StrictResponseError(); err != nil {
		fmt.Fprintln(os.Stderr, "testenv: "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Require returns e, or skips t when the harness is not running.
func Require(t testing.TB, e *Env) *Env {
	t.Helper()
	if e == nil {
		t.Skip(MissingAssetsMessage())
	}
	return e
}

// Context returns a context cancelled at the end of t (or after timeout).
func Context(t testing.TB, timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// Eventually polls fn until it returns true or timeout passes, then fails t with fn's last
// message.
func Eventually(t testing.TB, timeout time.Duration, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var msg string
	for {
		var ok bool
		ok, msg = fn()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v: %s", timeout, msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
