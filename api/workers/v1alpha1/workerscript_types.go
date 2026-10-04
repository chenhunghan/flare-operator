package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

// Module types of WorkerModule.Type and the upload part Content-Type each one is sent with.
const (
	// ModuleESM is an ES module (application/javascript+module; recordings 0036, 0062, 0183).
	ModuleESM = "esm"
	// ModuleCommonJS is a CommonJS module (application/javascript).
	ModuleCommonJS = "cjs"
	// ModuleText is a text module imported as a string (text/plain).
	ModuleText = "text"
	// ModuleJSON is a JSON module (application/json; UNVERIFIED: not in the pinned spec's list
	// of part content types).
	ModuleJSON = "json"
	// ModuleWasmBase64 is a WebAssembly module whose content is base64 (sent decoded, as
	// application/wasm).
	ModuleWasmBase64 = "wasm-base64"
	// ModuleWasm is a WebAssembly module given as its raw bytes (moduleSource files only; sent
	// as application/wasm).
	ModuleWasm = "wasm"
)

// Binding types supported by WorkerBinding.Type (the pinned spec's workers_binding_item).
const (
	BindingPlainText   = "plain_text"
	BindingSecretText  = "secret_text"
	BindingJSON        = "json"
	BindingKVNamespace = "kv_namespace"
	BindingQueue       = "queue"
	BindingD1          = "d1"
	BindingVPCService  = "vpc_service"
	BindingService     = "service"
	BindingR2Bucket    = "r2_bucket"
	BindingSendEmail   = "send_email"
	// BindingAssets gives the Worker's code a fetcher of its static assets (forProvider.assets).
	BindingAssets = "assets"
)

// WorkerModule is one module (one multipart part) of the script.
type WorkerModule struct {
	// Type of the module: esm (default), cjs, text, json or wasm-base64 (content is base64 and is
	// uploaded decoded as application/wasm).
	// +optional
	// +kubebuilder:default=esm
	// +kubebuilder:validation:Enum=esm;cjs;text;json;wasm-base64
	Type string `json:"type,omitempty"`
	// Content of the module.
	Content string `json:"content"`
}

// WorkerSourceRef takes the modules from a ConfigMap in the same namespace: every key of data is
// a module (keys of binaryData are uploaded as WebAssembly). The type of a data key comes from
// types, else from its extension: .cjs → cjs, .json → json, .txt/.html/.css/.md → text,
// .wasm → wasm-base64, anything else (.js, .mjs) → esm. A change of the ConfigMap is uploaded.
type WorkerSourceRef struct {
	// Name of the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Types overrides the module type of individual keys (esm, cjs, text, json, wasm-base64).
	// +optional
	Types map[string]string `json:"types,omitempty"`
}

