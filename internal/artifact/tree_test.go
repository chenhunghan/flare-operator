package artifact

import (
	"strings"
	"testing"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{
		"a":            "a",
		"./a/b.txt":    "a/b.txt",
		"a/b/":         "a/b",
		"...":          "...",
		"..a/.b/c..":   "..a/.b/c..",
		"ünïcödé/файл": "ünïcödé/файл",
	}
	for in, want := range ok {
		if got, err := cleanRel(in, false); err != nil || got != want {
			t.Errorf("cleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "./", "/a", "..", "../a", "a/..", "a/./b", "a//b", `a\b`, "a\x00b", "a\nb", "\xff", strings.Repeat("a", MaxPathLength+1)} {
		if got, err := cleanRel(in, false); err == nil {
			t.Errorf("cleanRel(%q) = %q, want an error", in, got)
		}
	}
	for _, in := range []string{"", ".", "./"} {
		if got, err := cleanRel(in, true); err != nil || got != "" {
			t.Errorf("cleanRel(%q, root) = %q, %v; want the root", in, got, err)
		}
	}
}

func TestUnder(t *testing.T) {
	for _, c := range []struct {
		p, root, rel string
		in           bool
	}{
		{"a/b", "", "a/b", true},
		{"a/b", "a", "b", true},
		{"a", "a", "", false},
		{"ab/c", "a", "", false},
		{"a/b/c", "a/b", "c", true},
	} {
		rel, in := under(c.p, c.root)
		if rel != c.rel && c.in || in != c.in {
			t.Errorf("under(%q, %q) = %q, %v", c.p, c.root, rel, in)
		}
	}
}

func TestTreeDigest(t *testing.T) {
	a, err := newTree(map[string][]byte{"index.html": []byte("hi"), "css/a.css": []byte("x")}, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newTree(map[string][]byte{"css/a.css": []byte("x"), "index.html": []byte("hi")}, testLimits())
	if a.Digest != b.Digest || !strings.HasPrefix(a.Digest, "sha256:") || len(a.Digest) != 71 {
		t.Errorf("digests %s / %s", a.Digest, b.Digest)
	}
	// Path and content boundaries are unambiguous.
	c, _ := newTree(map[string][]byte{"index.htm": []byte("lhi"), "css/a.css": []byte("x")}, testLimits())
	d, _ := newTree(map[string][]byte{"index.html": []byte("hi!"), "css/a.css": []byte("x")}, testLimits())
	if c.Digest == a.Digest || d.Digest == a.Digest {
		t.Error("different trees share a digest")
	}
	if a.Len() != 2 || a.Bytes != 3 || a.Paths()[0] != "css/a.css" {
		t.Errorf("Len %d Bytes %d Paths %v", a.Len(), a.Bytes, a.Paths())
	}
	f, ok := a.File("index.html")
	if !ok || f.ContentType != "text/html; charset=utf-8" || string(f.Content) != "hi" {
		t.Errorf("File(index.html) = %+v, %v", f, ok)
	}
	if fs := a.Files(); len(fs) != 2 || fs[1].Path != "index.html" {
		t.Errorf("Files() = %+v", fs)
	}
}

func TestContentType(t *testing.T) {
	for p, want := range map[string]string{
		"a/INDEX.HTML": "text/html; charset=utf-8",
		"app.mjs":      "text/javascript; charset=utf-8",
		"x.wasm":       "application/wasm",
		"f.woff2":      "font/woff2",
		"noext":        DefaultContentType,
		"weird.xyz":    DefaultContentType,
	} {
		if got := ContentType(p); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestCacheLRU(t *testing.T) {
	c := newCache(10)
	tree := func(n int) *Tree { return &Tree{Bytes: int64(n)} }
	c.put("a", tree(4))
	c.put("b", tree(4))
	if _, ok := c.get("a"); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	c.put("c", tree(4)) // evicts b
	if _, ok := c.get("b"); ok {
		t.Error("b not evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("a evicted")
	}
	c.put("huge", tree(11))
	if _, ok := c.get("huge"); ok {
		t.Error("an entry larger than the cache was stored")
	}
	if n, used := c.size(); n != 2 || used != 8 {
		t.Errorf("size = %d entries, %d bytes", n, used)
	}
	off := newCache(0)
	off.put("a", tree(1))
	if _, ok := off.get("a"); ok {
		t.Error("disabled cache stored an entry")
	}
}
