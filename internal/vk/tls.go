package vk

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RenewFraction is the share of a certificate's lifetime after which it is replaced.
const RenewFraction = 0.8

// errNoCertificate: GetCertificate before the first certificate.
var errNoCertificate = errors.New("the kubelet API has no serving certificate yet")

// certHolder keeps the current certificate and the Ready channel of a ServingCert.
type certHolder struct {
	cur       atomic.Pointer[tls.Certificate]
	ready     chan struct{}
	readyOnce sync.Once
}

func newCertHolder() certHolder { return certHolder{ready: make(chan struct{})} }

// GetCertificate implements ServingCert.
func (h *certHolder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := h.cur.Load(); c != nil {
		return c, nil
	}
	return nil, errNoCertificate
}

// Ready implements ServingCert.
func (h *certHolder) Ready() <-chan struct{} { return h.ready }

// Current returns the certificate being served (nil before the first).
func (h *certHolder) Current() *tls.Certificate { return h.cur.Load() }

func (h *certHolder) set(c *tls.Certificate) {
	h.cur.Store(c)
	h.readyOnce.Do(func() { close(h.ready) })
}

// renewAt is when a certificate valid from notBefore to notAfter is replaced.
func renewAt(leaf *x509.Certificate) time.Time {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotBefore.Add(time.Duration(float64(life) * RenewFraction))
}

// sleepCtx waits d or until ctx ends or wake fires; it reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-wake:
	}
	return true
}

// backoff is the retry delay after the n-th consecutive failure (1 s doubling to 1 min).
func backoff(n int) time.Duration {
	d := time.Second << min(n, 6)
	return min(d, time.Minute)
}

// SelfSignedCert is TLSModeSelfSigned: an ECDSA P-256 certificate for the node name and address
// signed by its own key, valid for a year and replaced at RenewFraction of it.
type SelfSignedCert struct {
	certHolder
	nodeName, address string
	lifetime          time.Duration
}

// NewSelfSignedCert returns a self-signed ServingCert.
func NewSelfSignedCert(nodeName, address string) *SelfSignedCert {
	return &SelfSignedCert{certHolder: newCertHolder(), nodeName: nodeName, address: address, lifetime: 365 * 24 * time.Hour}
}

// Run implements ServingCert.
func (s *SelfSignedCert) Run(ctx context.Context) error {
	for {
		c, err := selfSigned(s.nodeName, s.address, s.lifetime)
		if err != nil {
			return err
		}
		s.set(c)
		if !sleepCtx(ctx, time.Until(renewAt(c.Leaf)), nil) {
			return nil
		}
	}
}

func selfSigned(nodeName, address string, lifetime time.Duration) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "system:node:" + nodeName, Organization: []string{"system:nodes"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{nodeName},
	}
	if ip := net.ParseIP(address); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// SecretCert is TLSModeSecret: tls.crt and tls.key of a Secret, read with get (no list or watch)
// every Interval and swapped in when the Secret changes.
type SecretCert struct {
	certHolder
	cs        kubernetes.Interface
	namespace string
	name      string
	log       logr.Logger
	// Interval is how often the Secret is read (default 30 s).
	Interval time.Duration

	version string
}

// NewSecretCert returns a ServingCert backed by Secret namespace/name.
func NewSecretCert(cs kubernetes.Interface, namespace, name string, log logr.Logger) *SecretCert {
	return &SecretCert{certHolder: newCertHolder(), cs: cs, namespace: namespace, name: name, log: log, Interval: 30 * time.Second}
}

// Run implements ServingCert.
func (s *SecretCert) Run(ctx context.Context) error {
	for {
		if err := s.load(ctx); err != nil && ctx.Err() == nil {
			s.log.Error(err, "load the kubelet API certificate", "secret", s.namespace+"/"+s.name)
		}
		if !sleepCtx(ctx, s.Interval, nil) {
			return nil
		}
	}
}

func (s *SecretCert) load(ctx context.Context) error {
	sec, err := s.cs.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sec.ResourceVersion == s.version && s.Current() != nil {
		return nil
	}
	c, err := tls.X509KeyPair(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return fmt.Errorf("secret %s/%s: %w", s.namespace, s.name, err)
	}
	s.set(&c)
	s.version = sec.ResourceVersion
	s.log.Info("loaded the kubelet API certificate", "secret", s.namespace+"/"+s.name, "notAfter", c.Leaf.NotAfter)
	return nil
}