// SecretKeyRef selects a key of a Secret in the same namespace.
type SecretKeyRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// WorkerBinding is one entry of the script's bindings (metadata.bindings of the upload). Fields
// named in snake_case are the API's; the camelCase *Ref fields name objects in this namespace
// whose Cloudflare ID (or queue name) the controller fills in. A reference to an object that
// is not Ready yet makes the WorkerScript Synced=False, reason DependencyNotReady, and nothing
// is uploaded until it is.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.kvNamespaceRef) || size(self.kvNamespaceRef.name) > 0) && (!has(self.queueRef) || size(self.queueRef.name) > 0) && (!has(self.d1DatabaseRef) || size(self.d1DatabaseRef.name) > 0) && (!has(self.vpcServiceRef) || size(self.vpcServiceRef.name) > 0) && (!has(self.serviceRef) || size(self.serviceRef.name) > 0) && (!has(self.r2BucketRef) || size(self.r2BucketRef.name) > 0)",message="a kvNamespaceRef, queueRef, d1DatabaseRef, vpcServiceRef, serviceRef or r2BucketRef needs a non-empty name"
// +kubebuilder:validation:XValidation:rule="self.type == 'plain_text' ? has(self.text) : !has(self.text)",message="text is required for (and only valid with) type plain_text"
// +kubebuilder:validation:XValidation:rule="self.type == 'secret_text' ? has(self.secretKeyRef) : !has(self.secretKeyRef)",message="secretKeyRef is required for (and only valid with) type secret_text"
// +kubebuilder:validation:XValidation:rule="self.type == 'kv_namespace' ? has(self.namespace_id) != has(self.kvNamespaceRef) : !has(self.namespace_id) && !has(self.kvNamespaceRef)",message="type kv_namespace needs exactly one of namespace_id or kvNamespaceRef (only valid with that type)"
// +kubebuilder:validation:XValidation:rule="self.type == 'queue' ? has(self.queue_name) != has(self.queueRef) : !has(self.queue_name) && !has(self.queueRef)",message="type queue needs exactly one of queue_name or queueRef (only valid with that type)"
// +kubebuilder:validation:XValidation:rule="self.type == 'd1' ? has(self.database_id) != has(self.d1DatabaseRef) : !has(self.database_id) && !has(self.d1DatabaseRef)",message="type d1 needs exactly one of database_id or d1DatabaseRef (only valid with that type)"
// +kubebuilder:validation:XValidation:rule="self.type == 'vpc_service' ? has(self.service_id) != has(self.vpcServiceRef) : !has(self.service_id) && !has(self.vpcServiceRef)",message="type vpc_service needs exactly one of service_id or vpcServiceRef (only valid with that type)"
// +kubebuilder:validation:XValidation:rule="self.type == 'service' ? has(self.service) != has(self.serviceRef) : !has(self.service) && !has(self.serviceRef) && !has(self.environment) && !has(self.entrypoint)",message="type service needs exactly one of service or serviceRef (only valid with that type, as are environment and entrypoint)"
// +kubebuilder:validation:XValidation:rule="self.type == 'r2_bucket' ? has(self.bucket_name) != has(self.r2BucketRef) : !has(self.bucket_name) && !has(self.r2BucketRef) && !has(self.jurisdiction)",message="type r2_bucket needs exactly one of bucket_name or r2BucketRef (only valid with that type, as is jurisdiction)"
// +kubebuilder:validation:XValidation:rule="!(has(self.r2BucketRef) && has(self.jurisdiction))",message="jurisdiction goes with bucket_name; an r2BucketRef binds the R2Bucket's own jurisdiction"
// +kubebuilder:validation:XValidation:rule="self.type == 'send_email' || (!has(self.destination_address) && !has(self.allowed_destination_addresses) && !has(self.allowed_sender_addresses))",message="destination_address, allowed_destination_addresses and allowed_sender_addresses are only valid with type send_email"
// +kubebuilder:validation:XValidation:rule="!(has(self.destination_address) && has(self.allowed_destination_addresses))",message="set destination_address or allowed_destination_addresses, not both"
type WorkerBinding struct {
	// Name is the JavaScript variable name of the binding (env.<name>).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// Type of the binding.
	// +kubebuilder:validation:Enum=plain_text;secret_text;json;kv_namespace;queue;d1;vpc_service;service;r2_bucket;send_email;assets
	Type string `json:"type"`

	// Text of a plain_text binding.
	// +optional
	Text *string `json:"text,omitempty"`
	// SecretKeyRef is the value of a secret_text binding. It is write-only in Cloudflare: a
	// change of the Secret's value is detected through status.writeOnlyHash. The Secret must
	// carry the label cloudflare.flare.dev/worker-binding=true (and not be a service account
	// token): the Worker's code can return the value, so only Secrets opted in for Workers are
	// read.
	// +optional
	SecretKeyRef *SecretKeyRef `json:"secretKeyRef,omitempty"`
	// JSON value of a json binding (required for, and only valid with, type json; checked by
	// the controller: CEL cannot see a schemaless field).
	// +optional
	JSON *apiextensionsv1.JSON `json:"json,omitempty"`

	// NamespaceID of a kv_namespace binding.
	// +optional
	NamespaceID *string `json:"namespace_id,omitempty"`
	// KVNamespaceRef names a KVNamespace whose status.id is the namespace_id.
	// +optional
	KVNamespaceRef *commonv1alpha1.LocalRef `json:"kvNamespaceRef,omitempty"`

	// QueueName of a queue binding (queues are bound by name).
	// +optional
	QueueName *string `json:"queue_name,omitempty"`
	// QueueRef names a Queue whose queue name (status.atProvider.queue_name) is bound.
	// +optional
	QueueRef *commonv1alpha1.LocalRef `json:"queueRef,omitempty"`

	// DatabaseID of a d1 binding.
	// +optional
	DatabaseID *string `json:"database_id,omitempty"`
	// D1DatabaseRef names a D1Database whose status.id is the database_id.
	// +optional
	D1DatabaseRef *commonv1alpha1.LocalRef `json:"d1DatabaseRef,omitempty"`

	// ServiceID of a vpc_service binding (a Workers VPC service).
	// +optional
	ServiceID *string `json:"service_id,omitempty"`
	// VPCServiceRef names a VPCService whose status.id is the service_id. The VPCService cannot
	// finish deleting while this binding exists (Cloudflare does not check, 0091).
	// +optional
	VPCServiceRef *commonv1alpha1.LocalRef `json:"vpcServiceRef,omitempty"`

	// Service is the Worker script name of a service binding.
	// +optional
	Service *string `json:"service,omitempty"`
	// ServiceRef names a WorkerScript whose script name is bound.
	// +optional
	ServiceRef *commonv1alpha1.LocalRef `json:"serviceRef,omitempty"`
	// Environment of the bound Worker (service bindings).
	// +optional
	Environment *string `json:"environment,omitempty"`
	// Entrypoint of the bound Worker to invoke (service bindings).
	// +optional
	Entrypoint *string `json:"entrypoint,omitempty"`

	// BucketName of an r2_bucket binding (the R2 bucket's name).
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	BucketName *string `json:"bucket_name,omitempty"`
	// Jurisdiction of the R2 bucket of an r2_bucket binding (a bucket made in a jurisdiction is
	// found only with it). Only with bucket_name.
	// +optional
	// +kubebuilder:validation:Enum=eu;fedramp;fedramp-high;us
	Jurisdiction *string `json:"jurisdiction,omitempty"`
	// R2BucketRef names an R2Bucket whose bucket name (status.id) and jurisdiction are bound.
	// The R2Bucket cannot finish deleting while this binding exists.
	// +optional
	R2BucketRef *commonv1alpha1.LocalRef `json:"r2BucketRef,omitempty"`

	// DestinationAddress restricts a send_email binding to this one destination address. A
	// send_email binding needs Email Routing on a zone of the account, with the destination
	// addresses verified there; the operator does not check that.
	// +optional
	// +kubebuilder:validation:MaxLength=320
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+$`
	DestinationAddress *string `json:"destination_address,omitempty"`
	// AllowedDestinationAddresses restricts a send_email binding to these destination addresses
	// (not together with destination_address).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=320
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@[^@\s]+$`
	AllowedDestinationAddresses []string `json:"allowed_destination_addresses,omitempty"`
	// AllowedSenderAddresses restricts the sender addresses of a send_email binding.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=320
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@[^@\s]+$`
	AllowedSenderAddresses []string `json:"allowed_sender_addresses,omitempty"`
}

// Values of WorkerAssetsConfig.HTMLHandling and NotFoundHandling (the pinned spec's
// workers_assets-2 config enums).
const (
	HTMLHandlingAutoTrailingSlash  = "auto-trailing-slash"
	HTMLHandlingForceTrailingSlash = "force-trailing-slash"
	HTMLHandlingDropTrailingSlash  = "drop-trailing-slash"
	HTMLHandlingNone               = "none"

	NotFoundHandlingNone    = "none"
	NotFoundHandling404Page = "404-page"
	NotFoundHandlingSPA     = "single-page-application"
)

// WorkerAssetsConfig is how Cloudflare serves the static assets (metadata.assets.config of the
// upload). Unset fields take Cloudflare's defaults.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.run_worker_first) && has(self.run_worker_first_paths))",message="set run_worker_first or run_worker_first_paths, not both"
type WorkerAssetsConfig struct {
	// HTMLHandling decides the redirects and rewrites of requests for HTML content (Cloudflare's
	// default: auto-trailing-slash).
	// +optional
	// +kubebuilder:validation:Enum=auto-trailing-slash;force-trailing-slash;drop-trailing-slash;none
	HTMLHandling string `json:"html_handling,omitempty"`
	// NotFoundHandling decides the answer to a request that matches no asset when the Worker
	// does not run (Cloudflare's default: none, a 404).
	// +optional
	// +kubebuilder:validation:Enum=none;404-page;single-page-application
	NotFoundHandling string `json:"not_found_handling,omitempty"`
	// RunWorkerFirst true runs the Worker's code before every request, even one that matches an
	// asset (the code can serve assets through an assets binding). Needs modules.
	// +optional
	RunWorkerFirst *bool `json:"run_worker_first,omitempty"`
	// RunWorkerFirstPaths runs the Worker's code first only for requests matching these rules
	// (sent as run_worker_first's list form): each starts with "/" or "!/" (a negative rule,
	// which wins), "*" is a glob, and at least one rule is not negative. Needs modules.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=1024
	// +kubebuilder:validation:items:Pattern=`^!?/`
	// +kubebuilder:validation:XValidation:rule="self.exists(r, !r.startsWith('!'))",message="run_worker_first_paths needs at least one rule that is not negative"
	RunWorkerFirstPaths []string `json:"run_worker_first_paths,omitempty"`
	// BasePath is the URL path prefix the assets are served under (Cloudflare's default: /).
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^/`
	BasePath string `json:"base_path,omitempty"`
}

