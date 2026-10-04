package vk

import (
	"crypto/sha256"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"golang.org/x/time/rate"
	"k8s.io/apiserver/pkg/authentication/authenticator"
)

// Bounds on bearer-token authentication, the only kind that costs an API call (a TokenReview):
// client certificates (kube-apiserver's way in) are verified locally and requests without
// credentials fail at once, so neither is limited here.
const (
	// MaxConcurrentTokenReviews caps TokenReviews in flight; beyond it a token request gets 401
	// at once rather than queueing on the API client.
	MaxConcurrentTokenReviews = 16
	// AuthFailureRate and AuthFailureBurst limit failed token authentications per source IP;
	// past them a token request from that IP gets 401 without a TokenReview.
	AuthFailureRate  = rate.Limit(1)
	AuthFailureBurst = 10
	// RejectedTokenTTL and RejectedTokenCacheSize bound the cache of tokens that failed
	// authentication (by SHA-256), answered 401 without another TokenReview.
	RejectedTokenTTL       = 30 * time.Second
	RejectedTokenCacheSize = 4096
	// maxTrackedIPs bounds the per-IP failure table; past it the table starts over.
	maxTrackedIPs = 10000
)

// GuardAuth puts the token bounds in front of a's authentication. Authorization (the
// SubjectAccessReview, cached by a) only runs for authenticated callers.
func GuardAuth(a nodeutil.Auth) nodeutil.Auth {
	return &guardedAuth{
		Auth:     a,
		failures: map[string]*rate.Limiter{},
		rejected: map[[32]byte]time.Time{},
		reviews:  make(chan struct{}, MaxConcurrentTokenReviews),
		now:      time.Now,
	}
}

type guardedAuth struct {
	nodeutil.Auth
	now     func() time.Time
	reviews chan struct{}

	mu       sync.Mutex
	failures map[string]*rate.Limiter
	rejected map[[32]byte]time.Time
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func sourceIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// AuthenticateRequest implements authenticator.Request.
func (g *guardedAuth) AuthenticateRequest(r *http.Request) (*authenticator.Response, bool, error) {
	token := bearerToken(r)
	hasCert := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	if token == "" || hasCert {
		return g.Auth.AuthenticateRequest(r)
	}
	ip, sum := sourceIP(r), sha256.Sum256([]byte(token))
	if err := g.precheck(ip, sum); err != nil {
		return nil, false, err
	}
	select {
	case g.reviews <- struct{}{}:
		defer func() { <-g.reviews }()
	default:
		return nil, false, errAuthUnavailable
	}
	resp, ok, err := g.Auth.AuthenticateRequest(r)
	if !ok {
		g.recordFailure(ip, sum)
	}
	return resp, ok, err
}

// precheck refuses a token from an IP over its failure budget, or one rejected recently.
func (g *guardedAuth) precheck(ip string, sum [32]byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if l, ok := g.failures[ip]; ok && l.TokensAt(now) < 1 {
		return errTooManyAuthFailures
	}
	if exp, ok := g.rejected[sum]; ok {
		if now.Before(exp) {
			if l := g.failures[ip]; l != nil {
				l.AllowN(now, 1)
			}
			return errInvalidTokenCached
		}
		delete(g.rejected, sum)
	}
	return nil
}

func (g *guardedAuth) recordFailure(ip string, sum [32]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	l, ok := g.failures[ip]
	if !ok {
		if len(g.failures) >= maxTrackedIPs {
			g.failures = map[string]*rate.Limiter{}
		}
		l = rate.NewLimiter(AuthFailureRate, AuthFailureBurst)
		g.failures[ip] = l
	}
	l.AllowN(now, 1)
	if len(g.rejected) >= RejectedTokenCacheSize {
		for k, exp := range g.rejected {
			if !now.Before(exp) {
				delete(g.rejected, k)
			}
		}
		if len(g.rejected) >= RejectedTokenCacheSize {
			g.rejected = map[[32]byte]time.Time{}
		}
	}
	g.rejected[sum] = now.Add(RejectedTokenTTL)
}
