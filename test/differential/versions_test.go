package differential

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// TestPinnedVersionsMatch runs in `make test` (no build tag): the versions the differential
// tests look for must be the ones `make differential-tools` installs.
func TestPinnedVersionsMatch(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"WRANGLER_VERSION": WranglerVersion, "CLOUDFLARED_VERSION": CloudflaredVersion} {
		m := regexp.MustCompile(`(?m)^` + name + ` \?= (\S+)$`).FindSubmatch(mk)
		if m == nil || string(m[1]) != want {
			t.Errorf("Makefile %s = %q, want %q (test/differential/versions.go)", name, m, want)
		}
	}
	b, err := os.ReadFile("npm/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct{ Dependencies map[string]string }
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	if got := pkg.Dependencies["wrangler"]; got != WranglerVersion {
		t.Errorf("npm/package.json pins wrangler %q, want %q", got, WranglerVersion)
	}
	lock, err := os.ReadFile("npm/package-lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Packages map[string]struct{ Version string } `json:"packages"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		t.Fatal(err)
	}
	if got := l.Packages["node_modules/wrangler"].Version; got != WranglerVersion {
		t.Errorf("npm/package-lock.json resolves wrangler %q, want %q", got, WranglerVersion)
	}
}
