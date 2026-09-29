package workerscript

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// desired is the resolved upload of a WorkerScript: modules, metadata with the IDs its
// references resolved to, and the hashes the reconciler compares with what it last applied.
type desired struct {
	modules  []module
	metadata apiUploadMetadata
	// contentHash covers main_module and the modules; settingsHash the settings with secret
	// values left out; secretsHash the secret values ("" without secret_text bindings).
	contentHash, settingsHash, secretsHash string
}

// problem is a reason the desired state cannot be built yet (dependency) or at all (invalid).
type problem struct {
	reason, msg string
}

// ReasonInvalidSpec marks a forProvider the controller cannot upload (a module name or content
// it cannot encode, a main_module missing from the ConfigMap, ...).
const ReasonInvalidSpec = "InvalidSpec"

func dependency(format string, args ...any) *problem {
	return &problem{reason: commonv1alpha1.ReasonDependency, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *problem {
	return &problem{reason: ReasonInvalidSpec, msg: fmt.Sprintf(format, args...)}
}

// resolve builds the desired upload. A *problem means nothing may be uploaded yet.
func (r *Reconciler) resolve(ctx context.Context, ws *workersv1alpha1.WorkerScript) (*desired, *problem, error) {
	fp := ws.Spec.ForProvider
	mods, p, err := r.modules(ctx, ws)
	if p != nil || err != nil {
		return nil, p, err
	}
	found := false
	for _, m := range mods {
		found = found || m.Name == fp.MainModule
	}
	if !found {
		return nil, invalid("main_module %q is not one of the modules", fp.MainModule), nil
	}
	bindings, secrets, p, err := r.bindings(ctx, ws)
	if p != nil || err != nil {
		return nil, p, err
	}
	obs, p := observability(fp.Observability)
	if p != nil {
		return nil, p, nil
	}
	d := &desired{
		modules: mods,
		metadata: apiUploadMetadata{MainModule: fp.MainModule, apiSettingsBody: apiSettingsBody{
			CompatibilityDate:  fp.CompatibilityDate,
			CompatibilityFlags: append([]string{}, fp.CompatibilityFlags...),
			Bindings:           bindings,
			Observability:      obs,
			Logpush:            fp.Logpush,
		}},
	}
	d.contentHash = contentHash(fp.MainModule, mods)
	d.settingsHash = hashJSON(withoutSecretText(d.metadata.apiSettingsBody))
	d.secretsHash = secretsHash(secrets)
	return d, nil, nil
}

// validModuleName rejects names a multipart part cannot carry unambiguously.
func validModuleName(n string) bool {
	if n == "" || len(n) > 255 {
		return false
	}
	for _, c := range n {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// moduleType infers the type of a ConfigMap key from its extension.
func moduleType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".cjs":
		return workersv1alpha1.ModuleCommonJS
	case ".json":
		return workersv1alpha1.ModuleJSON
	case ".txt", ".html", ".htm", ".css", ".md", ".csv", ".svg":
		return workersv1alpha1.ModuleText
	case ".wasm":
		return workersv1alpha1.ModuleWasmBase64
	}
	return workersv1alpha1.ModuleESM
}

// moduleBytes decodes a module's content for upload (base64 for wasm-base64).
func moduleBytes(name, typ, content string) ([]byte, *problem) {
	if typ == workersv1alpha1.ModuleWasmBase64 {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
		if err != nil {
			return nil, invalid("module %s: content of a wasm-base64 module is not base64: %v", name, err)
		}
		return b, nil
	}
	return []byte(content), nil
}

// modules resolves inline modules or the ConfigMap of sourceRef, sorted by name.
func (r *Reconciler) modules(ctx context.Context, ws *workersv1alpha1.WorkerScript) ([]module, *problem, error) {
	fp := ws.Spec.ForProvider
	var out []module
	add := func(name, typ string, content []byte) *problem {
		if !validModuleName(name) {
			return invalid("module name %q is invalid", name)
		}
		if _, ok := moduleContentTypes[typ]; !ok {
			return invalid("module %s: unknown type %q (esm, cjs, text, json, wasm-base64)", name, typ)
		}
		out = append(out, module{Name: name, Type: typ, Content: content})
		return nil
	}
	switch {
	case fp.SourceRef != nil:
		var cm corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: fp.SourceRef.Name}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, dependency("ConfigMap %s (sourceRef) not found", fp.SourceRef.Name), nil
			}
			return nil, nil, err
		}
		for k, v := range cm.Data {
			typ := fp.SourceRef.Types[k]
			if typ == "" {
				typ = moduleType(k)
			}
			b, p := moduleBytes(k, typ, v)
			if p != nil {
				return nil, p, nil
			}
			if p := add(k, typ, b); p != nil {
				return nil, p, nil
			}
		}
		for k, v := range cm.BinaryData {
			// binaryData holds the raw bytes: uploaded as WebAssembly.
			if p := add(k, workersv1alpha1.ModuleWasmBase64, v); p != nil {
				return nil, p, nil
			}
		}
		for k := range fp.SourceRef.Types {
			if _, ok := cm.Data[k]; !ok {
				if _, ok := cm.BinaryData[k]; !ok {
					return nil, invalid("sourceRef.types names %q, which is not a key of ConfigMap %s", k, cm.Name), nil
				}
			}
		}
		if len(out) == 0 {
			return nil, invalid("ConfigMap %s has no data", cm.Name), nil
		}
	default:
		for name, m := range fp.Modules {
			typ := m.Type
			if typ == "" {
				typ = workersv1alpha1.ModuleESM
			}
			b, p := moduleBytes(name, typ, m.Content)
			if p != nil {
				return nil, p, nil
			}
			if p := add(name, typ, b); p != nil {
				return nil, p, nil
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil, nil
}

// refReady returns the referenced managed object's problem, if any: missing, being deleted,
// using another CloudflareAccount, or not Ready.
func refReady(ws *workersv1alpha1.WorkerScript, kind string, mg commonv1alpha1.Managed) *problem {
	switch {
	case !mg.GetDeletionTimestamp().IsZero():
		return dependency("%s %s is being deleted", kind, mg.GetName())
	case mg.GetResourceSpec().AccountRef.Name != ws.Spec.AccountRef.Name:
		return dependency("%s %s uses CloudflareAccount %s, not %s", kind, mg.GetName(), mg.GetResourceSpec().AccountRef.Name, ws.Spec.AccountRef.Name)
	case !reconcile.IsReady(mg):
		return dependency("%s %s is not Ready", kind, mg.GetName())
	}
	return nil
}

// getRef reads the referenced object; a missing one is a dependency problem.
func (r *Reconciler) getRef(ctx context.Context, ws *workersv1alpha1.WorkerScript, kind, name string, obj reconcile.ManagedObject) (*problem, error) {
	if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return dependency("%s %s not found", kind, name), nil
		}
		return nil, err
	}
	return refReady(ws, kind, obj), nil
}

