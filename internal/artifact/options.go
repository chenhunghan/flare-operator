package artifact

import (
	"crypto/x509"
	"flag"
	"fmt"
	"math"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Limits bound what one load may read and keep in memory. Zero fields take the defaults.
type Limits struct {
	// MaxBytes bounds the total size of the tree's files.
	MaxBytes int64
	// MaxFiles bounds the number of files.
	MaxFiles int64
	// MaxArchiveBytes bounds the bytes downloaded: the archive of a url source, the sum of the
	// (compressed) layer sizes of an image.
	MaxArchiveBytes int64
	// MaxExpandedBytes bounds the bytes decompressed from an archive or the image's layers,
	// including entries outside the selected path and entries later layers replace.
	MaxExpandedBytes int64
	// MaxCompressionRatio bounds decompressed bytes / compressed bytes (checked once more than
	// 1 MiB has been decompressed).
	MaxCompressionRatio int64
}

// Defaults of Limits and Options.CacheBytes.
const (
	DefaultMaxBytes            = 64 << 20
	DefaultMaxFiles            = 20000
	DefaultMaxArchiveBytes     = 64 << 20
	DefaultMaxExpandedBytes    = 256 << 20
	DefaultMaxCompressionRatio = 100
	DefaultCacheBytes          = 128 << 20
)

// ratioGrace is how many decompressed bytes are allowed before the ratio is checked, so that
// small archives of very compressible text are not refused.
const ratioGrace = 1 << 20

func (l Limits) withDefaults() Limits {
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&l.MaxBytes, DefaultMaxBytes)
	def(&l.MaxFiles, DefaultMaxFiles)
	def(&l.MaxArchiveBytes, DefaultMaxArchiveBytes)
	def(&l.MaxExpandedBytes, DefaultMaxExpandedBytes)
	def(&l.MaxCompressionRatio, DefaultMaxCompressionRatio)
	return l
}

// Options configure a Loader.
type Options struct {
	Limits Limits
	// CacheBytes bounds the loaded OCI and URL trees kept in memory; 0 or less disables the
	// cache. BindFlags defaults it to DefaultCacheBytes.
	CacheBytes int64
	// AllowedCIDRs are address ranges a url or ociRef source may connect to although they are
	// not public (e.g. an in-cluster registry). Public addresses are always allowed.
	AllowedCIDRs []netip.Prefix
	// RootCAs verify servers' TLS certificates (nil: the system roots). Tests set it.
	RootCAs *x509.CertPool
	// UserAgent of registry and download requests.
	UserAgent string
}

// BindFlags registers the --artifact-* manager flags into fs, writing into o. Defaults are the
// package defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	o.Limits = o.Limits.withDefaults()
	o.CacheBytes = DefaultCacheBytes
	fs.Var((*byteQuantity)(&o.Limits.MaxBytes), "artifact-max-bytes",
		"largest total size of an artifact's files (ConfigMaps, OCI image, archive), a quantity such as 64Mi; the tree is held in memory")
	fs.Int64Var(&o.Limits.MaxFiles, "artifact-max-files", o.Limits.MaxFiles, "most files in one artifact")
	fs.Var((*byteQuantity)(&o.Limits.MaxArchiveBytes), "artifact-max-archive-bytes",
		"largest download of one artifact: a url archive, or the sum of an image's compressed layers")
	fs.Var((*byteQuantity)(&o.Limits.MaxExpandedBytes), "artifact-max-expanded-bytes",
		"most bytes decompressed from one archive or image (zip/tar bomb limit; entries outside the selected path count too)")
	fs.Int64Var(&o.Limits.MaxCompressionRatio, "artifact-max-compression-ratio", o.Limits.MaxCompressionRatio,
		"largest decompressed/compressed ratio of an archive or image (zip/tar bomb limit; checked after the first 1Mi)")
	fs.Var((*byteQuantity)(&o.CacheBytes), "artifact-cache-bytes",
		"memory for cached OCI and url artifacts, by digest (0 disables the cache)")
	fs.Var((*prefixList)(&o.AllowedCIDRs), "artifact-allowed-cidr",
		"a non-public address range (CIDR) url and ociRef sources may connect to, e.g. an in-cluster registry (repeatable; loopback, private, link-local and metadata addresses are refused otherwise)")
}

// Validate rejects limits the loader cannot work with. Zero limits take the defaults.
func (o Options) Validate() error {
	l := o.Limits
	if l.MaxBytes < 0 || l.MaxFiles < 0 || l.MaxArchiveBytes < 0 || l.MaxExpandedBytes < 0 || l.MaxCompressionRatio < 0 {
		return fmt.Errorf("artifact limits must not be negative")
	}
	l = l.withDefaults()
	switch {
	case l.MaxExpandedBytes < l.MaxBytes:
		return fmt.Errorf("artifact-max-expanded-bytes (%d) must be at least artifact-max-bytes (%d)", l.MaxExpandedBytes, l.MaxBytes)
	}
	return nil
}

// byteQuantity is an int64 flag written as a Kubernetes quantity (64Mi, 1G, 1048576).
type byteQuantity int64

func (b *byteQuantity) String() string {
	if b == nil {
		return "0"
	}
	return resource.NewQuantity(int64(*b), resource.BinarySI).String()
}

func (b *byteQuantity) Set(s string) error {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return err
	}
	v, ok := q.AsInt64()
	if !ok || v < 0 {
		return fmt.Errorf("%q is not a non-negative whole number of bytes", s)
	}
	*b = byteQuantity(v)
	return nil
}

// prefixList is a repeatable CIDR flag.
type prefixList []netip.Prefix

func (p *prefixList) String() string {
	if p == nil {
		return "[]"
	}
	s := make([]string, len(*p))
	for i, v := range *p {
		s[i] = v.String()
	}
	return "[" + strings.Join(s, " ") + "]"
}

func (p *prefixList) Set(v string) error {
	pf, err := netip.ParsePrefix(strings.TrimSpace(v))
	if err != nil {
		// A bare address is a /32 or /128.
		a, aerr := netip.ParseAddr(strings.TrimSpace(v))
		if aerr != nil {
			return fmt.Errorf("%q is not a CIDR: %w", v, err)
		}
		pf = netip.PrefixFrom(a, a.BitLen())
	}
	*p = append(*p, pf.Masked())
	return nil
}

// ratioOK reports whether expanded bytes from compressed bytes stay within the ratio.
func (l Limits) ratioOK(expanded, compressed int64) bool {
	if expanded <= ratioGrace {
		return true
	}
	if compressed <= 0 {
		return false
	}
	if compressed > math.MaxInt64/l.MaxCompressionRatio {
		return true
	}
	return expanded <= compressed*l.MaxCompressionRatio
}
