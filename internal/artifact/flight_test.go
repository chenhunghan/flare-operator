package artifact

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

// waitWaiters waits until the loader's shared loads have n waiting callers in total.
func waitWaiters(t *testing.T, l *Loader, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.flights.mu.Lock()
		got := 0
		for _, f := range l.flights.m {
			got += f.waiters
		}
		l.flights.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d callers wait for shared loads, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type loadResult struct {
	tree *Tree
	err  error
}

// load runs l.Load in a goroutine.
func load(c context.Context, l *Loader, ns string, src sharedv1alpha1.ArtifactSource) <-chan loadResult {
	ch := make(chan loadResult, 1)
	go func() {
		tr, err := l.Load(c, nil, ns, src)
		ch <- loadResult{tr, err}
	}()
	return ch
}

func TestURLSourceSharedLoadContext(t *testing.T) {
	archive := tarOf(t, file("index.html", "ok"))
	started := make(chan struct{}, 4)
	release := make(chan struct{}) // one token lets one blocked download finish
	gone := make(chan struct{}, 4)
	var hits atomic.Int32
	srv, l := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write(archive)
		case <-r.Context().Done():
			gone <- struct{}{}
		}
	}))
	src := urlSource(srv.URL+"/a.tar", sum(archive), "")

	// When its only caller gives up, the shared download is cancelled.
	actx, cancelA := context.WithCancel(ctx(t))
	a := load(actx, l, "a", src)
	<-started
	waitWaiters(t, l, 1)
	cancelA()
	if r := <-a; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", r.err)
	}
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the download went on with nobody waiting for it")
	}

	// A caller that gives up does not fail another caller (another namespace) waiting for
	// the same download.
	actx, cancelA = context.WithCancel(ctx(t))
	a = load(actx, l, "a", src)
	<-started
	b := load(ctx(t), l, "b", src)
	waitWaiters(t, l, 2)
	cancelA()
	if r := <-a; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", r.err)
	}
	release <- struct{}{}
	r := <-b
	if r.err != nil {
		t.Fatalf("the other caller failed: %v", r.err)
	}
	if f, _ := r.tree.File("index.html"); string(f.Content) != "ok" {
		t.Errorf("index.html = %q", f.Content)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("%d downloads, want 2 (one cancelled, one shared)", n)
	}
}

func TestOCISourceSharedLoadContext(t *testing.T) {
	r := newTestRegistry(t, "", "")
	r.pushImage(t, "site:v1", image(t, tarOf(t, file("index.html", "ok"))))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var blocked atomic.Bool
	gate := func(req *http.Request) {
		// Block the first blob download of the shared load.
		if !strings.Contains(req.URL.Path, "/blobs/") || !blocked.CompareAndSwap(false, true) {
			return
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}
	r.gate.Store(&gate)
	src := ociSource(r.host+"/site:v1", "", "")

	actx, cancelA := context.WithCancel(ctx(t))
	a := load(actx, r.loader, "a", src)
	<-started
	b := load(ctx(t), r.loader, "b", src)
	waitWaiters(t, r.loader, 2)
	cancelA()
	if res := <-a; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", res.err)
	}
	close(release)
	res := <-b
	if res.err != nil {
		t.Fatalf("the other caller failed: %v", res.err)
	}
	if f, _ := res.tree.File("index.html"); string(f.Content) != "ok" {
		t.Errorf("index.html = %q", f.Content)
	}
}

func TestOCISourceIndexAttestation(t *testing.T) {
	// A single-platform non-amd64 image pushed with BuildKit's default provenance: an index of
	// the image and an attestation manifest.
	r := newTestRegistry(t, "", "")
	img := image(t, tarOf(t, file("index.html", "arm")))
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	att, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte(`{"_type":"https://in-toto.io/Statement/v0.1"}`), "application/vnd.in-toto+json"))
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex),
		mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: att, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"},
			Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": d.String()}}})
	ref, err := name.ParseReference(r.host + "/attested:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx, r.push...); err != nil {
		t.Fatal(err)
	}
	tr, err := r.loader.Load(ctx(t), nil, "ns", ociSource(r.host+"/attested:v1", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := tr.File("index.html"); string(f.Content) != "arm" {
		t.Errorf("index.html = %q, want arm", f.Content)
	}
}
