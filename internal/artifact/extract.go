package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
)

// maxLinkHops bounds a chain of symbolic links.
const maxLinkHops = 16

// entry is a file or a symbolic link of the tree being extracted.
type entry struct {
	content []byte
	// link is the target of a symbolic link, relative to the extractor's root; isLink marks it.
	link   string
	isLink bool
}

// extractor unpacks archives (one or more layers) into a tree under a selected root, enforcing
// the limits and path rules. Entries outside the root are read (and counted) but not kept.
type extractor struct {
	lim  Limits
	root string // selected directory of the archive ("" = all), already cleaned
	// oci enables OCI layer semantics: whiteout files, and absolute symlink targets relative
	// to the image root. Without it an absolute link target is an error.
	oci bool

	entries map[string]entry // accumulated, relative to root
	size    int64            // bytes of the accumulated regular files

	// layer staging (one archive or one image layer)
	layer      map[string]entry
	layerSize  int64
	whiteouts  map[string]bool // archive-root paths deleted from lower layers
	opaque     map[string]bool // archive-root directories whose lower content is hidden
	compressed int64           // compressed bytes of every archive/layer (ratio denominator)
	expanded   int64           // decompressed bytes read so far
	err        error           // sticky limit error raised inside a reader
	// network marks a remote source stream (OCI layers): a read error of it is a fetch
	// failure, not a malformed archive.
	network bool
	srcErr  error
}

func newExtractor(lim Limits, root string, oci bool) *extractor {
	return &extractor{lim: lim, root: root, oci: oci, entries: map[string]entry{}}
}

// counter counts decompressed bytes against MaxExpandedBytes and the compression ratio.
type counter struct {
	r io.Reader
	x *extractor
}

