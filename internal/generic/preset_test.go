package generic

import (
	"testing"
)

// TestVectorizePresetNotImmutableDrift: a VectorizeIndex created from config.preset is read
// back with config {dimensions, metric} and no preset (the GET result's config is
// vectorize_index-dimension-configuration in the pinned spec). The immutable check must not
// take the missing preset for a change, or the object stays Synced=False/Immutable forever.
// A changed preset is still caught through the write-only hash.
func TestVectorizePresetNotImmutableDrift(t *testing.T) {
	// The generated descriptor's lists (internal/generic/descriptors cannot be imported here).
	r := &Reconciler{Descriptor: Descriptor{
		Kind: "VectorizeIndex", IDField: "name", NameField: "name",
		Immutable:    []string{"config", "description", "name"},
		CreateFields: []string{"config", "description", "name"},
		WriteOnly:    []string{"config.preset"},
	}}
	desired := map[string]any{"name": "idx", "config": map[string]any{"preset": "@cf/baai/bge-small-en-v1.5"}}
	obs := map[string]any{"name": "idx", "config": map[string]any{"dimensions": float64(384), "metric": "cosine"}}
	prev, known := parseWriteOnlyHash(WriteOnlyHash(desired, writeOnlyUnder(r.Descriptor.WriteOnly, r.Descriptor.CreateFields)))
	if !known {
		t.Fatal("write-only hash not recorded")
	}
	if imm := r.immutableChanges(desired, obs, prev, known); len(imm) != 0 {
		t.Errorf("preset index read back with dimensions and metric: immutable changes %v, want none", imm)
	}
	changed := map[string]any{"name": "idx", "config": map[string]any{"preset": "@cf/baai/bge-base-en-v1.5"}}
	if imm := r.immutableChanges(changed, obs, prev, known); len(imm) != 1 || imm[0] != "config" {
		t.Errorf("changed preset: immutable changes %v, want [config]", imm)
	}
	// Dimensions and metric are still compared with what Cloudflare reports.
	dims := map[string]any{"name": "idx", "config": map[string]any{"dimensions": float64(768), "metric": "cosine"}}
	if imm := r.immutableChanges(dims, obs, prev, known); len(imm) != 1 || imm[0] != "config" {
		t.Errorf("changed dimensions: immutable changes %v, want [config]", imm)
	}
}
