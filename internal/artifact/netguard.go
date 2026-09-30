package artifact

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// nonPublic are the address ranges a source may not connect to unless allowlisted: everything
// that is not the public internet (RFC 6890 special-purpose registries), including the ranges
// that embed an IPv4 address in IPv6 (so a private IPv4 cannot be reached through them).
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // carrier-grade NAT (also Alibaba Cloud's metadata 100.100.100.200)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, cloud metadata (169.254.169.254)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, broadcast
		// IPv6
		"::/96",          // unspecified, loopback, IPv4-compatible
		"::ffff:0:0/96",  // IPv4-mapped (checked after unmapping, listed for completeness)
		"64:ff9b::/96",   // NAT64
		"64:ff9b:1::/48", // local-use NAT64
		"100::/64",       // discard
		"2001::/23",      // IETF protocol assignments (Teredo, ORCHID, ...)
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"fc00::/7",       // unique local (also AWS metadata fd00:ec2::254)
		"fe80::/10",      // link-local
		"fec0::/10",      // site-local (deprecated)
		"ff00::/8",       // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// addrPolicy decides which resolved addresses a connection may use.
type addrPolicy struct {
	allowed []netip.Prefix
}

// errAddressRefused is returned when a connection would reach a non-public address.
type errAddressRefused struct{ addr netip.Addr }

func (e errAddressRefused) Error() string {
	return fmt.Sprintf("connection to %s refused: not a public address (allow its range with --artifact-allowed-cidr)", e.addr)
}

func (p addrPolicy) check(a netip.Addr) error {
	a = a.Unmap().WithZone("")
	for _, pf := range p.allowed {
		if pf.Contains(a) {
			return nil
		}
	}
	for _, pf := range nonPublic {
		if pf.Contains(a) {
			return errAddressRefused{a}
		}
	}
	return nil
}

// control runs after DNS resolution, for every address the dialer tries, so neither a DNS
// answer (rebinding) nor a redirect can reach a refused address.
func (p addrPolicy) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("connection to %q refused: not an IP address", host)
	}
	return p.check(a)
}

// errPlainHTTP is returned for any request that is not https.
var errPlainHTTP = errors.New("only https is allowed")

// httpsOnly refuses every non-https request. With upgradeInitial, a plain-http request that is
// not a redirect is sent over https instead: go-containerregistry picks http by itself for
// registries named by an IP literal, localhost or *.local, and the loader always uses TLS.
type httpsOnly struct {
	base           http.RoundTripper
	userAgent      string
	upgradeInitial bool
}

func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if !(h.upgradeInitial && req.URL.Scheme == "http" && req.Response == nil) {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, fmt.Errorf("request to %s://%s refused: %w", req.URL.Scheme, req.URL.Host, errPlainHTTP)
		}
		req = req.Clone(req.Context())
		req.URL.Scheme = "https"
	}
	if h.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", h.userAgent)
	}
	return h.base.RoundTrip(req)
}

// maxRedirects bounds the redirects of a url download.
const maxRedirects = 5

// newTransport returns the guarded base transport: no proxy from the environment (a proxy
// would make the address check meaningless), the address policy at connect time, TLS 1.2+.
func newTransport(o Options) *http.Transport {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: addrPolicy{allowed: o.AllowedCIDRs}.control}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// downloadClient is the client of url sources: https only, redirects only to https.
func downloadClient(base http.RoundTripper, userAgent string) *http.Client {
	return &http.Client{
		Transport: httpsOnly{base: base, userAgent: userAgent},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s://%s refused: %w", req.URL.Scheme, req.URL.Host, errPlainHTTP)
			}
			return nil
		},
	}
}
