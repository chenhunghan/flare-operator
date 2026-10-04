package reconcile_test

import (
	"testing"

	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// TestMain validates every flarefake response against the pinned spec (testenv.StrictMain).
func TestMain(m *testing.M) { testenv.StrictMain(m) }
