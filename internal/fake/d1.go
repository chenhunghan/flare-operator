package fake

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// D1 databases: /accounts/{account_id}/d1/database.
// Source recordings: test/recordings/2026-09-29/0017…0027.
// Only the control plane is emulated; SQL execution is not (except the recorded validation).

type d1Database struct {
	UUID         string
	Name         string
	Created      time.Time
	Region       string
	Jurisdiction *string
	Replication  string
	FileSize     int
	Seq          int64
}

var d1Regions = []string{"WNAM", "ENAM", "WEUR", "EEUR", "APAC", "OC"}

// Each D1 endpoint returns a different shape (a real API quirk):
//   create (0017): created_in_region, read_replication
//   get/patch (0020, 0023): running_in_region, read_replication
//   list (0021): neither region nor read_replication

func (d *d1Database) base() map[string]any {
	return map[string]any{
		"uuid": d.UUID, "name": d.Name, "created_at": tsMilli(d.Created), "version": "production",
		"file_size": d.FileSize, "num_tables": 0, "jurisdiction": d.Jurisdiction,
	}
}

func (d *d1Database) createJSON() map[string]any {
	m := d.base()
	m["created_in_region"] = d.Region
	m["read_replication"] = map[string]any{"mode": d.Replication}
	return m
}

func (d *d1Database) getJSON() map[string]any {
	m := d.base()
	m["running_in_region"] = d.Region
	m["read_replication"] = map[string]any{"mode": d.Replication}
	return m
}

func (s *Server) registerD1() {
	base := "/accounts/{account_id}/d1/database"
	s.handle(http.MethodPost, base, d1Create)
	s.handle(http.MethodGet, base, d1List)
	s.handle(http.MethodGet, base+"/{database_id}", d1Get)
	s.handle(http.MethodPatch, base+"/{database_id}", d1Patch)
	s.handle(http.MethodDelete, base+"/{database_id}", d1Delete)
	s.handle(http.MethodPost, base+"/{database_id}/query", d1Query)
	s.handle(http.MethodGet, base+"/{database_id}/time_travel/bookmark", d1Bookmark)
}

func d1NotFound(id string) response {
	return fail(http.StatusNotFound, 7404, fmt.Sprintf("The database %s could not be found", id)) // 0025, 0027
}

