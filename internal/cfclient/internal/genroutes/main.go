// Command genroutes writes internal/cfclient/zz_generated_routes.go: every path template of the
// pinned OpenAPI spec (spec/openapi.json.gz), which cfclient's metrics use as the bounded
// route_template label. Run it with `go generate ./internal/cfclient/` (from any directory).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"flare.dev/operator/internal/cfclient/internal/routespec"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	paths, err := routespec.SpecPaths(filepath.Join(root, "spec", "openapi.json.gz"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	src, err := routespec.Render(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "cfclient", "zz_generated_routes.go"), src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
