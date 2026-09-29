package reconcile_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/reconcile"
)

func TestBaseURLPolicy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		base   string
		policy reconcile.BaseURLPolicy
		ok     bool
	}{
		{"default-empty", "", reconcile.BaseURLPolicy{}, true},
		{"default-explicit", cfclient.DefaultBaseURL + "/", reconcile.BaseURLPolicy{}, true},
		{"override-denied", "http://169.254.169.254/latest", reconcile.BaseURLPolicy{}, false},
		{"override-any", "http://flarefake:8787/client/v4", reconcile.BaseURLPolicy{AllowAny: true}, true},
		{"allowlisted", "http://flarefake:8787/client/v4/", reconcile.BaseURLPolicy{Allowed: []string{"http://flarefake:8787/client/v4"}}, true},
		{"not-allowlisted", "http://flarefake:8787/client/v4/../x", reconcile.BaseURLPolicy{Allowed: []string{"http://flarefake:8787/client/v4"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Allows(tc.base); got != tc.ok {
				t.Fatalf("Allows(%q)=%v", tc.base, got)
			}
			acct := readyAccount(1, 1, true)
			acct.Spec.BaseURL = tc.base
			built := 0
			a := reconcile.NewAccounts(newKube(t, acct, tokenSecret("t")), reconcile.WithBaseURLPolicy(tc.policy),
				reconcile.WithClientFactory(func(o cfclient.Options) (cfclient.Client, error) { built++; return cfclient.New(o) }))
			_, err := a.ClientFor(ctx, acct)
			var ae *reconcile.AccountError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("ClientFor: %v", err)
			case !tc.ok && (!errors.As(err, &ae) || ae.Reason != cloudflarev1alpha1.ReasonBaseURLNotAllowed || built != 0):
				t.Fatalf("want BaseURLNotAllowed without building a client, got %v (built %d)", err, built)
			}
			if !tc.ok && strings.Contains(err.Error(), tc.base) && tc.base != "" {
				t.Errorf("message echoes the URL: %v", err)
			}
		})
	}
}

func TestResolveLabelsManagedObject(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	w.Labels = map[string]string{"keep": "me"}
	kube := newKube(t, readyAccount(1, 1, true), tokenSecret("t"), w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	w.Status.Note = "in-memory status survives"
	rv := w.ResourceVersion
	a := reconcile.NewAccounts(kube, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}))
	if _, err := a.Resolve(ctx, w); err != nil {
		t.Fatal(err)
	}
	if w.Labels[reconcile.AccountLabel] != "acct" || w.Labels["keep"] != "me" || w.ResourceVersion == rv || w.Status.Note == "" {
		t.Fatalf("in memory: labels %v rv %s->%s note %q", w.Labels, rv, w.ResourceVersion, w.Status.Note)
	}
	var stored Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[reconcile.AccountLabel] != "acct" || stored.Labels["keep"] != "me" {
		t.Fatalf("stored labels %v", stored.Labels)
	}
	// Already labelled: no write.
	rv = w.ResourceVersion
	if _, err := a.Resolve(ctx, w); err != nil || w.ResourceVersion != rv {
		t.Fatalf("second Resolve wrote: %v rv %s->%s", err, rv, w.ResourceVersion)
	}
	// The label follows accountRef changes, even when the new account does not exist.
	w.Spec.AccountRef.Name = "other"
	if _, err := a.Resolve(ctx, w); !reconcile.IsAccountNotReady(err) || w.Labels[reconcile.AccountLabel] != "other" {
		t.Fatalf("relabel: %v %v", err, w.Labels)
	}
	// A reader-only Accounts labels in memory only.
	w2 := widget(nil, "")
	if _, err := reconcile.NewAccounts(client.Reader(newKube(t))).Resolve(ctx, w2); !reconcile.IsAccountNotReady(err) || w2.Labels[reconcile.AccountLabel] != "acct" {
		t.Fatalf("reader-only: %v %v", err, w2.Labels)
	}
}

func TestDeletingAccountKeepsCachedClient(t *testing.T) {
	ctx := context.Background()
	acct := readyAccount(1, 1, true)
	sec := tokenSecret("t")
	kube := newKube(t, acct, sec)
	a := reconcile.NewAccounts(kube, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}))
	c1, err := a.ClientFor(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClientFor(ctx, acct); !reconcile.IsAccountNotReady(err) {
		t.Fatalf("live account without Secret: %v", err)
	}
	now := metav1.Now()
	acct.DeletionTimestamp = &now
	if c2, err := a.ClientFor(ctx, acct); err != nil || c2 != c1 {
		t.Fatalf("deleting account: %v same=%v", err, c2 == c1)
	}
	a.Forget(client.ObjectKeyFromObject(acct))
	if _, err := a.ClientFor(ctx, acct); !reconcile.IsAccountNotReady(err) {
		t.Fatalf("after Forget: %v", err)
	}
}

func TestConditionMessageSanitized(t *testing.T) {
	w := widget(nil, "")
	reconcile.MarkSyncError(w, "", errors.New("line1\nline2\x1b[0m "+strings.Repeat("x", 5000)))
	m := reconcile.GetCondition(w, "Synced").Message
	if strings.ContainsAny(m, "\n\x1b") || len(m) > reconcile.MaxConditionMessage+len("…") || !strings.HasPrefix(m, "line1 line2") {
		t.Errorf("message %q (%d bytes)", m[:40], len(m))
	}
}

func TestAccountLabelValue(t *testing.T) {
	if v := reconcile.AccountLabelValue("acct.prod-1"); v != "acct.prod-1" {
		t.Error(v)
	}
	long := strings.Repeat("a", 64)
	v := reconcile.AccountLabelValue(long)
	if v == long || len(validation.IsValidLabelValue(v)) != 0 || v != reconcile.AccountLabelValue(long) {
		t.Errorf("long name → %q", v)
	}
}