// WorkerAssets are the Worker's static assets: the files of an artifact, uploaded with the
// Workers assets upload flow and served by Cloudflare in front of (or instead of) the Worker's
// code. As with wrangler, the root files _headers and _redirects become the custom headers and
// redirects rules, and a root .assetsignore (gitignore syntax, matched case-insensitively)
// excludes files; these three are not served. Files may be at most 25 MiB, and each is served
// with the Content-Type wrangler gives its extension.
type WorkerAssets struct {
	// Source of the files. A change of their content uploads the new and changed files only.
	Source sharedv1alpha1.ArtifactSource `json:"source"`
	// Config of how the assets are served.
	// +optional
	Config *WorkerAssetsConfig `json:"config,omitempty"`
}

// WorkerObservabilityLogs are the Workers Logs settings.
type WorkerObservabilityLogs struct {
	// Enabled turns Workers Logs on.
	Enabled bool `json:"enabled"`
	// InvocationLogs adds one log per invocation.
	// +optional
	InvocationLogs *bool `json:"invocation_logs,omitempty"`
	// HeadSamplingRate of logs, from 0 to 1 (a number, or a decimal string such as "0.1").
	// +optional
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? (self == 0 || self == 1) : self.matches('^(0(\\\\.[0-9]{1,6})?|1(\\\\.0{1,6})?)$')",message="must be between 0 and 1"
	HeadSamplingRate *intstr.IntOrString `json:"head_sampling_rate,omitempty"`
}

