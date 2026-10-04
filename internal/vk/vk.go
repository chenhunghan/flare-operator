package vk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/go-logr/logr"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	vkslog "github.com/virtual-kubelet/virtual-kubelet/log/slog"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// EventComponent is the source component of the virtual kubelet's Pod events.
const EventComponent = "flare-workers-vk"

// PodSyncWorkers is the number of virtual-kubelet PodController workers.
const PodSyncWorkers = 2

// Deps are what the virtual kubelet runs with. Clientset, Cache, StatusMapper, Resolver and
// Streamer are required.
type Deps struct {
	// Clientset talks to the API server (node, lease, Pods, CSRs, auth reviews).
	Clientset kubernetes.Interface
	// Cache is the controller-runtime cache holding WorkerScripts (and the Pods of the node);
	// the provider reads WorkerScripts from it and follows their changes.
	Cache cache.Cache
	// StatusMapper computes the status of stand-in Pods (standin.NewStatusMapper).
	StatusMapper standin.StatusMapper
	// Resolver maps a Pod to its Worker (NewResolver).
	Resolver PodResolver
	// Streamer produces the log stream (workerlogs.NewStreamer).
	Streamer workerlogs.Streamer
	// ParseOptions reads the containerLogs query (default workerlogs.ParseOptions; normally
	// workerlogs.ParseOptions).
	ParseOptions ParseOptionsFunc
	// Cert overrides the serving certificate that Config.TLS selects (tests).
	Cert ServingCert
	// Listener overrides the listener on Config.ListenAddr (tests).
	Listener net.Listener
	// AuthClientset makes the kubelet API's TokenReviews and SubjectAccessReviews (default
	// Clientset). Give it its own rate limit, so callers that force reviews cannot starve the
	// node lease, Pod status and CSR calls of Clientset.
	AuthClientset kubernetes.Interface
	// Limits bound the kubelet API's connections, log requests and streams.
	Limits ServerLimits
	Log    logr.Logger
	Now    func() time.Time
}

// VirtualKubelet runs the Workers virtual node: the Node and its Lease, the PodController and
// the authenticated kubelet API. It is a controller-runtime Runnable that needs leader election
// (one active replica).
type VirtualKubelet struct {
	cfg  Config
	deps Deps

	provider *Provider
	cert     ServingCert
	ready    chan struct{}
}

