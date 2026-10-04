// Command flarefake runs the Cloudflare API emulator as a standalone HTTP server, for kind/e2e
// tests or local development. Point a Cloudflare client at http://<addr>/client/v4.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/version"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	specPath := flag.String("spec", "", "validate requests against this OpenAPI spec (e.g. spec/openapi.json.gz); empty disables")
	reject := flag.Bool("reject-schema-violations", false, "answer schema-invalid requests with 400 instead of only journaling them")
	generic := flag.Bool("generic", true, "emulate the generator.yaml kinds marked 'emulate: generic' with the generic profile (UNVERIFIED; needs -spec)")
	validateResponses := flag.Bool("validate-responses", true, "with -spec, also check every emulated response against the spec's response schema; violations are journaled, logged and listed at GET /_fake/response_violations, never answered")
	logLag := flag.Duration("log-ingestion-lag", 0, "how long injected Workers log events take to become queryable (the real service: 15-30s)")
	tailURLBase := flag.String("tail-url-base", "", "ws:// or wss:// base of the tail WebSocket URLs (default: the scheme and host a request came in on)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get().String("flarefake"))
		return
	}
	log.Print(version.Get().String("flarefake"))

	opts := fake.Options{RejectSchemaViolations: *reject, ValidateResponses: *validateResponses,
		LogIngestionLag: *logLag, TailURLBase: *tailURLBase}
	opts.OnResponseViolation = func(v fake.ResponseViolation) {
		if v.Allowed == "" {
			log.Printf("response schema violation: %s", v)
		}
	}
	if *specPath != "" {
		spec, err := fake.LoadSpec(*specPath)
		if err != nil {
			log.Fatalf("load spec: %v", err)
		}
		opts.Spec = spec
		if *generic {
			opts.Generic = fake.GeneratedGenericKinds()
		}
	}
	log.Printf("flarefake listening on http://%s (API at /client/v4, control at /_fake)", *addr)
	log.Fatal(http.ListenAndServe(*addr, fake.New(opts)))
}
