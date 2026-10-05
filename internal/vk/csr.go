package vk

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// CSR metadata.
const (
	// CSRLabelNode labels every CSR the virtual kubelet creates with its node name.
	CSRLabelNode = "flare.dev/virtual-node-name"
	// CSRApprovalReason is the reason of the Approved condition the virtual kubelet sets on its
	// own CSRs.
	CSRApprovalReason = "FlareWorkersVirtualKubeletSelfApproval"
	// CSRLabelOwner labels every CSR with the release's owner ClusterRole (when its name is a
	// valid label value), and app.kubernetes.io/managed-by is CSRManagedBy, so leftovers are easy
	// to find: kubectl delete csr -l app.kubernetes.io/managed-by=flare-operator-workers-vk.
	CSRLabelOwner = "flare.dev/owner-cluster-role"
	CSRManagedBy  = "flare-operator-workers-vk"
)

// errCSRRefused: the CSR was denied or failed.
var errCSRRefused = errors.New("the certificate signing request was refused")

// CSRCert is TLSModeCSR. It requests a kubernetes.io/kubelet-serving certificate for
// CN=system:node:<node>, O=system:nodes with SANs <node> and the node's address, from a fresh
// ECDSA P-256 key, for Lifetime (spec.expirationSeconds); kube-controller-manager signs it.
//
// With Approve it approves that CSR itself, and nothing else: it approves only the object it
// just created (same UID, through the update's optimistic lock), only when the API server
// recorded the creator's own identity as the requester, and only for exactly the request it
// sent (its key, its CN, its SANs). It never lists or approves other CSRs.
//
// The certificate is replaced at RenewFraction of its lifetime, or at once when SetAddress
// changes the address. The old one keeps serving until the new one is issued.
type CSRCert struct {
	certHolder
	cs       kubernetes.Interface
	nodeName string
	approve  bool
	lifetime time.Duration
	log      logr.Logger
	// PollInterval is how often a pending CSR is read (default 1 s).
	PollInterval time.Duration
	// ApprovalTimeout bounds the wait for a self-approved CSR's certificate before a new CSR is
	// made (default 5 min). Without Approve the wait has no bound: an administrator approves.
	ApprovalTimeout time.Duration
	// OwnerClusterRole, when set, is put on every CSR as CSRLabelOwner.
	OwnerClusterRole string

	mu          sync.Mutex
	address     string
	addrChanged chan struct{}
}

// NewCSRCert returns a CSR-backed ServingCert for nodeName at address.
func NewCSRCert(cs kubernetes.Interface, nodeName, address string, approve bool, lifetime time.Duration, log logr.Logger) *CSRCert {
	return &CSRCert{
		certHolder: newCertHolder(), cs: cs, nodeName: nodeName, approve: approve, lifetime: lifetime, log: log,
		PollInterval: time.Second, ApprovalTimeout: 5 * time.Minute,
		address: address, addrChanged: make(chan struct{}, 1),
	}
}

// SetAddress changes the node address the certificate must name; a change renews it.
func (c *CSRCert) SetAddress(address string) {
	c.mu.Lock()
	changed := address != c.address
	c.address = address
	c.mu.Unlock()
	if changed {
		select {
		case c.addrChanged <- struct{}{}:
		default:
		}
	}
}

func (c *CSRCert) currentAddress() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.address
}

// Run implements ServingCert.
func (c *CSRCert) Run(ctx context.Context) error {
	failures := 0
	for {
		addr := c.currentAddress()
		cert, err := c.obtain(ctx, addr)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Error(err, "obtain the kubelet API serving certificate")
			if !sleepCtx(ctx, backoff(failures), nil) {
				return nil
			}
			failures++
			continue
		}
		failures = 0
		c.set(cert)
		c.log.Info("serving certificate issued", "notAfter", cert.Leaf.NotAfter, "renewAt", renewAt(cert.Leaf))
		for {
			if !sleepCtx(ctx, time.Until(renewAt(cert.Leaf)), c.addrChanged) {
				return nil
			}
			if time.Now().After(renewAt(cert.Leaf)) || c.currentAddress() != addr {
				break
			}
		}
	}
}

