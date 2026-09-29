package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Workers KV values and keys: /accounts/{account_id}/storage/kv/namespaces/{namespace_id}/
// values/{key_name}, …/metadata/{key_name} and …/keys. No recording covers them; they follow
// what wrangler's `kv key put/get/delete/list` send and read (SOURCED below) and the pinned
// spec. Bulk writes and reads are not emulated.

type kvValue struct {
	Value      []byte
	Metadata   json.RawMessage // nil when none was written
	Expiration int64           // Unix seconds; 0 = never
}

func (s *Server) registerKVValues() {
	base := "/accounts/{account_id}/storage/kv/namespaces/{namespace_id}"
	s.handle(http.MethodPut, base+"/values/{key_name}", kvValuePut)
	s.handle(http.MethodGet, base+"/values/{key_name}", kvValueGet)
	s.handle(http.MethodDelete, base+"/values/{key_name}", kvValueDelete)
	s.handle(http.MethodGet, base+"/metadata/{key_name}", kvMetadataGet)
	s.handle(http.MethodGet, base+"/keys", kvKeysList)
}

// namespace looks up the path's namespace; a missing one answers like GET of the namespace
// (0011, 0015; UNVERIFIED for these routes).
func (c *reqCtx) namespace() (*kvNamespace, *response) {
	n, found := c.account.kv[c.params["namespace_id"]]
	if !found {
		r := fail(http.StatusNotFound, 10013, "get namespace: 'namespace not found'")
		return nil, &r
	}
	return n, nil
}

// live returns the key's value unless it is missing or expired (expired keys are dropped).
func (n *kvNamespace) live(key string, now time.Time) (*kvValue, bool) {
	v, found := n.values[key]
	if found && v.Expiration != 0 && now.Unix() >= v.Expiration {
		delete(n.values, key)
		return nil, false
	}
	return v, found
}

func kvKeyNotFound() response {
	return fail(http.StatusNotFound, 10009, "get: 'key not found'") // UNVERIFIED (not recorded)
}

// kvValuePut: PUT …/values/{key}[?expiration=|expiration_ttl=]. wrangler sends the value as the
// raw body (text/plain for a string, no Content-Type header set) or, with metadata, as
// multipart/form-data with "value" and "metadata" parts. SOURCED (relies):
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/kv/helpers.ts#L231-L262. The result
// wrangler ignores; null is UNVERIFIED (the spec allows an object or null).
func kvValuePut(c *reqCtx) response {
	n, r := c.namespace()
	if r != nil {
		return *r
	}
	key := c.params["key_name"]
	v := &kvValue{Value: append([]byte(nil), c.body...)}
	if mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		parts, r := readParts(c)
		if r != nil {
			return *r
		}
		val, found := parts["value"]
		if !found {
			return fail(http.StatusBadRequest, 10001, "multipart body has no value part") // UNVERIFIED
		}
		v.Value = val
		if meta, found := parts["metadata"]; found {
			if !json.Valid(meta) {
				return fail(http.StatusBadRequest, 10001, "metadata is not valid JSON") // UNVERIFIED
			}
			v.Metadata = json.RawMessage(bytes.TrimSpace(meta))
		}
	}
	// expiration (absolute) and expiration_ttl (relative), both in seconds: the spec and wrangler
	// (helpers.ts#L238-L246, which sets them as query parameters). The minimum of 60 s and its
	// error are not emulated (UNVERIFIED).
	if e := c.query.Get("expiration"); e != "" {
		if s, err := strconv.ParseInt(e, 10, 64); err == nil {
			v.Expiration = s
		}
	}
	if ttl := c.query.Get("expiration_ttl"); ttl != "" {
		if s, err := strconv.ParseInt(ttl, 10, 64); err == nil {
			v.Expiration = c.now.Unix() + s
		}
	}
	if n.values == nil {
		n.values = map[string]*kvValue{}
	}
	n.values[key] = v
	return ok(nil)
}

// kvValueGet answers the raw value, not an envelope: SOURCED (relies) wrangler returns
// response.arrayBuffer() of a 2xx and fails on anything else,
// wrangler@4.143.0:wrangler-dist/cli.js#L56672-L56691 (fetchKVGetValueBase). The spec's 200
// content type is application/octet-stream; the 404 envelope is UNVERIFIED.
func kvValueGet(c *reqCtx) response {
	n, r := c.namespace()
	if r != nil {
		return *r
	}
	v, found := n.live(c.params["key_name"], c.now)
	if !found {
		return kvKeyNotFound()
	}
	return response{status: http.StatusOK, raw: v.Value, rawType: "application/octet-stream"}
}

// kvValueDelete: deleting a missing key succeeds too (UNVERIFIED; wrangler ignores the result,
// helpers.ts#L279-L292).
func kvValueDelete(c *reqCtx) response {
	n, r := c.namespace()
	if r != nil {
		return *r
	}
	delete(n.values, c.params["key_name"])
	return ok(nil)
}

// kvMetadataGet: the result is the metadata written with the value, or null (UNVERIFIED).
func kvMetadataGet(c *reqCtx) response {
	n, r := c.namespace()
	if r != nil {
		return *r
	}
	v, found := n.live(c.params["key_name"], c.now)
	if !found {
		return kvKeyNotFound()
	}
	if v.Metadata == nil {
		return ok(nil)
	}
	return ok(v.Metadata)
}

// kvKeysList: GET …/keys[?prefix=&limit=&cursor=]. Keys come in lexicographic order of their
// UTF-8 bytes (DOCS: https://developers.cloudflare.com/kv/api/list-keys/). result_info carries
// count and cursor; wrangler follows the cursor until it is absent, null or "",
// SOURCED (relies) wrangler@4.143.0:wrangler-dist/cli.js#L56475-L56510,L56692-L56695
// (fetchListResultBase, hasCursor), for `kv key list` (helpers.ts#L121-L133). The cursor is an
// opaque token (here the last key returned, base64url-encoded), "" on the last page, and
// limit defaults to 1000 (spec; clamped to 10…1000): the cursor format and the "" are UNVERIFIED.
func kvKeysList(c *reqCtx) response {
	n, r := c.namespace()
	if r != nil {
		return *r
	}
	prefix := c.query.Get("prefix")
	limit := c.intQuery("limit", 1000)
	if limit < 10 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	after, _ := decodeKVCursor(c.query.Get("cursor"))
	var keys []string
	for k := range n.values {
		if strings.HasPrefix(k, prefix) && (after == "" || k > after) {
			if _, live := n.live(k, c.now); live {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys) // Go compares strings bytewise: UTF-8 byte order
	cursor := ""
	if len(keys) > limit {
		keys = keys[:limit]
		cursor = encodeKVCursor(keys[len(keys)-1])
	}
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		v := n.values[k]
		item := map[string]any{"name": k}
		if v.Expiration != 0 {
			item["expiration"] = v.Expiration
		}
		if v.Metadata != nil {
			item["metadata"] = v.Metadata
		}
		out = append(out, item)
	}
	return okList(out, map[string]any{"count": len(out), "cursor": cursor})
}

func encodeKVCursor(lastKey string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(lastKey))
}

// decodeKVCursor returns the key a cursor continues after; a malformed cursor starts over
// (the API's answer to one is UNVERIFIED).
func decodeKVCursor(cur string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}
