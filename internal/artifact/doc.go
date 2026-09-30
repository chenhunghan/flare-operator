// Package artifact loads the deployable content of a kind (Worker modules, static assets, Pages
// sites) from a sharedv1alpha1.ArtifactSource: labelled ConfigMaps, an OCI image, or an HTTPS
// archive pinned by SHA-256. A Loader returns an immutable in-memory Tree (path → bytes, with a
// content type by extension) and a stable content digest; controllers compare the digest with
// what they last uploaded.
//
// Every source is untrusted input, so the loader enforces, whatever the source:
//
//   - Paths: relative, slash-separated, no "." or ".." segments, no backslashes or control
//     characters; a path cannot be both a file and a directory.
//   - Links: a symbolic link must resolve, within the selected root, to a regular file (its
//     content is copied); a hard link must name an earlier regular file inside the root.
//     Absolute link targets are refused for archives; in OCI images they are relative to the
//     image root. Device files, FIFOs and sockets are refused.
//   - Size (Limits, from the manager's --artifact-* flags): files and bytes of the tree, bytes
//     downloaded, bytes decompressed and the decompressed/compressed ratio (zip and tar bombs).
//   - Network: HTTPS only, redirects only to HTTPS, no proxy from the environment, and every
//     connection's resolved address must be public unless an allowlisted CIDR contains it: the
//     check runs at connect time, so DNS rebinding and redirects cannot reach loopback,
//     private, link-local (cloud metadata) or other special-purpose ranges (SSRF).
//   - Opt-in: ConfigMaps and pull Secrets are read only with the label
//     cloudflare.flare.dev/artifact=true (sharedv1alpha1.LabelArtifact).
//
// Loaded OCI and URL trees are cached by digest (bounded by Options.CacheBytes). A cached OCI
// tree is returned only after the caller's own credentials fetched the manifest, so one
// namespace cannot read another's private image through the cache.
package artifact
