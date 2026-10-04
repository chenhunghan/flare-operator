package artifact

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

// testRegistry is an in-memory OCI registry (go-containerregistry pkg/registry) served over
// TLS, optionally behind basic auth, counting manifest GETs.
type testRegistry struct {
	srv       *httptest.Server
	loader    *Loader
	host      string
	manifests atomic.Int32
	push      []remote.Option
	// gate, when set, runs before every request is served (a test blocks there).
	gate atomic.Pointer[func(*http.Request)]
}

func newTestRegistry(t *testing.T, user, pass string, mut ...func(*Options)) *testRegistry {
	t.Helper()
	r := &testRegistry{}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if user != "" {
			u, p, ok := req.BasicAuth()
			if !ok || u != user || p != pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, http.StatusUnauthorized)
				return
			}
		}
		if g := r.gate.Load(); g != nil {
			(*g)(req)
		}
		if strings.Contains(req.URL.Path, "/manifests/") && req.Method == http.MethodGet {
			r.manifests.Add(1)
		}
		reg.ServeHTTP(w, req)
	})
	r.srv, r.loader = tlsServer(t, h, mut...)
	r.host = strings.TrimPrefix(r.srv.URL, "https://")
	// go-containerregistry would push to 127.0.0.1 over plain http; upgrade as the loader does.
	r.push = []remote.Option{remote.WithTransport(httpsOnly{base: r.srv.Client().Transport, upgradeInitial: true})}
	if user != "" {
		r.push = append(r.push, remote.WithAuth(&authn.Basic{Username: user, Password: pass}))
	}
	return r
}

// image builds an image from layers (each a tar archive, gzip-compressed as an OCI layer).
func image(t *testing.T, layers ...[]byte) v1.Image {
	t.Helper()
	var ls []v1.Layer
	for _, b := range layers {
		ls = append(ls, static.NewLayer(gz(t, b), types.OCILayer))
	}
	img, err := mutate.AppendLayers(empty.Image, ls...)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// pushImage writes img as repo:tag and returns its digest.
func (r *testRegistry) pushImage(t *testing.T, repo string, img v1.Image) string {
	t.Helper()
	ref, err := name.ParseReference(r.host + "/" + repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, r.push...); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

func ociSource(image, p string, secret string) sharedv1alpha1.ArtifactSource {
	s := &sharedv1alpha1.OCIArtifactSource{Image: image, Path: p}
	if secret != "" {
		s.PullSecretRef = &sharedv1alpha1.ArtifactSecretRef{Name: secret}
	}
	return sharedv1alpha1.ArtifactSource{OCIRef: s}
}

func TestOCISource(t *testing.T) {
	r := newTestRegistry(t, "", "")
	lower := tarOf(t, dir("site/"), file("site/index.html", "v1"), file("site/old.html", "gone"), file("site/dir/x", "hidden"),
		file("site/file-then-dir", "f"), file("etc/passwd", "root"))
	upper := tarOf(t, file("site/.wh.old.html", ""), file("site/dir/.wh..wh..opq", ""), file("site/dir/y", "new"),
		file("site/index.html", "v2"), file("site/file-then-dir/z", "d"), symlink("site/abs", "/site/index.html"),
		symlink("site/rel", "dir/y"))
	digest := r.pushImage(t, "site:v1", image(t, lower, upper))

	tr, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/site:v1", "site", ""))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range tr.Files() {
		got[f.Path] = string(f.Content)
	}
	want := map[string]string{"index.html": "v2", "dir/y": "new", "file-then-dir/z": "d", "abs": "v2", "rel": "new"}
	if len(got) != len(want) {
		t.Errorf("files %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
	if tr.ResolvedDigest != digest {
		t.Errorf("ResolvedDigest %q, want %q", tr.ResolvedDigest, digest)
	}

	// Pinned by digest: same tree, same resolved digest; the cache serves it, but the manifest
	// is fetched every time.
	before := r.manifests.Load()
	tr2, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/site@"+digest, "site", ""))
	if err != nil || tr2.Digest != tr.Digest || tr2.ResolvedDigest != digest {
		t.Fatalf("by digest: %v, %+v", err, tr2)
	}
	if n := r.manifests.Load() - before; n != 1 {
		t.Errorf("cached load made %d manifest GETs, want 1", n)
	}

	// A new push to the tag resolves to the new digest.
	d2 := r.pushImage(t, "site:v1", image(t, tarOf(t, file("site/index.html", "v3"))))
	tr3, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/site:v1", "site", ""))
	if err != nil || tr3.ResolvedDigest != d2 || tr3.Digest == tr.Digest {
		t.Errorf("after re-push: %v, resolved %q (want %q)", err, tr3.ResolvedDigest, d2)
	}
	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/nope:v1", "", ""))
	wantKind(t, err, KindFetch, "")
}

func TestOCISourceRejects(t *testing.T) {
	r := newTestRegistry(t, "", "", func(o *Options) { o.Limits.MaxArchiveBytes = 4 << 10 })
	r.pushImage(t, "escape:v1", image(t, tarOf(t, symlink("site/x", "/etc/passwd"))))
	_, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/escape:v1", "site", ""))
	wantKind(t, err, KindRejected, "outside the selected path")

	r.pushImage(t, "device:v1", image(t, tarOf(t, tent{name: "site/null", typ: 0x33})))
	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/device:v1", "site", ""))
	wantKind(t, err, KindRejected, "device")

	r.pushImage(t, "traversal:v1", image(t, tarOf(t, file("../../x", "y"))))
	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/traversal:v1", "", ""))
	wantKind(t, err, KindRejected, `".." segment`)

	big := make([]byte, 64<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(rng.Uint32()) // incompressible
	}
	r.pushImage(t, "big:v1", image(t, tarOf(t, file("site/big", string(big)))))
	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/big:v1", "site", ""))
	wantKind(t, err, KindRejected, "--artifact-max-archive-bytes")

	odd, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("not a tar"), "application/vnd.example.blob"))
	if err != nil {
		t.Fatal(err)
	}
	r.pushImage(t, "odd:v1", odd)
	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/odd:v1", "", ""))
	wantKind(t, err, KindRejected, "only tar layers")

	_, err = r.loader.Load(ctx(t), nil, "ns", ociSource("UPPER/Case::bad", "", ""))
	wantKind(t, err, KindInvalid, "")
}

func TestOCISourceIndex(t *testing.T) {
	r := newTestRegistry(t, "", "")
	img := image(t, tarOf(t, file("index.html", "arm")))
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img,
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}})
	ref, _ := name.ParseReference(r.host + "/multi:v1")
	if err := remote.WriteIndex(ref, idx, r.push...); err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	tr, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/multi:v1", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := tr.File("index.html"); string(f.Content) != "arm" || tr.ResolvedDigest != d.String() {
		t.Errorf("index.html %q, resolved %q (index %s)", f.Content, tr.ResolvedDigest, d)
	}
}

