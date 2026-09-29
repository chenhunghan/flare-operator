package fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

// Resource Tagging (account level): GET / PUT / DELETE /accounts/{account_id}/tags.
//
// UNVERIFIED: no recording exists yet. The model follows the pinned spec (operationIds tags-get,
// tags-set, tags-delete) and docs/cloudflare-service-catalog.md:
//   - PUT replaces the whole tag map (replace-all, not merge).
//   - GET on a resource that was never tagged answers 500 (catalog note; the error code is
//     unknown, 1000-range placeholder used here).
//   - DELETE removes all tags and answers 204 with no body (spec); later GETs return an empty map.
//   - The etag is "v1:" + base64url(sha256(canonical tags JSON)[:16]) (spec description) and
//     If-Match mismatches answer 412 (spec; error code UNVERIFIED).
//   - The emulator does not check that the tagged resource exists.

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
		// docs/cloudflare-service-catalog.md: never-tagged resources answer 500.
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
