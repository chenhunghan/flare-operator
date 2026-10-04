package pagesproject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	d1v1alpha1 "github.com/chenhunghan/flare-operator/api/d1/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/r2bind"
	"github.com/chenhunghan/flare-operator/internal/generic"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// ReasonInvalidSpec marks a forProvider the controller cannot send.
const ReasonInvalidSpec = "InvalidSpec"

// configMaps are the deployment-config fields this controller manages as maps (binding or
// variable name → object). A set deployment config is authoritative for them: a key Cloudflare
// has and the config lacks is removed with a null in the PATCH (the pinned spec marks every
// such request value nullable).
var configMaps = []string{"env_vars", "kv_namespaces", "d1_databases", "r2_buckets", "queue_producers", "services"}

// desired is the resolved project: the create body, and the hashes the reconciler compares
// with what it last applied.
type desired struct {
	// body is the create body: name, production_branch and the set parts of build_config,
	// deployment_configs and source. Secret values are included.
	body map[string]any
	// settingsHash covers body without secret values; secretsHash the secret values ("" when
	// there are none).
	settingsHash, secretsHash string
}

// problem is a reason the desired state cannot be built yet (dependency) or at all (invalid).
type problem struct {
	reason, msg string
}

func dependency(format string, args ...any) *problem {
	return &problem{reason: commonv1alpha1.ReasonDependency, msg: fmt.Sprintf(format, args...)}
}

// resolve builds the desired project. A *problem means nothing may be written yet.
func (r *Reconciler) resolve(ctx context.Context, pp *pagesv1alpha1.PagesProject, name string) (*desired, *problem, error) {
	fp := pp.Spec.ForProvider
	body := map[string]any{"name": name, "production_branch": fp.ProductionBranch}
	if bc := fp.BuildConfig; bc != nil {
		if m := jsonObject(bc); len(m) > 0 {
			body["build_config"] = m
		}
	}
	secrets := map[string]string{}
	if dc := fp.DeploymentConfigs; dc != nil {
		cfgs := map[string]any{}
		for env, cfg := range map[string]*pagesv1alpha1.PagesDeploymentConfig{"production": dc.Production, "preview": dc.Preview} {
			if cfg == nil {
				continue
			}
			m, p, err := r.config(ctx, pp, env, cfg, secrets)
			if p != nil || err != nil {
				return nil, p, err
			}
			cfgs[env] = m
		}
		if len(cfgs) > 0 {
			body["deployment_configs"] = cfgs
		}
	}
	if fp.Source != nil {
		body["source"] = jsonObject(fp.Source)
	}
	d := &desired{body: body}
	d.settingsHash = hashJSON(withoutSecrets(body))
	d.secretsHash = secretsHash(secrets)
	return d, nil, nil
}