func (c *counter) Read(p []byte) (int, error) {
	if c.x.err != nil {
		return 0, c.x.err
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF && c.x.srcErr == nil {
		c.x.srcErr = err
	}
	c.x.expanded += int64(n)
	switch {
	case c.x.expanded > c.x.lim.MaxExpandedBytes:
		c.x.err = rejected("the archive expands to more than %d bytes (--artifact-max-expanded-bytes)", c.x.lim.MaxExpandedBytes)
	case !c.x.lim.ratioOK(c.x.expanded, c.x.compressed):
		c.x.err = rejected("the archive expands more than %d times its compressed size (--artifact-max-compression-ratio)", c.x.lim.MaxCompressionRatio)
	}
	if c.x.err != nil {
		return n, c.x.err
	}
	return n, err
}

// fail returns the sticky limit error if a reader raised one (the archive readers may wrap or
// replace it), else err classified as a rejected archive.
func (x *extractor) fail(err error, what string) error {
	if x.err != nil {
		return x.err
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if x.network && x.srcErr != nil {
		return fetchErr(x.srcErr, "%s", what)
	}
	return newErr(KindRejected, err, "%s", what)
}

func (x *extractor) beginLayer() {
	x.layer = map[string]entry{}
	x.layerSize = 0
	x.whiteouts = map[string]bool{}
	x.opaque = map[string]bool{}
}

// endLayer applies the staged layer: whiteouts and opaque directories remove lower entries, a
// new entry replaces a lower entry at its path and every lower entry below it (a file replacing
// a directory) or above it (a directory replacing a file).
func (x *extractor) endLayer() error {
	hide := map[string]bool{}
	for p := range x.whiteouts {
		hide[p] = true
	}
	for p := range x.opaque {
		hide[p] = true
	}
	for rel := range x.layer {
		hide[x.full(rel)] = true
	}
	if x.opaque[""] || x.opaque[x.root] {
		// The whole root (or an ancestor of it) is opaque: nothing of the lower layers is kept.
		x.entries = map[string]entry{}
		x.size = 0
	}
	if len(hide) > 0 && len(x.entries) > 0 {
		for rel, e := range x.entries {
			full := x.full(rel)
			gone := x.whiteouts[full]
			for d := path.Dir(full); !gone && d != "."; d = path.Dir(d) {
				gone = hide[d]
			}
			if gone {
				x.size -= int64(len(e.content))
				delete(x.entries, rel)
			}
		}
	}
	for rel := range x.layer {
		// Lower entries at an ancestor path (a file where the layer now has a directory). A
		// clash inside one layer is kept, for newTree to refuse.
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			if old, ok := x.entries[d]; ok {
				x.size -= int64(len(old.content))
				delete(x.entries, d)
			}
		}
	}
	for rel, e := range x.layer {
		if old, ok := x.entries[rel]; ok {
			x.size -= int64(len(old.content))
		}
		x.entries[rel] = e
		x.size += int64(len(e.content))
	}
	x.layer = nil
	return x.checkCounts()
}

func (x *extractor) checkCounts() error {
	if x.size+x.layerSize > x.lim.MaxBytes {
		return rejected("the artifact's files total more than %d bytes (--artifact-max-bytes)", x.lim.MaxBytes)
	}
	if int64(len(x.entries)+len(x.layer)) > x.lim.MaxFiles {
		return rejected("the artifact has more than %d files (--artifact-max-files)", x.lim.MaxFiles)
	}
	return nil
}

// full returns the archive-root path of a root-relative path.
func (x *extractor) full(rel string) string {
	if x.root == "" {
		return rel
	}
	return x.root + "/" + rel
}

// stage records a regular file or symbolic link at archive path p (already cleaned) if it lies
// under the root.
func (x *extractor) stage(p string, e entry) error {
	rel, in := under(p, x.root)
	if !in {
		return nil
	}
	if old, ok := x.layer[rel]; ok {
		x.layerSize -= int64(len(old.content))
	}
	x.layer[rel] = e
	x.layerSize += int64(len(e.content))
	return x.checkCounts()
}

// linkTarget resolves the target of a symbolic link at archive path p to a root-relative path.
func (x *extractor) linkTarget(p, target string) (string, error) {
	if target == "" {
		return "", rejected("symbolic link %q has an empty target", p)
	}
	var full string
	if strings.HasPrefix(target, "/") {
		if !x.oci {
			return "", rejected("symbolic link %q has an absolute target %q", p, target)
		}
		full = path.Clean(target)[1:] // relative to the image root
	} else {
		full = path.Join(path.Dir(p), target)
	}
	if full == ".." || strings.HasPrefix(full, "../") {
		return "", rejected("symbolic link %q escapes the archive root (%q)", p, target)
	}
	rel, in := under(full, x.root)
	if !in {
		return "", rejected("symbolic link %q points outside the selected path (%q)", p, target)
	}
	return rel, nil
}

// readFile reads a regular file's content, checking the size limit before reading.
func (x *extractor) readFile(p string, r io.Reader, size int64) ([]byte, error) {
	if size < 0 || x.size+x.layerSize+size > x.lim.MaxBytes {
		return nil, rejected("file %q (%d bytes) takes the artifact over %d bytes (--artifact-max-bytes)", p, size, x.lim.MaxBytes)
	}
	// The declared size is untrusted until read: preallocate at most 1 MiB.
	buf := bytes.NewBuffer(make([]byte, 0, min(size, 1<<20)))
	n, err := io.Copy(buf, io.LimitReader(r, size+1))
	if err != nil {
		return nil, x.fail(err, "read "+p)
	}
	if n != size {
		return nil, rejected("file %q: read %d bytes, the archive declares %d", p, n, size)
	}
	return buf.Bytes(), nil
}

// addTar reads one tar stream (decompressed; r must count through x) into the staged layer.
func (x *extractor) addTar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return x.fail(err, "read tar archive")
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// A PAX global header (release tarballs made from a repository carry one): metadata
			// of the archive, not a file.
			continue
		}
		p, err := cleanRel(hdr.Name, true)
		if err != nil {
			return newErr(KindRejected, err, "tar entry")
		}
		if x.oci {
			base, dir := path.Base(p), path.Dir(p)
			if base == ".wh..wh..opq" {
				if dir == "." {
					dir = ""
				}
				x.opaque[dir] = true
				continue
			}
			if name, ok := strings.CutPrefix(base, ".wh."); ok && p != "" {
				x.whiteouts[path.Join(dir, name)] = true
				continue
			}
		}
		_, in := under(p, x.root)
		if in && isSparse(hdr) {
			// archive/tar expands a sparse file's holes into zeros that are not in the (counted)
			// stream, so they would escape the expanded-size and ratio limits.
			return rejected("%q is a sparse file, which artifacts do not support", p)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg: // archive/tar reports the old TypeRegA as TypeReg
			if !in || p == "" {
				continue // skipped: tr.Next drains (and counts) the content
			}
			b, err := x.readFile(p, tr, hdr.Size)
			if err != nil {
				return err
			}
			if err := x.stage(p, entry{content: b}); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !in {
				continue
			}
			t, err := x.linkTarget(p, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := x.stage(p, entry{link: t, isLink: true}); err != nil {
				return err
			}
		case tar.TypeLink:
			if !in {
				continue
			}
			tp, err := cleanRel(strings.TrimPrefix(hdr.Linkname, "/"), false)
			if err != nil {
				return newErr(KindRejected, err, "hard link %q", p)
			}
			rel, tin := under(tp, x.root)
			if !tin {
				return rejected("hard link %q points outside the selected path (%q)", p, hdr.Linkname)
			}
			e, ok := x.layer[rel]
			if !ok {
				e, ok = x.entries[rel]
			}
			if !ok || e.isLink {
				return rejected("hard link %q: %q is not an earlier regular file", p, hdr.Linkname)
			}
			if err := x.stage(p, e); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if in {
				return rejected("%q is a device file or FIFO", p)
			}
		default:
			if in {
				return rejected("%q has an unsupported tar entry type %q", p, string(hdr.Typeflag))
			}
		}
	}
	if x.err != nil {
		return x.err
	}
	return nil
}

