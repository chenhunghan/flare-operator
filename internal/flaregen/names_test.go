package flaregen

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckNames(t *testing.T) {
	a := KindNames{Kind: "Rule", Plural: "rules", Singular: "rule", ShortNames: []string{"cfrule"}, Source: "a"}
	b := KindNames{Kind: "Rule", Plural: "rules", Singular: "rule", Source: "b"}
	c := KindNames{Kind: "Settings", Plural: "settings", Singular: "settings", ShortNames: []string{"cfrule"}, Source: "c"}
	d := KindNames{Kind: "Policy", Plural: "policies", Singular: "policy", ShortNames: []string{"settings"}, Source: "d"}

	if got := CheckNames([]KindNames{a, d}); len(got) != 0 {
		t.Errorf("distinct kinds: %v", got)
	}
	// A kind's own repeated names (plural = singular) are not a collision.
	if got := CheckNames([]KindNames{c}); len(got) != 0 {
		t.Errorf("own names: %v", got)
	}
	var got []string
	for _, x := range CheckNames([]KindNames{a, b, c, d}) {
		var who []string
		for _, k := range x.Kinds {
			who = append(who, k.Source)
		}
		got = append(got, x.What+" "+x.Name+" "+strings.Join(who, ","))
	}
	want := []string{
		"kind Rule a,b",
		"resource name cfrule a,c",   // a short name of one kind, another's short name
		"resource name rule a,b",     // singulars
		"resource name rules a,b",    // plurals
		"resource name settings c,d", // a plural/singular of one kind, a short name of another
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CheckNames:\n got %q\nwant %q", got, want)
	}
}

// TestGenerateNameCollision: a configured kind that takes a name of another configured kind or
// of a hand-written kind fails the run with the fix in the message; nothing is renamed.
func TestGenerateNameCollision(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("testdata", "generator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := loadFragment(t)
	opts := Options{Module: "example.com/m"}
	if _, err := Generate(doc, cfg, opts); err != nil {
		t.Fatalf("golden config: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate      func(c *Config)
		handWritten []KindNames
		want        string
	}{
		"hand-written kind": {
			handWritten: []KindNames{{Kind: "Widget", Plural: "widgets", Singular: "widget", Source: "hand-written CRD x.yaml"}},
			want:        `kind "Widget" is claimed by Widget (hand-written CRD x.yaml) and Widget (generator.yaml widgets.gadgets`,
		},
		"hand-written short name": {
			handWritten: []KindNames{{Kind: "Gizmo", Plural: "gizmos", Singular: "gizmo", ShortNames: []string{"cfwidget"}, Source: "hand-written CRD g.yaml"}},
			want:        `resource name "cfwidget" is claimed by Gizmo (hand-written CRD g.yaml) and Widget`,
		},
		"configured kind": {
			mutate: func(c *Config) { c.Kinds[1].Kind = "Widget" },
			want:   `kind "Widget" is claimed by Widget (generator.yaml widgets.gadgets`,
		},
		"configured plural": {
			mutate: func(c *Config) { c.Kinds[1].Plural = "widgets" },
			want:   `resource name "widgets" is claimed by Widget`,
		},
		"configured short name": {
			mutate: func(c *Config) { c.Kinds[1].ShortNames = []string{"cfwidget"} },
			want:   `resource name "cfwidget" is claimed by Widget`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := LoadConfig(filepath.Join("testdata", "generator.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(c)
			}
			o := opts
			o.HandWritten = tc.handWritten
			out, err := Generate(doc, c, o)
			var ce *CollisionError
			if !errors.As(err, &ce) {
				t.Fatalf("Generate = %v, %v; want a *CollisionError", out, err)
			}
			msg := err.Error()
			for _, s := range []string{tc.want, "API group flare.dev", "kind: and plural:", "never renames"} {
				if !strings.Contains(msg, s) {
					t.Errorf("error does not contain %q:\n%s", s, msg)
				}
			}
		})
	}
}

// TestLoadHandWrittenNames reads the controller-gen CRDs of the hand-written kinds and skips
// the flaregen ones.
func TestLoadHandWrittenNames(t *testing.T) {
	names, err := LoadHandWrittenNames(filepath.Join(repoRoot(t), CRDDir), DefaultGroup)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, n := range names {
		kinds = append(kinds, n.Kind)
		if n.Plural == "" || n.Singular == "" || len(n.ShortNames) == 0 || !strings.HasPrefix(n.Source, "hand-written CRD ") {
			t.Errorf("incomplete names: %+v", n)
		}
	}
	want := []string{"CloudflareAccount", "PagesDeployment", "PagesProject", "Tunnel", "VPCService", "WorkerScript"}
	if !reflect.DeepEqual(sortedStrings(kinds), want) {
		t.Errorf("hand-written kinds = %v, want %v", kinds, want)
	}
	if other, err := LoadHandWrittenNames(filepath.Join(repoRoot(t), CRDDir), "other.example.com"); err != nil || len(other) != 0 {
		t.Errorf("another group: %v, %v", other, err)
	}
}