func dockerConfigSecret(ns, name, host, user, pass string, labelled bool) *corev1.Secret {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{"https://" + host + "/v1/": map[string]string{"auth": auth}}})
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: b}}
	if labelled {
		s.Labels = map[string]string{sharedv1alpha1.LabelArtifact: "true"}
	}
	return s
}

func TestOCISourcePullSecret(t *testing.T) {
	r := newTestRegistry(t, "robot", "s3cret")
	r.pushImage(t, "private:v1", image(t, tarOf(t, file("index.html", "private"))))
	opaque := dockerConfigSecret("a", "opaque", r.host, "robot", "s3cret", true)
	opaque.Type = corev1.SecretTypeOpaque
	c := fake.NewClientBuilder().WithObjects(
		dockerConfigSecret("a", "pull", r.host, "robot", "s3cret", true),
		dockerConfigSecret("a", "unlabelled", r.host, "robot", "s3cret", false),
		dockerConfigSecret("a", "wrong", r.host, "robot", "nope", true),
		opaque,
	).Build()
	img := r.host + "/private:v1"

	tr, err := r.loader.Load(ctx(t), c, "a", ociSource(img, "", "pull"))
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := tr.File("index.html"); string(f.Content) != "private" {
		t.Errorf("index.html = %q", f.Content)
	}
	// The cache holds the tree now, but another namespace without credentials must not get it.
	_, err = r.loader.Load(ctx(t), c, "b", ociSource(img, "", ""))
	wantKind(t, err, KindFetch, "")
	_, err = r.loader.Load(ctx(t), c, "a", ociSource(img, "", "wrong"))
	wantKind(t, err, KindFetch, "")
	for _, s := range []string{"unlabelled", "opaque", "missing"} {
		_, err = r.loader.Load(ctx(t), c, "a", ociSource(img, "", s))
		wantKind(t, err, KindDependency, "is not usable")
	}
	// The Secret of namespace a is not visible from namespace b.
	_, err = r.loader.Load(ctx(t), c, "b", ociSource(img, "", "pull"))
	wantKind(t, err, KindDependency, "is not usable")
	for _, e := range []error{err} {
		if strings.Contains(e.Error(), "s3cret") {
			t.Errorf("error leaks the password: %v", e)
		}
	}
}

func TestDockerConfigKeychain(t *testing.T) {
	k := dockerConfigKeychain{"index.docker.io": {Username: "hub"}, "ghcr.io": {Username: "gh"}}
	for img, want := range map[string]string{"nginx": "hub", "docker.io/library/nginx": "hub", "ghcr.io/o/r": "gh", "quay.io/o/r": ""} {
		ref, _ := name.ParseReference(img)
		a, err := k.Resolve(ref.Context())
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := a.Authorization()
		if cfg.Username != want {
			t.Errorf("%s: user %q, want %q", img, cfg.Username, want)
		}
	}
	for in, want := range map[string]string{"https://index.docker.io/v1/": "index.docker.io", "GHCR.io": "ghcr.io", "r.example:5000/x": "r.example:5000"} {
		if got := registryKey(in); got != want {
			t.Errorf("registryKey(%q) = %q, want %q", in, got, want)
		}
	}
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "bad", Labels: map[string]string{sharedv1alpha1.LabelArtifact: "true"}},
		Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"x":{"auth":"!!"}}}`)}}
	_, err := pullSecretKeychain(ctx(t), fake.NewClientBuilder().WithObjects(bad).Build(), "a", "bad")
	wantKind(t, err, KindDependency, "not a valid docker config")
	var _ client.Reader = fake.NewClientBuilder().Build()
}
