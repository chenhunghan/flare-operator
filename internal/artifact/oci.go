package artifact

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
)

// tarLayers are the layer media types the loader unpacks. Foreign and non-distributable layers
// (downloaded from URLs outside the registry) are refused.
var tarLayers = map[types.MediaType]bool{
	types.DockerLayer:             true,
	types.DockerUncompressedLayer: true,
	types.OCILayer:                true,
	types.OCILayerZStd:            true,
	types.OCIUncompressedLayer:    true,
}

// maxLayers bounds the layers of an image (each is one registry request). Docker's own limit is
// 127 layers.
const maxLayers = 128

func (l *Loader) loadOCI(ctx context.Context, c client.Reader, ns string, s *sharedv1alpha1.OCIArtifactSource) (*Tree, error) {
	ref, err := name.ParseReference(s.Image)
	if err != nil {
		return nil, newErr(KindInvalid, err, "ociRef image")
	}
	root, err := cleanRel(s.Path, true)
	if err != nil {
		return nil, newErr(KindInvalid, err, "ociRef path")
	}
	var keys authn.Keychain = anonymous{}
	if s.PullSecretRef != nil {
		if keys, err = pullSecretKeychain(ctx, c, ns, s.PullSecretRef.Name); err != nil {
			return nil, err
		}
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(l.oci), remote.WithAuthFromKeychain(keys)}
	if l.userAgent != "" {
		opts = append(opts, remote.WithUserAgent(l.userAgent))
	}
	// Every load fetches the manifest with the caller's credentials, also on a cache hit: it
	// resolves a tag, and it proves that this caller may read the image before the cache
	// hands out content another namespace pulled.
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, fetchErr(err, "get manifest of %s", ref.Name())
	}
	resolved := desc.Digest.String()
	key := "oci\x00" + ref.Context().Name() + "@" + resolved + "\x00" + root
	if t, ok := l.cache.get(key); ok {
		return t, nil
	}
	v, err, _ := l.flights.Do(key, func() (any, error) {
		img, err := imageOf(desc)
		if err != nil {
			return nil, err
		}
		files, err := l.extractImage(img, root)
		if err != nil {
			return nil, err
		}
		t, err := newTree(files, l.lim)
		if err != nil {
			return nil, err
		}
		t = t.withResolvedDigest(resolved)
		l.cache.put(key, t)
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Tree), nil
}

// imageOf returns the image of desc: the image itself, the only image of an index, or the
// linux/amd64 image of a multi-platform index (the content of a static-site image is the same
// on every platform).
func imageOf(desc *remote.Descriptor) (v1.Image, error) {
	switch {
	case desc.MediaType.IsIndex():
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fetchErr(err, "read image index")
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, fetchErr(err, "read image index")
		}
		var images []v1.Descriptor
		for _, m := range im.Manifests {
			if m.MediaType.IsImage() {
				images = append(images, m)
			}
		}
		if len(images) == 1 {
			img, err := idx.Image(images[0].Digest)
			if err != nil {
				return nil, fetchErr(err, "read image %s", images[0].Digest)
			}
			return img, nil
		}
		img, err := desc.Image()
		if err != nil {
			return nil, fetchErr(err, "pick the linux/amd64 image of the index")
		}
		return img, nil
	case desc.MediaType.IsImage():
		img, err := desc.Image()
		if err != nil {
			return nil, fetchErr(err, "read image")
		}
		return img, nil
	}
	return nil, rejected("the reference is a %q, not an image or image index", desc.MediaType)
}

// extractImage applies the image's layers in order into a tree under root.
func (l *Loader) extractImage(img v1.Image, root string) (map[string][]byte, error) {
	mf, err := img.Manifest()
	if err != nil {
		return nil, fetchErr(err, "read image manifest")
	}
	if len(mf.Layers) > maxLayers {
		return nil, rejected("the image has %d layers, more than %d", len(mf.Layers), maxLayers)
	}
	var total int64
	for _, d := range mf.Layers {
		if !tarLayers[d.MediaType] {
			return nil, rejected("layer %s has media type %q; only tar layers (optionally gzip or zstd) are unpacked", d.Digest, d.MediaType)
		}
		if d.Size < 0 {
			return nil, rejected("layer %s has a negative size", d.Digest)
		}
		total += d.Size
		if total > l.lim.MaxArchiveBytes {
			return nil, rejected("the image's layers total more than %d bytes (--artifact-max-archive-bytes)", l.lim.MaxArchiveBytes)
		}
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fetchErr(err, "read image layers")
	}
	x := newExtractor(l.lim, root, true)
	x.network = true
	x.compressed = total
	for _, layer := range layers {
		if err := x.addLayer(layer); err != nil {
			return nil, err
		}
	}
	return x.finish()
}

// addLayer unpacks one layer and reads its stream to the end, which makes go-containerregistry
// verify the layer's digest.
func (x *extractor) addLayer(layer v1.Layer) error {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fetchErr(err, "open layer")
	}
	defer func() { _ = rc.Close() }()
	c := &counter{r: rc, x: x}
	x.beginLayer()
	if err := x.addTar(c); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, c); err != nil {
		return x.fail(err, "read layer")
	}
	return x.endLayer()
}

// anonymous is a keychain without credentials.
type anonymous struct{}

func (anonymous) Resolve(authn.Resource) (authn.Authenticator, error) { return authn.Anonymous, nil }

// dockerConfigKeychain resolves registry credentials from a .dockerconfigjson document.
type dockerConfigKeychain map[string]authn.AuthConfig

func (k dockerConfigKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	host := strings.ToLower(r.RegistryStr())
	cands := []string{host}
	if host == name.DefaultRegistry { // index.docker.io
		cands = append(cands, "docker.io", "registry-1.docker.io")
	}
	for _, h := range cands {
		if cfg, ok := k[h]; ok {
			return authn.FromConfig(cfg), nil
		}
	}
	return authn.Anonymous, nil
}

// registryKey normalises an "auths" key ("https://index.docker.io/v1/", "ghcr.io") to a
// registry host[:port].
func registryKey(k string) string {
	k = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(k), "https://"), "http://")
	if i := strings.IndexByte(k, '/'); i >= 0 {
		k = k[:i]
	}
	return k
}

// pullSecretKeychain reads a labelled kubernetes.io/dockerconfigjson Secret.
func pullSecretKeychain(ctx context.Context, c client.Reader, ns, secretName string) (authn.Keychain, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, unusable("Secret", secretName, ", and be of type kubernetes.io/dockerconfigjson")
		}
		return nil, err
	}
	b, ok := sec.Data[corev1.DockerConfigJsonKey]
	if sec.Labels[sharedv1alpha1.LabelArtifact] != "true" || sec.Type != corev1.SecretTypeDockerConfigJson || !ok {
		return nil, unusable("Secret", secretName, ", and be of type kubernetes.io/dockerconfigjson")
	}
	var doc struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		// The JSON error could quote the Secret's content: do not wrap it.
		return nil, dependency("Secret %s: %s is not a valid docker config", secretName, corev1.DockerConfigJsonKey)
	}
	// authn.AuthConfig decodes each "auth" (base64 of user:password) into Username/Password.
	keys := dockerConfigKeychain{}
	for k, cfg := range doc.Auths {
		keys[registryKey(k)] = cfg
	}
	return keys, nil
}
