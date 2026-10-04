package testenv

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/fake"
)

// RandomHex returns n random bytes hex-encoded (2n characters).
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomAccountID returns a fresh 32-character account ID, so tests sharing one fake do not
// see each other's resources.
func RandomAccountID() string { return RandomHex(16) }

// Namespace creates a namespace with a unique name. (envtest runs no namespace controller, so
// namespaces are never really deleted; unique names keep tests independent.)
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	name := "t-" + strings.ToLower(RandomHex(5))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.Client.Create(Context(t, 10*time.Second), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

// AccountOptions configures CreateAccount. Zero values give a Ready-able account.
type AccountOptions struct {
	// AccountID defaults to RandomAccountID().
	AccountID string
	// Token defaults to a random value.
	Token string
	// SecretName defaults to <name>-token; SecretKey defaults to "token".
	SecretName, SecretKey string
	// NoSecret skips creating the Secret.
	NoSecret bool
	// NoRegister skips registering Token in the fake (so verification fails).
	NoRegister bool
	// FakeToken customises the registered token (Value and, when empty, AccountID are filled in).
	FakeToken fake.Token
	// RateLimit defaults to a budget high enough for tests (1000 rps, burst 1000).
	RateLimit *cloudflarev1alpha1.RateLimitSpec
}

// Account is what CreateAccount made.
type Account struct {
	*cloudflarev1alpha1.CloudflareAccount
	Token     string
	AccountID string
}

// CreateAccount creates the token Secret (unless NoSecret), registers the token with the fake
// (unless NoRegister) and creates a CloudflareAccount pointing at the fake. It does not wait.
func (e *Env) CreateAccount(t testing.TB, namespace, name string, o AccountOptions) *Account {
	t.Helper()
	ctx := Context(t, 10*time.Second)
	if o.AccountID == "" {
		o.AccountID = RandomAccountID()
	}
	if o.Token == "" {
		o.Token = "tok-" + RandomHex(12)
	}
	if o.SecretName == "" {
		o.SecretName = name + "-token"
	}
	if o.SecretKey == "" {
		o.SecretKey = "token"
	}
	if o.RateLimit == nil {
		o.RateLimit = &cloudflarev1alpha1.RateLimitSpec{RequestsPerFiveMinutes: 300000, Burst: 1000}
	}
	if !o.NoRegister {
		ft := o.FakeToken
		ft.Value = o.Token
		if ft.AccountID == "" && ft.Kind != "user" {
			ft.AccountID = o.AccountID
		}
		e.Fake.AddToken(ft)
	}
	if !o.NoSecret {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: o.SecretName},
			Data:       map[string][]byte{o.SecretKey: []byte(o.Token)},
		}
		if err := e.Client.Create(ctx, sec); err != nil {
			t.Fatalf("create token secret: %v", err)
		}
	}
	acct := &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID:      o.AccountID,
			TokenSecretRef: cloudflarev1alpha1.SecretKeySelector{Name: o.SecretName, Key: o.SecretKey},
			BaseURL:        e.BaseURL,
			RateLimit:      o.RateLimit,
		},
	}
	if err := e.Client.Create(ctx, acct); err != nil {
		t.Fatalf("create CloudflareAccount: %v", err)
	}
	return &Account{CloudflareAccount: acct, Token: o.Token, AccountID: o.AccountID}
}

// CreateReadyAccount creates a CloudflareAccount (see CreateAccount) and waits until the
// account controller reports Ready=True. A manager must be running (StartManager).
func (e *Env) CreateReadyAccount(t testing.TB, namespace, name string) *Account {
	t.Helper()
	a := e.CreateAccount(t, namespace, name, AccountOptions{})
	a.CloudflareAccount = e.WaitAccountCondition(t, namespace, name, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	return a
}

// WaitAccountCondition waits until the account's Ready condition (for its current generation)
// has status and reason (reason "" matches any), and returns the account.
func (e *Env) WaitAccountCondition(t testing.TB, namespace, name string, status metav1.ConditionStatus, reason string) *cloudflarev1alpha1.CloudflareAccount {
	t.Helper()
	var acct cloudflarev1alpha1.CloudflareAccount
	Eventually(t, 30*time.Second, func() (bool, string) {
		if err := e.Client.Get(Context(t, 5*time.Second), client.ObjectKey{Namespace: namespace, Name: name}, &acct); err != nil {
			return false, err.Error()
		}
		c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		if c == nil {
			return false, "no Ready condition yet"
		}
		ok := c.Status == status && (reason == "" || c.Reason == reason) && c.ObservedGeneration == acct.Generation
		return ok, fmt.Sprintf("Ready=%s reason=%s message=%q (generation %d/%d)", c.Status, c.Reason, c.Message, c.ObservedGeneration, acct.Generation)
	})
	return &acct
}