// expectedRequest is the x509 request for the node at address.
func (c *CSRCert) expectedRequest(address string) (*x509.CertificateRequest, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return nil, fmt.Errorf("node address %q is not an IP", address)
	}
	return &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "system:node:" + c.nodeName, Organization: []string{"system:nodes"}},
		DNSNames:    []string{c.nodeName},
		IPAddresses: []net.IP{ip},
	}, nil
}

func (c *CSRCert) obtain(ctx context.Context, address string) (_ *tls.Certificate, retErr error) {
	tmpl, err := c.expectedRequest(address)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, err
	}
	reqPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	csrs := c.cs.CertificatesV1().CertificateSigningRequests()
	created, err := csrs.Create(ctx, &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "flare-workers-vk-" + c.nodeName + "-",
			Labels:       c.csrLabels(),
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           reqPEM,
			SignerName:        certificatesv1.KubeletServingSignerName,
			Usages:            []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageServerAuth},
			ExpirationSeconds: ptr.To(int32(c.lifetime / time.Second)),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create a CSR: %w", err)
	}
	log := c.log.WithValues("csr", created.Name)
	// The CSR object is not needed once its certificate is in memory, nor when it gives none.
	defer c.deleteOwn(created)
	if c.approve {
		if err := c.approveOwn(ctx, created, reqPEM, key.Public(), address); err != nil {
			return nil, err
		}
		log.Info("approved our own kubelet-serving CSR")
	} else {
		log.Info("waiting for an administrator to approve the kubelet-serving CSR: kubectl certificate approve " + created.Name)
	}
	waitCtx := ctx
	if c.approve {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, c.ApprovalTimeout)
		defer cancel()
	}
	certPEM, err := c.waitIssued(waitCtx, created)
	if err != nil {
		return nil, err
	}
	return keyPair(certPEM, key)
}

func (c *CSRCert) csrLabels() map[string]string {
	l := map[string]string{CSRLabelNode: c.nodeName, "app.kubernetes.io/managed-by": CSRManagedBy}
	if c.OwnerClusterRole != "" && len(validation.IsValidLabelValue(c.OwnerClusterRole)) == 0 {
		l[CSRLabelOwner] = c.OwnerClusterRole
	}
	return l
}

// deleteOwn deletes created, a CSR this process made, once it is done with it: its certificate
// is installed in memory, or it gave none (denied, failed, timed out, or abandoned at
// shutdown), so neither renewals nor retries leave CSRs behind. The UID precondition makes it
// delete only that object, never another CSR of the same name. It is best effort: without RBAC
// delete on certificatesigningrequests the CSR is left to kube-controller-manager's cleaner.
func (c *CSRCert) deleteOwn(created *certificatesv1.CertificateSigningRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uid := created.UID
	err := c.cs.CertificatesV1().CertificateSigningRequests().Delete(ctx, created.Name,
		metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	switch {
	case err == nil:
		c.log.V(1).Info("deleted our unissued CSR", "csr", created.Name)
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
	case apierrors.IsForbidden(err):
		c.log.V(1).Info("cannot delete our unissued CSR (no RBAC delete); kube-controller-manager cleans it up", "csr", created.Name)
	default:
		c.log.Info("delete our unissued CSR", "csr", created.Name, "error", err.Error())
	}
}

// approveOwn approves created, the CSR this process just made, after checking that it is
// exactly that: the API server's copy has the same UID, the requester the API server recorded
// at creation (our own identity), the request bytes we sent (our key, CN and SANs), and the
// kubelet-serving signer. The update carries the UID and resourceVersion, so a CSR deleted and
// recreated under the same name in between is never approved.
func (c *CSRCert) approveOwn(ctx context.Context, created *certificatesv1.CertificateSigningRequest, reqPEM []byte, pub crypto.PublicKey, address string) error {
	if created.Spec.Username == "" {
		return errors.New("the API server recorded no requester on our CSR; not approving it")
	}
	if err := c.checkRequest(created.Spec.Request, pub, address); err != nil {
		return err
	}
	csrs := c.cs.CertificatesV1().CertificateSigningRequests()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := csrs.Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch {
		case cur.UID != created.UID:
			return fmt.Errorf("CSR %s was replaced (UID %s, ours %s); not approving it", cur.Name, cur.UID, created.UID)
		case cur.Spec.Username != created.Spec.Username:
			return fmt.Errorf("CSR %s has requester %q, ours is %q; not approving it", cur.Name, cur.Spec.Username, created.Spec.Username)
		case !bytes.Equal(cur.Spec.Request, reqPEM) || cur.Spec.SignerName != certificatesv1.KubeletServingSignerName:
			return fmt.Errorf("CSR %s is not the request we sent; not approving it", cur.Name)
		}
		if hasCondition(cur, certificatesv1.CertificateApproved) {
			return nil
		}
		if hasCondition(cur, certificatesv1.CertificateDenied) || hasCondition(cur, certificatesv1.CertificateFailed) {
			return fmt.Errorf("CSR %s: %w", cur.Name, errCSRRefused)
		}
		cur.Status.Conditions = append(cur.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Reason:  CSRApprovalReason,
			Message: fmt.Sprintf("the Cloudflare Workers virtual kubelet approved its own serving certificate for node %s", c.nodeName),
		})
		_, err = csrs.UpdateApproval(ctx, cur.Name, cur, metav1.UpdateOptions{})
		return err
	})
}

