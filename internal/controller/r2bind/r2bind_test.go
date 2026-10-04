package r2bind_test

import (
	"testing"

	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/r2bind"
)

func str(s string) *string { return &s }

func TestOf(t *testing.T) {
	bucket := func(spec, observed *string) *r2v1alpha1.R2Bucket {
		rb := &r2v1alpha1.R2Bucket{}
		rb.Status.ID = "media"
		rb.Spec.ForProvider.Jurisdiction = spec
		rb.Status.AtProvider.Jurisdiction = observed
		return rb
	}
	for name, tc := range map[string]struct {
		rb   *r2v1alpha1.R2Bucket
		want string
	}{
		"none":                 {bucket(nil, nil), ""},
		"default":              {bucket(str("default"), str("default")), ""},
		"spec only":            {bucket(str("eu"), nil), "eu"},
		"observed wins":        {bucket(nil, str("fedramp")), "fedramp"},
		"observed default":     {bucket(nil, str("default")), ""},
		"observed and spec eu": {bucket(str("eu"), str("eu")), "eu"},
	} {
		b, j := r2bind.Of(tc.rb)
		if b != "media" || j != tc.want {
			t.Errorf("%s: Of = %q, %q; want media, %q", name, b, j, tc.want)
		}
	}
	if b, _ := r2bind.Of(&r2v1alpha1.R2Bucket{}); b != "" {
		t.Errorf("an R2Bucket without status.id binds %q", b)
	}
}
