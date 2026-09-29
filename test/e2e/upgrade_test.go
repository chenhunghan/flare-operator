//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
)

// The upgrade test runs in two phases around a `helm upgrade` (make e2e-upgrade,
// hack/e2e-upgrade.sh), selected by E2E_UPGRADE_PHASE:
//
//	pre   against the previous chart and manager: create an account and one object of each
//	      of KVNamespace, Queue, D1Database and Tunnel in namespace E2E_UPGRADE_NAMESPACE, wait
//	      until they are Ready, and save their Cloudflare IDs and the flarefake pod in a
//	      ConfigMap there
//	post  after the upgrade: the manager runs E2E_UPGRADE_IMAGE, the same objects are Ready
//	      and Synced with the same IDs, the new manager has read every resource and made no
//	      Cloudflare write (no duplicate create, no drift from the version change); then the
//	      namespace is deleted
//
// Without E2E_UPGRADE_PHASE both tests skip, so `make e2e` does not run them.

const upgradeStateCM = "flare-e2e-upgrade-state"

type upgradeState struct {
	AccountID    string            `json:"accountID"`
	IDs          map[string]string `json:"ids"`
	FakePodUID   string            `json:"fakePodUID"`
	ManagerImage string            `json:"managerImage"`
}

func upgradeNamespace() string { return getenv("E2E_UPGRADE_NAMESPACE", "flare-e2e-upgrade") }

func upgradeSuite(t *testing.T, phase string) *suite {
	t.Helper()
	if got := os.Getenv("E2E_UPGRADE_PHASE"); got != phase {
		t.Skipf("E2E_UPGRADE_PHASE=%q (run by make e2e-upgrade)", got)
	}
	s := newSuite(t)
	s.ns = upgradeNamespace()
	return s
}

// upgradeObjects are the managed objects the upgrade test keeps across the upgrade.
func (s *suite) upgradeObjects() []commonv1alpha1.Managed {
	acct := commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete}
	return []commonv1alpha1.Managed{
		&kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "kv"},
			Spec: kvv1alpha1.KVNamespaceSpec{ResourceSpec: acct, ForProvider: kvv1alpha1.KVNamespaceParameters{Title: ptr.To("flare-e2e-upgrade-kv")}}},
		&queuesv1alpha1.Queue{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "queue"},
			Spec: queuesv1alpha1.QueueSpec{ResourceSpec: acct, ForProvider: queuesv1alpha1.QueueParameters{
				QueueName: ptr.To("flare-e2e-upgrade-queue"),
				Settings:  &queuesv1alpha1.QueueSettingsParameters{DeliveryDelay: ptr.To[int64](5), DeliveryPaused: ptr.To(false)},
			}}},
		&d1v1alpha1.D1Database{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "db"},
			Spec: d1v1alpha1.D1DatabaseSpec{ResourceSpec: acct, ForProvider: d1v1alpha1.D1DatabaseParameters{
				Name: ptr.To("flare-e2e-upgrade-db"), PrimaryLocationHint: ptr.To("WEUR"),
			}}},
		s.tunnelSpec("tun"),
	}
}

func upgradeKey(mg commonv1alpha1.Managed) string { return fmt.Sprintf("%T/%s", mg, mg.GetName()) }

func (s *suite) fakePodUID() string {
	s.t.Helper()
	var pl corev1.PodList
	if err := s.c.List(s.ctx(), &pl, client.InNamespace(s.cfg.OperatorNS), client.MatchingLabels{
		"app.kubernetes.io/instance": s.cfg.Release, "app.kubernetes.io/component": "flarefake",
	}); err != nil {
		s.t.Fatal(err)
	}
	for _, p := range pl.Items {
		if p.DeletionTimestamp == nil {
			return string(p.UID)
		}
	}
	s.t.Fatal("no flarefake pod")
	return ""
}

// managerImage returns the image of the Ready manager pods (empty unless all of them run the
// same one) and the earliest start time among them.
func (s *suite) managerImage() (string, time.Time) {
	s.t.Helper()
	img, start := "", time.Time{}
	for _, p := range s.managerPods() {
		if p.DeletionTimestamp != nil || !podReady(&p) {
			continue
		}
		for _, c := range p.Spec.Containers {
			if c.Name != "manager" {
				continue
			}
			if img != "" && img != c.Image {
				return "", time.Time{}
			}
			img = c.Image
		}
		if p.Status.StartTime != nil && (start.IsZero() || p.Status.StartTime.Before(&metav1.Time{Time: start})) {
			start = p.Status.StartTime.Time
		}
	}
	return img, start
}

func TestUpgradePre(t *testing.T) {
	s := upgradeSuite(t, "pre")
	var ns corev1.Namespace
	if err := s.c.Get(s.ctx(), client.ObjectKey{Name: s.ns}, &ns); err == nil {
		t.Fatalf("namespace %s already exists (a previous upgrade run did not clean up)", s.ns)
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	o := &objects{}
	s.testAccount(t, o)
	objs := s.upgradeObjects()
	for _, mg := range objs {
		s.create(mg.(client.Object))
	}
	img, _ := s.managerImage()
	st := upgradeState{AccountID: s.accountID, IDs: map[string]string{}, FakePodUID: s.fakePodUID(), ManagerImage: img}
	for _, mg := range objs {
		s.waitManaged(mg, 3*time.Minute)
		st.IDs[upgradeKey(mg)] = mg.GetResourceStatus().ID
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	s.create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: upgradeStateCM}, Data: map[string]string{"state": string(b)}})
	t.Logf("before the upgrade: manager %s, objects %v", st.ManagerImage, st.IDs)
}