// config builds deployment_configs.<env>. Maps are always sent (empty: none), and so are
// compatibility_flags; unset scalars are left out (Cloudflare keeps its value).
func (r *Reconciler) config(ctx context.Context, pp *pagesv1alpha1.PagesProject, env string, cfg *pagesv1alpha1.PagesDeploymentConfig,
	secrets map[string]string) (map[string]any, *problem, error) {
	m := map[string]any{"compatibility_flags": append([]any{}, toAny(cfg.CompatibilityFlags)...)}
	if cfg.CompatibilityDate != "" {
		m["compatibility_date"] = cfg.CompatibilityDate
	}
	if cfg.AlwaysUseLatestCompatibilityDate != nil {
		m["always_use_latest_compatibility_date"] = *cfg.AlwaysUseLatestCompatibilityDate
	}
	if cfg.FailOpen != nil {
		m["fail_open"] = *cfg.FailOpen
	}
	if cfg.BuildImageMajorVersion != nil {
		m["build_image_major_version"] = int(*cfg.BuildImageMajorVersion)
	}
	if cfg.Placement != nil {
		m["placement"] = map[string]any{"mode": cfg.Placement.Mode}
	}
	for _, k := range configMaps {
		m[k] = map[string]any{}
	}
	seen := map[string]string{}
	add := func(field, name string, v map[string]any) *problem {
		if prev, dup := seen[name]; dup {
			return &problem{reason: ReasonInvalidSpec, msg: fmt.Sprintf("deployment_configs.%s: %s is both a %s and a %s binding", env, name, prev, field)}
		}
		seen[name] = field
		m[field].(map[string]any)[name] = v
		return nil
	}
	for _, ev := range cfg.EnvVars {
		v := map[string]any{"type": ev.Type}
		if ev.Type == "" {
			v["type"] = pagesv1alpha1.EnvVarPlainText
		}
		switch v["type"] {
		case pagesv1alpha1.EnvVarSecretText:
			if ev.SecretKeyRef == nil {
				return nil, &problem{reason: ReasonInvalidSpec, msg: fmt.Sprintf("env var %s: secret_text needs secretKeyRef", ev.Name)}, nil
			}
			val, p, err := r.secretValue(ctx, pp, ev.SecretKeyRef)
			if p != nil || err != nil {
				if p != nil {
					p.msg = "deployment_configs." + env + " env var " + ev.Name + ": " + p.msg
				}
				return nil, p, err
			}
			v["value"] = val
			secrets[env+"/"+ev.Name] = val
		default:
			if ev.Value == nil {
				return nil, &problem{reason: ReasonInvalidSpec, msg: fmt.Sprintf("env var %s: plain_text needs value", ev.Name)}, nil
			}
			v["value"] = *ev.Value
		}
		if p := add("env_vars", ev.Name, v); p != nil {
			return nil, p, nil
		}
	}
	for _, b := range cfg.KVNamespaces {
		id := deref(b.NamespaceID)
		if b.KVNamespaceRef != nil {
			var kv kvv1alpha1.KVNamespace
			p, err := r.getRef(ctx, pp, "KVNamespace", b.KVNamespaceRef.Name, &kv)
			if p != nil || err != nil {
				return nil, withBinding(p, env, b.Name), err
			}
			id = kv.Status.ID
		}
		if p := add("kv_namespaces", b.Name, map[string]any{"namespace_id": id}); p != nil {
			return nil, p, nil
		}
	}
	for _, b := range cfg.D1Databases {
		id := deref(b.ID)
		if b.D1DatabaseRef != nil {
			var db d1v1alpha1.D1Database
			p, err := r.getRef(ctx, pp, "D1Database", b.D1DatabaseRef.Name, &db)
			if p != nil || err != nil {
				return nil, withBinding(p, env, b.Name), err
			}
			id = db.Status.ID
		}
		if p := add("d1_databases", b.Name, map[string]any{"id": id}); p != nil {
			return nil, p, nil
		}
	}
	for _, b := range cfg.R2Buckets {
		bucket, jurisdiction := deref(b.BucketName), deref(b.Jurisdiction)
		if b.R2BucketRef != nil {
			// The R2Bucket's name and jurisdiction (r2bind.Of).
			var rb r2v1alpha1.R2Bucket
			p, err := r.getRef(ctx, pp, "R2Bucket", b.R2BucketRef.Name, &rb)
			if p != nil || err != nil {
				return nil, withBinding(p, env, b.Name), err
			}
			bucket, jurisdiction = r2bind.Of(&rb)
		}
		v := map[string]any{"name": bucket}
		if jurisdiction != "" {
			v["jurisdiction"] = jurisdiction
		}
		if p := add("r2_buckets", b.Name, v); p != nil {
			return nil, p, nil
		}
	}
	for _, b := range cfg.QueueProducers {
		qn := deref(b.QueueName)
		if b.QueueRef != nil {
			var q queuesv1alpha1.Queue
			p, err := r.getRef(ctx, pp, "Queue", b.QueueRef.Name, &q)
			if p != nil || err != nil {
				return nil, withBinding(p, env, b.Name), err
			}
			if qn = deref(q.Status.AtProvider.QueueName); qn == "" {
				return nil, dependency("deployment_configs.%s binding %s: Queue %s reports no queue_name yet", env, b.Name, q.Name), nil
			}
		}
		if p := add("queue_producers", b.Name, map[string]any{"name": qn}); p != nil {
			return nil, p, nil
		}
	}
	for _, b := range cfg.Services {
		svc := deref(b.Service)
		if b.ServiceRef != nil {
			var ws workersv1alpha1.WorkerScript
			p, err := r.getRef(ctx, pp, "WorkerScript", b.ServiceRef.Name, &ws)
			if p != nil || err != nil {
				return nil, withBinding(p, env, b.Name), err
			}
			svc = ws.ScriptName()
		}
		v := map[string]any{"service": svc}
		if b.Environment != nil {
			v["environment"] = *b.Environment
		}
		if b.Entrypoint != nil {
			v["entrypoint"] = *b.Entrypoint
		}
		if p := add("services", b.Name, v); p != nil {
			return nil, p, nil
		}
	}
	for _, field := range []string{"kv_namespaces", "d1_databases", "r2_buckets"} {
		for name, v := range m[field].(map[string]any) {
			for _, id := range v.(map[string]any) {
				if id == "" {
					return nil, dependency("deployment_configs.%s binding %s: the referenced object has no Cloudflare ID yet", env, name), nil
				}
			}
		}
	}
	return m, nil, nil
}

