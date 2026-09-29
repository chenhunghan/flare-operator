package fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Resource Tagging (account level): GET / PUT / DELETE /accounts/{account_id}/tags.
//
// No recording exists yet. The model follows the pinned spec (operationIds tags-get, tags-set,
// tags-delete, tags-list) and the docs (DOCS below:
// https://developers.cloudflare.com/resource-tagging/how-to/manage-tags/ and
// https://developers.cloudflare.com/resource-tagging/how-to/filter-resources/):
//   - PUT replaces the whole tag map ("This operation replaces all existing tags"; DOCS).
//   - GET on a resource that was never tagged (or does not exist) answers 500 (DOCS); the error
//     code is UNVERIFIED (1000 used here).
//   - DELETE removes all tags and answers "204 No Content" (DOCS); later GETs return an empty map.
//   - The etag is "v1:" + base64url(sha256(canonical tags JSON)[:16]) (spec description) and
//     If-Match mismatches answer 412 (spec; the docs do not mention If-Match; error code
//     UNVERIFIED).
//   - The emulator does not check that the tagged resource exists.
//   - GET /tags/resources (tags-list) lists resources that currently carry at least one tag,
//     filtered by type/id/name/tag, with cursor pagination at a fixed page size of 100 and a
//     null cursor on the last page, and at most 20 tag filters (else code 1010) (DOCS). Whether
//     resources whose tags were all deleted are listed, the ordering and the cursor format are
//     UNVERIFIED; the live endpoint may also lag writes (it is an index).

// tagResourceTypes are the account-level resource_type values of the pinned spec's tags-set.
var tagResourceTypes = map[string]bool{
	"access_application": true, "access_group": true, "account": true, "account_ruleset": true,
	"ai_gateway": true, "alerting_policy": true, "alerting_webhook": true, "cloudflared_tunnel": true,
	"cws_deployment": true, "cws_policy": true, "cws_policy_set": true, "cws_workload": true,
	"d1_database": true, "durable_object_namespace": true, "gateway_list": true, "gateway_rule": true,
	"image": true, "infrastructure_target": true, "kv_namespace": true, "load_balancer_monitor": true,
	"load_balancer_pool": true, "pages_project": true, "queue": true, "r2_bucket": true,
	"resource_share": true, "stream_live_input": true, "stream_video": true, "vectorize_index": true,
	"worker": true, "worker_version": true,
}

// tagListTypes are the values the tags-list ?type= filter accepts: the pinned spec's
// resource-tagging_resource_type, i.e. tagResourceTypes plus the zone-level types. Zone-level
// resources cannot be stored through the account-level tags-set, so filtering on one of them
// matches nothing here. UNVERIFIED (no recording).
var tagListTypes = func() map[string]bool {
	m := map[string]bool{
		"access_application_policy": true, "api_gateway_operation": true, "custom_certificate": true,
		"custom_hostname": true, "dns_record": true, "healthcheck": true, "load_balancer": true,
		"managed_client_certificate": true, "worker_route": true, "zone": true, "zone_ruleset": true,
	}
	for t := range tagResourceTypes {
		m[t] = true
	}
	return m
}()

type tagRecord struct {
	Type, ID, WorkerID string
	Tags               map[string]string
	UpdatedAt          time.Time
}

func (r *tagRecord) etag() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r.Tags) // encoding/json sorts map keys, matching RFC 8785 for string maps
	sum := sha256.Sum256(bytes.TrimSpace(buf.Bytes()))
	return "v1:" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (c *reqCtx) tagResourceName(typ, id string) string {
	a := c.account
	switch typ {
	case "kv_namespace":
		if n, ok := a.kv[id]; ok {
			return n.Title
		}
	case "d1_database":
		if d, ok := a.d1[id]; ok {
			return d.Name
		}
	case "queue":
		if q, ok := a.queues[id]; ok {
			return q.Name
		}
	case "cloudflared_tunnel":
		if t, ok := a.tunnels[id]; ok {
			return t.Name
		}
	}
	return ""
}

