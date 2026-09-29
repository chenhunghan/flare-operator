package kindsuite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/testenv"
)

// Every generated kind either synthesizes a forProvider with its required fields or has a
// fixture; Mutate changes exactly one field.
func TestSynthesizeEveryKind(t *testing.T) {
	for _, e := range descriptors.Entries() {
		t.Run(e.Kind, func(t *testing.T) {
			s, err := LoadSchema(testenv.RepoRoot(), e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join("testdata", e.Kind+".json")); err == nil {
				return // the fixture decides
			}
			fp, err := s.Synthesize("flare-spike-0abc")
			if err != nil {
				t.Fatalf("synthesize: %v", err)
			}
			for _, f := range s.Required {
				if _, ok := fp[f]; !ok {
					t.Errorf("required %s missing in %v", f, fp)
				}
			}
			if e.UpdateMethod == "" {
				return
			}
			out, what, ok := s.Mutate(fp, e.UpdateFields, nil)
			if !ok {
				t.Fatalf("no mutable field in %v", e.UpdateFields)
			}
			a, _ := json.Marshal(fp)
			b, _ := json.Marshal(out)
			if string(a) == string(b) {
				t.Errorf("Mutate(%s) changed nothing: %s", what, a)
			}
		})
	}
}

func TestSynthesizeHonorsSchema(t *testing.T) {
	e, ok := descriptors.Lookup("aigateway.cloudflare.flare.dev", "AIGateway")
	if !ok {
		t.Skip("no AIGateway kind")
	}
	s, err := LoadSchema(testenv.RepoRoot(), e)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := s.Synthesize("flare-spike-0abc")
	if err != nil {
		t.Fatal(err)
	}
	// id: pattern ^[a-z0-9_]+(?:-[a-z0-9_]+)*$, maxLength 64.
	if id, _ := fp["id"].(string); !regexp.MustCompile(`^[a-z0-9_]+(?:-[a-z0-9_]+)*$`).MatchString(id) {
		t.Errorf("id %q", fp["id"])
	}
	// Numbers respect minimum 0 and are whole for integer fields.
	if v, ok := fp["cache_ttl"].(int64); !ok || v < 0 {
		t.Errorf("cache_ttl %v (%T)", fp["cache_ttl"], fp["cache_ttl"])
	}
	out, what, ok := s.Mutate(fp, []string{"cache_ttl"}, nil)
	if !ok || what != "cache_ttl" || out["cache_ttl"] == fp["cache_ttl"] {
		t.Errorf("Mutate cache_ttl: %v %q %v", ok, what, out)
	}
}
