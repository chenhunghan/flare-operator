package fake

import (
	"testing"
)

// The settings read (GET, and PATCH's echo) withholds the writeOnly secret fields of secret_text
// and secret_key bindings (UNVERIFIED: spec writeOnly; no recording reads a secret back) and keeps
// every other binding as uploaded (0065); the result satisfies the pinned spec.
func TestWorkerSettingsWithholdSecrets(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	meta := map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01", "bindings": []any{
		map[string]any{"type": "plain_text", "name": "MODE", "text": "a"},
		map[string]any{"type": "secret_text", "name": "TOKEN", "text": "hunter2"},
		map[string]any{"type": "secret_key", "name": "KEY", "format": "raw", "algorithm": map[string]any{"name": "HMAC", "hash": "SHA-256"},
			"usages": []any{"sign", "verify"}, "key_base64": "c2VjcmV0"},
	}}
	if st, env, _ := c.upload("w", meta); st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	check := func(what string, env testEnv) {
		t.Helper()
		bindings, _ := resultMap(t, env)["bindings"].([]any)
		if len(bindings) != 3 {
			t.Fatalf("%s: bindings %v", what, bindings)
		}
		want := map[string]map[string]bool{
			"MODE":  {"type": true, "name": true, "text": true},
			"TOKEN": {"type": true, "name": true},
			"KEY":   {"type": true, "name": true, "format": true, "algorithm": true, "usages": true},
		}
		for _, b := range bindings {
			bm := b.(map[string]any)
			w := want[bm["name"].(string)]
			if len(bm) != len(w) {
				t.Errorf("%s: binding %v, want keys %v", what, bm, w)
			}
			for k := range bm {
				if !w[k] {
					t.Errorf("%s: binding %s returns %s", what, bm["name"], k)
				}
			}
		}
	}
	_, env, _ := c.do("GET", acct+"/workers/scripts/w/settings", nil)
	check("GET", env)
	body, ct := multipartBody(t, "settings", map[string]any{"logpush": true}, nil)
	st, env, _ := c.raw("PATCH", acct+"/workers/scripts/w/settings", ct, body)
	if st != 200 {
		t.Fatalf("patch: %d %v", st, env.Errors)
	}
	check("PATCH", env)
	for _, v := range s.ResponseViolations() {
		if v.Allowed == "" {
			t.Errorf("response violates the pinned spec: %s", v)
		}
	}
}
