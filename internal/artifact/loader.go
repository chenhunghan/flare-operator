package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

// Loader loads ArtifactSources. It is safe for concurrent use; one Loader serves the manager.
type Loader struct {
	lim       Limits
	userAgent string
	download  *http.Client
	oci       http.RoundTripper
	cache     *cache
	flights   flights
}

// NewLoader returns a Loader. Zero limits take the defaults.
func NewLoader(o Options) (*Loader, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	o.Limits = o.Limits.withDefaults()
	base := newTransport(o)
	return &Loader{
		lim:       o.Limits,
		userAgent: o.UserAgent,
		download:  downloadClient(base, o.UserAgent),
		oci:       httpsOnly{base: base, userAgent: o.UserAgent, upgradeInitial: true},
		cache:     newCache(o.CacheBytes),
	}, nil
}

// Limits returns the loader's limits (defaults applied).
func (l *Loader) Limits() Limits { return l.lim }

// Load reads src for an object in namespace. ConfigMaps and pull Secrets are read through c
// (the controller's client), only from namespace and only with the label
// flare.dev/artifact=true. A failure is an *Error (see Kind) unless it is an error
// of the Kubernetes API or the context.
func (l *Loader) Load(ctx context.Context, c client.Reader, namespace string, src sharedv1alpha1.ArtifactSource) (*Tree, error) {
	n := 0
	for _, set := range []bool{src.ConfigMapRef != nil, src.OCIRef != nil, src.URL != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, invalid("set exactly one of configMapRef, ociRef or url")
	}
	switch {
	case src.ConfigMapRef != nil:
		return l.loadConfigMaps(ctx, c, namespace, src.ConfigMapRef)
	case src.OCIRef != nil:
		return l.loadOCI(ctx, c, namespace, src.OCIRef)
	default:
		return l.loadURL(ctx, src.URL)
	}
}

// unusable is one message for a missing, an unlabelled and a wrongly typed object, so a spec
// cannot probe which ConfigMaps or Secrets exist.
func unusable(kind, name, extra string) *Error {
	return dependency("%s %s is not usable: it must exist in the object's namespace and carry the label %s=true%s",
		kind, name, sharedv1alpha1.LabelArtifact, extra)
}

func (l *Loader) loadConfigMaps(ctx context.Context, c client.Reader, ns string, s *sharedv1alpha1.ConfigMapArtifactSource) (*Tree, error) {
	if len(s.ConfigMaps) == 0 {
		return nil, invalid("configMapRef lists no ConfigMaps")
	}
	files := map[string][]byte{}
	from := map[string]string{}
	var total int64
	for _, ref := range s.ConfigMaps {
		dir, err := cleanRel(ref.Path, true)
		if err != nil {
			return nil, newErr(KindInvalid, err, "configMapRef %s path", ref.Name)
		}
		var cm corev1.ConfigMap
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, unusable("ConfigMap", ref.Name, "")
			}
			return nil, err
		}
		if cm.Labels[sharedv1alpha1.LabelArtifact] != "true" {
			return nil, unusable("ConfigMap", ref.Name, "")
		}
		add := func(key, rel string, b []byte) error {
			// Check rel before joining: path.Join would resolve its ".." segments away.
			r, err := cleanRel(rel, false)
			if err != nil {
				return newErr(KindInvalid, err, "ConfigMap %s key %s", cm.Name, key)
			}
			p, err := cleanRel(path.Join(dir, r), false)
			if err != nil {
				return newErr(KindInvalid, err, "ConfigMap %s key %s", cm.Name, key)
			}
			if prev, dup := from[p]; dup {
				return rejected("path %q comes from both %s and ConfigMap %s key %s", p, prev, cm.Name, key)
			}
			total += int64(len(b))
			if total > l.lim.MaxBytes {
				return rejected("the artifact's files total more than %d bytes (--artifact-max-bytes)", l.lim.MaxBytes)
			}
			files[p], from[p] = b, "ConfigMap "+cm.Name+" key "+key
			return nil
		}
		if len(ref.Items) > 0 {
			for _, it := range ref.Items {
				b, ok := configMapValue(&cm, it.Key)
				if !ok {
					return nil, dependency("ConfigMap %s has no key %q (items)", cm.Name, it.Key)
				}
				if err := add(it.Key, it.Path, b); err != nil {
					return nil, err
				}
			}
			continue
		}
		for k, v := range cm.Data {
			if err := add(k, k, []byte(v)); err != nil {
				return nil, err
			}
		}
		for k, v := range cm.BinaryData {
			if err := add(k, k, v); err != nil {
				return nil, err
			}
		}
	}
	if len(files) == 0 {
		return nil, rejected("the ConfigMaps hold no files")
	}
	return newTree(files, l.lim)
}

