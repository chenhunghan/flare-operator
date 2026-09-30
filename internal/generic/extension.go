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
//   - forProvider.<Field> set to a non-empty value that the observed one does not cover (Covers)
//     → PUT <item><Path> with it as the body. This is an update: managementPolicies must allow
//     Update, otherwise Synced=False names the field. Observe-only objects only read.
//   - forProvider.<Field> set to an empty value (isEmptyValue: {} or, since the generated types
//     drop empty lists, {"rules": []} for R2 CORS) → clear it: when Cloudflare has a non-empty
//     value, DELETE <item><Path> if the spec has that operation (Delete), else PUT the empty
//     value. An empty or absent observed value is in sync. (Covers alone cannot see this: the
//     empty value covers everything, so a policy the user meant to revoke would stay and the
//     object would report Synced=True.)
//   - forProvider.<Field> unset → not managed: whatever Cloudflare has is left as is (and shown
//     in atProvider).
//   - The sub-resource goes with the item: nothing is deleted when the object is.
type SubResource struct {
	Field string // top-level forProvider/atProvider field, e.g. "cors"
	Path  string // appended to ItemPath, e.g. "/cors"
	// Delete: the spec has a DELETE at <item><Path>; an empty desired value is applied with it.
	Delete bool
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

// observedScopeHeader is scopeHeader with every non-Update header field that addresses another
// place than the one the resource was last read from (headerChanges) set back to the observed
// value (the header dropped when that is the field's Default).
func (x Extension) observedScopeHeader(desired, lastObserved map[string]any) http.Header {
	h := x.scopeHeader(desired)
	for _, hf := range x.Headers {
		if hf.Update {
			continue
		}
		was, ok := lastObserved[x.ObservedName(hf.Field)].(string)
		if !ok || was == "" || hf.effective(desired[hf.Field]) == was {
			continue
		}
		if h == nil {
			h = http.Header{}
		}
		if was == hf.Default {
			h.Del(hf.Header)
		} else {
			h.Set(hf.Header, was)
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

// changedSubResources lists the sub-resources whose desired value is set and not in sync with
// the observed one: a non-empty value not covered by it, or an empty value (clear) while
// Cloudflare has a non-empty one (see SubResource).
func (r *Reconciler) changedSubResources(desired, obs map[string]any) []SubResource {
	var out []SubResource
	for _, s := range r.Extension.SubResources {
		v, ok := desired[s.Field]
		switch {
		case !ok || v == nil:
		case isEmptyValue(v):
			if !isEmptyValue(obs[s.Field]) {
				out = append(out, s)
			}
		case !Covers(v, obs[s.Field]):
			out = append(out, s)
		}
	}
	return out
}

// writeSubResource applies the desired value of one sub-resource: PUT, or for an empty value
// with a DELETE operation, DELETE (a 404 means it is already gone).
func (r *Reconciler) writeSubResource(ctx context.Context, sc scope, id string, s SubResource, desired map[string]any) error {
	req := cfclient.Request{Method: http.MethodPut, Path: s.subPath(sc, r.Descriptor.ItemPath, id), Body: desired[s.Field]}
	if s.Delete && isEmptyValue(desired[s.Field]) {
		req.Method, req.Body = http.MethodDelete, nil
	}
	_, err := sc.do(ctx, req)
	if req.Method == http.MethodDelete && cfclient.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		verb := "update"
		if req.Method == http.MethodDelete {
			verb = "remove"
		}
		return fmt.Errorf("%s %s: %w", verb, s.Field, err)
	}
	return nil
}

// isEmptyValue reports whether a JSON value holds no data: null, an empty list, or an object
// whose members are all empty ({} and {"rules": []}).
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, m := range t {
			if !isEmptyValue(m) {
				return false
			}
		}
		return true
	}
	return false
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