func d1Create(c *reqCtx) response {
	var req struct {
		Name                string  `json:"name"`
		PrimaryLocationHint string  `json:"primary_location_hint"`
		Jurisdiction        *string `json:"jurisdiction"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if strings.TrimSpace(req.Name) == "" { // UNVERIFIED message; shape follows 0019
		return fail(http.StatusBadRequest, 7400, "Invalid property: name => Required").withStyle(styleErrorBare)
	}
	region := c.s.opts.D1Region
	if req.PrimaryLocationHint != "" {
		valid := false
		for _, r := range d1Regions {
			valid = valid || r == req.PrimaryLocationHint
		}
		if !valid { // 0019: validation errors come back without result/messages
			return fail(http.StatusBadRequest, 7400, "Invalid property: primary_location_hint => Invalid enum value. Expected 'WNAM' | 'ENAM' | 'WEUR' | 'EEUR' | 'APAC' | 'OC'").withStyle(styleErrorBare)
		}
		region = req.PrimaryLocationHint
	}
	for _, d := range c.account.d1 {
		if d.Name == req.Name { // 0018
			return fail(http.StatusBadRequest, 7502, fmt.Sprintf("Database with name: '%s' already exists", req.Name))
		}
	}
	d := &d1Database{UUID: c.s.ids.next(uuidV4), Name: req.Name, Created: c.now, Region: region,
		Jurisdiction: req.Jurisdiction, Replication: "disabled", FileSize: 8192, Seq: c.s.nextSeq()}
	c.account.d1[d.UUID] = d
	return ok(d.createJSON())
}

func d1List(c *reqCtx) response {
	name := c.query.Get("name")
	var items []*d1Database
	for _, d := range sortedBySeq(c.account.d1, func(d *d1Database) int64 { return d.Seq }) {
		// The spec calls name "a database name to search for"; emulate substring search so clients
		// that look up a database by name must still match the exact name themselves.
		// UNVERIFIED (0021 only used a full name).
		if name == "" || strings.Contains(d.Name, name) {
			items = append(items, d)
		}
	}
	// 0021: default per_page 100
	pageItems, page, perPage, _ := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 100), 100)
	out := make([]any, 0, len(pageItems))
	for _, d := range pageItems {
		out = append(out, d.base())
	}
	// 0021: no total_pages.
	return okList(out, PageInfo{Count: len(out), Page: intp(page), PerPage: intp(perPage), TotalCount: intp(len(items))})
}

// d1Get also finds a database by its exact name in place of the UUID: wrangler resolves names
// with GET …/d1/database/{name}?fields=uuid,name. SOURCED (relies):
// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/d1/utils.ts#L93-L98, and the spec's
// database_id parameter is a oneOf of a UUID and a name. The fields filter is ignored (the whole
// object is returned; wrangler reads only uuid and name): UNVERIFIED.
func d1Get(c *reqCtx) response {
	d, found := c.account.d1[c.params["database_id"]]
	if !found {
		for _, x := range c.account.d1 {
			if x.Name == c.params["database_id"] {
				d, found = x, true
			}
		}
	}
	if !found {
		return d1NotFound(c.params["database_id"])
	}
	return ok(d.getJSON())
}

func d1Patch(c *reqCtx) response {
	d, found := c.account.d1[c.params["database_id"]]
	if !found {
		return d1NotFound(c.params["database_id"])
	}
	var req struct {
		ReadReplication *struct {
			Mode string `json:"mode"`
		} `json:"read_replication"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.ReadReplication != nil {
		if m := req.ReadReplication.Mode; m != "auto" && m != "disabled" {
			return fail(http.StatusBadRequest, 7400, "Invalid property: read_replication.mode") // UNVERIFIED message
		}
		d.Replication = req.ReadReplication.Mode
	}
	return ok(d.getJSON()) // 0023
}

func d1Delete(c *reqCtx) response {
	id := c.params["database_id"]
	if _, found := c.account.d1[id]; !found {
		return d1NotFound(id)
	}
	delete(c.account.d1, id)
	return ok(nil) // 0026
}

func d1Query(c *reqCtx) response {
	d, found := c.account.d1[c.params["database_id"]]
	if !found {
		return d1NotFound(c.params["database_id"])
	}
	var req struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if countSQLStatements(req.SQL) > 1 && len(req.Params) > 0 { // 0022
		return fail(http.StatusBadRequest, 7400, "The request is malformed: params with multiple statements is not supported")
	}
	_ = d
	return fail(http.StatusNotImplemented, 99999, "flarefake: D1 SQL execution is not emulated")
}

func d1Bookmark(c *reqCtx) response {
	if _, found := c.account.d1[c.params["database_id"]]; !found {
		return d1NotFound(c.params["database_id"])
	}
	// 0024: "00000000-0000000a-000050f5-<32 hex>"; only the shape is meaningful to clients.
	return ok(map[string]any{"bookmark": fmt.Sprintf("00000000-%08x-%08x-%s", 10, c.now.Unix()&0xffffffff, hex32())})
}

// countSQLStatements counts non-empty statements, ignoring semicolons inside string literals,
// quoted identifiers and comments.
func countSQLStatements(sql string) int {
	count, sawContent := 0, false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			end := strings.IndexByte(sql[i+1:], ch)
			if end < 0 {
				return count + 1
			}
			i += end + 1
			sawContent = true
		case ch == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(sql)
			}
		case ch == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += end + 3
			} else {
				i = len(sql)
			}
		case ch == ';':
			if sawContent {
				count++
			}
			sawContent = false
		case ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r':
			sawContent = true
		}
	}
	if sawContent {
		count++
	}
	return count
}
