package generic

import (
	"encoding/json"
	"testing"

	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
)

func jsonValue(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		want, got string
		ok        bool
	}{
		{`{"a":1}`, `{"a":1,"b":2}`, true}, // the API adds fields
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":{"x":1}}`, `{"a":{"x":1,"y":2}}`, true},
		{`{"a":{"x":1}}`, `{"a":null}`, false},
		{`{"a":null}`, `{}`, true}, // unset
		{`[1,2]`, `[1,2]`, true},
		{`[1,2]`, `[1,2,3]`, false},
		{`[{"k":1}]`, `[{"k":1,"extra":true}]`, true},
		{`"eu"`, `null`, false},
		{`true`, `true`, true},
	} {
		if got := Covers(jsonValue(t, c.want), jsonValue(t, c.got)); got != c.ok {
			t.Errorf("Covers(%s, %s) = %v", c.want, c.got, got)
		}
	}
}

func TestWriteOnlyHash(t *testing.T) {
	wo := []string{"secret", "hint"}
	h1 := WriteOnlyHash(map[string]any{"secret": "a", "hint": "WEUR", "other": 1}, wo)
	h2 := WriteOnlyHash(map[string]any{"hint": "WEUR", "secret": "a"}, wo)
	if h1 != h2 {
		t.Errorf("hash depends on unrelated fields or order: %s vs %s", h1, h2)
	}
	fields, known := parseWriteOnlyHash(h1)
	if !known || len(fields) != 2 || fields["hint"] != valueHash("WEUR") {
		t.Errorf("parse %s: %v %v", h1, fields, known)
	}
	if h := WriteOnlyHash(map[string]any{}, wo); h != "v1" {
		t.Errorf("no write-only fields set: %q, want v1 (known, empty)", h)
	}
	if _, known := parseWriteOnlyHash(""); known {
		t.Error("empty hash must be unknown (adopted resource)")
	}
	if WriteOnlyHash(map[string]any{"hint": "ENAM"}, wo) == WriteOnlyHash(map[string]any{"hint": "WEUR"}, wo) {
		t.Error("different values hash equal")
	}
}

func TestForProviderAndAtProvider(t *testing.T) {
	title := "t"
	kv := &kvv1alpha1.KVNamespace{}
	kv.Spec.ForProvider.Title = &title
	fp, err := ForProvider(kv)
	if err != nil || len(fp) != 1 || fp["title"] != "t" {
		t.Fatalf("ForProvider = %v, %v (unset fields must be absent)", fp, err)
	}
	q := &queuesv1alpha1.Queue{}
	stale := "stale"
	q.Status.AtProvider.QueueName = &stale
	if err := SetAtProvider(q, json.RawMessage(`{"queue_id":"x","settings":{"delivery_delay":5},"unknown":1}`)); err != nil {
		t.Fatal(err)
	}
	ap := q.Status.AtProvider
	if ap.QueueName != nil || ap.QueueID == nil || *ap.QueueID != "x" || ap.Settings == nil || *ap.Settings.DeliveryDelay != 5 {
		t.Errorf("SetAtProvider must replace, not merge: %+v", ap)
	}
	if err := SetAtProvider(q, json.RawMessage(`{"queue_id":5}`)); err == nil {
		t.Error("type mismatch accepted")
	}
	if q.Status.AtProvider.QueueID != nil {
		t.Error("a failed decode must leave atProvider empty")
	}
	if _, err := ForProvider(struct{}{}); err == nil {
		t.Error("ForProvider of a non-generated type")
	}
}

func TestNestedWriteOnly(t *testing.T) {
	desired := jsonValue(t, `{"settings":{"delivery_delay":5,"delivery_paused":false},"name":"q"}`).(map[string]any)
	wo := []string{"settings.delivery_paused"}
	if v, ok := pathValue(desired, "settings.delivery_paused"); !ok || v != false {
		t.Errorf("pathValue = %v, %v", v, ok)
	}
	if _, ok := pathValue(desired, "name.x"); ok {
		t.Error("pathValue through a scalar")
	}
	if got := nestedPaths(append(wo, "settings", "other.x"), "settings"); len(got) != 1 || got[0] != "delivery_paused" {
		t.Errorf("nestedPaths = %v", got)
	}
	stripped := without(desired["settings"], []string{"delivery_paused"})
	if !Covers(stripped, jsonValue(t, `{"delivery_delay":5,"message_retention_period":86400}`)) {
		t.Errorf("without = %v", stripped)
	}
	if _, still := desired["settings"].(map[string]any)["delivery_paused"]; !still {
		t.Error("without modified its input")
	}
	h := WriteOnlyHash(desired, wo)
	if fields, _ := parseWriteOnlyHash(h); fields["settings.delivery_paused"] != valueHash(false) {
		t.Errorf("hash %s lacks the nested path", h)
	}

	r := &Reconciler{Descriptor: Descriptor{WriteOnly: wo, UpdateFields: []string{"settings"}}}
	obs := jsonValue(t, `{"settings":{"delivery_delay":5,"message_retention_period":86400}}`).(map[string]any)
	prev, known := parseWriteOnlyHash(h)
	if r.differs("settings", desired["settings"], desired, obs, prev, known, true) {
		t.Error("a nested write-only field absent from GET counts as drift")
	}
	if !r.differs("settings", desired["settings"], desired, obs, nil, false, true) {
		t.Error("unknown write-only state must be applied once")
	}
	paused := jsonValue(t, `{"settings":{"delivery_delay":5,"delivery_paused":true}}`).(map[string]any)
	if !r.differs("settings", paused["settings"], paused, obs, prev, known, true) {
		t.Error("a changed nested write-only field is not detected")
	}
	drifted := jsonValue(t, `{"settings":{"delivery_delay":0}}`).(map[string]any)
	if !r.differs("settings", desired["settings"], desired, drifted, prev, known, true) {
		t.Error("drift of a readable sibling is not detected")
	}
}
