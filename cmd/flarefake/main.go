// Command flarefake runs the Cloudflare API emulator as a standalone HTTP server, for kind/e2e
// tests or local development. Point a Cloudflare client at http://<addr>/client/v4.
package main

import (
	"flag"
	"log"
	"net/http"

	"flare.dev/operator/internal/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	specPath := flag.String("spec", "", "validate requests against this OpenAPI spec (e.g. spec/openapi.json.gz); empty disables")
	reject := flag.Bool("reject-schema-violations", false, "answer schema-invalid requests with 400 instead of only journaling them")
	generic := flag.Bool("generic", true, "emulate the generator.yaml kinds marked 'emulate: generic' with the generic profile (UNVERIFIED; needs -spec)")
	flag.Parse()

	opts := fake.Options{RejectSchemaViolations: *reject}
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
