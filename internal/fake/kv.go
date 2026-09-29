package fake

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Workers KV namespaces: /accounts/{account_id}/storage/kv/namespaces.
// Source recordings: test/recordings/2026-09-29/0003…0016, kv-order-*.

type kvNamespace struct {
	ID           string
	Title        string
	Jurisdiction string
	Created      time.Time
}

func (n *kvNamespace) json() map[string]any {
	m := map[string]any{"id": n.ID, "title": n.Title, "supports_url_encoding": true}
	if n.Jurisdiction != "" {
		m["jurisdiction"] = n.Jurisdiction // UNVERIFIED: not yet recorded with a jurisdiction
	}
	return m
}

// kvLegacySunset is when /accounts/{id}/workers/namespaces stops working (Cloudflare changelog).
var kvLegacySunset = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

func (s *Server) registerKV() {
	for _, base := range []string{"/accounts/{account_id}/storage/kv/namespaces", "/accounts/{account_id}/workers/namespaces"} {
		legacy := strings.Contains(base, "/workers/")
		wrap := func(h handler) handler {
			if !legacy {
				return h
			}
			return func(c *reqCtx) response {
				if !c.now.Before(kvLegacySunset) {
					// Post-sunset behavior UNVERIFIED; model it as the route being gone.
					return fail(http.StatusNotFound, 7000, "No route for that URI")
				}
				return h(c)
			}
		}
		s.handle(http.MethodPost, base, wrap(kvCreate))
		s.handle(http.MethodGet, base, wrap(kvList))
		s.handle(http.MethodGet, base+"/{namespace_id}", wrap(kvGet))
		s.handle(http.MethodPut, base+"/{namespace_id}", wrap(kvRename))
		s.handle(http.MethodDelete, base+"/{namespace_id}", wrap(kvDelete))
	}
}

func kvTitleTaken(a *account, title, exceptID string) bool {
	for _, n := range a.kv {
		if n.Title == title && n.ID != exceptID {
			return true
		}
	}
	return false
}

// 0004/0010: duplicate title on create or rename.
func kvDuplicate() response {
	return fail(http.StatusBadRequest, 10014, "a namespace with this account ID and title already exists")
}

func kvCreate(c *reqCtx) response {
	var req struct {
		Title        string `json:"title"`
		Jurisdiction string `json:"jurisdiction"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.Title == "" {
		return fail(http.StatusBadRequest, 10019, "title is required") // UNVERIFIED code/message
	}
	if kvTitleTaken(c.account, req.Title, "") {
		return kvDuplicate()
	}
	n := &kvNamespace{ID: c.s.ids.next(hex32), Title: req.Title, Jurisdiction: req.Jurisdiction, Created: c.now}
	c.account.kv[n.ID] = n
	return ok(n.json()) // 0003: 200, not 201
}

func kvList(c *reqCtx) response {
	items := sortedValues(c.account.kv) // default order: id ascending (0007/0008, kv-order-list-default)
	desc := c.query.Get("direction") == "desc"
	switch c.query.Get("order") {
	case "title": // kv-order-list-title-asc
		sort.SliceStable(items, func(i, j int) bool { return items[i].Title < items[j].Title })
	}
	if desc {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	pageItems, page, perPage, totalPages := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 20), 20)
	out := make([]any, 0, len(pageItems))
	for _, n := range pageItems {
		out = append(out, n.json())
	}
	return okList(out, PageInfo{Count: len(out), Page: intp(page), PerPage: intp(perPage), TotalCount: intp(len(items)), TotalPages: intp(totalPages)})
}

func kvGet(c *reqCtx) response {
	n, found := c.account.kv[c.params["namespace_id"]]
	if !found {
		return fail(http.StatusNotFound, 10013, "get namespace: 'namespace not found'") // 0011, 0015
	}
	return ok(n.json())
}

func kvRename(c *reqCtx) response {
	n, found := c.account.kv[c.params["namespace_id"]]
	if !found {
		return fail(http.StatusNotFound, 10013, "namespace not found") // UNVERIFIED for PUT
	}
	var req struct {
		Title string `json:"title"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.Title == "" {
		return fail(http.StatusBadRequest, 10019, "title is required") // UNVERIFIED code/message
	}
	if kvTitleTaken(c.account, req.Title, n.ID) {
		return kvDuplicate() // 0010
	}
	n.Title = req.Title
	return ok(n.json()) // 0009: returns the renamed namespace
}

func kvDelete(c *reqCtx) response {
	id := c.params["namespace_id"]
	if _, found := c.account.kv[id]; !found {
		return fail(http.StatusNotFound, 10013, "namespace not found") // 0014: differs from GET's message
	}
	delete(c.account.kv, id)
	return ok(nil) // 0013
}
