// Package v1alpha1 holds API types that several flare-operator kinds embed but that belong to
// no API group of their own: ArtifactSource, the deployable content (Worker modules, static
// assets, Pages sites) of a kind, read by internal/artifact.
//
// The package has no +groupName and no root types, so it produces no CRD; `make generate`
// (controller-gen) writes its deepcopy methods and every CRD that embeds a type gets its schema
// and CEL rules. api/common/v1alpha1 is a frozen contract, which is why these types live here.
//
// +kubebuilder:object:generate=true
package v1alpha1

// LabelArtifact opts a ConfigMap (configMapRef) or an image pull Secret (ociRef.pullSecretRef)
// in to being read as an artifact source: the operator reads them with its own cluster-wide
// access and uploads their content to Cloudflare, where the page or Worker built from it can
// serve it, so without the opt-in anyone allowed to create a kind with an ArtifactSource could
// publish any ConfigMap of the namespace or borrow any registry credential in it. Same pattern
// as cloudflare.flare.dev/worker-binding for secret_text bindings.
const LabelArtifact = "cloudflare.flare.dev/artifact"

// ArtifactSource says where the files of a deployable artifact come from. Exactly one of
// configMapRef, ociRef or url is set. The loader (internal/artifact) turns it into an in-memory
// file tree (path → bytes) with a content digest, within the size and file-count limits of the
// manager flags --artifact-max-*.
//
// +kubebuilder:validation:XValidation:rule="(has(self.configMapRef) ? 1 : 0) + (has(self.ociRef) ? 1 : 0) + (has(self.url) ? 1 : 0) == 1",message="set exactly one of configMapRef, ociRef or url"
type ArtifactSource struct {
	// ConfigMapRef takes the files from one or more ConfigMaps in the object's namespace. Each
	// ConfigMap must carry the label cloudflare.flare.dev/artifact=true. A ConfigMap holds at
	// most 1 MiB (data and binaryData together), so larger artifacts belong in ociRef or url.
	// +optional
	ConfigMapRef *ConfigMapArtifactSource `json:"configMapRef,omitempty"`
	// OCIRef takes the files from the layers of an OCI image (e.g. one built FROM scratch with
	// the site's files). Pin it by digest: a tag is resolved again on every sync, and the digest
	// it resolved to is reported in status.
	// +optional
	OCIRef *OCIArtifactSource `json:"ociRef,omitempty"`
	// URL downloads a tar, tar.gz or zip archive over HTTPS and checks its SHA-256.
	// +optional
	URL *URLArtifactSource `json:"url,omitempty"`
}

// ConfigMapArtifactSource lists the ConfigMaps an artifact is assembled from. Their files are
// merged; two files at the same path are an error.
type ConfigMapArtifactSource struct {
	// ConfigMaps in the object's namespace, each labelled cloudflare.flare.dev/artifact=true.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	ConfigMaps []ConfigMapArtifact `json:"configMaps"`
}

// ConfigMapArtifact is one ConfigMap of an artifact. Every key of data and binaryData is a file
// (text keys as their UTF-8 bytes, binaryData keys as their bytes). ConfigMap keys cannot hold
// "/", so path places the keys in a directory and items maps single keys to nested paths.
type ConfigMapArtifact struct {
	// Name of the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Path is the directory, relative to the artifact root, that this ConfigMap's files are
	// placed in (default: the root). Slash-separated; no "." or ".." segments, no leading "/".
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+)(/([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+))*$`
	Path string `json:"path,omitempty"`
	// Items selects keys and gives each its own path (relative to path). When set, only the
	// listed keys are used and each must exist; when empty, every key is a file named after it.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Items []ArtifactKeyToPath `json:"items,omitempty"`
}

// ArtifactKeyToPath maps one ConfigMap key to a file path.
type ArtifactKeyToPath struct {
	// Key of the ConfigMap (data or binaryData).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
	// Path of the file, relative to the ConfigMap's path. Slash-separated; no "." or ".."
	// segments, no leading "/".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+)(/([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+))*$`
	Path string `json:"path"`
}

// OCIArtifactSource is an OCI image whose layers hold the artifact's files. The layers are
// applied in order (with OCI whiteouts), as a container runtime would unpack them; only regular
// files are kept. Symbolic links resolve within path; device files are rejected.
type OCIArtifactSource struct {
	// Image is the reference: registry/repository[:tag][@sha256:<digest>]. Pinning by digest is
	// recommended: the content then cannot change under the object, and a cached pull needs one
	// manifest request per sync. The registry is always contacted over HTTPS.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Image string `json:"image"`
	// Path is the directory of the image's filesystem that becomes the artifact root (default:
	// the image root). Slash-separated; no "." or ".." segments, no leading "/".
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+)(/([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+))*$`
	Path string `json:"path,omitempty"`
	// PullSecretRef names a kubernetes.io/dockerconfigjson Secret in the object's namespace
	// with the registry credentials. It must carry the label cloudflare.flare.dev/artifact=true.
	// Without it the image is pulled anonymously.
	// +optional
	PullSecretRef *ArtifactSecretRef `json:"pullSecretRef,omitempty"`
}

// ArtifactSecretRef names a Secret in the object's namespace.
type ArtifactSecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// URLArtifactSource is an archive downloaded over HTTPS. Redirects are followed only to other
// HTTPS URLs, and addresses in private, loopback, link-local (cloud metadata) and other
// non-public ranges are refused unless the manager's --artifact-allowed-cidr lists them. The
// archive format (tar, gzip-compressed tar, zip) is detected from its content.
type URLArtifactSource struct {
	// URL of the archive (https only).
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	URL string `json:"url"`
	// SHA256 of the archive, lowercase hex. The download is rejected unless it matches.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
	// Path is the directory inside the archive that becomes the artifact root (default: the
	// archive root). Slash-separated; no "." or ".." segments, no leading "/".
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+)(/([^/\\.][^/\\]*|\.[^/\\.][^/\\]*|\.\.[^/\\]+))*$`
	Path string `json:"path,omitempty"`
}

// ArtifactStatus is what a kind reports about the artifact it last loaded.
type ArtifactStatus struct {
	// Digest of the file tree ("sha256:<hex>" over the sorted paths and contents); the same
	// files give the same digest whatever the source.
	// +optional
	Digest string `json:"digest,omitempty"`
	// ResolvedDigest is the manifest digest an ociRef resolved to (the digest of its tag at the
	// last sync, or the pinned digest).
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`
	// Files is the number of files in the tree.
	// +optional
	Files int32 `json:"files,omitempty"`
	// Bytes is the total size of the files.
	// +optional
	Bytes int64 `json:"bytes,omitempty"`
}
