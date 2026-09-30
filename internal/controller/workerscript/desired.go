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
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/reconcile"
)

// desired is the resolved upload of a WorkerScript: modules, metadata with the IDs its
// references resolved to, and the hashes the reconciler compares with what it last applied.
type desired struct {
	modules  []module
	metadata apiUploadMetadata
	// assets is the static assets' manifest and config (nil without forProvider.assets).
	assets *assetPlan
	// contentHash covers main_module and the modules; settingsHash the settings with secret
	// values left out; secretsHash the secret values ("" without secret_text bindings);
	// assetsHash the asset manifest and config ("" without assets).
	contentHash, settingsHash, secretsHash, assetsHash string
	// artifacts are the loaded artifacts (nil without moduleSource and assets).
	artifacts *workersv1alpha1.WorkerArtifactsStatus
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
	var arts workersv1alpha1.WorkerArtifactsStatus
	mods, p, err := r.modules(ctx, ws, &arts)
	if p != nil || err != nil {
		return nil, p, err
	}
	if fp.MainModule != "" {
		found := false
		for _, m := range mods {
			found = found || m.Name == fp.MainModule
		}
		if !found {
			return nil, invalid("main_module %q is not one of the modules", fp.MainModule), nil
		}
	} else if fp.Assets == nil || len(mods) > 0 {
		return nil, invalid("main_module is required (unless the Worker is assets-only: assets and no modules)"), nil
	}
	var plan *assetPlan
	if fp.Assets != nil {
		tree, p, err := r.loadArtifact(ctx, ws, "assets.source", fp.Assets.Source)
		if p != nil || err != nil {
			return nil, p, err
		}
		arts.Assets = artifactStatus(tree)
		m, p := r.manifests.get(tree)
		if p != nil {
			return nil, p, nil
		}
		plan = newAssetPlan(m, fp.Assets.Config)
		arts.AssetFiles = int32(len(m.files))
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
		assets:  plan,
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
	if plan != nil {
		d.assetsHash = plan.hash
	}
	if arts.Modules != nil || arts.Assets != nil {
		d.artifacts = &arts
	}
	return d, nil, nil
}

// loader is the artifact loader (Deps.Artifacts), or one with the default limits.
func (r *Reconciler) loader() *artifact.Loader {
	r.defaultLoaderOnce.Do(func() {
		if r.Artifacts == nil {
			r.defaultLoader, r.defaultLoaderErr = artifact.NewLoader(artifact.Options{UserAgent: "flare-operator"})
		}
	})
	if r.Artifacts != nil {
		return r.Artifacts
	}
	return r.defaultLoader
}

// loadArtifact loads src. A missing or not opted-in ConfigMap or Secret is a dependency
// problem, an unusable source or refused content an invalid spec; fetch failures (registry,
// download) and Kubernetes errors are errors, retried with backoff.
func (r *Reconciler) loadArtifact(ctx context.Context, ws *workersv1alpha1.WorkerScript, what string, src sharedv1alpha1.ArtifactSource) (*artifact.Tree, *problem, error) {
	l := r.loader()
	if l == nil {
		return nil, nil, fmt.Errorf("%s: no artifact loader: %v", what, r.defaultLoaderErr)
	}
	t, err := l.Load(ctx, r.Client, ws.Namespace, src)
	if err == nil {
		return t, nil, nil
	}
	switch artifact.KindOf(err) {
	case artifact.KindDependency:
		return nil, dependency("%s: %v", what, err), nil
	case artifact.KindInvalid, artifact.KindRejected:
		return nil, invalid("%s: %v", what, err), nil
	}
	return nil, nil, fmt.Errorf("%s: %w", what, err)
}

func artifactStatus(t *artifact.Tree) *sharedv1alpha1.ArtifactStatus {
	return &sharedv1alpha1.ArtifactStatus{Digest: t.Digest, ResolvedDigest: t.ResolvedDigest, Files: int32(t.Len()), Bytes: t.Bytes}
}

// artifactModuleType infers the module type of a moduleSource file from its extension.
func artifactModuleType(p string) (string, bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return workersv1alpha1.ModuleESM, true
	case ".cjs":
		return workersv1alpha1.ModuleCommonJS, true
	case ".json":
		return workersv1alpha1.ModuleJSON, true
	case ".wasm":
		return workersv1alpha1.ModuleWasm, true
	case ".txt", ".html", ".htm", ".css", ".md", ".csv", ".svg":
		return workersv1alpha1.ModuleText, true
	}
	return "", false
}

// maxModules bounds the modules of a script (as forProvider.modules' MaxProperties).
const maxModules = 64

