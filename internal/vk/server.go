package vk

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"golang.org/x/net/netutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// HandlerConfig configures the kubelet API handler.
type HandlerConfig struct {
	NodeName string
	Resolver PodResolver
	Streamer workerlogs.Streamer
	// ParseOptions reads the containerLogs query (default workerlogs.ParseOptions).
	ParseOptions ParseOptionsFunc
	// Pods lists the Pods bound to the node (GET /pods); nil answers an empty list.
	Pods func(context.Context) ([]*corev1.Pod, error)
	// StartTime is the node's start time in /stats/summary.
	StartTime time.Time
	Now       func() time.Time
	Log       logr.Logger
	// Limits bound log requests and streams (defaults: ServerLimits.WithDefaults).
	Limits ServerLimits
}

// NewHandler returns the kubelet API (unauthenticated: wrap it with nodeutil.WithAuth):
//   - GET /containerLogs/{namespace}/{pod}/{container}: the Worker's logs (logsHandler; it
//     reads the query itself because virtual-kubelet's parser makes an absent tailLines and
//     tailLines=0 the same);
//   - /exec, /attach, /portForward, /run: 501 with ErrUnsupported's message, before any stream
//     upgrade, so kubectl shows it;
//   - GET /pods, /stats/summary, /metrics/resource: virtual-kubelet's api.PodHandler with an
//     empty summary and no metrics, so metrics-server and scrapers see a healthy, empty node;
//   - anything else: 404.
func NewHandler(c HandlerConfig) http.Handler {
	if c.ParseOptions == nil {
		c.ParseOptions = workerlogs.ParseOptions
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.StartTime.IsZero() {
		c.StartTime = c.Now()
	}
	pods := c.Pods
	if pods == nil {
		pods = func(context.Context) ([]*corev1.Pod, error) { return nil, nil }
	}
	rest := api.PodHandler(api.PodHandlerConfig{
		// exec, attach and port-forward never get here (routed to notImplemented below).
		RunInContainer:    func(context.Context, string, string, string, []string, api.AttachIO) error { return ErrUnsupported },
		AttachToContainer: func(context.Context, string, string, string, api.AttachIO) error { return ErrUnsupported },
		PortForward:       func(context.Context, string, string, int32, io.ReadWriteCloser) error { return ErrUnsupported },
		GetContainerLogs: func(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
			return nil, ErrUnsupported // never reached: /containerLogs is routed to logsHandler
		},
		GetPods:               pods,
		GetPodsFromKubernetes: pods,
		GetStatsSummary: func(context.Context) (*statsv1alpha1.Summary, error) {
			return &statsv1alpha1.Summary{
				Node: statsv1alpha1.NodeStats{NodeName: c.NodeName, StartTime: metav1.NewTime(c.StartTime)},
				Pods: []statsv1alpha1.PodStats{},
			}, nil
		},
		GetMetricsResource: func(context.Context) ([]*dto.MetricFamily, error) { return nil, nil },
	}, false)

	mux := http.NewServeMux()
	mux.Handle("GET /containerLogs/{namespace}/{pod}/{container}", newLogsHandler(c))
	notImplemented := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, ErrUnsupported) })
	for _, p := range []string{"/exec/", "/attach/", "/portForward/", "/run/"} {
		mux.Handle(p, notImplemented)
	}
	mux.Handle("/", rest)
	return mux
}

// ServerTLSConfig is the kubelet API's server TLS: the certificate from cert, TLS 1.2 or later, and a
// requested (not required) client certificate, verified by the x509 authenticator against the
// cluster's client CA so that bearer tokens work as well.
func ServerTLSConfig(cert ServingCert) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cert.GetCertificate,
		ClientAuth:     tls.RequestClientCert,
		NextProtos:     []string{"http/1.1"},
	}
}

// Serve runs the authenticated kubelet API on l until ctx ends, with at most maxConns open
// connections (0: DefaultMaxConnections); further connections wait in the listen backlog.
func Serve(ctx context.Context, l net.Listener, h http.Handler, auth nodeutil.Auth, tlsCfg *tls.Config, maxConns int) error {
	if maxConns <= 0 {
		maxConns = DefaultMaxConnections
	}
	l = netutil.LimitListener(l, maxConns)
	srv := &http.Server{
		Handler:           nodeutil.WithAuth(auth, h),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// HTTP/1.1 only, like the kubelet's streaming endpoints (no HTTP/2 rapid-reset exposure).
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(l, "", "") }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	_ = srv.Close() // ends follow streams Shutdown does not wait for
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
