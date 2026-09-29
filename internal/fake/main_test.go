package fake

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// TestMain turns on strict response validation for every Server the package's tests create
// (except under -short, which skips loading the 26 MB spec), and fails the run when any
// emulated response violated the pinned spec without an allowlist entry.
func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		spec, err := LoadDefaultSpec()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load spec:", err)
			os.Exit(1)
		}
		EnableStrictResponses(spec)
	}
	code := m.Run()
	if err := StrictResponseError(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
