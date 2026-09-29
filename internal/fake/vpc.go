package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Workers VPC connectivity services: /accounts/{account_id}/connectivity/directory/services.
// Source recordings: test/recordings/2026-09-29/0052…0061, 0066, 0070, 0076, 0089, 0100, 0105,
// 0110…0112.

type vpcService struct {
	ID          string
	Type        string
	Name        string
	HTTPPort    *int
	HTTPSPort   *int
	TCPPort     *int
	AppProtocol *string
	Host        vpcHost
	TLSSettings json.RawMessage
	Created     time.Time
	Updated     time.Time
	Seq         int64
}

type vpcNetwork struct {
	TunnelID    string   `json:"tunnel_id"`
	ResolverIPs []string `json:"resolver_ips,omitempty"`
}

type vpcHost struct {
	IPv4            *string     `json:"ipv4"`
	IPv6            *string     `json:"ipv6"`
	Hostname        *string     `json:"hostname"`
	Network         *vpcNetwork `json:"network"`
	ResolverNetwork *vpcNetwork `json:"resolver_network"`
}

type vpcRequest struct {
	Name        *string         `json:"name"`
	Type        *string         `json:"type"`
	Host        *vpcHost        `json:"host"`
	HTTPPort    *int            `json:"http_port"`
	HTTPSPort   *int            `json:"https_port"`
	TCPPort     *int            `json:"tcp_port"`
	AppProtocol *string         `json:"app_protocol"`
	TLSSettings json.RawMessage `json:"tls_settings"` // echoed only when set (0193; absent in 0052)
}

func (v *vpcService) json() map[string]any {
	host := v.Host // all five keys always present, nulls included (0052)
	m := map[string]any{
		"service_id": v.ID, "type": v.Type, "name": v.Name, "host": host,
		"created_at": tsSecond(v.Created), "updated_at": tsSecond(v.Updated),
	}
	if len(v.TLSSettings) > 0 && string(v.TLSSettings) != "null" {
		m["tls_settings"] = v.TLSSettings
	}
	if v.Type == "tcp" { // 0056
		m["tcp_port"] = v.TCPPort
		m["app_protocol"] = v.AppProtocol
	} else {
		m["http_port"] = v.HTTPPort
		m["https_port"] = v.HTTPSPort
	}
	return m
}

func (s *Server) registerVPC() {
	base := "/accounts/{account_id}/connectivity/directory/services"
	s.handle(http.MethodPost, base, vpcCreate)
	s.handle(http.MethodGet, base, vpcList)
	s.handle(http.MethodGet, base+"/{service_id}", vpcGet)
	s.handle(http.MethodPut, base+"/{service_id}", vpcPut)
	s.handle(http.MethodDelete, base+"/{service_id}", vpcDelete)
}

func vpcNotFound(id string) response {
	return fail(http.StatusNotFound, 5104, fmt.Sprintf("Service (ID: %q) not found", id)) // 0058, 0111
}

func vpcInvalid(msg string) response {
	return fail(http.StatusBadRequest, 5101, "request contained invalid parameters: "+msg)
}

// endPosition reports the 1-based line/column of the last byte, as serde does for
// "missing field" errors at the end of an object (0061: "at line 1 column 42").
func endPosition(body []byte) (int, int) {
	s := strings.TrimRight(string(body), " \t\r\n")
	line := strings.Count(s, "\n") + 1
	col := len(s) - strings.LastIndex(s, "\n") - 1
	return line, col
}

// parseVPC decodes and validates a create/replace body. exceptID excludes a service from the
// duplicate-name check (for PUT).
func parseVPC(c *reqCtx, exceptID string) (*vpcService, *response) {
	var req vpcRequest
	if err := json.Unmarshal(c.body, &req); err != nil {
		r := fail(http.StatusBadRequest, 5102, "invalid json: bad json data: "+err.Error())
		return nil, &r
	}
	for _, f := range []struct {
		name    string
		missing bool
	}{{"name", req.Name == nil}, {"type", req.Type == nil}, {"host", req.Host == nil}} {
		if f.missing { // 0061
			line, col := endPosition(c.body)
			r := fail(http.StatusBadRequest, 5102, fmt.Sprintf("invalid json: bad json data: missing field `%s` at line %d column %d", f.name, line, col))
			return nil, &r
		}
	}
	for _, other := range c.account.vpc {
		if other.Name == *req.Name && other.ID != exceptID { // 0059
			r := vpcInvalid(fmt.Sprintf("Service name '%s' already exists", *req.Name))
			return nil, &r
		}
	}
	h := *req.Host
	for _, n := range []*vpcNetwork{h.Network, h.ResolverNetwork} {
		if n == nil {
			continue
		}
		t, found := c.account.tunnels[n.TunnelID]
		if !found || t.Deleted != nil { // 0060, 0105
			r := vpcInvalid("Tunnel ID Not Found")
			return nil, &r
		}
	}
	if h.Hostname != nil { // 0066: hostnames are lowercased
		lower := strings.ToLower(*h.Hostname)
		h.Hostname = &lower
	}
	return &vpcService{Type: *req.Type, Name: *req.Name, HTTPPort: req.HTTPPort, HTTPSPort: req.HTTPSPort,
		TCPPort: req.TCPPort, AppProtocol: req.AppProtocol, Host: h, TLSSettings: req.TLSSettings}, nil
}

func vpcCreate(c *reqCtx) response {
	svc, r := parseVPC(c, "")
	if r != nil {
		return *r
	}
	svc.ID = c.s.ids.next(func() string { return uuidV7(c.now) }) // 0052: UUIDv7
	svc.Created, svc.Updated = c.now, c.now
	svc.Seq = c.s.nextSeq()
	c.account.vpc[svc.ID] = svc
	return ok(svc.json())
}

func vpcList(c *reqCtx) response {
	out := []any{}
	for _, v := range sortedBySeq(c.account.vpc, func(v *vpcService) int64 { return v.Seq }) {
		out = append(out, v.json())
	}
	return ok(out) // 0112: no result_info
}

func vpcGet(c *reqCtx) response {
	v, found := c.account.vpc[c.params["service_id"]]
	if !found {
		return vpcNotFound(c.params["service_id"])
	}
	// 0100: still returned (with a dangling tunnel_id) after its tunnel was deleted.
	return ok(v.json())
}

// vpcPut is a full replace, and resets created_at (0066).
func vpcPut(c *reqCtx) response {
	id := c.params["service_id"]
	if _, found := c.account.vpc[id]; !found {
		return vpcNotFound(id)
	}
	svc, r := parseVPC(c, id)
	if r != nil {
		return *r
	}
	svc.ID = id
	svc.Seq = c.account.vpc[id].Seq
	svc.Created, svc.Updated = c.now, c.now
	c.account.vpc[id] = svc
	return ok(svc.json())
}

func vpcDelete(c *reqCtx) response {
	id := c.params["service_id"]
	if _, found := c.account.vpc[id]; !found {
		return vpcNotFound(id) // UNVERIFIED for DELETE
	}
	// 0089: allowed even while a Worker still binds the service.
	delete(c.account.vpc, id)
	return ok(nil)
}