// WorkerObservability is the script's observability setting (metadata.observability).
type WorkerObservability struct {
	// Enabled turns observability on.
	Enabled bool `json:"enabled"`
	// HeadSamplingRate from 0 to 1 (a number, or a decimal string such as "0.1").
	// +optional
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? (self == 0 || self == 1) : self.matches('^(0(\\\\.[0-9]{1,6})?|1(\\\\.0{1,6})?)$')",message="must be between 0 and 1"
	HeadSamplingRate *intstr.IntOrString `json:"head_sampling_rate,omitempty"`
	// Logs settings.
	// +optional
	Logs *WorkerObservabilityLogs `json:"logs,omitempty"`
}

// WorkersDev is the script's workers.dev route (POST …/scripts/{name}/subdomain).
type WorkersDev struct {
	// Enabled serves the script at https://<script>.<account subdomain>.workers.dev.
	Enabled bool `json:"enabled"`
	// PreviewsEnabled serves preview URLs of versions (default false).
	// +optional
	PreviewsEnabled *bool `json:"previews_enabled,omitempty"`
}

// WorkerScriptParameters are the configurable fields of a WorkerScript: the upload
// (PUT /accounts/{account_id}/workers/scripts/{script_name}, multipart: a "metadata" part plus
// one part per module) and the per-script workers.dev route.
//
// +kubebuilder:validation:XValidation:rule="(has(self.modules) ? 1 : 0) + (has(self.sourceRef) ? 1 : 0) + (has(self.moduleSource) ? 1 : 0) == (has(self.assets) && !has(self.main_module) ? 0 : 1)",message="set exactly one of modules, sourceRef or moduleSource (none only for an assets-only Worker: assets without main_module)"
// +kubebuilder:validation:XValidation:rule="has(self.main_module) || has(self.assets)",message="main_module is required (unless the Worker is assets-only)"
// +kubebuilder:validation:XValidation:rule="!has(self.modules) || (has(self.main_module) && self.main_module in self.modules)",message="main_module must name one of modules"
// +kubebuilder:validation:XValidation:rule="!has(self.moduleTypes) || has(self.moduleSource)",message="moduleTypes is only valid with moduleSource"
// +kubebuilder:validation:XValidation:rule="has(self.main_module) || !has(self.bindings) || size(self.bindings) == 0",message="an assets-only Worker (no main_module) has no bindings"
// +kubebuilder:validation:XValidation:rule="has(self.main_module) || !has(self.assets) || !has(self.assets.config) || (!has(self.assets.config.run_worker_first) && !has(self.assets.config.run_worker_first_paths))",message="run_worker_first needs a Worker's code (main_module)"
// +kubebuilder:validation:XValidation:rule="!has(self.bindings) || self.bindings.filter(b, b.type == 'assets').size() <= (has(self.assets) ? 1 : 0)",message="an assets binding needs forProvider.assets, and there is at most one"
// +kubebuilder:validation:XValidation:rule="has(self.script_name) == has(oldSelf.script_name)",message="script_name cannot be added or removed"
type WorkerScriptParameters struct {
	// ScriptName is the Cloudflare script name. Defaults to metadata.name. Immutable.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9_][a-z0-9-_]*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="script_name is immutable"
	ScriptName string `json:"script_name,omitempty"`
	// Modules are the script's modules inline: module name (the part and file name, e.g.
	// index.js) → content. Exactly one of modules, sourceRef or moduleSource.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	Modules map[string]WorkerModule `json:"modules,omitempty"`
	// SourceRef takes the modules from a ConfigMap. Exactly one of modules, sourceRef or
	// moduleSource.
	// +optional
	SourceRef *WorkerSourceRef `json:"sourceRef,omitempty"`
	// ModuleSource takes the modules from an artifact (labelled ConfigMaps, an OCI image or an
	// HTTPS archive; docs/artifacts.md), for example a bundle built by CI. Every file is a
	// module named by its path; its type comes from moduleTypes, else from its extension: .js
	// and .mjs → esm, .cjs → cjs, .json → json, .wasm → wasm (raw bytes), .txt, .html, .htm,
	// .css, .md, .csv and .svg → text. A file of any other type is an error. At most 64
	// modules.
	// +optional
	ModuleSource *sharedv1alpha1.ArtifactSource `json:"moduleSource,omitempty"`
	// ModuleTypes overrides the module type of moduleSource files by path (esm, cjs, text, json,
	// wasm).
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, self[k] in ['esm', 'cjs', 'text', 'json', 'wasm'])",message="module types are esm, cjs, text, json or wasm"
	ModuleTypes map[string]string `json:"moduleTypes,omitempty"`
	// MainModule is the module that exports the handlers (metadata.main_module). Required
	// unless the Worker is assets-only (assets, and no modules).
	// +optional
	// +kubebuilder:validation:MinLength=1
	MainModule string `json:"main_module,omitempty"`
	// Assets are static assets served with (or, without main_module, instead of) the Worker's
	// code. An assets binding (type assets) lets the code fetch them.
	// +optional
	Assets *WorkerAssets `json:"assets,omitempty"`
	// CompatibilityDate of the Workers runtime, e.g. 2026-09-01.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
	CompatibilityDate string `json:"compatibility_date,omitempty"`
	// CompatibilityFlags of the Workers runtime, e.g. nodejs_compat.
	// +optional
	// +listType=set
	CompatibilityFlags []string `json:"compatibility_flags,omitempty"`
	// Bindings of the script (env.<name>).
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Bindings []WorkerBinding `json:"bindings,omitempty"`
	// Observability settings.
	// +optional
	Observability *WorkerObservability `json:"observability,omitempty"`
	// Logpush enables Workers Trace Events Logpush for the script.
	// +optional
	Logpush *bool `json:"logpush,omitempty"`
	// WorkersDev enables or disables the script's workers.dev route. Unset leaves it as it is.
	// +optional
	WorkersDev *WorkersDev `json:"workersDev,omitempty"`
}

