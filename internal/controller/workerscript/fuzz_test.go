package workerscript

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

// FuzzBuildMultipart builds upload bodies from fuzzed module names, types and contents and
// parses them back with mime/multipart: for every module name the controller accepts
// (validModuleName) the round trip is exact (part order, names, file names, content types,
// bytes), and the metadata part is valid JSON. Seeds run with every `go test`; a real run is
// `go test -fuzz=FuzzBuildMultipart ./internal/controller/workerscript/` (not part of CI).
func FuzzBuildMultipart(f *testing.F) {
	f.Add("index.js", "esm", []byte("export default {}"), "lib/util.js", "cjs", []byte("module.exports = 1"))
	f.Add(`we"ird\name.js`, "esm", []byte("--boundary\r\n"), "data.json", "json", []byte(`{"a":1}`))
	f.Add("ünïcødé.txt", "text", []byte{0, 1, 2, 255}, "a.wasm", "wasm", []byte("\x00asm"))
	f.Add("x", "unknown", []byte(""), "y", "esm", []byte("\r\n--"))
	f.Fuzz(func(t *testing.T, n1, t1 string, c1 []byte, n2, t2 string, c2 []byte) {
		mods := []module{{Name: n1, Type: t1, Content: c1}, {Name: n2, Type: t2, Content: c2}}
		for _, m := range mods {
			if !validModuleName(m.Name) {
				return
			}
		}
		meta := map[string]any{"main_module": n1, "compatibility_date": "2026-09-01"}
		body, ct, err := buildMultipart("metadata", "metadata.json", meta, mods)
		_, known1 := moduleContentTypes[t1]
		_, known2 := moduleContentTypes[t2]
		if !known1 || !known2 {
			if err == nil {
				t.Fatalf("unknown module type accepted")
			}
			return
		}
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
			t.Fatalf("content type %q: %v", ct, err)
		}
		r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		type got struct {
			name, file, ct string
			data           []byte
		}
		var parts []got
		for {
			p, err := r.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			data, err := io.ReadAll(p)
			if err != nil {
				t.Fatalf("read part: %v", err)
			}
			parts = append(parts, got{p.FormName(), p.FileName(), p.Header.Get("Content-Type"), data})
		}
		if len(parts) != 3 {
			t.Fatalf("%d parts, want 3", len(parts))
		}
		var md map[string]any
		if parts[0].name != "metadata" || parts[0].file != "metadata.json" || json.Unmarshal(parts[0].data, &md) != nil || md["main_module"] != n1 {
			t.Fatalf("metadata part %q/%q: %s", parts[0].name, parts[0].file, parts[0].data)
		}
		for i, m := range mods {
			p := parts[i+1]
			// FileName() applies filepath.Base; the form name is the exact part name.
			if p.name != m.Name || p.ct != moduleContentTypes[m.Type] || !bytes.Equal(p.data, m.Content) {
				t.Fatalf("module %d: got name %q type %q (%d bytes), want %q %q (%d bytes)", i, p.name, p.ct, len(p.data), m.Name,
					moduleContentTypes[m.Type], len(m.Content))
			}
		}
	})
}
