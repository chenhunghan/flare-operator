package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
)

// PagesDeploymentParameters are the configurable fields of a PagesDeployment: a Direct Upload
// of source to the project named by projectRef, on branch.
type PagesDeploymentParameters struct {
	// ProjectRef names the PagesProject (same namespace, same CloudflareAccount) to deploy to.
	// Immutable. Nothing is deployed until the project is Ready.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="projectRef is immutable"
	ProjectRef commonv1alpha1.LocalRef `json:"projectRef"`
	// Branch of the deployment. Empty, or the project's production branch, makes a production
	// deployment; any other branch a preview deployment (served at its branch alias). A change
	// deploys again.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Branch string `json:"branch,omitempty"`
	// Source of the site's files. Files at the root named _headers, _redirects and _routes.json
	// are sent as the deployment's routing files and _worker.js as its advanced-mode Worker (as
	// is: the operator does not bundle); a _worker.js directory and a functions directory are not
	// supported. Every other file is uploaded as an asset. A deployment is made when the
	// artifact's content digest or branch changes (status.deployedHash).
	// +optional
	Source *sharedv1alpha1.ArtifactSource `json:"source,omitempty"`
	// CommitHash recorded on the deployment (deployment_trigger.metadata.commit_hash). When unset
	// the operator records a 40-hex identifier derived from this object and the content, which
	// lets it find a deployment it made but could not record before a restart. A set commit hash
	// (a git commit, which other deployments may carry too) does not identify a deployment: one
	// lost that way is not looked up, and may be left in Cloudflare.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]+$`
	CommitHash string `json:"commit_hash,omitempty"`
	// CommitMessage recorded on the deployment (at most 384 bytes, as wrangler truncates it). A
	// change alone does not deploy again.
	// +optional
	// +kubebuilder:validation:MaxLength=384
	CommitMessage string `json:"commit_message,omitempty"`
}

// PagesDeploymentSpec defines the desired state of a PagesDeployment.
//
// +kubebuilder:validation:XValidation:rule="size(self.accountRef.name) > 0 && size(self.accountRef.name) <= 253",message="accountRef.name must name a CloudflareAccount in this namespace (1-253 characters)",fieldPath=".accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:validation:XValidation:rule="has(self.forProvider.source) || (has(self.managementPolicies) && size(self.managementPolicies) > 0 && !('*' in self.managementPolicies) && !('Create' in self.managementPolicies) && !('Update' in self.managementPolicies))",message="forProvider.source is required unless managementPolicies exclude Create and Update (e.g. [\"Observe\"])"
type PagesDeploymentSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	// ForProvider is the deployment.
	ForProvider PagesDeploymentParameters `json:"forProvider"`
}

// PagesStageObservation is a deployment stage (latest_stage).
type PagesStageObservation struct {
	// Name of the stage: queued, initialize, clone_repo, build or deploy.
	// +optional
	Name string `json:"name,omitempty"`
	// Status of the stage: success, idle, active, failure, canceled or skipped.
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	StartedOn string `json:"started_on,omitempty"`
	// +optional
	EndedOn string `json:"ended_on,omitempty"`
}

// PagesDeploymentObservation is the deployment as last read from Cloudflare
// (GET …/pages/projects/{project_name}/deployments/{deployment_id}).
type PagesDeploymentObservation struct {
	// ID of the deployment.
	// +optional
	ID string `json:"id,omitempty"`
	// ShortID is the 8-character short ID (the <short_id>.<project>.pages.dev host).
	// +optional
	ShortID string `json:"short_id,omitempty"`
	// ProjectName is the Cloudflare project the deployment belongs to.
	// +optional
	ProjectName string `json:"project_name,omitempty"`
	// Environment is production or preview.
	// +optional
	Environment string `json:"environment,omitempty"`
	// Branch of the deployment (deployment_trigger.metadata.branch).
	// +optional
	Branch string `json:"branch,omitempty"`
	// CommitHash of the deployment (deployment_trigger.metadata.commit_hash).
	// +optional
	CommitHash string `json:"commit_hash,omitempty"`
	// URL is the deployment's own URL.
	// +optional
	URL string `json:"url,omitempty"`
	// Aliases are the alias URLs pointing to this deployment (a preview branch alias).
	// +optional
	Aliases []string `json:"aliases,omitempty"`
	// LatestStage is the deployment's current stage.
	// +optional
	LatestStage *PagesStageObservation `json:"latest_stage,omitempty"`
	// Production reports that this is the project's live production deployment (its
	// canonical_deployment). Cloudflare refuses to delete it.
	// +optional
	Production bool `json:"production,omitempty"`
	// +optional
	CreatedOn string `json:"created_on,omitempty"`
	// +optional
	ModifiedOn string `json:"modified_on,omitempty"`
}

// PagesDeploymentStatus defines the observed state of a PagesDeployment.
type PagesDeploymentStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	// AtProvider is the deployment as last read from Cloudflare.
	// +optional
	AtProvider PagesDeploymentObservation `json:"atProvider,omitempty"`
	// Artifact is the content last deployed.
	// +optional
	Artifact *sharedv1alpha1.ArtifactStatus `json:"artifact,omitempty"`
	// DeployedHash is a hash of the artifact digest and branch last deployed: a deployment is
	// made when it changes.
	// +optional
	DeployedHash string `json:"deployedHash,omitempty"`
}

// PagesDeployment is one Cloudflare Pages Direct Upload deployment (x-fern-sdk-group-name
// "pages.deployments"): the files of an artifact uploaded to a PagesProject. A change of the
// content or branch makes a new deployment (the previous ones stay in the project's history,
// for rollbacks in the dashboard); status.id is always the newest one this object made.
//
// Deleting it deletes its deployment in Cloudflare (default deletion policy Delete), except the
// project's live production deployment, which Cloudflare refuses to delete: that one is kept,
// with a Warning event ExternalResourceKept.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfpagesdeploy;cfpd,categories={cloudflare,managed,pages}
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.id) && size(oldSelf.status.id) > 0) || self.spec.accountRef.name == oldSelf.spec.accountRef.name",message="spec.accountRef is immutable once the resource exists (status.id is set): the resource lives in that account. To move it, delete this object (deletionPolicy Orphan keeps the resource) and create a new one",fieldPath=".spec.accountRef.name",reason="FieldValueInvalid"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id",description="The Cloudflare deployment ID"
// +kubebuilder:printcolumn:name="PROJECT",type="string",JSONPath=".status.atProvider.project_name"
// +kubebuilder:printcolumn:name="ENV",type="string",JSONPath=".status.atProvider.environment"
// +kubebuilder:printcolumn:name="STAGE",type="string",JSONPath=".status.atProvider.latest_stage.status"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.atProvider.url",priority=1
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type PagesDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PagesDeploymentSpec `json:"spec"`
	// +optional
	Status PagesDeploymentStatus `json:"status,omitempty"`
}

// GetResourceSpec implements commonv1alpha1.Managed.
func (o *PagesDeployment) GetResourceSpec() *commonv1alpha1.ResourceSpec {
	return &o.Spec.ResourceSpec
}

// GetResourceStatus implements commonv1alpha1.Managed.
func (o *PagesDeployment) GetResourceStatus() *commonv1alpha1.ResourceStatus {
	return &o.Status.ResourceStatus
}

// PagesDeploymentList is a list of PagesDeployment.
//
// +kubebuilder:object:root=true
type PagesDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PagesDeployment `json:"items"`
}

var _ commonv1alpha1.Managed = &PagesDeployment{}
