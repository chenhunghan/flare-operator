package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxPathLength bounds one file path (bytes).
const MaxPathLength = 1024

// File is one file of a Tree. Content is shared with the loader's cache: never modify it.
type File struct {
	Path        string
	Content     []byte
	ContentType string
}

// Tree is a loaded artifact. It is immutable and may be shared between callers.
type Tree struct {
	files map[string]File
	paths []string

	// Digest is "sha256:<hex>" over the sorted paths and contents (see digestOf). The same files
	// give the same digest whatever the source.
	Digest string
	// ResolvedDigest is the manifest digest an OCI reference resolved to ("" for other sources).
	ResolvedDigest string
	// Bytes is the total size of the files.
	Bytes int64
}

// Paths returns the file paths in sorted order. The slice is shared: do not modify it.
func (t *Tree) Paths() []string { return t.paths }

// Len is the number of files.
func (t *Tree) Len() int { return len(t.paths) }

// File returns the file at p.
func (t *Tree) File(p string) (File, bool) {
	f, ok := t.files[p]
	return f, ok
}

// Files returns every file in path order.
func (t *Tree) Files() []File {
	out := make([]File, 0, len(t.paths))
	for _, p := range t.paths {
		out = append(out, t.files[p])
	}
	return out
}

// withResolvedDigest returns a copy of t (sharing the files) that reports d.
func (t *Tree) withResolvedDigest(d string) *Tree {
	c := *t
	c.ResolvedDigest = d
	return &c
}

// newTree builds a Tree from path → content after checking the limits and that no path is
// both a file and a directory.
func newTree(files map[string][]byte, lim Limits) (*Tree, error) {
	if int64(len(files)) > lim.MaxFiles {
		return nil, rejected("the artifact has %d files, more than the limit of %d (--artifact-max-files)", len(files), lim.MaxFiles)
	}
	t := &Tree{files: make(map[string]File, len(files)), paths: make([]string, 0, len(files))}
	for p, b := range files {
		t.Bytes += int64(len(b))
		t.paths = append(t.paths, p)
		t.files[p] = File{Path: p, Content: b, ContentType: ContentType(p)}
	}
	if t.Bytes > lim.MaxBytes {
		return nil, rejected("the artifact's files total %d bytes, more than the limit of %d (--artifact-max-bytes)", t.Bytes, lim.MaxBytes)
	}
	sort.Strings(t.paths)
	for _, p := range t.paths {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if _, clash := files[d]; clash {
				return nil, rejected("%q is both a file and a directory", d)
			}
		}
	}
	t.Digest = digestOf(t)
	return t, nil
}

// digestOf hashes a version prefix, then for every file in path order its path, its length
// and its content.
func digestOf(t *Tree) string {
	h := sha256.New()
	h.Write([]byte("flare-artifact-v1\x00"))
	for _, p := range t.paths {
		b := t.files[p].Content
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(b))))
		h.Write([]byte{0})
		h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// cleanRel validates a relative, slash-separated path from an archive, a spec field or a
// ConfigMap and returns it without a leading "./" or a trailing "/". It rejects absolute paths,
// "." and ".." segments (anywhere, even where cleaning would resolve them), empty segments,
// backslashes, NUL and other control characters, and invalid UTF-8. An empty result means the
// root and is allowed only when allowRoot.
func cleanRel(p string, allowRoot bool) (string, error) {
	orig := p
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q is absolute", orig)
	}
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		if allowRoot {
			return "", nil
		}
		return "", fmt.Errorf("path %q names no file", orig)
	}
	if len(p) > MaxPathLength {
		return "", fmt.Errorf("path %.64q... is longer than %d bytes", p, MaxPathLength)
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("path %q is not valid UTF-8", orig)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", fmt.Errorf("path %q contains a backslash or a control character", orig)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("path %q has an empty segment", orig)
		case ".", "..":
			return "", fmt.Errorf("path %q has a %q segment", orig, seg)
		}
	}
	return p, nil
}

// under reports whether p lies in the directory root ("" is everything) and returns p relative
// to root.
func under(p, root string) (string, bool) {
	if root == "" {
		return p, true
	}
	rest, ok := strings.CutPrefix(p, root+"/")
	return rest, ok && rest != ""
}

// contentTypes maps lower-case extensions to the Content-Type served for them. It is a fixed
// table, not mime.TypeByExtension, which reads the host's /etc/mime.types and would make the
// result depend on the manager's image.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".cjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".xml":         "application/xml",
	".rss":         "application/rss+xml",
	".atom":        "application/atom+xml",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".bmp":         "image/bmp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
	".zip":         "application/zip",
	".gz":          "application/gzip",
	".py":          "text/x-python; charset=utf-8",
}

// DefaultContentType is served for extensions the table does not know.
const DefaultContentType = "application/octet-stream"

// ContentType returns the Content-Type for a file path, by its extension.
func ContentType(p string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(p))]; ok {
		return ct
	}
	return DefaultContentType
}