func withBinding(p *problem, env, name string) *problem {
	if p != nil {
		p.msg = "deployment_configs." + env + " binding " + name + ": " + p.msg
	}
	return p
}

// refReady returns the referenced managed object's problem, if any: being deleted, using
// another CloudflareAccount, or not Ready.
func refReady(pp *pagesv1alpha1.PagesProject, kind string, mg commonv1alpha1.Managed) *problem {
	switch {
	case !mg.GetDeletionTimestamp().IsZero():
		return dependency("%s %s is being deleted", kind, mg.GetName())
	case mg.GetResourceSpec().AccountRef.Name != pp.Spec.AccountRef.Name:
		return dependency("%s %s uses CloudflareAccount %s, not %s", kind, mg.GetName(), mg.GetResourceSpec().AccountRef.Name, pp.Spec.AccountRef.Name)
	case !reconcile.IsReady(mg):
		return dependency("%s %s is not Ready", kind, mg.GetName())
	}
	return nil
}

func (r *Reconciler) getRef(ctx context.Context, pp *pagesv1alpha1.PagesProject, kind, name string, obj reconcile.ManagedObject) (*problem, error) {
	if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return dependency("%s %s not found", kind, name), nil
		}
		return nil, err
	}
	return refReady(pp, kind, obj), nil
}

// secretValue reads a secret_text value. Only a Secret labelled
// flare.dev/worker-binding=true (and not a service account token) is read: the
// operator reads Secrets with its own cluster-wide access, and the Pages Functions code can
// return the value, so without the opt-in anyone allowed to create PagesProjects could read
// every Secret of the namespace. One message for every refusal, so a spec cannot probe which
// Secrets exist.
func (r *Reconciler) secretValue(ctx context.Context, pp *pagesv1alpha1.PagesProject, ref *pagesv1alpha1.PagesSecretKeyRef) (string, *problem, error) {
	unusable := func() (string, *problem, error) {
		return "", dependency("Secret %s key %s is not usable: it must exist, carry the label %s=true, and not be a service account token",
			ref.Name, ref.Key, pagesv1alpha1.LabelSecretOptIn), nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return unusable()
		}
		return "", nil, err
	}
	v, ok := sec.Data[ref.Key]
	if !ok || sec.Labels[pagesv1alpha1.LabelSecretOptIn] != "true" || sec.Type == corev1.SecretTypeServiceAccountToken {
		return unusable()
	}
	return string(v), nil, nil
}