// bindings resolves spec bindings to API bindings (in spec order) and collects the secret
// values (binding name → text).
func (r *Reconciler) bindings(ctx context.Context, ws *workersv1alpha1.WorkerScript) ([]map[string]any, map[string]string, *problem, error) {
	out := []map[string]any{}
	secrets := map[string]string{}
	for _, b := range ws.Bindings() {
		m := map[string]any{"name": b.Name, "type": b.Type}
		var p *problem
		var err error
		if b.JSON != nil && b.Type != workersv1alpha1.BindingJSON {
			return nil, nil, invalid("binding %s: json is only valid with type json", b.Name), nil
		}
		switch b.Type {
		case workersv1alpha1.BindingPlainText:
			m["text"] = deref(b.Text)
		case workersv1alpha1.BindingSecretText:
			var text string
			text, p, err = r.secretValue(ctx, ws, b)
			m["text"] = text
			secrets[b.Name] = text
		case workersv1alpha1.BindingJSON:
			var v any
			if b.JSON == nil || json.Unmarshal(b.JSON.Raw, &v) != nil {
				p = invalid("json is required and must be a JSON value")
			}
			m["json"] = v
		case workersv1alpha1.BindingKVNamespace:
			id := deref(b.NamespaceID)
			if b.KVNamespaceRef != nil {
				var kv kvv1alpha1.KVNamespace
				if p, err = r.getRef(ctx, ws, "KVNamespace", b.KVNamespaceRef.Name, &kv); p == nil && err == nil {
					id = kv.Status.ID
				}
			}
			m["namespace_id"] = id
		case workersv1alpha1.BindingQueue:
			qn := deref(b.QueueName)
			if b.QueueRef != nil {
				var q queuesv1alpha1.Queue
				if p, err = r.getRef(ctx, ws, "Queue", b.QueueRef.Name, &q); p == nil && err == nil {
					qn = deref(q.Status.AtProvider.QueueName)
					if qn == "" {
						p = dependency("Queue %s reports no queue_name yet", q.Name)
					}
				}
			}
			m["queue_name"] = qn
		case workersv1alpha1.BindingD1:
			id := deref(b.DatabaseID)
			if b.D1DatabaseRef != nil {
				var db d1v1alpha1.D1Database
				if p, err = r.getRef(ctx, ws, "D1Database", b.D1DatabaseRef.Name, &db); p == nil && err == nil {
					id = db.Status.ID
				}
			}
			// database_id (the spec's current name; "id" is deprecated). How GET …/settings
			// echoes a d1 binding is UNVERIFIED (not recorded).
			m["database_id"] = id
		case workersv1alpha1.BindingVPCService:
			id := deref(b.ServiceID)
			if b.VPCServiceRef != nil {
				var vs workersvpcv1alpha1.VPCService
				if p, err = r.getRef(ctx, ws, "VPCService", b.VPCServiceRef.Name, &vs); p == nil && err == nil {
					id = vs.Status.ID
				}
			}
			m["service_id"] = id // 0062: {type, name, service_id}
		case workersv1alpha1.BindingService:
			svc := deref(b.Service)
			if b.ServiceRef != nil {
				var target workersv1alpha1.WorkerScript
				if p, err = r.getRef(ctx, ws, "WorkerScript", b.ServiceRef.Name, &target); p == nil && err == nil {
					svc = target.ScriptName()
				}
			}
			m["service"] = svc
			if b.Environment != nil {
				m["environment"] = *b.Environment
			}
			if b.Entrypoint != nil {
				m["entrypoint"] = *b.Entrypoint
			}
		default:
			p = invalid("unsupported type %q", b.Type)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if p != nil {
			p.msg = "binding " + b.Name + ": " + p.msg
			return nil, nil, p, nil
		}
		for _, k := range []string{"namespace_id", "queue_name", "database_id", "service_id", "service"} {
			if v, ok := m[k]; ok && v == "" {
				return nil, nil, dependency("binding %s: the referenced object has no Cloudflare ID yet", b.Name), nil
			}
		}
		out = append(out, m)
	}
	return out, secrets, nil, nil
}

func (r *Reconciler) secretValue(ctx context.Context, ws *workersv1alpha1.WorkerScript, b workersv1alpha1.WorkerBinding) (string, *problem, error) {
	ref := b.SecretKeyRef
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", dependency("Secret %s not found", ref.Name), nil
		}
		return "", nil, err
	}
	v, ok := sec.Data[ref.Key]
	if !ok {
		return "", dependency("Secret %s has no key %s", ref.Name, ref.Key), nil
	}
	return string(v), nil, nil
}