// isSparse reports whether hdr is a GNU sparse file: the old GNU type, or GNU sparse PAX
// records of any version (archive/tar keeps them in PAXRecords).
func isSparse(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// addZip reads a zip archive held in memory into the staged layer.
func (x *extractor) addZip(b []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return newErr(KindRejected, err, "read zip archive")
	}
	for _, f := range zr.File {
		p, err := cleanRel(f.Name, true)
		if err != nil {
			return newErr(KindRejected, err, "zip entry")
		}
		_, in := under(p, x.root)
		mode := f.Mode()
		if !in || p == "" || mode.IsDir() {
			continue
		}
		if f.Flags&0x1 != 0 {
			return rejected("zip entry %q is encrypted", p)
		}
		switch {
		case mode.IsRegular(), mode&fs.ModeSymlink != 0:
		default:
			return rejected("%q is a device file, FIFO or socket", p)
		}
		rc, err := f.Open()
		if err != nil {
			return newErr(KindRejected, err, "zip entry %q", p)
		}
		c := &counter{r: rc, x: x}
		if mode&fs.ModeSymlink != 0 {
			target, err := io.ReadAll(io.LimitReader(c, MaxPathLength+1))
			_ = rc.Close()
			if err != nil {
				return x.fail(err, "read zip entry "+p)
			}
			t, err := x.linkTarget(p, string(target))
			if err != nil {
				return err
			}
			if err := x.stage(p, entry{link: t, isLink: true}); err != nil {
				return err
			}
			continue
		}
		if f.UncompressedSize64 > uint64(x.lim.MaxBytes) {
			_ = rc.Close()
			return rejected("file %q (%d bytes) takes the artifact over %d bytes (--artifact-max-bytes)", p, f.UncompressedSize64, x.lim.MaxBytes)
		}
		content, err := x.readFile(p, c, int64(f.UncompressedSize64))
		_ = rc.Close()
		if err != nil {
			return err
		}
		if err := x.stage(p, entry{content: content}); err != nil {
			return err
		}
	}
	return nil
}

// Archive formats of url sources, detected from the content.
const (
	formatTar   = "tar"
	formatTarGz = "tar.gz"
	formatZip   = "zip"
)

// detectFormat recognises gzip (a gzip-compressed tar), zip and tar by their magic bytes.
func detectFormat(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return formatTarGz
	case len(b) >= 4 && string(b[:2]) == "PK" && (string(b[2:4]) == "\x03\x04" || string(b[2:4]) == "\x05\x06"):
		return formatZip
	case len(b) >= 262 && string(b[257:262]) == "ustar":
		return formatTar
	}
	return ""
}

// extractArchive unpacks one verified archive (url source) into a tree.
func extractArchive(b []byte, root string, lim Limits) (map[string][]byte, error) {
	x := newExtractor(lim, root, false)
	x.compressed = int64(len(b))
	x.beginLayer()
	var err error
	switch detectFormat(b) {
	case formatTarGz:
		var zr *gzip.Reader
		zr, err = gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, newErr(KindRejected, err, "read gzip archive")
		}
		err = x.addTar(&counter{r: zr, x: x})
	case formatTar:
		err = x.addTar(&counter{r: bytes.NewReader(b), x: x})
	case formatZip:
		err = x.addZip(b)
	default:
		return nil, rejected("the archive is not a tar, tar.gz or zip file")
	}
	if err != nil {
		return nil, err
	}
	if err := x.endLayer(); err != nil {
		return nil, err
	}
	return x.finish()
}

// finish resolves symbolic links (to copies of their target files) and returns the files.
func (x *extractor) finish() (map[string][]byte, error) {
	out := make(map[string][]byte, len(x.entries))
	for rel, e := range x.entries {
		if !e.isLink {
			out[rel] = e.content
		}
	}
	size := x.size
	for rel, e := range x.entries {
		if !e.isLink {
			continue
		}
		t := e.link
		for hop := 0; ; hop++ {
			if hop > maxLinkHops {
				return nil, rejected("symbolic link %q: more than %d links in a chain (a loop?)", rel, maxLinkHops)
			}
			next, ok := x.entries[t]
			if !ok {
				return nil, rejected("symbolic link %q: target %q is not a file in the artifact (dangling, or a directory)", rel, t)
			}
			if !next.isLink {
				out[rel] = next.content
				size += int64(len(next.content))
				break
			}
			t = next.link
		}
		if size > x.lim.MaxBytes {
			return nil, rejected("the artifact's files (symbolic links copied) total more than %d bytes (--artifact-max-bytes)", x.lim.MaxBytes)
		}
	}
	if len(out) == 0 {
		root := x.root
		if root == "" {
			root = "/"
		}
		return nil, rejected("the artifact has no files under %q", root)
	}
	return out, nil
}