// validModuleName rejects names a multipart part cannot carry unambiguously. Invalid UTF-8 is
// rejected too: JSON encoding would turn it into U+FFFD in the metadata's main_module, which
// then no longer names the part (found by FuzzBuildMultipart).
func validModuleName(n string) bool {
	if n == "" || len(n) > 255 || !utf8.ValidString(n) {
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

// modules resolves inline modules, the ConfigMap of sourceRef or the artifact of moduleSource
// (reported in arts), sorted by name. An assets-only Worker has none.
func (r *Reconciler) modules(ctx context.Context, ws *workersv1alpha1.WorkerScript, arts *workersv1alpha1.WorkerArtifactsStatus) ([]module, *problem, error) {
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
	case fp.ModuleSource != nil:
		tree, p, err := r.loadArtifact(ctx, ws, "moduleSource", *fp.ModuleSource)
		if p != nil || err != nil {
			return nil, p, err
		}
		arts.Modules = artifactStatus(tree)
		if tree.Len() > maxModules {
			return nil, invalid("moduleSource has %d files; a script has at most %d modules", tree.Len(), maxModules), nil
		}
		for _, f := range tree.Files() {
			typ := fp.ModuleTypes[f.Path]
			if typ == "" {
				var known bool
				if typ, known = artifactModuleType(f.Path); !known {
					return nil, invalid("moduleSource file %s: its module type cannot be told from the extension; set moduleTypes[%q] or leave the file out", f.Path, f.Path), nil
				}
			}
			if p := add(f.Path, typ, f.Content); p != nil {
				return nil, p, nil
			}
		}
		for k := range fp.ModuleTypes {
			if _, ok := tree.File(k); !ok {
				return nil, invalid("moduleTypes names %q, which is not a file of moduleSource", k), nil
			}
		}
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
	case len(fp.Modules) == 0:
		return nil, nil, nil // an assets-only Worker
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
		case workersv1alpha1.BindingR2Bucket:
			// {name, type, bucket_name[, jurisdiction]}: the spec's workers_binding_kind_r2_bucket,
			// as wrangler sends it (create-worker-upload-form.ts#L294-L318 at workers-sdk@3bdcd0d).
			// TODO(FS-r2): resolve an r2BucketRef once the R2Bucket kind exists.
			m["bucket_name"] = deref(b.BucketName)
			if b.Jurisdiction != nil {
				m["jurisdiction"] = *b.Jurisdiction
			}
		case workersv1alpha1.BindingSendEmail:
			// The spec's workers_binding_kind_send_email; wrangler sends destination_address or
			// allowed_destination_addresses, never both, with allowed_sender_addresses
			// (wrangler@4.143.0:wrangler-dist/cli.js#L163135-L163153, #L24516-L24519). Whether
			// the API accepts it without Email Routing on a zone is UNVERIFIED.
			if b.DestinationAddress != nil {
				m["destination_address"] = *b.DestinationAddress
			}
			if len(b.AllowedDestinationAddresses) > 0 {
				m["allowed_destination_addresses"] = append([]string(nil), b.AllowedDestinationAddresses...)
			}
			if len(b.AllowedSenderAddresses) > 0 {
				m["allowed_sender_addresses"] = append([]string(nil), b.AllowedSenderAddresses...)
			}
		case workersv1alpha1.BindingAssets:
			// {name, type: assets} (create-worker-upload-form.ts#L669-L674).
			if ws.Spec.ForProvider.Assets == nil {
				p = invalid("an assets binding needs forProvider.assets")
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

// LabelWorkerBinding opts a Secret in to secret_text bindings: only a Secret labelled
// cloudflare.flare.dev/worker-binding=true can be read through a WorkerScript's secretKeyRef.
// The operator reads Secrets with its own, cluster-wide access, and the Worker's code (which the
// WorkerScript's author writes) can return a binding's value, so without the opt-in anyone
// allowed to create WorkerScripts could read every Secret of the namespace, the CloudflareAccount
// token included.
const LabelWorkerBinding = "cloudflare.flare.dev/worker-binding"

func (r *Reconciler) secretValue(ctx context.Context, ws *workersv1alpha1.WorkerScript, b workersv1alpha1.WorkerBinding) (string, *problem, error) {
	ref := b.SecretKeyRef
	// One message for a missing, a not-opted-in and a refused Secret, so a WorkerScript cannot
	// probe which Secrets exist.
	unusable := func() (string, *problem, error) {
		return "", dependency("Secret %s key %s is not usable: it must exist, carry the label %s=true, and not be a service account token",
			ref.Name, ref.Key, LabelWorkerBinding), nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return unusable()
		}
		return "", nil, err
	}
	v, ok := sec.Data[ref.Key]
	if !ok || sec.Labels[LabelWorkerBinding] != "true" || sec.Type == corev1.SecretTypeServiceAccountToken {
		return unusable()
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