// samplingRate converts a 0..1 int-or-string to a JSON number.
func samplingRate(v *intstr.IntOrString) (*json.Number, *problem) {
	if v == nil {
		return nil, nil
	}
	s := v.String()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 1 {
		return nil, invalid("head_sampling_rate %q must be a number from 0 to 1", s)
	}
	n := json.Number(s)
	return &n, nil
}

func observability(o *workersv1alpha1.WorkerObservability) (*apiObservability, *problem) {
	if o == nil {
		return nil, nil
	}
	rate, p := samplingRate(o.HeadSamplingRate)
	if p != nil {
		return nil, p
	}
	out := &apiObservability{Enabled: o.Enabled, HeadSamplingRate: rate}
	if o.Logs != nil {
		lr, p := samplingRate(o.Logs.HeadSamplingRate)
		if p != nil {
			return nil, p
		}
		out.Logs = &apiObsLogs{Enabled: o.Logs.Enabled, InvocationLogs: o.Logs.InvocationLogs, HeadSamplingRate: lr}
	}
	return out, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// contentHash hashes main_module and every module's name, type and bytes (sorted by name).
func contentHash(main string, mods []module) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00", main)
	for _, m := range mods {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", m.Name, m.Type, len(m.Content))
		h.Write(m.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v) // map keys sorted, struct fields in order: stable
	return sha(b)
}

// withoutSecretText returns s with the text of secret_text bindings removed.
func withoutSecretText(s apiSettingsBody) apiSettingsBody {
	out := s
	out.Bindings = make([]map[string]any, 0, len(s.Bindings))
	for _, b := range s.Bindings {
		if b["type"] == workersv1alpha1.BindingSecretText {
			c := map[string]any{}
			for k, v := range b {
				if k != "text" {
					c[k] = v
				}
			}
			b = c
		}
		out.Bindings = append(out.Bindings, b)
	}
	return out
}

// secretsHash hashes the secret values ("" when there are none).
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
