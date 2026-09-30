package artifact

import (
	"archive/tar"
	"io/fs"
	"testing"
)

// FuzzExtractArchive feeds arbitrary bytes (seeded with valid and hostile tar, tar.gz and zip
// archives) to the archive extractor. Whatever the input, it must not panic, must stay within
// the limits, and every path it returns must be a clean relative path under the root; a
// failure must be a classified *Error.
//
//	go test ./internal/artifact -run '^$' -fuzz FuzzExtractArchive -fuzztime 60s
func FuzzExtractArchive(f *testing.F) {
	seeds := [][]byte{
		tarOf(f, file("site/index.html", "hi"), file("site/a/b.css", "x"), symlink("site/l", "index.html"), hardlink("site/h", "site/index.html")),
		tarOf(f, file("../evil", "x")),
		tarOf(f, symlink("site/x", "../../etc/passwd")),
		tarOf(f, symlink("a", "b"), symlink("b", "a")),
		tarOf(f, tent{name: "dev", typ: tar.TypeChar}),
		gz(f, tarOf(f, file("site/big", string(make([]byte, 1<<16))))),
		zipOf(f, zent{name: "site/index.html", body: "hi"}, zent{name: "site/l", body: "index.html", mode: fs.ModeSymlink | 0o777}),
		zipOf(f, zent{name: "../x", body: "y"}),
		sparseTar("site/big", 1<<30, 2),
		oldGNUSparseTar("site/big", 1<<30),
		{},
	}
	for _, s := range seeds {
		f.Add(s, "site")
		f.Add(s, "")
	}
	lim := Limits{MaxBytes: 1 << 20, MaxFiles: 64, MaxArchiveBytes: 1 << 20, MaxExpandedBytes: 4 << 20, MaxCompressionRatio: 50}
	f.Fuzz(func(t *testing.T, b []byte, root string) {
		root, err := cleanRel(root, true)
		if err != nil {
			return
		}
		files, err := extractArchive(b, root, lim)
		if err != nil {
			if KindOf(err) == 0 {
				t.Fatalf("unclassified error: %v", err)
			}
			return
		}
		tr, err := newTree(files, lim)
		if err != nil {
			if KindOf(err) != KindRejected {
				t.Fatalf("newTree: unclassified error: %v", err)
			}
			return
		}
		if int64(tr.Len()) > lim.MaxFiles || tr.Bytes > lim.MaxBytes {
			t.Fatalf("limits exceeded: %d files, %d bytes", tr.Len(), tr.Bytes)
		}
		for _, p := range tr.Paths() {
			if c, err := cleanRel(p, false); err != nil || c != p {
				t.Fatalf("unclean path %q (%v)", p, err)
			}
		}
	})
}
