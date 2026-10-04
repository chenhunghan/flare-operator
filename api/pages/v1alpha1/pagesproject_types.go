package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
)

// Environment variable types (the pinned spec's pages_env_vars discriminator).
const (
	EnvVarPlainText  = "plain_text"
	EnvVarSecretText = "secret_text"
)

// LabelSecretOptIn opts a Secret in to being read for a secret_text environment variable. It is
// the label WorkerScript secret_text bindings use (flare.dev/worker-binding=true):
// the Pages Functions code can return the value, so only Secrets opted in for Workers code are
// read.
const LabelSecretOptIn = "flare.dev/worker-binding"

// PagesSecretKeyRef selects a key of a Secret in the same namespace.
type PagesSecretKeyRef struct {
	// Name of the Secret. It must carry the label flare.dev/worker-binding=true and
	// not be a service account token.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key in the Secret's data.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// PagesEnvVar is one environment variable of builds and Pages Functions (env_vars).
//
// +kubebuilder:validation:XValidation:rule="self.type == 'plain_text' ? (has(self.value) && !has(self.secretKeyRef)) : (has(self.secretKeyRef) && !has(self.value))",message="a plain_text variable needs value, a secret_text variable needs secretKeyRef (and not value)"
type PagesEnvVar struct {
	// Name of the variable (env.<name>).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// Type is plain_text (default) or secret_text. A secret_text value is write-only in
	// Cloudflare: a change of the Secret's value is detected through status.writeOnlyHash.
	// +optional
	// +kubebuilder:default=plain_text
	// +kubebuilder:validation:Enum=plain_text;secret_text
	Type string `json:"type,omitempty"`
	// Value of a plain_text variable.
	// +optional
	// +kubebuilder:validation:MaxLength=5120
	Value *string `json:"value,omitempty"`
	// SecretKeyRef is the value of a secret_text variable.
	// +optional
	SecretKeyRef *PagesSecretKeyRef `json:"secretKeyRef,omitempty"`
}

// PagesKVBinding binds a KV namespace (kv_namespaces).
//
// +kubebuilder:validation:XValidation:rule="has(self.namespace_id) != has(self.kvNamespaceRef)",message="set exactly one of namespace_id or kvNamespaceRef"
type PagesKVBinding struct {
	// Name of the binding (env.<name>).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// NamespaceID of the KV namespace.
	// +optional
	NamespaceID *string `json:"namespace_id,omitempty"`
	// KVNamespaceRef names a KVNamespace in this namespace whose status.id is bound.
	// +optional
	KVNamespaceRef *commonv1alpha1.LocalRef `json:"kvNamespaceRef,omitempty"`
}

// PagesD1Binding binds a D1 database (d1_databases).
//
// +kubebuilder:validation:XValidation:rule="has(self.id) != has(self.d1DatabaseRef)",message="set exactly one of id or d1DatabaseRef"
type PagesD1Binding struct {
	// Name of the binding.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// ID (UUID) of the D1 database.
	// +optional
	ID *string `json:"id,omitempty"`
	// D1DatabaseRef names a D1Database in this namespace whose status.id is bound.
	// +optional
	D1DatabaseRef *commonv1alpha1.LocalRef `json:"d1DatabaseRef,omitempty"`
}

// PagesR2Binding binds an R2 bucket (r2_buckets).
//
// +kubebuilder:validation:XValidation:rule="has(self.bucket_name) != has(self.r2BucketRef)",message="set exactly one of bucket_name or r2BucketRef"
// +kubebuilder:validation:XValidation:rule="!(has(self.r2BucketRef) && has(self.jurisdiction))",message="jurisdiction goes with bucket_name; an r2BucketRef binds the R2Bucket's own jurisdiction"
type PagesR2Binding struct {
	// Name of the binding.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// BucketName is the R2 bucket (the API's r2_buckets.<binding>.name).
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	BucketName *string `json:"bucket_name,omitempty"`
	// Jurisdiction of the bucket (e.g. eu), when it has one. Only with bucket_name.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Jurisdiction *string `json:"jurisdiction,omitempty"`
	// R2BucketRef names an R2Bucket in this namespace whose bucket name (status.id) and
	// jurisdiction are bound. The R2Bucket cannot finish deleting while this binding exists.
	// +optional
	R2BucketRef *commonv1alpha1.LocalRef `json:"r2BucketRef,omitempty"`
}

// PagesQueueBinding binds a queue producer (queue_producers).
//
// +kubebuilder:validation:XValidation:rule="has(self.queue_name) != has(self.queueRef)",message="set exactly one of queue_name or queueRef"
type PagesQueueBinding struct {
	// Name of the binding.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// QueueName of the queue (the API's queue_producers.<binding>.name).
	// +optional
	QueueName *string `json:"queue_name,omitempty"`
	// QueueRef names a Queue in this namespace whose queue name (status.atProvider.queue_name) is
	// bound.
	// +optional
	QueueRef *commonv1alpha1.LocalRef `json:"queueRef,omitempty"`
}

// PagesServiceBinding binds a Worker (services).
//
// +kubebuilder:validation:XValidation:rule="has(self.service) != has(self.serviceRef)",message="set exactly one of service or serviceRef"
type PagesServiceBinding struct {
	// Name of the binding.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name"`
	// Service is the Worker script name.
	// +optional
	Service *string `json:"service,omitempty"`
	// ServiceRef names a WorkerScript in this namespace whose script name is bound.
	// +optional
	ServiceRef *commonv1alpha1.LocalRef `json:"serviceRef,omitempty"`
	// Environment of the bound Worker.
	// +optional
	Environment *string `json:"environment,omitempty"`
	// Entrypoint of the bound Worker to invoke.
	// +optional
	Entrypoint *string `json:"entrypoint,omitempty"`
}

// PagesPlacement is the Smart Placement setting of Pages Functions.
type PagesPlacement struct {
	// Mode of placement (e.g. smart).
	// +kubebuilder:validation:MinLength=1
	Mode string `json:"mode"`
}

// PagesDeploymentConfig is the configuration of one environment (production or preview) of
// a project: what builds and Pages Functions see. Once set, it is authoritative for its
// environment variables, bindings and compatibility flags: those that Cloudflare has and this
// config lacks are removed. Unset scalar fields keep Cloudflare's value.
type PagesDeploymentConfig struct {
	// CompatibilityDate of Pages Functions (e.g. 2026-09-01).
	// +optional
	// +kubebuilder:validation:MaxLength=32
	CompatibilityDate string `json:"compatibility_date,omitempty"`
	// CompatibilityFlags of Pages Functions (e.g. nodejs_compat); empty removes them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	CompatibilityFlags []string `json:"compatibility_flags,omitempty"`
	// AlwaysUseLatestCompatibilityDate for Pages Functions.
	// +optional
	AlwaysUseLatestCompatibilityDate *bool `json:"always_use_latest_compatibility_date,omitempty"`
	// FailOpen serves the site when the deployment config cannot be applied.
	// +optional
	FailOpen *bool `json:"fail_open,omitempty"`
	// BuildImageMajorVersion of the Pages build image.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99
	BuildImageMajorVersion *int32 `json:"build_image_major_version,omitempty"`
	// Placement of Pages Functions.
	// +optional
	Placement *PagesPlacement `json:"placement,omitempty"`
	// EnvVars are the environment variables.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=128
	EnvVars []PagesEnvVar `json:"env_vars,omitempty"`
	// KVNamespaces bound to Pages Functions.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	KVNamespaces []PagesKVBinding `json:"kv_namespaces,omitempty"`
	// D1Databases bound to Pages Functions.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	D1Databases []PagesD1Binding `json:"d1_databases,omitempty"`
	// R2Buckets bound to Pages Functions (by bucket name or r2BucketRef).
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	R2Buckets []PagesR2Binding `json:"r2_buckets,omitempty"`
	// QueueProducers bound to Pages Functions.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	QueueProducers []PagesQueueBinding `json:"queue_producers,omitempty"`
	// Services (Workers) bound to Pages Functions.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Services []PagesServiceBinding `json:"services,omitempty"`
}

// PagesDeploymentConfigs are the production and preview configurations. An unset one is left
// as Cloudflare has it.
type PagesDeploymentConfigs struct {
	// Production is the config of production deployments (the production branch).
	// +optional
	Production *PagesDeploymentConfig `json:"production,omitempty"`
	// Preview is the config of preview deployments (every other branch).
	// +optional
	Preview *PagesDeploymentConfig `json:"preview,omitempty"`
}

// PagesBuildConfig is the build configuration (used by Git-connected builds). Unset fields keep
// Cloudflare's value. The write-only web_analytics_token is not managed.
type PagesBuildConfig struct {
	// BuildCaching enables the build cache.
	// +optional
	BuildCaching *bool `json:"build_caching,omitempty"`
	// BuildCommand builds the project (e.g. npm run build).
	// +optional
	BuildCommand *string `json:"build_command,omitempty"`
	// DestinationDir is the build's output directory.
	// +optional
	DestinationDir *string `json:"destination_dir,omitempty"`
	// RootDir is the directory the build runs in.
	// +optional
	RootDir *string `json:"root_dir,omitempty"`
	// WebAnalyticsTag is the Web Analytics site tag.
	// +optional
	WebAnalyticsTag *string `json:"web_analytics_tag,omitempty"`
}

// PagesSourceConfig is the Git repository configuration of a Git-connected project. The
// operator passes it through to Cloudflare; connecting the repository (the GitHub or GitLab
// app authorization) is done in the Cloudflare dashboard.
type PagesSourceConfig struct {
	// Owner of the repository.
	// +optional
	Owner string `json:"owner,omitempty"`
	// OwnerID of the repository owner.
	// +optional
	OwnerID string `json:"owner_id,omitempty"`
	// RepoName of the repository.
	// +optional
	RepoName string `json:"repo_name,omitempty"`
	// RepoID of the repository.
	// +optional
	RepoID string `json:"repo_id,omitempty"`
	// ProductionBranch of the repository.
	// +optional
	ProductionBranch string `json:"production_branch,omitempty"`
	// PRCommentsEnabled posts deployment comments on pull requests.
	// +optional
	PRCommentsEnabled *bool `json:"pr_comments_enabled,omitempty"`
	// ProductionDeploymentsEnabled deploys commits to the production branch.
	// +optional
	ProductionDeploymentsEnabled *bool `json:"production_deployments_enabled,omitempty"`
	// PreviewDeploymentSetting: all, none or custom (with the branch includes/excludes).
	// +optional
	// +kubebuilder:validation:Enum=all;none;custom
	PreviewDeploymentSetting string `json:"preview_deployment_setting,omitempty"`
	// PreviewBranchIncludes trigger preview deployments (wildcards allowed).
	// +optional
	// +listType=atomic
	PreviewBranchIncludes []string `json:"preview_branch_includes,omitempty"`
	// PreviewBranchExcludes never trigger preview deployments.
	// +optional
	// +listType=atomic
	PreviewBranchExcludes []string `json:"preview_branch_excludes,omitempty"`
	// PathIncludes trigger preview deployments.
	// +optional
	// +listType=atomic
	PathIncludes []string `json:"path_includes,omitempty"`
	// PathExcludes never trigger preview deployments.
	// +optional
	// +listType=atomic
	PathExcludes []string `json:"path_excludes,omitempty"`
}

// PagesSource connects a project to a Git repository (pass-through; see PagesSourceConfig).
type PagesSource struct {
	// Type of the Git provider.
	// +kubebuilder:validation:Enum=github;gitlab
	Type string `json:"type"`
	// Config of the repository.
	Config PagesSourceConfig `json:"config"`
}

// PagesProjectParameters are the configurable fields of a PagesProject
// (POST /accounts/{account_id}/pages/projects, PATCH …/{project_name}).
//
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed"
type PagesProjectParameters struct {
	// Name of the project, its Cloudflare ID and the <name>.pages.dev subdomain. Defaults to
	// metadata.name. Immutable.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=58
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name,omitempty"`
	// ProductionBranch identifies production deployments: a deployment to this branch (or to no
	// branch) is a production deployment, every other branch a preview.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ProductionBranch string `json:"production_branch"`
	// BuildConfig of Git-connected builds.
	// +optional
	BuildConfig *PagesBuildConfig `json:"build_config,omitempty"`
	// DeploymentConfigs of production and preview deployments: environment variables, bindings,
	// compatibility settings.
	// +optional
	DeploymentConfigs *PagesDeploymentConfigs `json:"deployment_configs,omitempty"`
	// Source connects the project to a GitHub or GitLab repository (pass-through: the
	// repository must already be authorized for Cloudflare Pages). Without it the project is a
	// Direct Upload project that PagesDeployments deploy to.
	// +optional
	Source *PagesSource `json:"source,omitempty"`
}

// PagesProjectSpec defines the desired state of a PagesProject.
//
// +kubebuilder:validation:XValidation:rule="size(self.accountRef.name) > 0 && size(self.accountRef.name) <= 253",message="accountRef.name must name a CloudflareAccount in this namespace (1-253 characters)",fieldPath=".accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:validation:XValidation:rule="has(self.forProvider) || (has(self.managementPolicies) && size(self.managementPolicies) > 0 && !('*' in self.managementPolicies) && !('Create' in self.managementPolicies) && !('Update' in self.managementPolicies))",message="forProvider is required unless managementPolicies exclude Create and Update (e.g. [\"Observe\"])"
type PagesProjectSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	// ForProvider is the project (optional for an observe-only object).
	// +optional
	ForProvider *PagesProjectParameters `json:"forProvider,omitempty"`
}

