package descriptors_test

import (
	"testing"

	"flare.dev/operator/internal/testenv"
)

// TestMain validates every flarefake response against the pinned spec (testenv.StrictMain).
func TestMain(m *testing.M) { testenv.StrictMain(m) }
