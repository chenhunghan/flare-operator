package artifact

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestExtractArchiveFormats(t *testing.T) {
	ents := []tent{dir("site/"), file("site/index.html", "<h1>hi</h1>"), file("./site/css/app.css", "body{}"),
		file("README", "outside"), symlink("site/home.html", "index.html")}
	want := map[string]string{"index.html": "<h1>hi</h1>", "css/app.css": "body{}", "home.html": "<h1>hi</h1>"}
	zipped := zipOf(t, zent{name: "site/index.html", body: "<h1>hi</h1>"}, zent{name: "site/css/app.css", body: "body{}"},
		zent{name: "README", body: "outside"}, zent{name: "site/home.html", body: "index.html", mode: fs.ModeSymlink | 0o777})
	for name, b := range map[string][]byte{"tar": tarOf(t, ents...), "tar.gz": gz(t, tarOf(t, ents...)), "zip": zipped} {
		t.Run(name, func(t *testing.T) {
			files, err := extractArchive(b, "site", testLimits())
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for p, c := range files {
				got[p] = string(c)
			}
			if !maps.Equal(got, want) {
				t.Errorf("files = %v, want %v", got, want)
			}
		})
	}
	// The whole archive when path is empty.
	files, err := extractArchive(tarOf(t, ents...), "", testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(files)); !slices.Equal(keys, []string{"README", "site/css/app.css", "site/home.html", "site/index.html"}) {
		t.Errorf("paths = %v", keys)
	}
}

func TestExtractArchiveRejects(t *testing.T) {
	cases := map[string]struct {
		archive []byte
		root    string
		want    string
	}{
		"parent traversal":       {tarOf(t, file("../evil", "x")), "", `".." segment`},
		"inner traversal":        {tarOf(t, file("a/../../evil", "x")), "", `".." segment`},
		"cleanable traversal":    {tarOf(t, file("a/../b", "x")), "", `".." segment`},
		"absolute":               {tarOf(t, file("/etc/passwd", "x")), "", "absolute"},
		"backslash":              {tarOf(t, file(`a\..\b`, "x")), "", "backslash"},
		"control char":           {tarOf(t, file("a\x01b", "x")), "", "control"},
		"empty segment":          {tarOf(t, file("a//b", "x")), "", "empty segment"},
		"symlink escapes":        {tarOf(t, symlink("site/x", "../../etc/passwd")), "site", "escapes"},
		"symlink leaves path":    {tarOf(t, file("secret", "s"), symlink("site/x", "../secret")), "site", "outside the selected path"},
		"symlink absolute":       {tarOf(t, symlink("x", "/etc/passwd")), "", "absolute target"},
		"symlink dangling":       {tarOf(t, symlink("x", "nope")), "", "dangling"},
		"symlink to directory":   {tarOf(t, file("d/f", "x"), symlink("x", "d")), "", "dangling, or a directory"},
		"symlink loop":           {tarOf(t, symlink("a", "b"), symlink("b", "a")), "", "loop"},
		"hardlink escapes":       {tarOf(t, file("secret", "s"), hardlink("site/x", "secret")), "site", "outside the selected path"},
		"hardlink traversal":     {tarOf(t, hardlink("x", "../../etc/passwd")), "", `".." segment`},
		"hardlink missing":       {tarOf(t, hardlink("x", "nope")), "", "not an earlier regular file"},
		"char device":            {tarOf(t, tent{name: "dev/null", typ: tar.TypeChar}), "", "device"},
		"block device":           {tarOf(t, tent{name: "sda", typ: tar.TypeBlock}), "", "device"},
		"fifo":                   {tarOf(t, tent{name: "p", typ: tar.TypeFifo}), "", "FIFO"},
		"file and directory":     {tarOf(t, file("a", "x"), file("a/b", "y")), "", ""},
		"zip traversal":          {zipOf(t, zent{name: "../evil", body: "x"}), "", `".." segment`},
		"zip absolute":           {zipOf(t, zent{name: "/evil", body: "x"}), "", "absolute"},
		"zip backslash":          {zipOf(t, zent{name: `..\evil`, body: "x"}), "", "backslash"},
		"zip symlink escapes":    {zipOf(t, zent{name: "x", body: "../../etc/passwd", mode: fs.ModeSymlink | 0o777}), "", "escapes"},
		"zip device":             {zipOf(t, zent{name: "dev", mode: fs.ModeDevice | 0o644}), "", "device"},
		"not an archive":         {[]byte(strings.Repeat("hello", 200)), "", "not a tar"},
		"empty":                  {gz(t, tarOf(t)), "", "no files"},
		"nothing under the path": {tarOf(t, file("a", "x")), "site", "no files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := extractArchive(c.archive, c.root, testLimits())
			if name == "file and directory" {
				// Extraction keeps both; the tree refuses the clash.
				files, xerr := extractArchive(c.archive, c.root, testLimits())
				if xerr != nil {
					t.Fatal(xerr)
				}
				_, err = newTree(files, testLimits())
				wantKind(t, err, KindRejected, "both a file and a directory")
				return
			}
			wantKind(t, err, KindRejected, c.want)
		})
	}
}