// PagesProjectObservation is the project as last read from Cloudflare
// (GET /accounts/{account_id}/pages/projects/{project_name}). Environment variable values
// and binding IDs are not copied into status.
type PagesProjectObservation struct {
	// ID is the project's UUID (the external ID is its name).
	// +optional
	ID string `json:"id,omitempty"`
	// Name of the project.
	// +optional
	Name string `json:"name,omitempty"`
	// Subdomain is <name>.pages.dev (or a variant Cloudflare chose).
	// +optional
	Subdomain string `json:"subdomain,omitempty"`
	// URL is https://<subdomain>.
	// +optional
	URL string `json:"url,omitempty"`
	// Domains are the project's custom domains.
	// +optional
	Domains []string `json:"domains,omitempty"`
	// +optional
	ProductionBranch string `json:"production_branch,omitempty"`
	// +optional
	CreatedOn string `json:"created_on,omitempty"`
	// SourceType is github or gitlab for a Git-connected project, empty for Direct Upload.
	// +optional
	SourceType string `json:"source_type,omitempty"`
	// LatestDeploymentID is the newest deployment (any environment).
	// +optional
	LatestDeploymentID string `json:"latest_deployment_id,omitempty"`
	// CanonicalDeploymentID is the live production deployment.
	// +optional
	CanonicalDeploymentID string `json:"canonical_deployment_id,omitempty"`
	// ProductionEnvVars and PreviewEnvVars are the names of the environment variables (values
	// are not copied).
	// +optional
	ProductionEnvVars []string `json:"production_env_vars,omitempty"`
	// +optional
	PreviewEnvVars []string `json:"preview_env_vars,omitempty"`
}