// WorkerScriptSpec defines the desired state of a WorkerScript.
//
// +kubebuilder:validation:XValidation:rule="size(self.accountRef.name) > 0 && size(self.accountRef.name) <= 253",message="accountRef.name must name a CloudflareAccount in this namespace (1-253 characters)",fieldPath=".accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:validation:XValidation:rule="has(self.forProvider) || (has(self.managementPolicies) && size(self.managementPolicies) > 0 && !('*' in self.managementPolicies) && !('Create' in self.managementPolicies) && !('Update' in self.managementPolicies))",message="forProvider is required unless managementPolicies exclude Create and Update (e.g. [\"Observe\"])"
type WorkerScriptSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	// ForProvider is the script (optional for an observe-only object).
	// +optional
	ForProvider *WorkerScriptParameters `json:"forProvider,omitempty"`
}

// WorkerBindingObservation is a binding as Cloudflare reports it (name and type only; values
// are not copied into status).
type WorkerBindingObservation struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// WorkerSubdomainObservation is the script's workers.dev route.
type WorkerSubdomainObservation struct {
	Enabled         bool `json:"enabled"`
	PreviewsEnabled bool `json:"previews_enabled"`
}

// WorkerScriptObservation is the script as last read from Cloudflare: the upload response or
// the script list (tag, etag, handlers, created_on, modified_on), GET …/settings (bindings,
// compatibility, usage model, logpush), GET …/deployments (deployment and version) and
// GET …/subdomain.
type WorkerScriptObservation struct {
	// ID is the script name.
	// +optional
	ID string `json:"id,omitempty"`
	// Tag is the script's immutable Cloudflare identifier.
	// +optional
	Tag string `json:"tag,omitempty"`
	// Etag of the script content.
	// +optional
	Etag string `json:"etag,omitempty"`
	// Handlers the script exports (fetch, scheduled, ...).
	// +optional
	Handlers []string `json:"handlers,omitempty"`
	// HasAssets reports that the script has static assets.
	// +optional
	HasAssets bool `json:"has_assets,omitempty"`
	// +optional
	CreatedOn string `json:"created_on,omitempty"`
	// +optional
	ModifiedOn string `json:"modified_on,omitempty"`
	// +optional
	CompatibilityDate string `json:"compatibility_date,omitempty"`
	// +optional
	CompatibilityFlags []string `json:"compatibility_flags,omitempty"`
	// +optional
	UsageModel string `json:"usage_model,omitempty"`
	// +optional
	Logpush *bool `json:"logpush,omitempty"`
	// Bindings (names and types).
	// +optional
	Bindings []WorkerBindingObservation `json:"bindings,omitempty"`
	// DeploymentID is the active deployment (GET …/deployments; not the upload response's
	// deployment_id, which is the version ID without dashes).
	// +optional
	DeploymentID string `json:"deployment_id,omitempty"`
	// VersionID is the version the active deployment serves.
	// +optional
	VersionID string `json:"version_id,omitempty"`
	// Subdomain is the workers.dev route (read when forProvider.workersDev is set).
	// +optional
	Subdomain *WorkerSubdomainObservation `json:"subdomain,omitempty"`
	// URL is https://<script>.<account subdomain>.workers.dev while the route is enabled.
	// +optional
	URL string `json:"url,omitempty"`
}

