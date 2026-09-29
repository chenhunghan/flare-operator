package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func withVars(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := Version, Commit, Date
	Version, Commit, Date = v, c, d
	t.Cleanup(func() { Version, Commit, Date = ov, oc, od })
}

func buildInfo(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Settings: settings}, true }
}

func TestLdflagsWin(t *testing.T) {
	withVars(t, "v1.2.3", "abc123", "2026-09-29T00:00:00Z")
	in := get(buildInfo(debug.BuildSetting{Key: "vcs.revision", Value: "fromvcs"}, debug.BuildSetting{Key: "vcs.time", Value: "t"}))
	if in.Version != "v1.2.3" || in.Commit != "abc123" || in.Date != "2026-09-29T00:00:00Z" {
		t.Fatalf("got %+v", in)
	}
}

func TestVCSFallback(t *testing.T) {
	withVars(t, "dev", "", "")
	in := get(buildInfo(
		debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	))
	if in.Version != "dev" || in.Commit != "deadbeef" || in.Date != "2026-01-02T03:04:05Z" || !in.Modified {
		t.Fatalf("got %+v", in)
	}
	s := in.String("flare-operator")
	if !strings.HasPrefix(s, "flare-operator dev (commit deadbeef-dirty, built 2026-01-02T03:04:05Z, go") {
		t.Fatalf("String() = %q", s)
	}
}

func TestNoBuildInfo(t *testing.T) {
	withVars(t, "dev", "", "")
	in := get(func() (*debug.BuildInfo, bool) { return nil, false })
	if in.Commit != "unknown" || in.Date != "unknown" || in.Platform == "" || in.GoVersion == "" {
		t.Fatalf("got %+v", in)
	}
	kv := in.KeysAndValues()
	if len(kv)%2 != 0 || kv[0] != "version" || kv[1] != "dev" {
		t.Fatalf("KeysAndValues() = %v", kv)
	}
}
