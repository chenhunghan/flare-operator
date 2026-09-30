package pagesdeployment

import (
	"strings"
	"testing"
)

// The fixture of wrangler's "should upload a directory of files": logo.png with the content
// "foobar" hashes to 2082190357cfd3617ccfe04f340c6247
// (cloudflare/workers-sdk@485cfb3:packages/wrangler/src/__tests__/pages/deploy.test.ts#L469-L491).
func TestHashFileMatchesWrangler(t *testing.T) {
	if got := HashFile("logo.png", []byte("foobar")); got != "2082190357cfd3617ccfe04f340c6247" {
		t.Fatalf("hashFile = %s", got)
	}
	// The extension is part of the hash; the directory is not.
	if HashFile("a/logo.png", []byte("foobar")) != HashFile("logo.png", []byte("foobar")) {
		t.Error("the directory changed the hash")
	}
	if HashFile("logo.txt", []byte("foobar")) == HashFile("logo.png", []byte("foobar")) {
		t.Error("the extension did not change the hash")
	}
}

func TestNodeExt(t *testing.T) {
	for in, want := range map[string]string{"a.html": "html", "dir/a.tar.gz": "gz", ".bashrc": "", "README": "", "x.": "",
		"d.x/README": "", "..a": "a", "dir/.env.local": "local"} {
		if got := nodeExt(in); got != want {
			t.Errorf("nodeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIgnored(t *testing.T) {
	for p, want := range map[string]bool{"_headers": true, "_worker.js": true, "functions/api.js": true, ".wrangler/x": true,
		"a/node_modules/x.js": true, "x/.git/HEAD": true, "a/.DS_Store": true, "index.html": false, "sub/_headers": false,
		"docs/functions/x.html": false, ".well-known/security.txt": false} {
		if got := ignored(p); got != want {
			t.Errorf("ignored(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestBuckets(t *testing.T) {
	big := assetFile{content: make([]byte, 30<<20)}
	small := assetFile{content: []byte("x")}
	var files []assetFile
	files = append(files, big, big)
	for i := 0; i < 2500; i++ {
		files = append(files, small)
	}
	bs := buckets(files)
	// 30 MiB + 30 MiB exceed 40 MiB: one each; the second takes 1999 small files (2000 per
	// bucket), the rest go into a third.
	if len(bs) != 3 || len(bs[0]) != 1 || len(bs[1]) != 2000 || len(bs[2]) != 501 {
		var n []int
		for _, b := range bs {
			n = append(n, len(b))
		}
		t.Fatalf("buckets %v", n)
	}
}

func TestDecodeClaims(t *testing.T) {
	// header {"alg":"none"} . {"exp":123,"max_file_count_allowed":7} (base64url, no padding)
	tok := "eyJhbGciOiJub25lIn0.eyJleHAiOjEyMywibWF4X2ZpbGVfY291bnRfYWxsb3dlZCI6N30.x"
	if c := decodeClaims(tok); c.Exp != 123 || c.MaxFileCountAllowed != 7 {
		t.Fatalf("claims %+v", c)
	}
	if c := decodeClaims("not-a-jwt"); c.Exp != 0 {
		t.Fatalf("claims of garbage %+v", c)
	}
}

func TestPendingKeyRoundTrip(t *testing.T) {
	k := pendingKey("site", strings.Repeat("a", 64), strings.Repeat("b", 40))
	if strings.ContainsAny(k, "\r\n") {
		t.Fatal(k)
	}
}

// ContentType follows mime@3.0.0's getType as wrangler's validate walk calls it.
func TestContentTypeMatchesWranglerMime(t *testing.T) {
	for p, want := range map[string]string{
		"index.html": "text/html", "a/b/APP.JS": "application/javascript", "favicon.ico": "image/vnd.microsoft.icon",
		"captions.vtt": "text/vtt", "page.xhtml": "application/xhtml+xml", "data.jsonld": "application/ld+json",
		"cal.ics": "text/calendar", "c.yaml": "text/yaml", "x.tif": "image/tiff", "a.apng": "image/apng",
		"module.wasm": "application/wasm", "a.tar.gz": "application/gzip",
		"README": "application/octet-stream", "d/README": "application/octet-stream", "x.unknownext": "application/octet-stream",
		"html": "text/html", "d/html": "application/octet-stream", ".html": "text/html", "d/.html": "application/octet-stream",
		"x.": "application/octet-stream", `d\a.css`: "text/css",
	} {
		if got := ContentType(p); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", p, got, want)
		}
	}
}
