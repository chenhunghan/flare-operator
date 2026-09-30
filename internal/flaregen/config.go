package flaregen

import (
	"fmt"
	"os"
	"regexp"
	"strings"

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
	// ShortNames are kubectl short names of the kind. They must not collide with the short
	// names or resource names of core Kubernetes or common CRDs (flare-operator uses the
	// "cf" prefix); docs/api-reference.md lists them.
	ShortNames []string `json:"shortNames,omitempty"`
	// PrintColumns are kind-specific kubectl columns, shown after READY, SYNCED and
	// EXTERNAL-ID and before AGE. JSONPath must name a field of the CRD schema.
	PrintColumns []PrintColumn `json:"printColumns,omitempty"`

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

	// RequestHeaders send forProvider fields as request headers (header parameters of the
	// spec's operations) instead of in a body; see RequestHeader and generic.HeaderField.
	RequestHeaders []RequestHeader `json:"requestHeaders,omitempty"`
	// ObservedAs maps a top-level forProvider field to the top-level atProvider field that reads
	// it back under another name (e.g. storageClass: storage_class). The forProvider field is
	// then not derived write-only; drift is compared with the atProvider field.
	ObservedAs map[string]string `json:"observedAs,omitempty"`
	// SubResources are fixed sub-paths of the item (e.g. /cors) with GET and PUT operations,
	// managed as one forProvider field each; see generic.SubResource.
	SubResources []SubResourceConfig `json:"subResources,omitempty"`

	// Emulate selects how flarefake emulates the kind: "" (a hand-written profile in
	// internal/fake, or not at all) or "generic" (the descriptor-driven generic profile,
	// internal/fake/generic.go; listed in internal/fake/zz_generated_generic.go). See
	// docs/generator-scaleout.md.
	Emulate string `json:"emulate,omitempty"`

	// Why documents the overrides (recording numbers, UNVERIFIED notes). Not emitted.
	Why map[string]string `json:"why,omitempty"`
}

// RequestHeader maps a top-level forProvider field to a header parameter of the spec.
type RequestHeader struct {
	// Header is the header parameter's name, e.g. cf-r2-jurisdiction.
	Header string `json:"header"`
	// Field is the top-level forProvider field. For SentOn "all" it must not be a body field: it
	// is added to forProvider with the header parameter's schema. For SentOn "update" it may be a
	// create-body field (the update then carries it in the header instead).
	Field string `json:"field"`
	// SentOn is "all" (default): the header goes on every request of the object (create, get,
	// list, update, delete, sub-resources); the create operation must declare it, and the field
	// becomes Immutable (with a CEL rule that also refuses setting or clearing it once the
	// resource exists). "update": only the update request carries it; the update operation must
	// declare it, and the field becomes an UpdateField.
	SentOn string `json:"sentOn,omitempty"`
}

// SubResourceConfig is one sub-resource of generator.yaml subResources.
type SubResourceConfig struct {
	// Field is the top-level forProvider/atProvider field (it must not exist already).
	Field string `json:"field"`
	// Path is appended to the item path, e.g. /cors. The spec must define GET and PUT there:
	// forProvider.<field> takes the PUT body's schema, atProvider.<field> the GET result's.
	Path string `json:"path"`
	// ServerSet are dotted paths into the GET result (list elements traversed, e.g. rules.id)
	// of members the API may assign itself: compared only where forProvider sets them (see
	// generic.SubResource.ServerSet). Each path must exist in the GET result's schema.
	ServerSet []string `json:"serverSet,omitempty"`
}

// PrintColumn is one additional kubectl printer column.
type PrintColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string | integer | number | boolean | date
	JSONPath string `json:"jsonPath"`
	// Priority 0 shows the column in plain `kubectl get`; 1 only with -o wide.
	Priority    int32  `json:"priority,omitempty"`
	Description string `json:"description,omitempty"`
}

var (
	shortNameRe   = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	columnNameRe  = regexp.MustCompile(`^[A-Z][A-Z0-9-]*$`)
	columnPathRe  = regexp.MustCompile(`^(\.[a-zA-Z_][a-zA-Z0-9_]*)+$`)
	columnTypeSet = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "date": true}
)

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
		for _, sn := range k.ShortNames {
			if !shortNameRe.MatchString(sn) {
				return nil, fmt.Errorf("generator config: %s: shortName %q must match %s", k.FernGroup, sn, shortNameRe)
			}
		}
		for _, pc := range k.PrintColumns {
			switch {
			case !columnNameRe.MatchString(pc.Name):
				return nil, fmt.Errorf("generator config: %s: printColumns name %q must match %s", k.FernGroup, pc.Name, columnNameRe)
			case !columnTypeSet[pc.Type]:
				return nil, fmt.Errorf("generator config: %s: printColumns %s: type %q", k.FernGroup, pc.Name, pc.Type)
			case !columnPathRe.MatchString(pc.JSONPath):
				return nil, fmt.Errorf("generator config: %s: printColumns %s: jsonPath %q must be a plain dotted path such as .status.atProvider.name", k.FernGroup, pc.Name, pc.JSONPath)
			case pc.Priority < 0 || pc.Priority > 1:
				return nil, fmt.Errorf("generator config: %s: printColumns %s: priority %d (want 0 or 1)", k.FernGroup, pc.Name, pc.Priority)
			}
		}
		for _, h := range k.RequestHeaders {
			switch {
			case h.Header == "" || h.Field == "":
				return nil, fmt.Errorf("generator config: %s: requestHeaders need header and field", k.FernGroup)
			case h.SentOn != "" && h.SentOn != "all" && h.SentOn != "update":
				return nil, fmt.Errorf("generator config: %s: requestHeaders %s: sentOn %q (want all or update)", k.FernGroup, h.Header, h.SentOn)
			}
		}
		for _, sr := range k.SubResources {
			if sr.Field == "" || !strings.HasPrefix(sr.Path, "/") || strings.ContainsAny(sr.Path, "{}") {
				return nil, fmt.Errorf("generator config: %s: subResources need a field and a literal path starting with / (got %q, %q)",
					k.FernGroup, sr.Field, sr.Path)
			}
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
