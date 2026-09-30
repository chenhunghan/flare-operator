package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"testing"
	"time"
)

// tent is one archive entry for the test builders.
type tent struct {
	name     string
	body     string
	typ      byte // tar type flag; 0 = regular file
	linkname string
}

func file(name, body string) tent { return tent{name: name, body: body} }
func symlink(name, target string) tent {
	return tent{name: name, typ: tar.TypeSymlink, linkname: target}
}
func hardlink(name, target string) tent {
	return tent{name: name, typ: tar.TypeLink, linkname: target}
}
func dir(name string) tent { return tent{name: name, typ: tar.TypeDir} }

func tarOf(t testing.TB, ents ...tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: 0o644, ModTime: time.Unix(0, 0), Typeflag: e.typ, Linkname: e.linkname, Format: tar.FormatPAX}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zent is one zip entry: mode selects symlinks and special files.
type zent struct {
	name string
	body string
	mode fs.FileMode
}

func zipOf(t testing.TB, ents ...zent) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// wantKind fails t unless err is an *Error of kind k (and, if sub != "", mentions sub).
func wantKind(t testing.TB, err error, k Kind, sub string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want a %v error, got %v", k, err)
	}
	if e.Kind != k {
		t.Fatalf("want a %v error, got %v: %v", k, e.Kind, err)
	}
	if sub != "" && !bytes.Contains([]byte(err.Error()), []byte(sub)) {
		t.Fatalf("error %q does not mention %q", err, sub)
	}
}

func testLimits() Limits { return Limits{}.withDefaults() }
