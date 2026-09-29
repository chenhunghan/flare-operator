package fake

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// Spec wraps the pinned Cloudflare OpenAPI document and a router over it. It is used to check
// that *clients* (our operator) send schema-valid requests; it never decides the emulated
// server's response — real Cloudflare answers invalid bodies with product-specific error codes,
// which the profiles reproduce from recordings.
type Spec struct {
	Doc    *openapi3.T
	router routers.Router
}

// DefaultSpecPath returns spec/openapi.json.gz at the repository root.
func DefaultSpecPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "spec", "openapi.json.gz")
}

var (
	specOnce sync.Once
	specVal  *Spec
	specErr  error
)

// LoadDefaultSpec loads the pinned spec once per process.
func LoadDefaultSpec() (*Spec, error) {
	specOnce.Do(func() { specVal, specErr = LoadSpec(DefaultSpecPath()) })
	return specVal, specErr
}

// LoadSpec reads an OpenAPI document (optionally gzipped) and builds a router for it.
func LoadSpec(path string) (*Spec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("load spec: %w", err)
	}
	// Route on paths only: the spec's server URL is https://api.cloudflare.com/client/v4.
	doc.Servers = openapi3.Servers{{URL: "/client/v4"}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("build router: %w", err)
	}
	return &Spec{Doc: doc, router: router}, nil
}

// SchemaViolation describes a request that does not conform to the pinned spec.
type SchemaViolation struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Error  string `json:"error"`
}

// ValidateRequest checks a request (with its body already buffered in body) against the spec.
// It returns nil when the operation is unknown to the spec only if allowUnknown is set.
func (s *Spec) ValidateRequest(r *http.Request, body []byte) error {
	clone := r.Clone(context.Background())
	clone.Body = io.NopCloser(strings.NewReader(string(body)))
	route, params, err := s.router.FindRoute(clone)
	if err != nil {
		return fmt.Errorf("no such operation in pinned spec: %w", err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request:    clone,
		PathParams: params,
		Route:      route,
		Options: &openapi3filter.Options{
			// Auth is emulated separately; the spec's security schemes are not what we test here.
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			MultiError:         true,
		},
	}
	// kin-openapi cannot decode the spec's Workers multipart schemas (script upload and settings
	// PATCH fail with "unsupported schema of request body" even when valid), so for a multipart
	// body sent to a Workers operation that the spec declares multipart, only the path and query
	// are validated; the Workers profile parses the parts itself. Every other request, including a
	// multipart body sent to a JSON-only operation, has its body validated as usual.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" && workersMultipartOp(route) {
		in.Options.ExcludeRequestBody = true
	}
	return openapi3filter.ValidateRequest(context.Background(), in)
}

// workersMultipartOp reports whether route is a Workers operation (a path under
// /accounts/{account_id}/workers/) whose pinned-spec request body accepts multipart/form-data.
func workersMultipartOp(route *routers.Route) bool {
	if route == nil || route.Operation == nil || route.Operation.RequestBody == nil || route.Operation.RequestBody.Value == nil {
		return false
	}
	if !strings.HasPrefix(route.Path, "/accounts/{account_id}/workers/") {
		return false
	}
	return route.Operation.RequestBody.Value.Content.Get("multipart/form-data") != nil
}
