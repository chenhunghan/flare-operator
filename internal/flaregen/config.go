package flaregen

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is generator.yaml: which resources become kinds, with per-kind overrides.
type Config struct {
	// Version of every generated API group (default v1alpha1).
	Version string `json:"version,omitempty"`
	// GroupSuffix is appended to the product (default cloudflare.flare.dev).
	GroupSuffix string       `json:"groupSuffix,omitempty"`
	Kinds       []KindConfig `json:"kinds"`
}

// KindConfig selects one resource of the model and overrides what the model derives.
type KindConfig struct {
	// FernGroup is the x-fern-sdk-group-name of the resource's operations (required).
	FernGroup string `json:"fernGroup"`
	// Path disambiguates when a fern group holds several resources: the
	// collection path (CRUD) or the item path (singleton), exactly as in the spec.
	Path string `json:"path,omitempty"`

	Kind  string `json:"kind,omitempty"`  // default: singular of the last fern segment
	Group string `json:"group,omitempty"` // product; default: first fern segment without "_"
	// Plural is the CRD resource name (default: lower-case Kind, pluralised).
	Plural string `json:"plural,omitempty"`

	IDField      string `json:"idField,omitempty"`
	NameField    string `json:"nameField,omitempty"`    // "-" disables adoption by name
	UpdateMethod string `json:"updateMethod,omitempty"` // PUT | PATCH | "-" (none)

	// Immutable and WriteOnly are added to what the model derives
	// (create-only fields and fields the GET response never returns).
	Immutable []string `json:"immutable,omitempty"`
	WriteOnly []string `json:"writeOnly,omitempty"`
	// NotWriteOnly removes derived write-only fields (the API does return them).
	NotWriteOnly []string `json:"notWriteOnly,omitempty"`

	DefaultDeletionPolicy string `json:"defaultDeletionPolicy,omitempty"` // Delete | Orphan
	// TagResourceType is the resource_type of the account-level Resource Tagging API
	// (/accounts/{account_id}/tags) for ownership tags; "" disables ownership tagging.
	TagResourceType string `json:"tagResourceType,omitempty"`
	ListOrder       string `json:"listOrder,omitempty"`

	// Fields overrides schema facts per dotted JSON path (e.g. "settings.delivery_delay"),
	// applied to both forProvider and atProvider wherever the path exists.
	Fields map[string]FieldOverride `json:"fields,omitempty"`

	// Emulate selects how flarefake emulates the kind: "" (a hand-written profile in
	// internal/fake, or not at all) or "generic" (the descriptor-driven generic profile,
	// internal/fake/generic.go; listed in internal/fake/zz_generated_generic.go). See
	// docs/generator-scaleout.md.
	Emulate string `json:"emulate,omitempty"`

	// Why documents the overrides (recording numbers, UNVERIFIED notes). Not emitted.
	Why map[string]string `json:"why,omitempty"`
}

// FieldOverride corrects the spec where recordings show different behavior.
type FieldOverride struct {
	Type string `json:"type,omitempty"` // string | integer | number | boolean
	Enum []any  `json:"enum,omitempty"` // replaces the spec's enum
	// DropEnum removes the spec's enum entirely.
	DropEnum bool `json:"dropEnum,omitempty"`
}

// LoadConfig reads generator.yaml.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses generator.yaml content and fills defaults.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("generator config: %w", err)
	}
	if c.Version == "" {
		c.Version = "v1alpha1"
	}
	if c.GroupSuffix == "" {
		c.GroupSuffix = "cloudflare.flare.dev"
	}
	for i, k := range c.Kinds {
		if k.FernGroup == "" {
			return nil, fmt.Errorf("generator config: kinds[%d]: fernGroup is required", i)
		}
		switch k.DefaultDeletionPolicy {
		case "", "Delete", "Orphan":
		default:
			return nil, fmt.Errorf("generator config: %s: defaultDeletionPolicy %q", k.FernGroup, k.DefaultDeletionPolicy)
		}
		switch k.UpdateMethod {
		case "", "PUT", "PATCH", "-":
		default:
			return nil, fmt.Errorf("generator config: %s: updateMethod %q", k.FernGroup, k.UpdateMethod)
		}
		switch k.Emulate {
		case "", "generic":
		default:
			return nil, fmt.Errorf("generator config: %s: emulate %q (want \"generic\" or nothing)", k.FernGroup, k.Emulate)
		}
		for p, o := range k.Fields {
			switch o.Type {
			case "", "string", "integer", "number", "boolean":
			default:
				return nil, fmt.Errorf("generator config: %s: fields.%s: type %q", k.FernGroup, p, o.Type)
			}
		}
	}
	return &c, nil
}