// patchBody is the PATCH that makes cur match d: the desired body, with a null for every key
// of a managed map that cur has and d lacks. The name is not sent (it cannot change).
func patchBody(d *desired, cur *APIProject) map[string]any {
	out := map[string]any{}
	for k, v := range jsonValue(d.body).(map[string]any) {
		if k != "name" {
			out[k] = v
		}
	}
	cfgs, _ := out["deployment_configs"].(map[string]any)
	for env, c := range cfgs {
		cfg := c.(map[string]any)
		have := cur.DeploymentConfigs[env]
		for _, field := range configMaps {
			want, _ := cfg[field].(map[string]any)
			got, _ := have[field].(map[string]any)
			for name := range got {
				if _, keep := want[name]; !keep {
					want[name] = nil
				}
			}
			cfg[field] = want
		}
	}
	return out
}

// drift lists what cur reports differently from d (nil when in sync). Only what d sets is
// compared; a secret_text variable's value is never returned and is tracked by
// status.writeOnlyHash instead; managed maps must have exactly d's keys.
func drift(d *desired, cur *APIProject) []string {
	var out []string
	want := jsonValue(d.body).(map[string]any)
	if want["production_branch"] != cur.ProductionBranch {
		out = append(out, "production_branch")
	}
	if bc, ok := want["build_config"]; ok && !generic.Covers(bc, jsonValue(cur.BuildConfig)) {
		out = append(out, "build_config")
	}
	if src, ok := want["source"]; ok && !generic.Covers(src, jsonValue(cur.Source)) {
		out = append(out, "source")
	}
	cfgs, _ := want["deployment_configs"].(map[string]any)
	envs := make([]string, 0, len(cfgs))
	for env := range cfgs {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		cfg := cfgs[env].(map[string]any)
		have, _ := jsonValue(cur.DeploymentConfigs[env]).(map[string]any)
		for k, v := range cfg {
			field := "deployment_configs." + env + "." + k
			switch {
			case k == "compatibility_flags":
				if !sameSet(v, have[k]) {
					out = append(out, field)
				}
			case isConfigMap(k):
				if !mapMatches(k, v.(map[string]any), have[k]) {
					out = append(out, field)
				}
			default:
				if !generic.Covers(v, have[k]) {
					out = append(out, field)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func isConfigMap(k string) bool {
	for _, f := range configMaps {
		if f == k {
			return true
		}
	}
	return false
}

// mapMatches: got has exactly want's keys, and each entry covers want's (a secret_text entry by
// type only).
func mapMatches(field string, want map[string]any, got any) bool {
	g, _ := got.(map[string]any)
	if len(g) != len(want) {
		return false
	}
	for name, w := range want {
		gv, ok := g[name]
		if !ok {
			return false
		}
		wm := w.(map[string]any)
		if field == "env_vars" && wm["type"] == pagesv1alpha1.EnvVarSecretText {
			wm = map[string]any{"type": wm["type"]}
		}
		if !generic.Covers(wm, gv) {
			return false
		}
	}
	return true
}

func sameSet(a, b any) bool {
	strs := func(v any) []string {
		l, _ := v.([]any)
		out := make([]string, 0, len(l))
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(strs(a), "\x00") == strings.Join(strs(b), "\x00")
}

// withoutSecrets returns body with the values of secret_text variables removed.
func withoutSecrets(body map[string]any) any {
	v := jsonValue(body)
	cfgs, _ := v.(map[string]any)["deployment_configs"].(map[string]any)
	for _, c := range cfgs {
		evs, _ := c.(map[string]any)["env_vars"].(map[string]any)
		for _, ev := range evs {
			if m := ev.(map[string]any); m["type"] == pagesv1alpha1.EnvVarSecretText {
				delete(m, "value")
			}
		}
	}
	return v
}

func secretsHash(secrets map[string]string) string {
	if len(secrets) == 0 {
		return ""
	}
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s=%s\n", n, sha([]byte(secrets[n])))
	}
	return sha([]byte(b.String()))
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v) // map keys sorted: stable
	return sha(b)
}

// jsonValue round-trips v through encoding/json, so it compares with decoded API responses.
func jsonValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func jsonObject(v any) map[string]any {
	m, _ := jsonValue(v).(map[string]any)
	return m
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
