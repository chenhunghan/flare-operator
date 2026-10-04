package vk

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// DefaultCSRLifetime is TLSConfig.Lifetime when unset (the CSR's spec.expirationSeconds).
const DefaultCSRLifetime = 24 * time.Hour

// MinCSRLifetime is the shortest lifetime the CSR API accepts (spec.expirationSeconds >= 600).
const MinCSRLifetime = 10 * time.Minute

// WithDefaults returns c with every unset field defaulted.
func (c Config) WithDefaults() Config {
	if c.NodeName == "" {
		c.NodeName = DefaultNodeName
	}
	if c.AddressMode == "" {
		c.AddressMode = AddressPodIP
	}
	if c.ListenAddr == "" {
		port := DefaultListenPort
		if c.AddressMode == AddressHostIP {
			port = DefaultHostNetworkPort
		}
		c.ListenAddr = ":" + strconv.Itoa(port)
	}
	if c.TLS.Mode == "" {
		c.TLS.Mode = TLSModeCSR
	}
	if c.TLS.Lifetime == 0 {
		c.TLS.Lifetime = DefaultCSRLifetime
	}
	if c.APIBudget == 0 {
		c.APIBudget = DefaultAPIBudget
	}
	if c.Logs.DefaultWindow == 0 {
		c.Logs.DefaultWindow = workerlogs.DefaultWindow
	}
	if c.Logs.MaxEvents == 0 {
		c.Logs.MaxEvents = workerlogs.DefaultMaxEvents
	}
	if c.Logs.PageSize == 0 {
		c.Logs.PageSize = workerlogs.MaxPageSize
	}
	if c.Logs.TailPingInterval == 0 {
		c.Logs.TailPingInterval = workerlogs.DefaultTailPingInterval
	}
	if c.Logs.MaxFollowers == 0 {
		c.Logs.MaxFollowers = workerlogs.DefaultMaxFollowers
	}
	return c
}

// Validate rejects a configuration the virtual kubelet cannot run with. Call it on
// WithDefaults' result.
func (c Config) Validate() error {
	var errs []error
	if msgs := validation.IsDNS1123Subdomain(c.NodeName); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("node name %q: %v", c.NodeName, msgs))
	}
	switch c.AddressMode {
	case AddressPodIP, AddressHostIP:
	default:
		errs = append(errs, fmt.Errorf("address mode %q: want %q or %q", c.AddressMode, AddressPodIP, AddressHostIP))
	}
	if net.ParseIP(c.Address) == nil {
		errs = append(errs, fmt.Errorf("node address %q is not an IP (set it from the downward API: status.podIP or status.hostIP)", c.Address))
	}
	if _, err := c.ListenPort(); err != nil {
		errs = append(errs, err)
	}
	switch c.TLS.Mode {
	case TLSModeCSR:
		if c.TLS.Lifetime < MinCSRLifetime {
			errs = append(errs, fmt.Errorf("certificate lifetime %s is below the CSR API minimum of %s", c.TLS.Lifetime, MinCSRLifetime))
		}
	case TLSModeSelfSigned:
	case TLSModeSecret:
		if c.TLS.SecretName == "" || c.TLS.SecretNamespace == "" {
			errs = append(errs, errors.New("tls mode secret needs the Secret's namespace and name"))
		}
	default:
		errs = append(errs, fmt.Errorf("tls mode %q: want %q, %q or %q", c.TLS.Mode, TLSModeCSR, TLSModeSelfSigned, TLSModeSecret))
	}
	if c.OwnerClusterRole == "" {
		errs = append(errs, errors.New("the owner ClusterRole is required (the Node is garbage-collected with it)"))
	}
	if _, err := labels.Parse(c.NamespaceSelector); err != nil {
		errs = append(errs, fmt.Errorf("namespace selector: %w", err))
	}
	if c.APIBudget < 1 {
		errs = append(errs, fmt.Errorf("API budget %d: want at least 1 request per 5 minutes", c.APIBudget))
	}
	l := c.Logs
	if l.DefaultWindow < 0 || l.MaxEvents < 1 || l.PageSize < 1 || l.PageSize > workerlogs.MaxPageSize ||
		l.TailPingInterval <= 0 || l.MaxFollowers < 1 {
		errs = append(errs, fmt.Errorf("log limits %+v: windows and intervals must be positive, counts at least 1, the page size at most %d",
			l, workerlogs.MaxPageSize))
	}
	return errors.Join(errs...)
}

// ListenPort is the port of ListenAddr: the node's status.daemonEndpoints.kubeletEndpoint.Port.
func (c Config) ListenPort() (int32, error) {
	_, p, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return 0, fmt.Errorf("listen address %q: %w", c.ListenAddr, err)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("listen address %q: the port must be 1-65535", c.ListenAddr)
	}
	return int32(n), nil
}
