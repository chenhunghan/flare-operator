package vk

import (
	"context"
	"fmt"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/kubernetes"
)

// The cluster's client CA, published by kube-apiserver (read with the built-in Role
// kube-system/extension-apiserver-authentication-reader).
const (
	ClientCANamespace = "kube-system"
	ClientCAConfigMap = "extension-apiserver-authentication"
	ClientCAKey       = "client-ca-file"
)

// ClientCA is the dynamic client CA: the ConfigMap's client-ca-file, followed with a watch on
// that one ConfigMap so CA rotation is picked up.
type ClientCA struct {
	*dynamiccertificates.ConfigMapCAController
}

// NewClientCA returns the client CA controller; call Start before serving.
func NewClientCA(cs kubernetes.Interface) (*ClientCA, error) {
	c, err := dynamiccertificates.NewDynamicCAFromConfigMapController("client-ca", ClientCANamespace, ClientCAConfigMap, ClientCAKey, cs)
	if err != nil {
		return nil, err
	}
	return &ClientCA{c}, nil
}

// Start follows the ConfigMap until ctx ends and waits (at most CALoadTimeout) for the first CA.
func (c *ClientCA) Start(ctx context.Context) error {
	go c.Run(ctx, 1)
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, CALoadTimeout, true, func(context.Context) (bool, error) {
		return len(c.CurrentCABundleContent()) > 0, nil
	})
	if err != nil {
		return fmt.Errorf("no client CA in ConfigMap %s/%s key %s after %s (kube-apiserver without --client-ca-file, or no RBAC to read it): %w",
			ClientCANamespace, ClientCAConfigMap, ClientCAKey, CALoadTimeout, err)
	}
	return nil
}

// CALoadTimeout bounds ClientCA.Start's wait for the first CA bundle.
var CALoadTimeout = time.Minute

// NewAuth returns the kubelet API's authentication and authorization (virtual-kubelet's
// nodeutil.WebhookAuth): a client certificate verified against ca, or a bearer token verified
// with a TokenReview; no anonymous access; then a SubjectAccessReview per request on
// nodes/<subresource> named nodeName, where the subresource is log for /logs, stats for
// /stats, metrics for /metrics and proxy for everything else, /containerLogs included (the
// kubelet's mapping, k8s.io/kubernetes pkg/kubelet/server/auth.go). Decisions are cached as the
// kubelet caches them (allow 5 minutes, deny 30 seconds; tokens 2 minutes).
func NewAuth(cs kubernetes.Interface, nodeName string, ca dynamiccertificates.CAContentProvider) (nodeutil.Auth, error) {
	return nodeutil.WebhookAuth(cs, nodeName, func(c *nodeutil.WebhookAuthConfig) error {
		c.AuthnConfig.ClientCertificateCAContentProvider = ca
		c.AuthnConfig.Anonymous = nil
		return nil
	})
}
