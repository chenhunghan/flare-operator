package kindsuite_test

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/generic/descriptors"
	"github.com/chenhunghan/flare-operator/internal/generic/kindsuite"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

var env *testenv.Env

// TestMain starts envtest and an in-process flarefake that serves the generator.yaml
// `emulate: generic` kinds with the generic profile. Outside -short the fake also journals
// spec violations of every request (the suite fails on them).
func TestMain(m *testing.M) {
	flag.Parse()
	opts := testenv.Options{}
	if !testing.Short() {
		spec, err := fake.LoadDefaultSpec()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load spec:", err)
			os.Exit(1)
		}
		opts.Fake.Spec = spec
		opts.Fake.Generic = fake.GeneratedGenericKinds()
	}
	testenv.Main(m, &env, opts)
}

// TestKinds runs the suite for every generated kind: the hand-written-profile kinds (KV,
// Queues, D1) prove the suite, the generic-profile kinds prove the profile.
func TestKinds(t *testing.T) {
	kindsuite.Run(t, env, descriptors.Entries(), kindsuite.Options{
		Skip: func(e descriptors.Entry) string {
			if testing.Short() && kindsuite.IsGeneric(e) {
				return "the generic profile needs the pinned spec (skipped with -short)"
			}
			return ""
		},
	})
}