func TestExtractArchiveLinks(t *testing.T) {
	files, err := extractArchive(tarOf(t, file("a/real.txt", "data"), hardlink("a/hard.txt", "a/real.txt"),
		symlink("a/sym.txt", "real.txt"), symlink("a/chain.txt", "sym.txt"), symlink("b.txt", "a/../a/real.txt")), "", testLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/real.txt", "a/hard.txt", "a/sym.txt", "a/chain.txt", "b.txt"} {
		if string(files[p]) != "data" {
			t.Errorf("%s = %q, want data", p, files[p])
		}
	}
	// A later entry at the same path replaces the earlier one (tar semantics).
	files, err = extractArchive(tarOf(t, file("x", "old"), file("x", "new")), "", testLimits())
	if err != nil || string(files["x"]) != "new" {
		t.Errorf("x = %q, %v; want new", files["x"], err)
	}
}

func TestExtractArchiveLimits(t *testing.T) {
	big := strings.Repeat("a", 3<<20) // compresses ~1000:1
	t.Run("ratio", func(t *testing.T) {
		_, err := extractArchive(gz(t, tarOf(t, file("big", big))), "", testLimits())
		wantKind(t, err, KindRejected, "--artifact-max-compression-ratio")
	})
	t.Run("ratio allows a small archive", func(t *testing.T) {
		if _, err := extractArchive(gz(t, tarOf(t, file("small", strings.Repeat("a", 512<<10)))), "", testLimits()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expanded bytes", func(t *testing.T) {
		lim := testLimits()
		lim.MaxExpandedBytes = 2 << 20
		lim.MaxCompressionRatio = 1 << 40
		// Entries outside the selected path count too.
		_, err := extractArchive(gz(t, tarOf(t, file("other/big", big), file("site/a", "x"))), "site", lim)
		wantKind(t, err, KindRejected, "--artifact-max-expanded-bytes")
	})
	t.Run("file bytes", func(t *testing.T) {
		lim := testLimits()
		lim.MaxBytes = 1 << 10
		_, err := extractArchive(tarOf(t, file("a", strings.Repeat("x", 600)), file("b", strings.Repeat("y", 600))), "", lim)
		wantKind(t, err, KindRejected, "--artifact-max-bytes")
		_, err = extractArchive(zipOf(t, zent{name: "a", body: strings.Repeat("x", 2000)}), "", lim)
		wantKind(t, err, KindRejected, "--artifact-max-bytes")
	})
	t.Run("symlink copies count", func(t *testing.T) {
		lim := testLimits()
		lim.MaxBytes = 1 << 10
		_, err := extractArchive(tarOf(t, file("a", strings.Repeat("x", 600)), symlink("b", "a")), "", lim)
		wantKind(t, err, KindRejected, "--artifact-max-bytes")
	})
	t.Run("files", func(t *testing.T) {
		lim := testLimits()
		lim.MaxFiles = 3
		_, err := extractArchive(tarOf(t, file("a", "1"), file("b", "2"), file("c", "3"), file("d", "4")), "", lim)
		wantKind(t, err, KindRejected, "--artifact-max-files")
	})
	t.Run("zip declared size lies", func(t *testing.T) {
		// A zip whose central directory understates an entry: archive/zip or the loader must
		// refuse it rather than read past the declared size.
		b := zipOf(t, zent{name: "a", body: strings.Repeat("x", 100)})
		i := bytes.LastIndex(b, []byte("PK\x01\x02")) // central directory header
		b[i+24] = 10                                  // uncompressed size (little endian) = 10
		b[i+25], b[i+26], b[i+27] = 0, 0, 0
		if _, err := extractArchive(b, "", testLimits()); err == nil {
			t.Fatal("accepted a zip entry larger than it declares")
		}
	})
}

func TestDetectFormat(t *testing.T) {
	if f := detectFormat(tarOf(t, file("a", "b"))); f != formatTar {
		t.Errorf("tar detected as %q", f)
	}
	if f := detectFormat(gz(t, []byte("x"))); f != formatTarGz {
		t.Errorf("gzip detected as %q", f)
	}
	if f := detectFormat(zipOf(t)); f != formatZip {
		t.Errorf("empty zip detected as %q", f)
	}
	if f := detectFormat([]byte("BZh9")); f != "" {
		t.Errorf("bzip2 detected as %q", f)
	}
}
