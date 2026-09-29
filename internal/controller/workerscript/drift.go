package workerscript

import (
	"encoding/json"
	"sort"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/generic"
)

// jsonValue round-trips v through encoding/json, so it compares with decoded API responses
// (numbers become float64, structs maps).
func jsonValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// settingsDrift lists the settings that GET …/settings reports differently from want (nil when
// in sync). Only what the settings read shows is compared (0065, 0091): an unset
// compatibility_date or logpush is not managed; a secret_text binding's value is never returned
// and is tracked by status.writeOnlyHash instead; observability only when the read reports it
// (UNVERIFIED whether it does). Bindings are matched by name and must agree on every field the
// desired binding sets (the API may add fields).
func settingsDrift(want apiSettingsBody, got *apiSettings) []string {
	var out []string
	if want.CompatibilityDate != "" && want.CompatibilityDate != got.CompatibilityDate {
		out = append(out, "compatibility_date")
	}
	if !sameSet(want.CompatibilityFlags, got.CompatibilityFlags) {
		out = append(out, "compatibility_flags")
	}
	if want.Logpush != nil && (got.Logpush == nil || *want.Logpush != *got.Logpush) {
		out = append(out, "logpush")
	}
	if !bindingsMatch(want.Bindings, got.Bindings) {
		out = append(out, "bindings")
	}
	if want.Observability != nil && got.Observability != nil && !generic.Covers(jsonValue(want.Observability), jsonValue(got.Observability)) {
		out = append(out, "observability")
	}
	return out
}

func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func bindingsMatch(want, got []map[string]any) bool {
	if len(want) != len(got) {
		return false
	}
	byName := map[string]map[string]any{}
	for _, g := range got {
		n, _ := g["name"].(string)
		byName[n] = g
	}
	for _, w := range want {
		n, _ := w["name"].(string)
		g, ok := byName[n]
		if !ok {
			return false
		}
		cmp := w
		if w["type"] == workersv1alpha1.BindingSecretText {
			cmp = map[string]any{"name": w["name"], "type": w["type"]}
		}
		if !generic.Covers(jsonValue(cmp), jsonValue(g)) {
			return false
		}
	}
	return true
}