// checkRequest verifies that a PEM CSR is the one for our node at address with key pub.
func (c *CSRCert) checkRequest(reqPEM []byte, pub crypto.PublicKey, address string) error {
	block, _ := pem.Decode(reqPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return errors.New("CSR request is not a PEM certificate request")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return err
	}
	want, err := c.expectedRequest(address)
	if err != nil {
		return err
	}
	type keyEqualer interface{ Equal(crypto.PublicKey) bool }
	k, ok := pub.(keyEqualer)
	switch {
	case req.Subject.CommonName != want.Subject.CommonName || !slices.Equal(req.Subject.Organization, want.Subject.Organization):
		return fmt.Errorf("CSR subject %q is not %q", req.Subject, want.Subject)
	case !slices.Equal(req.DNSNames, want.DNSNames) || len(req.IPAddresses) != 1 || !req.IPAddresses[0].Equal(want.IPAddresses[0]) ||
		len(req.EmailAddresses) > 0 || len(req.URIs) > 0:
		return fmt.Errorf("CSR SANs %v %v are not %v %v", req.DNSNames, req.IPAddresses, want.DNSNames, want.IPAddresses)
	case !ok || !k.Equal(req.PublicKey):
		return errors.New("CSR public key is not ours")
	}
	return nil
}

func hasCondition(csr *certificatesv1.CertificateSigningRequest, t certificatesv1.RequestConditionType) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == t && c.Status != corev1.ConditionFalse {
			return true
		}
	}
	return false
}

// waitIssued polls the CSR (get only: no list or watch) until it has a certificate.
func (c *CSRCert) waitIssued(ctx context.Context, created *certificatesv1.CertificateSigningRequest) ([]byte, error) {
	csrs := c.cs.CertificatesV1().CertificateSigningRequests()
	for {
		cur, err := csrs.Get(ctx, created.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("CSR %s was deleted before it was issued", created.Name)
		case err != nil && ctx.Err() != nil:
			return nil, fmt.Errorf("CSR %s not issued: %w", created.Name, ctx.Err())
		case err != nil:
			c.log.Error(err, "read CSR", "csr", created.Name)
		case cur.UID != created.UID:
			return nil, fmt.Errorf("CSR %s was replaced before it was issued", created.Name)
		case hasCondition(cur, certificatesv1.CertificateDenied), hasCondition(cur, certificatesv1.CertificateFailed):
			return nil, fmt.Errorf("CSR %s: %w", created.Name, errCSRRefused)
		case len(cur.Status.Certificate) > 0:
			return cur.Status.Certificate, nil
		}
		if !sleepCtx(ctx, c.PollInterval, nil) {
			return nil, fmt.Errorf("CSR %s not issued: %w", created.Name, ctx.Err())
		}
	}
}

// keyPair joins an issued PEM chain with our key, checking that the leaf certifies that key.
func keyPair(certPEM []byte, key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	var chain [][]byte
	for rest := certPEM; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "CERTIFICATE" {
			chain = append(chain, b.Bytes)
		}
	}
	if len(chain) == 0 {
		return nil, errors.New("the issued certificate has no PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("the issued certificate is not for our key")
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}
