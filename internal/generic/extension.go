package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"flare.dev/operator/internal/cfclient"
)

// Extension is per-kind behavior that Descriptor (a frozen contract) cannot express. cmd/flaregen
// emits it from generator.yaml (requestHeaders, observedAs, subResources) into
// descriptors.Entry, and kinds.NewReconciler hands it to the Reconciler. The zero value changes
// nothing, so kinds without these options reconcile exactly as before.
//
// See docs/generator-scaleout.md ("Per-kind extensions") for when to use each hook.
type Extension struct {
	// Headers are forProvider fields sent as request headers instead of in a request body.
	Headers []HeaderField
	// ObservedAs maps a top-level forProvider field to the top-level atProvider field that reads
	// it back under another name (R2: forProvider.storageClass is atProvider.storage_class).
	// Drift, immutability and PUT bodies compare with the atProvider field.
	ObservedAs map[string]string
	// SubResources are fixed sub-paths of the item managed as one forProvider field each.
	SubResources []SubResource
}

// HeaderField sends a top-level string field of forProvider as a request header.
type HeaderField struct {
	Header string // e.g. "cf-r2-jurisdiction"
	Field  string // top-level forProvider field, e.g. "jurisdiction"
	// Update: only the update request carries the header (the field is an UpdateField; R2's
	// PATCH sets cf-r2-storage-class and has no body). Otherwise the header goes on every request
	// the object makes for its resource (create, get, list, update, delete, sub-resources): the
	// field selects where the resource lives, so it is Immutable, and once the resource exists a
	// change is refused before any request (a GET with another value would not find the resource
	// and the reconciler would create a second one).
	Update bool
	// Default is the value the API assumes without the header (the spec's parameter default;
	// "" when it has none). An unset field and one set to Default address the same resource.
	Default string
}

// SubResource is a fixed sub-path of the item (R2: <item>/cors) managed as the top-level
// forProvider field Field:
//
//   - GET <item><Path> → status.atProvider.<Field>. A 404 means "not configured": the field is
//     absent from atProvider (the item itself was read just before).
//   - forProvider.<Field> set and not covered by the observed value (Covers) → PUT <item><Path>
//     with it as the body. This is an update: managementPolicies must allow Update, otherwise
//     Synced=False names the field. Observe-only objects only read.
//   - forProvider.<Field> unset → not managed: whatever Cloudflare has is left as is (and shown
//     in atProvider). To clear it, set the empty value the API accepts (R2 CORS: {"rules": []}).
//   - The sub-resource goes with the item: nothing is deleted separately.
type SubResource struct {
	Field string // top-level forProvider/atProvider field, e.g. "cors"
	Path  string // appended to ItemPath, e.g. "/cors"
}

// ObservedName is the atProvider field that reads back forProvider field f.
func (x Extension) ObservedName(f string) string {
	if o, ok := x.ObservedAs[f]; ok && o != "" {
		return o
	}
	return f
}

// scopeHeader builds the headers every request of the object carries (the non-Update Headers
// that forProvider sets).
func (x Extension) scopeHeader(desired map[string]any) http.Header {
	var h http.Header
	for _, hf := range x.Headers {
		if hf.Update {
			continue
		}
		if v, ok := desired[hf.Field].(string); ok && v != "" {
			if h == nil {
				h = http.Header{}
			}
			h.Set(hf.Header, v)
		}
	}
	return h
}

// effective is the value a non-Update header field addresses: the field, else the default.
func (h HeaderField) effective(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return h.Default
}

// headerChanges lists the non-Update header fields whose desired value addresses another place
// than the one the resource was last read from (status.atProvider): they must not change once
// the resource exists (HeaderField.Update). A field atProvider does not report is not checked.
func (x Extension) headerChanges(desired, lastObserved map[string]any) []string {
	var out []string
	for _, h := range x.Headers {
		if h.Update {
			continue
		}
		was, ok := lastObserved[x.ObservedName(h.Field)].(string)
		if !ok || was == "" {
			continue
		}
		if h.effective(desired[h.Field]) != was {
			out = append(out, h.Field)
		}
	}
	return out
}

// moveUpdateHeaders moves the Update header fields out of an update body into req's headers.
// When that empties the body, the request is sent without one (R2's PATCH defines none).
func (x Extension) moveUpdateHeaders(req *cfclient.Request, body map[string]any) {
	moved := false
	for _, h := range x.Headers {
		if !h.Update {
			continue
		}
		v, ok := body[h.Field]
		if !ok {
			continue
		}
		delete(body, h.Field)
		moved = true
		if v == nil {
			continue
		}
		if req.Header == nil {
			req.Header = http.Header{}
		}
		s, isString := v.(string)
		if !isString {
			b, _ := json.Marshal(v)
			s = string(b)
		}
		req.Header.Set(h.Header, s)
	}
	if moved && len(body) == 0 {
		req.Body = nil
	}
}

// subPath is the path of a sub-resource of the item id.
func (s SubResource) subPath(sc scope, itemPath, id string) string {
	return sc.path(itemPath, id) + s.Path
}

// observeSubResources reads every sub-resource of id into obs (a 404 leaves the field absent).
func (r *Reconciler) observeSubResources(ctx context.Context, sc scope, id string, obs map[string]any) error {
	for _, s := range r.Extension.SubResources {
		delete(obs, s.Field)
		resp, err := sc.do(ctx, cfclient.Request{Method: http.MethodGet, Path: s.subPath(sc, r.Descriptor.ItemPath, id)})
		if cfclient.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", s.Field, err)
		}
		if len(resp.Result) == 0 || string(resp.Result) == "null" {
			continue
		}
		var v any
		if err := json.Unmarshal(resp.Result, &v); err != nil {
			return fmt.Errorf("read %s: %w", s.Field, err)
		}
		obs[s.Field] = v
	}
	return nil
}

// changedSubResources lists the sub-resources whose desired value is set and not covered by
// the observed one.
func (r *Reconciler) changedSubResources(desired, obs map[string]any) []SubResource {
	var out []SubResource
	for _, s := range r.Extension.SubResources {
		if v, ok := desired[s.Field]; ok && v != nil && !Covers(v, obs[s.Field]) {
			out = append(out, s)
		}
	}
	return out
}

// putSubResource writes the desired value of one sub-resource.
func (r *Reconciler) putSubResource(ctx context.Context, sc scope, id string, s SubResource, desired map[string]any) error {
	_, err := sc.do(ctx, cfclient.Request{Method: http.MethodPut, Path: s.subPath(sc, r.Descriptor.ItemPath, id), Body: desired[s.Field]})
	if err != nil {
		return fmt.Errorf("update %s: %w", s.Field, err)
	}
	return nil
}

func subFields(ss []SubResource) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Field
	}
	return out
}

// lastObserved returns status.atProvider as JSON values (nil when unset or unreadable).
func lastObserved(obj any) map[string]any {
	v, err := structField(obj, "Status", "AtProvider")
	if err != nil {
		return nil
	}
	m, err := toMap(v.Interface())
	if err != nil {
		return nil
	}
	return m
}

// joinFields renders field names for condition messages.
func joinFields(fs []string) string { return strings.Join(fs, ", ") }