func configMapValue(cm *corev1.ConfigMap, key string) ([]byte, bool) {
	if v, ok := cm.Data[key]; ok {
		return []byte(v), true
	}
	v, ok := cm.BinaryData[key]
	return v, ok
}

var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (l *Loader) loadURL(ctx context.Context, s *sharedv1alpha1.URLArtifactSource) (*Tree, error) {
	u, err := url.Parse(s.URL)
	switch {
	case err != nil:
		return nil, newErr(KindInvalid, err, "url")
	case u.Scheme != "https":
		return nil, invalid("url must be https, not %q", u.Scheme)
	case u.Host == "" || u.Hostname() == "":
		return nil, invalid("url has no host")
	case u.User != nil:
		return nil, invalid("url must not carry credentials")
	case !sha256Hex.MatchString(s.SHA256):
		return nil, invalid("sha256 must be 64 lowercase hex digits")
	}
	root, err := cleanRel(s.Path, true)
	if err != nil {
		return nil, newErr(KindInvalid, err, "url path")
	}
	key := "url\x00" + u.String() + "\x00" + s.SHA256 + "\x00" + root
	if t, ok := l.cache.get(key); ok {
		return t, nil
	}
	return l.flights.do(ctx, key, func(ctx context.Context) (*Tree, error) {
		b, err := l.fetchArchive(ctx, u)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != s.SHA256 {
			return nil, rejected("the archive's SHA-256 is %s, not the expected %s", got, s.SHA256)
		}
		files, err := extractArchive(b, root, l.lim)
		if err != nil {
			return nil, err
		}
		t, err := newTree(files, l.lim)
		if err != nil {
			return nil, err
		}
		l.cache.put(key, t)
		return t, nil
	})
}

// fetchArchive downloads u into memory, at most MaxArchiveBytes.
func (l *Loader) fetchArchive(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, newErr(KindInvalid, err, "url")
	}
	resp, err := l.download.Do(req)
	if err != nil {
		return nil, fetchErr(err, "download %s", u.Redacted())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fetchErr(nil, "download %s: HTTP %d", u.Redacted(), resp.StatusCode)
	}
	if resp.ContentLength > l.lim.MaxArchiveBytes {
		return nil, rejected("the archive is %d bytes, more than %d (--artifact-max-archive-bytes)", resp.ContentLength, l.lim.MaxArchiveBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, l.lim.MaxArchiveBytes+1))
	if err != nil {
		return nil, fetchErr(err, "download %s", u.Redacted())
	}
	if int64(len(b)) > l.lim.MaxArchiveBytes {
		return nil, rejected("the archive is more than %d bytes (--artifact-max-archive-bytes)", l.lim.MaxArchiveBytes)
	}
	return b, nil
}

// String describes the limits (for the manager's start-up log).
func (l Limits) String() string {
	return fmt.Sprintf("maxBytes=%d maxFiles=%d maxArchiveBytes=%d maxExpandedBytes=%d maxCompressionRatio=%d",
		l.MaxBytes, l.MaxFiles, l.MaxArchiveBytes, l.MaxExpandedBytes, l.MaxCompressionRatio)
}