// New checks the configuration and returns a VirtualKubelet; Start runs it.
func New(cfg Config, d Deps) (*VirtualKubelet, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if d.Clientset == nil || d.Cache == nil || d.StatusMapper == nil || d.Resolver == nil || d.Streamer == nil {
		return nil, errors.New("vk.New: Clientset, Cache, StatusMapper, Resolver and Streamer are required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log.GetSink() == nil {
		d.Log = logr.Discard()
	}
	cert := d.Cert
	if cert == nil {
		cert = NewServingCert(cfg, d.Clientset, d.Log.WithName("tls"))
	}
	return &VirtualKubelet{
		cfg: cfg, deps: d, cert: cert, ready: make(chan struct{}),
		provider: NewProvider(d.Cache, d.StatusMapper, d.Now, d.Log.WithName("provider")),
	}, nil
}

// NewServingCert returns the ServingCert of c.TLS.Mode.
func NewServingCert(c Config, cs kubernetes.Interface, log logr.Logger) ServingCert {
	switch c.TLS.Mode {
	case TLSModeSelfSigned:
		return NewSelfSignedCert(c.NodeName, c.Address)
	case TLSModeSecret:
		return NewSecretCert(cs, c.TLS.SecretNamespace, c.TLS.SecretName, log)
	default:
		cc := NewCSRCert(cs, c.NodeName, c.Address, c.TLS.Approve, c.TLS.Lifetime, log)
		cc.OwnerClusterRole = c.OwnerClusterRole
		return cc
	}
}

// Provider is the pod lifecycle handler (for tests and wiring).
func (v *VirtualKubelet) Provider() *Provider { return v.provider }

// Ready is closed once the node is registered and the PodController runs.
func (v *VirtualKubelet) Ready() <-chan struct{} { return v.ready }

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the leader runs.
func (v *VirtualKubelet) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns when ctx ends or a part fails.
func (v *VirtualKubelet) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	log := v.deps.Log
	ctx = vklog.WithLogger(ctx, vkslog.FromSlog(slog.New(logr.ToSlogHandler(log.WithName("virtual-kubelet")))))
	cs, c := v.deps.Clientset, v.cfg

	desired, err := PrepareNode(ctx, cs, c)
	if err != nil {
		return err
	}

	go func() {
		if err := v.cert.Run(ctx); err != nil {
			cancel(fmt.Errorf("serving certificate: %w", err))
		}
	}()
	ca, err := NewClientCA(cs)
	if err != nil {
		return err
	}
	if err := ca.Start(ctx); err != nil {
		return err
	}
	authCS := v.deps.AuthClientset
	if authCS == nil {
		authCS = cs
	}
	auth, err := NewAuth(authCS, c.NodeName, ca)
	if err != nil {
		return err
	}

	// Pods bound to the node only; ConfigMaps, Secrets and Services are needed by the
	// PodController's constructor for the downward API, which is skipped: their informers are
	// never started, so they cost no API calls and no RBAC (nodeutil.NewNode would start them
	// unfiltered, cluster-wide).
	podFactory := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", c.NodeName).String()
	}))
	pods := podFactory.Core().V1().Pods()
	unused := informers.NewSharedInformerFactory(cs, 0)

	eb := record.NewBroadcaster(record.WithContext(ctx))
	eb.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	defer eb.Shutdown()
	recorder := eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: EventComponent, Host: c.NodeName})

	pc, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                 cs.CoreV1(),
		PodInformer:               pods,
		EventRecorder:             recorder,
		Provider:                  v.provider,
		ConfigMapInformer:         unused.Core().V1().ConfigMaps(),
		SecretInformer:            unused.Core().V1().Secrets(),
		ServiceInformer:           unused.Core().V1().Services(),
		SkipDownwardAPIResolution: true,
		PodEventFilterFunc:        func(_ context.Context, p *corev1.Pod) bool { return p.Spec.NodeName == c.NodeName },
	})
	if err != nil {
		return err
	}
	nc, err := node.NewNodeController(newNodeProvider(desired, v.cert.Ready()), desired, cs.CoreV1().Nodes(),
		node.WithNodeEnableLeaseV1(cs.CoordinationV1().Leases(corev1.NamespaceNodeLease), node.DefaultLeaseDuration),
		node.WithNodeStatusUpdateErrorHandler(v.recreateNode(desired)),
	)
	if err != nil {
		return err
	}

	wsInformer, err := v.deps.Cache.GetInformer(ctx, &workersv1alpha1.WorkerScript{})
	if err != nil {
		return fmt.Errorf("WorkerScript informer: %w", err)
	}
	onWS := func(obj any) {
		if ns, name, ok := workerScriptKey(obj); ok {
			v.provider.OnWorkerScript(ctx, ns, name)
		}
	}
	reg, err := wsInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: onWS, UpdateFunc: func(_, o any) { onWS(o) }, DeleteFunc: onWS,
	})
	if err != nil {
		return err
	}
	defer func() { _ = wsInformer.RemoveEventHandler(reg) }()

	l := v.deps.Listener
	if l == nil {
		var lc net.ListenConfig
		if l, err = lc.Listen(ctx, "tcp", c.ListenAddr); err != nil {
			return fmt.Errorf("kubelet API listener: %w", err)
		}
	}
	handler := NewHandler(HandlerConfig{
		NodeName: c.NodeName, Resolver: v.deps.Resolver, Streamer: v.deps.Streamer, ParseOptions: v.deps.ParseOptions,
		Pods: func(context.Context) ([]*corev1.Pod, error) { return pods.Lister().List(labels.Everything()) },
		Now:  v.deps.Now, Log: log.WithName("kubelet-api"), Limits: v.deps.Limits,
	})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(ctx, l, handler, GuardAuth(auth), ServerTLSConfig(v.cert), v.deps.Limits.WithDefaults().MaxConnections)
	}()

	podFactory.Start(ctx.Done())
	go func() { _ = pc.Run(ctx, PodSyncWorkers) }()
	select {
	case <-ctx.Done():
		return v.exitErr(ctx)
	case <-pc.Done():
		return fmt.Errorf("pod controller: %w", pc.Err())
	case <-pc.Ready():
	}
	go func() { _ = nc.Run(ctx) }()
	select {
	case <-ctx.Done():
		return v.exitErr(ctx)
	case <-nc.Done():
		return fmt.Errorf("node controller: %w", nc.Err())
	case <-nc.Ready():
	}
	close(v.ready)
	log.Info("virtual node running", "node", c.NodeName, "address", c.Address, "listen", c.ListenAddr, "tls", c.TLS.Mode)

	var runErr error
	select {
	case <-ctx.Done():
		runErr = v.exitErr(ctx)
	case <-pc.Done():
		runErr = fmt.Errorf("pod controller stopped: %w", pc.Err())
	case <-nc.Done():
		runErr = fmt.Errorf("node controller stopped: %w", nc.Err())
	case err := <-serveDone:
		runErr = fmt.Errorf("kubelet API server: %w", err)
	}
	cancel(runErr)
	<-pc.Done()
	<-nc.Done()
	return runErr
}

// exitErr is nil after a plain cancellation, else the failure that cancelled ctx.
func (v *VirtualKubelet) exitErr(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// recreateNode handles a Node status update failure: a deleted Node is created again (with the
// owner reference to the ClusterRole, if that still exists; when it does not, the release is
// going away and the error stands).
func (v *VirtualKubelet) recreateNode(desired *corev1.Node) node.ErrorHandler {
	return func(ctx context.Context, err error) error {
		if !apierrors.IsNotFound(err) {
			return err
		}
		cs := v.deps.Clientset
		cr, crErr := cs.RbacV1().ClusterRoles().Get(ctx, v.cfg.OwnerClusterRole, metav1.GetOptions{})
		if crErr != nil {
			return fmt.Errorf("node %s is gone and its owner ClusterRole %s cannot be read: %w", v.cfg.NodeName, v.cfg.OwnerClusterRole, crErr)
		}
		n := desired.DeepCopy()
		n.OwnerReferences = []metav1.OwnerReference{OwnerReference(cr)}
		n.ResourceVersion, n.UID = "", ""
		if _, cerr := cs.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return cerr
		}
		v.deps.Log.Info("recreated the deleted virtual node", "node", v.cfg.NodeName)
		return nil
	}
}