// PagesProjectStatus defines the observed state of a PagesProject.
type PagesProjectStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	// AtProvider is the project as last read from Cloudflare.
	// +optional
	AtProvider PagesProjectObservation `json:"atProvider,omitempty"`
	// SettingsHash is a hash of the settings last applied (with the IDs references resolved to;
	// secret values are tracked in writeOnlyHash).
	// +optional
	SettingsHash string `json:"settingsHash,omitempty"`
}

// PagesProject is a Cloudflare Pages project (x-fern-sdk-group-name "pages").
//
// Created with POST /accounts/{account_id}/pages/projects; its name is the external ID.
// Default deletion policy: Delete (a project is configuration and deployed code; the site's
// content lives in its source). Deleting it waits for this namespace's PagesDeployments of it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfpages;cfpp,categories={cloudflare,managed,pages}
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.id) && size(oldSelf.status.id) > 0) || self.spec.accountRef.name == oldSelf.spec.accountRef.name",message="spec.accountRef is immutable once the resource exists (status.id is set): the resource lives in that account. To move it, delete this object (deletionPolicy Orphan keeps the resource) and create a new one",fieldPath=".spec.accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id",description="The Cloudflare project name"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.atProvider.url"
// +kubebuilder:printcolumn:name="BRANCH",type="string",JSONPath=".status.atProvider.production_branch"
// +kubebuilder:printcolumn:name="LIVE",type="string",JSONPath=".status.atProvider.canonical_deployment_id",priority=1,description="The live production deployment"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type PagesProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PagesProjectSpec `json:"spec"`
	// +optional
	Status PagesProjectStatus `json:"status,omitempty"`
}

// GetResourceSpec implements commonv1alpha1.Managed.
func (o *PagesProject) GetResourceSpec() *commonv1alpha1.ResourceSpec { return &o.Spec.ResourceSpec }

// GetResourceStatus implements commonv1alpha1.Managed.
func (o *PagesProject) GetResourceStatus() *commonv1alpha1.ResourceStatus {
	return &o.Status.ResourceStatus
}

// ProjectName returns the Cloudflare project name: the external-id annotation when set (it pins
// or adopts a project), else spec.forProvider.name, else metadata.name.
func (o *PagesProject) ProjectName() string {
	if id := o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; id != "" {
		return id
	}
	if o.Spec.ForProvider != nil && o.Spec.ForProvider.Name != "" {
		return o.Spec.ForProvider.Name
	}
	return o.Name
}

// PagesProjectList is a list of PagesProject.
//
// +kubebuilder:object:root=true
type PagesProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PagesProject `json:"items"`
}

var _ commonv1alpha1.Managed = &PagesProject{}
