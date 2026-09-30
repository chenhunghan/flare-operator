# Artifact sources

Kinds that deploy content, such as Worker modules, static assets and Pages sites, take their
files from an `ArtifactSource` (`api/shared/v1alpha1`). The manager's loader
(`internal/artifact`) turns the source into an in-memory file tree. Each file has a path, its
bytes and a content type chosen by file extension. The tree also has a content digest, which
kinds report as `status.…artifact.digest`.

> Status: the API type and the loader exist, but no kind embeds an `ArtifactSource` yet. The
> full-stack kinds (R2, Pages, Workers static assets) will add it.

## Sources

Set exactly one of `configMapRef`, `ociRef` or `url`. A CEL rule enforces this.

### ConfigMaps

```yaml
source:
  configMapRef:
    configMaps:
      - name: site-root            # every data/binaryData key becomes a file at the root
      - name: site-css
        path: static/css           # its keys go under static/css/
      - name: site-misc
        items:                     # only these keys, each at its own path
          - {key: robots.txt, path: robots.txt}
          - {key: security.txt, path: .well-known/security.txt}
```

- Every ConfigMap must be in the object's namespace and carry the label
  `cloudflare.flare.dev/artifact=true`. The operator reads it with its own cluster-wide
  access and publishes its content, so it reads only ConfigMaps that opted in. This follows the
  same pattern as `cloudflare.flare.dev/worker-binding` for `secret_text` bindings.
- ConfigMap keys cannot contain `/`. Use `path` for a directory, or `items` for nested paths.
- Two files at the same path are an error.
- A ConfigMap holds at most 1 MiB of data. Use `ociRef` or `url` for larger sites.

### OCI image

```yaml
source:
  ociRef:
    image: ghcr.io/acme/site@sha256:…   # pin by digest (recommended)
    path: dist                          # directory of the image filesystem to use (default: /)
    pullSecretRef: {name: ghcr-pull}    # optional kubernetes.io/dockerconfigjson Secret
```

- The loader applies the layers in order, with OCI whiteouts, as a container runtime would.
  It keeps only the regular files under `path`.
- A tag is resolved on every sync, and kinds report the result in `resolvedDigest`. Pin by
  digest so the content cannot change without a spec change.
- Every load fetches the manifest with the object's own credentials, even when the content is
  cached. This proves access before the cache serves content that another namespace pulled.
- The pull Secret must be of type `kubernetes.io/dockerconfigjson` and carry the label
  `cloudflare.flare.dev/artifact=true`.
- The registry is always contacted over HTTPS. go-containerregistry would use plain HTTP for
  `localhost`, IP literals and `*.local`; the loader upgrades those requests to HTTPS instead.
- Only tar layers are unpacked: uncompressed, gzip or zstd. Foreign and non-distributable
  layers are refused.
- A multi-platform index uses its only image, ignoring attestation manifests (BuildKit's
  provenance and SBOM entries). If it has several images, the loader uses the `linux/amd64` one.
- Build a small image, for example `FROM scratch` + `COPY dist/ /dist/`. The limits count
  every layer byte, including entries outside `path`.

### HTTPS archive

```yaml
source:
  url:
    url: https://github.com/acme/site/releases/download/v1.2.0/site.tar.gz
    sha256: 3b0c…                      # lowercase hex of the archive
    path: public                       # directory inside the archive (default: root)
```

- The loader detects the format from the content: tar, gzip-compressed tar, or zip.
- It downloads the archive into memory and checks the SHA-256 before it reads any entry.
- The URL must use HTTPS. Redirects are followed only to HTTPS, at most 5 times. Credentials
  in the URL are refused, and proxy environment variables are ignored.

## Safety rules

The loader enforces these rules for every source:

| Threat | Rule |
|---|---|
| Path traversal | Paths must be relative and slash-separated, with no `.` or `..` segments, backslashes, control characters or invalid UTF-8, and at most 1024 bytes. A path cannot be both a file and a directory. |
| Links | A symbolic link must resolve, within `path`, to a regular file; its content is copied. A hard link must name an earlier regular file inside `path`. Absolute link targets are refused in archives. In images, absolute targets are relative to the image root. Chains are limited to 16 links. |
| Special files | Device files, FIFOs, sockets and sparse files under `path` are refused (a sparse file's holes are not in the archive stream the limits count). |
| Zip/tar bombs | Limits on downloaded bytes, decompressed bytes (including entries outside `path`) and the decompressed/compressed ratio (after the first 1 MiB), plus limits on file count and total size. |
| SSRF | Addresses are checked at connect time, after DNS resolution, for every redirect and every token-server request. Loopback, private (RFC 1918, ULA), link-local and cloud metadata (169.254.169.254, fd00:ec2::254), carrier-grade NAT (including 100.100.100.200), multicast, documentation, benchmarking and reserved ranges are refused. So are the IPv6 ranges that embed IPv4 (NAT64, 6to4, Teredo, IPv4-mapped). Use `--artifact-allowed-cidr` to permit, for example, an in-cluster registry. |
| Opt-in | ConfigMaps and pull Secrets are read only with `cloudflare.flare.dev/artifact=true`. A missing, unlabelled or wrongly typed object gets the same message, so a spec cannot probe which objects exist. |

## Limits (manager flags)

| Flag | Default | Chart value |
|---|---|---|
| `--artifact-max-bytes` | `64Mi` | `artifacts.maxBytes` |
| `--artifact-max-files` | `20000` | `artifacts.maxFiles` |
| `--artifact-max-archive-bytes` | `64Mi` | `artifacts.maxArchiveBytes` |
| `--artifact-max-expanded-bytes` | `256Mi` | `artifacts.maxExpandedBytes` |
| `--artifact-max-compression-ratio` | `100` | `artifacts.maxCompressionRatio` |
| `--artifact-cache-bytes` | `128Mi` | `artifacts.cacheBytes` |
| `--artifact-allowed-cidr` | none | `artifacts.allowedCIDRs` |

A url load holds the downloaded archive (up to `maxArchiveBytes`) and the tree it unpacks
(up to `maxBytes`) in memory together; an OCI load streams its layers but holds the tree.
Loaded OCI and URL trees are cached by digest. Size the manager's memory limit above
(`maxArchiveBytes` + `maxBytes`) × the parallel reconciles of the content kinds, plus
`cacheBytes`. With `networkPolicy.enabled`, add egress to your registries and
download hosts through `networkPolicy.extraEgress`.

## Errors

`artifact.KindOf(err)` classifies a failure so that a kind can choose its condition reason:

- **Dependency:** a ConfigMap or Secret is missing, unlabelled or of the wrong type. Retry when
  it changes.
- **Invalid:** a malformed reference, URL or path. Only a spec change fixes it.
- **Rejected:** the content breaks a limit or a safety rule, or its SHA-256 does not match.
- **Fetch:** a registry or server error, refused credentials, or a refused address. Retry with
  backoff.

Error messages never include credentials or file content.
