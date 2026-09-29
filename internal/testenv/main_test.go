package testenv

import "testing"

// TestMain validates every flarefake response against the pinned spec (StrictMain).
func TestMain(m *testing.M) { StrictMain(m) }