// WorkerScriptStatus defines the observed state of a WorkerScript.
type WorkerScriptStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	// AtProvider is the script as last read from Cloudflare.
	// +optional
	AtProvider WorkerScriptObservation `json:"atProvider,omitempty"`
	// ContentHash is a hash of the modules and main_module last uploaded (Cloudflare does not
	// return the content, so a change is detected against this).
	// +optional
	ContentHash string `json:"contentHash,omitempty"`
	// SettingsHash is a hash of the settings last applied (compatibility date and flags,
	// bindings with the IDs they resolved to, observability, logpush). Secret values are
	// tracked in writeOnlyHash.
	// +optional
	SettingsHash string `json:"settingsHash,omitempty"`
	// AssetsHash is a hash of the asset manifest (every served path with the hash and size of
	// its file, as the upload session was sent it) and the assets config last uploaded. The
	// assets upload flow runs only when it changes.
	// +optional
	AssetsHash string `json:"assetsHash,omitempty"`
	// Artifacts are the artifacts last loaded for moduleSource and assets.source.
	// +optional
	Artifacts *WorkerArtifactsStatus `json:"artifacts,omitempty"`
}

// WorkerArtifactsStatus reports the loaded artifacts of a WorkerScript.
type WorkerArtifactsStatus struct {
	// Modules is the artifact of forProvider.moduleSource.
	// +optional
	Modules *sharedv1alpha1.ArtifactStatus `json:"modules,omitempty"`
	// Assets is the artifact of forProvider.assets.source.
	// +optional
	Assets *sharedv1alpha1.ArtifactStatus `json:"assets,omitempty"`
	// AssetFiles is the number of files in the asset manifest (the artifact's files without
	// _headers, _redirects, .assetsignore and the files it ignores).
	// +optional
	AssetFiles int32 `json:"assetFiles,omitempty"`
}

