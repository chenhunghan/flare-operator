package artifact

import (
	"flag"
	"net/netip"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

func configMap(ns, name string, labelled bool, data map[string]string, bin map[string][]byte) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data, BinaryData: bin}
	if labelled {
		cm.Labels = map[string]string{sharedv1alpha1.LabelArtifact: "true"}
	}
	return cm
}

func cmSource(refs ...sharedv1alpha1.ConfigMapArtifact) sharedv1alpha1.ArtifactSource {
	return sharedv1alpha1.ArtifactSource{ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{ConfigMaps: refs}}
}

func TestConfigMapSource(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(
		configMap("ns", "root", true, map[string]string{"index.html": "<h1>hi</h1>", "app.js": "x()"}, map[string][]byte{"logo.png": {0x89, 'P', 'N', 'G'}}),
		configMap("ns", "css", true, map[string]string{"a.css": "body{}", "b.css": "p{}"}, nil),
		configMap("ns", "plain", false, map[string]string{"x": "y"}, nil),
		configMap("other", "theirs", true, map[string]string{"x": "y"}, nil),
		configMap("ns", "dup", true, map[string]string{"index.html": "again"}, nil),
		configMap("ns", "empty", true, nil, nil),
	).Build()
	l, err := NewLoader(Options{})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := l.Load(ctx(t), c, "ns", cmSource(
		sharedv1alpha1.ConfigMapArtifact{Name: "root"},
		sharedv1alpha1.ConfigMapArtifact{Name: "css", Path: "static/css", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "a.css", Path: "v1/a.css"}}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tr.Paths(), ","); got != "app.js,index.html,logo.png,static/css/v1/a.css" {
		t.Errorf("paths %s", got)
	}
	if f, _ := tr.File("logo.png"); f.ContentType != "image/png" || string(f.Content) != "\x89PNG" {
		t.Errorf("logo.png %+v", f)
	}

	// The same files from an archive have the same digest.
	archive := tarOf(t, file("index.html", "<h1>hi</h1>"), file("app.js", "x()"), file("logo.png", "\x89PNG"), file("static/css/v1/a.css", "body{}"))
	files, err := extractArchive(archive, "", l.Limits())
	if err != nil {
		t.Fatal(err)
	}
	if other, _ := newTree(files, l.Limits()); other.Digest != tr.Digest {
		t.Errorf("archive digest %s != ConfigMap digest %s", other.Digest, tr.Digest)
	}

	for name, c2 := range map[string]struct {
		src  sharedv1alpha1.ArtifactSource
		kind Kind
		sub  string
	}{
		"unlabelled":      {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "plain"}), KindDependency, "is not usable"},
		"missing":         {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "nope"}), KindDependency, "is not usable"},
		"other namespace": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "theirs"}), KindDependency, "is not usable"},
		"missing key": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "css", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "c.css", Path: "c.css"}}}),
			KindDependency, `no key "c.css"`},
		"duplicate path": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "root"}, sharedv1alpha1.ConfigMapArtifact{Name: "dup"}), KindRejected, "comes from both"},
		"traversal path": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "root", Path: "../x"}), KindInvalid, ""},
		"traversal item": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "css", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "a.css", Path: "a/../../b"}}}),
			KindInvalid, ""},
		"traversal item under path": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "css", Path: "d/e", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "a.css", Path: "a/../../x"}}}),
			KindInvalid, `".." segment`},
		"absolute item": {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "css", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "a.css", Path: "/etc/x"}}}),
			KindInvalid, ""},
		"no files":      {cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "empty"}), KindRejected, "no files"},
		"no configmaps": {cmSource(), KindInvalid, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := l.Load(ctx(t), c, "ns", c2.src)
			wantKind(t, err, c2.kind, c2.sub)
		})
	}

	small, _ := NewLoader(Options{Limits: Limits{MaxBytes: 16, MaxExpandedBytes: 16}})
	_, err = small.Load(ctx(t), c, "ns", cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "root"}))
	wantKind(t, err, KindRejected, "--artifact-max-bytes")
	few, _ := NewLoader(Options{Limits: Limits{MaxFiles: 2}})
	_, err = few.Load(ctx(t), c, "ns", cmSource(sharedv1alpha1.ConfigMapArtifact{Name: "root"}))
	wantKind(t, err, KindRejected, "--artifact-max-files")
}

func TestOptionsFlags(t *testing.T) {
	var o Options
	fs := flag.NewFlagSet("m", flag.ContinueOnError)
	o.BindFlags(fs)
	if err := o.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if got := fs.Lookup("artifact-max-bytes").DefValue; got != "64Mi" {
		t.Errorf("artifact-max-bytes default %q", got)
	}
	if o.CacheBytes != DefaultCacheBytes {
		t.Errorf("cache default %d", o.CacheBytes)
	}
	err := fs.Parse([]string{"--artifact-max-bytes=1Gi", "--artifact-max-expanded-bytes=2Gi", "--artifact-max-files=5",
		"--artifact-cache-bytes=0", "--artifact-allowed-cidr=10.96.0.0/12", "--artifact-allowed-cidr=fd00::1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Limits.MaxBytes != 1<<30 || o.Limits.MaxExpandedBytes != 2<<30 || o.Limits.MaxFiles != 5 || o.CacheBytes != 0 {
		t.Errorf("parsed %+v cache %d", o.Limits, o.CacheBytes)
	}
	if len(o.AllowedCIDRs) != 2 || o.AllowedCIDRs[1] != netip.MustParsePrefix("fd00::1/128") {
		t.Errorf("CIDRs %v", o.AllowedCIDRs)
	}
	for _, bad := range [][]string{{"--artifact-max-bytes=1.5"}, {"--artifact-max-bytes=-1"}, {"--artifact-allowed-cidr=nope"}} {
		var o2 Options
		fs2 := flag.NewFlagSet("m", flag.ContinueOnError)
		fs2.SetOutput(new(strings.Builder))
		o2.BindFlags(fs2)
		if err := fs2.Parse(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	o.Limits.MaxExpandedBytes = 1
	if err := o.Validate(); err == nil {
		t.Error("expanded < max bytes accepted")
	}
	if _, err := NewLoader(Options{Limits: Limits{MaxCompressionRatio: -1}}); err == nil {
		t.Error("negative ratio accepted")
	}
	if err := (Options{}).Validate(); err != nil {
		t.Errorf("zero options (defaults): %v", err)
	}
}
