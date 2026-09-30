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
	liftAccessRequired(doc) // readOnly/writeOnly required across allOf (spec_access.go)
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
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" && workersMultipartOp(route) {
		in.Options.ExcludeRequestBody = true
	}
	if as := plainTextBodyAs(r.Method, route, mt); as != "" {
		clone.Header.Set("Content-Type", as)
	}
	return openapi3filter.ValidateRequest(context.Background(), in)
}

// plainTextBodies are operations where the live API evidently accepts a text/plain body that
// the spec does not declare: an official client sends it that way on every call, so the call
// would fail in production otherwise. The body is then validated as the declared media type,
// so a malformed or schema-violating body still fails strict validation, and text/plain stays
// a violation everywhere else. Keyed by method and spec path template.
var plainTextBodies = map[string]string{
	// SOURCED (relies): wrangler's createQueue sends JSON.stringify(body) with no Content-Type,
	// so undici sends text/plain;charset=UTF-8,
	// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/queues/client.ts#L55-L64 (and
	// wrangler@4.143.0:wrangler-dist/cli.js#L56358-L56386 adds no Content-Type).
	http.MethodPost + " /accounts/{account_id}/queues": "application/json",
	// SOURCED (relies): `wrangler r2 bucket create` sends JSON.stringify({name, storageClass,
	// locationHint}) with only the cf-r2-jurisdiction header, so text/plain;charset=UTF-8
	// (createR2Bucket in src/r2/helpers/bucket.ts, wrangler@4.143.0:wrangler-dist/cli.js#L208160-L208178).
	http.MethodPost + " /accounts/{account_id}/r2/buckets": "application/json",
	// SOURCED (relies): `wrangler kv key put` sends a string value as the raw body with no
	// Content-Type (text/plain;charset=UTF-8); the spec allows only octet-stream and multipart,
	// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/kv/helpers.ts#L247-L259.
	http.MethodPut + " /accounts/{account_id}/storage/kv/namespaces/{namespace_id}/values/{key_name}": "application/octet-stream",
}

// plainTextBodyAs returns the media type a text/plain body to route is validated as, or "".
func plainTextBodyAs(method string, route *routers.Route, mt string) string {
	if mt != "text/plain" || route == nil || route.Operation == nil || route.Operation.RequestBody == nil ||
		route.Operation.RequestBody.Value == nil || route.Operation.RequestBody.Value.Content.Get("text/plain") != nil {
		return ""
	}
	return plainTextBodies[method+" "+route.Path]
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