func (r *tagRecord) json(name string) map[string]any {
	tags := map[string]string{}
	for k, v := range r.Tags {
		tags[k] = v
	}
	m := map[string]any{"type": r.Type, "id": r.ID, "name": name, "etag": r.etag(), "tags": tags}
	if len(r.Tags) > 0 {
		m["tags_updated_at"] = r.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if r.WorkerID != "" {
		m["worker_id"] = r.WorkerID
	}
	return m
}

func (s *Server) registerTags() {
	base := "/accounts/{account_id}/tags"
	s.handle(http.MethodGet, base, tagsGet)
	s.handle(http.MethodPut, base, tagsSet)
	s.handle(http.MethodDelete, base, tagsDelete)
	s.handle(http.MethodGet, base+"/resources", tagsList)
}

func tagKey(typ, id, worker string) string { return typ + "/" + worker + "/" + id }

func (c *reqCtx) tagStore() map[string]*tagRecord {
	if c.account.tags == nil {
		c.account.tags = map[string]*tagRecord{}
	}
	return c.account.tags
}

func badTagRequest(msg string) response {
	return fail(http.StatusBadRequest, 1001, msg) // UNVERIFIED code
}

func validTagTarget(typ, id, worker string) *response {
	switch {
	case !tagResourceTypes[typ]:
		r := badTagRequest("invalid resource_type")
		return &r
	case id == "":
		r := badTagRequest("resource_id is required")
		return &r
	case typ == "worker_version" && worker == "":
		r := badTagRequest("worker_id is required for worker_version resources")
		return &r
	}
	return nil
}

func tagsGet(c *reqCtx) response {
	typ, id, worker := c.query.Get("resource_type"), c.query.Get("resource_id"), c.query.Get("worker_id")
	if r := validTagTarget(typ, id, worker); r != nil {
		return *r
	}
	rec, found := c.tagStore()[tagKey(typ, id, worker)]
	if !found {
		// never-tagged resources answer 500. DOCS: https://developers.cloudflare.com/resource-tagging/how-to/manage-tags/
		return fail(http.StatusInternalServerError, 1000, "Internal Server Error")
	}
	return ok(rec.json(c.tagResourceName(typ, id)))
}

func (c *reqCtx) checkIfMatch(rec *tagRecord) *response {
	want := c.r.Header.Get("If-Match")
	if want == "" {
		return nil
	}
	cur := ""
	if rec != nil {
		cur = rec.etag()
	}
	if want != cur {
		r := fail(http.StatusPreconditionFailed, 1002, "Precondition failed: the resource's tags have been modified") // UNVERIFIED code
		return &r
	}
	return nil
}

func tagsSet(c *reqCtx) response {
	var req struct {
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		WorkerID     string            `json:"worker_id"`
		Tags         map[string]string `json:"tags"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if r := validTagTarget(req.ResourceType, req.ResourceID, req.WorkerID); r != nil {
		return *r
	}
	for k, v := range req.Tags {
		if k == "" || len(k) > 256 || len(v) > 1024 { // spec: keys ≤ 256, values ≤ 1024 characters
			return badTagRequest("invalid tag " + k)
		}
	}
	store := c.tagStore()
	key := tagKey(req.ResourceType, req.ResourceID, req.WorkerID)
	rec := store[key]
	if r := c.checkIfMatch(rec); r != nil {
		return *r
	}
	if rec == nil {
		rec = &tagRecord{Type: req.ResourceType, ID: req.ResourceID, WorkerID: req.WorkerID}
		store[key] = rec
	}
	rec.Tags = map[string]string{}
	for k, v := range req.Tags {
		rec.Tags[k] = v
	}
	rec.UpdatedAt = c.now
	return ok(rec.json(c.tagResourceName(rec.Type, rec.ID)))
}

func tagsDelete(c *reqCtx) response {
	var req struct {
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
		WorkerID     string `json:"worker_id"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if r := validTagTarget(req.ResourceType, req.ResourceID, req.WorkerID); r != nil {
		return *r
	}
	store := c.tagStore()
	key := tagKey(req.ResourceType, req.ResourceID, req.WorkerID)
	rec := store[key]
	if r := c.checkIfMatch(rec); r != nil {
		return *r
	}
	if rec == nil {
		rec = &tagRecord{Type: req.ResourceType, ID: req.ResourceID, WorkerID: req.WorkerID}
		store[key] = rec
	}
	rec.Tags = map[string]string{}
	rec.UpdatedAt = c.now
	return response{status: http.StatusNoContent}
}

// tagsList: GET /accounts/{account_id}/tags/resources (no recording; evidence and open points
// in the package comment above).
func tagsList(c *reqCtx) response {
	q := c.query
	types := map[string]bool{}
	for _, t := range q["type"] {
		if !tagListTypes[t] {
			return badTagRequest("invalid type " + t)
		}
		types[t] = true
	}
	if len(q["id"]) > 50 { // spec: id may be repeated up to 50 times
		return badTagRequest("too many id filters")
	}
	ids := map[string]bool{}
	for _, id := range q["id"] {
		ids[id] = true
	}
	fold := q.Get("case_insensitive") == "true"
	name := strings.ToLower(q.Get("name"))
	if len(q["tag"]) > 20 { // DOCS (filter-resources): "Maximum of 20 tag filters per query (error code 1010 if exceeded)"
		return fail(http.StatusBadRequest, 1010, "too many tag filters (maximum 20)") // status and message UNVERIFIED
	}
	var filters []func(map[string]string) bool
	for _, expr := range q["tag"] {
		f, valid := parseTagFilter(expr, fold)
		if !valid {
			return badTagRequest("invalid tag filter " + expr)
		}
		filters = append(filters, f)
	}
	store := c.tagStore()
	keys := make([]string, 0, len(store))
	for k := range store {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []any{}
	for _, k := range keys {
		rec := store[k]
		if len(rec.Tags) == 0 || (len(types) > 0 && !types[rec.Type]) || (len(ids) > 0 && !ids[rec.ID]) {
			continue
		}
		rname := c.tagResourceName(rec.Type, rec.ID)
		if name != "" && !strings.Contains(strings.ToLower(rname), name) {
			continue
		}
		match := true
		for _, f := range filters {
			if !f(rec.Tags) {
				match = false
				break
			}
		}
		if match {
			out = append(out, rec.json(rname))
		}
	}
	// Cursor pagination, fixed page size 100, null cursor on the last page (DOCS,
	// filter-resources). The cursor encodes the offset: its real format is opaque (UNVERIFIED).
	start := 0
	if cur := q.Get("cursor"); cur != "" {
		n, err := decodeTagCursor(cur)
		if err != nil || n > len(out) {
			return badTagRequest("invalid cursor")
		}
		start = n
	}
	end := min(start+tagsPageSize, len(out))
	var next any // null on the last page
	if end < len(out) {
		next = encodeTagCursor(end)
	}
	page := out[start:end]
	return okList(page, map[string]any{"count": len(page), "cursor": next})
}

const tagsPageSize = 100

func encodeTagCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"offset":%d}`, offset)))
}

func decodeTagCursor(c string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, err
	}
	var v struct {
		Offset *int `json:"offset"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Offset == nil || *v.Offset < 0 {
		return 0, fmt.Errorf("bad cursor")
	}
	return *v.Offset, nil
}

// parseTagFilter implements the spec's tag filter syntax: key, key=v1,v2, !key, key!=value.
func parseTagFilter(expr string, fold bool) (func(map[string]string) bool, bool) {
	eq := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	lookup := func(tags map[string]string, key string) (string, bool) {
		for k, v := range tags {
			if eq(k, key) {
				return v, true
			}
		}
		return "", false
	}
	switch {
	case expr == "" || expr == "!":
		return nil, false
	case strings.HasPrefix(expr, "!"):
		key := expr[1:]
		return func(t map[string]string) bool { _, has := lookup(t, key); return !has }, true
	case strings.Contains(expr, "!="):
		key, val, _ := strings.Cut(expr, "!=")
		return func(t map[string]string) bool { v, has := lookup(t, key); return !has || !eq(v, val) }, key != ""
	case strings.Contains(expr, "="):
		key, vals, _ := strings.Cut(expr, "=")
		want := strings.Split(vals, ",")
		return func(t map[string]string) bool {
			v, has := lookup(t, key)
			if !has {
				return false
			}
			for _, w := range want {
				if eq(v, w) {
					return true
				}
			}
			return false
		}, key != ""
	default:
		return func(t map[string]string) bool { _, has := lookup(t, expr); return has }, true
	}
}