func TestUpgradePost(t *testing.T) {
	s := upgradeSuite(t, "post")
	var cm corev1.ConfigMap
	if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: upgradeStateCM}, &cm); err != nil {
		t.Fatalf("upgrade state (run E2E_UPGRADE_PHASE=pre first): %v", err)
	}
	var st upgradeState
	if err := json.Unmarshal([]byte(cm.Data["state"]), &st); err != nil {
		t.Fatal(err)
	}
	s.accountID = st.AccountID
	// Tear the namespace down whatever happens below (cleanup waits for the finalizers).
	t.Cleanup(func() { s.cleanup() })

	// flarefake keeps its state in memory: if its pod was replaced, the objects' Cloudflare
	// side is gone and the checks below would test the emulator, not the upgrade.
	if uid := s.fakePodUID(); uid != st.FakePodUID {
		t.Fatalf("the flarefake pod changed across the upgrade (%s -> %s): keep flarefake.image.tag and its template unchanged", st.FakePodUID, uid)
	}
	want := os.Getenv("E2E_UPGRADE_IMAGE")
	var started time.Time
	eventually(t, 3*time.Minute, "the upgraded manager to be Ready", func() (bool, string) {
		img, start := s.managerImage()
		started = start
		if want != "" {
			return img == want, "manager image " + img + ", want " + want
		}
		return img != "" && img != st.ManagerImage, "manager image " + img + " (before: " + st.ManagerImage + ")"
	})
	img, _ := s.managerImage()
	t.Logf("manager %s -> %s, started %s", st.ManagerImage, img, started.Format(time.RFC3339))

	var acct cloudflarev1alpha1.CloudflareAccount
	eventually(t, 2*time.Minute, "CloudflareAccount Ready after the upgrade", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: "acct"}, &acct); err != nil {
			return false, err.Error()
		}
		r := cond(acct.Status.Conditions, commonv1alpha1.ConditionReady)
		return r != nil && r.Status == metav1.ConditionTrue, condString(acct.Status.Conditions)
	})

	objs := s.upgradeObjects()
	for _, mg := range objs {
		s.waitManaged(mg, 2*time.Minute)
		if id, was := mg.GetResourceStatus().ID, st.IDs[upgradeKey(mg)]; id != was {
			t.Errorf("%s: status.id %q after the upgrade, %q before", upgradeKey(mg), id, was)
		}
	}
	// The new manager reads every resource (its initial sync) and writes nothing: no
	// duplicate create, no update from a changed default. flarefake's journal still holds the
	// requests since the pre phase; only this account's entries since the new manager pods
	// started are looked at (pod start and journal times come from the same node clock on a
	// single-node cluster; a second of slack covers rounding).
	since := started.Add(-time.Second)
	var sinceUpgrade []journalEntry
	eventually(t, 3*time.Minute, "the upgraded manager to read every resource", func() (bool, string) {
		sinceUpgrade = nil
		for _, e := range s.journal() {
			if !e.Time.Before(since) {
				sinceUpgrade = append(sinceUpgrade, e)
			}
		}
		j := sinceUpgrade
		var missing []string
		for k, id := range st.IDs {
			if !slices.ContainsFunc(j, func(e journalEntry) bool { return e.Method == "GET" && strings.Contains(e.Path, id) }) {
				missing = append(missing, k)
			}
		}
		return len(missing) == 0, "not yet read: " + strings.Join(missing, ", ")
	})
	if w := writes(sinceUpgrade); len(w) > 0 {
		t.Errorf("Cloudflare writes by the upgraded manager: %v", w)
	}
	s.checkJournalClean(sinceUpgrade)
	t.Logf("upgraded manager: %d Cloudflare requests for this account since it started, %d writes", len(sinceUpgrade), len(writes(sinceUpgrade)))
	s.clearJournal()
	s.restartManager() // a second start of the new version: still no writes
	eventually(t, 3*time.Minute, "a GET of every resource after a restart", func() (bool, string) {
		j := s.journal()
		n := 0
		for _, id := range st.IDs {
			if slices.ContainsFunc(j, func(e journalEntry) bool { return e.Method == "GET" && strings.Contains(e.Path, id) }) {
				n++
			}
		}
		return n == len(st.IDs), fmt.Sprintf("%d of %d read", n, len(st.IDs))
	})
	time.Sleep(5 * time.Second)
	j := s.journal()
	if w := writes(j); len(w) > 0 {
		t.Errorf("Cloudflare writes after restarting the upgraded manager: %v", w)
	}
	t.Logf("after a restart of the upgraded manager: %d requests, %d writes", len(j), len(writes(j)))
	s.checkJournalClean(j)
	for _, mg := range objs {
		s.waitManaged(mg, 30*time.Second)
	}
	s.testManagerHealth(t)
}