// WorkerScript is a Cloudflare Workers script (x-fern-sdk-group-name "workers.legacy.scripts").
//
// Uploaded to /accounts/{account_id}/workers/scripts/{script_name} (account scope). Every
// upload creates a new version and deploys it at 100%. Default deletion policy: Delete (a script
// is code, not data).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfworker;cfscript,categories={cloudflare,managed,workers}
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.id) && size(oldSelf.status.id) > 0) || self.spec.accountRef.name == oldSelf.spec.accountRef.name",message="spec.accountRef is immutable once the resource exists (status.id is set): the resource lives in that account. To move it, delete this object (deletionPolicy Orphan keeps the resource) and create a new one",fieldPath=".spec.accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id",description="The Cloudflare script name"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.atProvider.url"
// +kubebuilder:printcolumn:name="VERSION",type="string",JSONPath=".status.atProvider.version_id",priority=1
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type WorkerScript struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkerScriptSpec `json:"spec"`
	// +optional
	Status WorkerScriptStatus `json:"status,omitempty"`
}

// GetResourceSpec implements commonv1alpha1.Managed.
func (o *WorkerScript) GetResourceSpec() *commonv1alpha1.ResourceSpec { return &o.Spec.ResourceSpec }

// GetResourceStatus implements commonv1alpha1.Managed.
func (o *WorkerScript) GetResourceStatus() *commonv1alpha1.ResourceStatus {
	return &o.Status.ResourceStatus
}

// ScriptName returns the Cloudflare script name: the external-id annotation when set (it pins
// or adopts a script), else spec.forProvider.script_name, else metadata.name.
func (o *WorkerScript) ScriptName() string {
	if id := o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; id != "" {
		return id
	}
	if o.Spec.ForProvider != nil && o.Spec.ForProvider.ScriptName != "" {
		return o.Spec.ForProvider.ScriptName
	}
	return o.Name
}

// Bindings returns spec.forProvider.bindings (nil without forProvider).
func (o *WorkerScript) Bindings() []WorkerBinding {
	if o.Spec.ForProvider == nil {
		return nil
	}
	return o.Spec.ForProvider.Bindings
}

// WorkerScriptList is a list of WorkerScript.
//
// +kubebuilder:object:root=true
type WorkerScriptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkerScript `json:"items"`
}

var _ commonv1alpha1.Managed = &WorkerScript{}
