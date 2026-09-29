package fake

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Cloudflare Tunnels (cfd_tunnel) and virtual networks.
// Source recordings: test/recordings/2026-09-29/0040…0051, 0095…0103, 0160…0164.

type tunnelConn struct {
	ColoName string
	UUID     string
	Opened   time.Time
}

type tunnelClient struct {
	ID      string
	Arch    string
	Version string
	RunAt   time.Time
	Conns   []tunnelConn
}

type tunnel struct {
	ID             string
	Name           string
	ConfigSrc      string
	Secret         string
	Created        time.Time
	Deleted        *time.Time
	EverConnected  bool
	ConnsActiveAt  *time.Time
	ConnsInactive  *time.Time
	Clients        []*tunnelClient
	Config         map[string]any
	ConfigVersion  int
	ConfigModified time.Time
	Seq            int64
}

type virtualNetwork struct {
	ID        string
	Name      string
	Comment   string
	IsDefault bool
	Created   time.Time
	Deleted   *time.Time
	Seq       int64
}

// tunnelConnectionsFieldSunset: from this date the "connections" field is no longer returned by
// tunnel list/get (Cloudflare changelog 2026-07-09); clients must use /connections instead.
var tunnelConnectionsFieldSunset = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func (t *tunnel) status() string {
	switch {
	case t.connected():
		return "healthy" // 0043
	case t.EverConnected:
		return "down" // 0097, 0099
	default:
		return "inactive" // 0040
	}
}

func (t *tunnel) connected() bool {
	for _, c := range t.Clients {
		if len(c.Conns) > 0 {
			return true
		}
	}
	return false
}

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return tsMicro(*t)
}

func (t *tunnel) json(acct string, now time.Time) map[string]any {
	m := map[string]any{
		"id": t.ID, "account_tag": acct, "created_at": tsMicro(t.Created), "deleted_at": tsPtr(t.Deleted),
		"name": t.Name, "conns_active_at": tsPtr(t.ConnsActiveAt), "conns_inactive_at": tsPtr(t.ConnsInactive),
		"tun_type": "cfd_tunnel", "metadata": map[string]any{}, "status": t.status(),
	}
	if now.Before(tunnelConnectionsFieldSunset) {
		conns := []any{}
		for _, c := range t.Clients {
			for _, cn := range c.Conns {
				conns = append(conns, cn.json(c))
			}
		}
		m["connections"] = conns
	}
	if t.Deleted == nil {
		m["remote_config"] = t.ConfigSrc == "cloudflare"
		m["config_src"] = t.ConfigSrc
	} else {
		m["remote_config"] = false // 0101: soft-deleted tunnels lose config_src
	}
	return m
}

func (cn tunnelConn) json(c *tunnelClient) map[string]any {
	return map[string]any{
		"colo_name": cn.ColoName, "uuid": cn.UUID, "id": cn.UUID, "is_pending_reconnect": false,
		"origin_ip": "203.0.113.10", "opened_at": tsMicro(cn.Opened), "client_id": c.ID, "client_version": c.Version,
	}
}

// token is the value cloudflared accepts via --token / TUNNEL_TOKEN: base64(JSON{a,t,s}).
func (t *tunnel) token(acct string) string {
	b, _ := json.Marshal(map[string]string{"a": acct, "t": t.ID, "s": t.Secret})
	return base64.StdEncoding.EncodeToString(b)
}

func (s *Server) registerTunnels() {
	base := "/accounts/{account_id}/cfd_tunnel"
	s.handle(http.MethodPost, base, tunnelCreate)
	s.handle(http.MethodGet, base, tunnelList)
	s.handle(http.MethodGet, base+"/{tunnel_id}", tunnelGet)
	s.handle(http.MethodDelete, base+"/{tunnel_id}", tunnelDelete)
	s.handle(http.MethodGet, base+"/{tunnel_id}/token", tunnelToken)
	s.handle(http.MethodGet, base+"/{tunnel_id}/connections", tunnelConnections)
	s.handle(http.MethodDelete, base+"/{tunnel_id}/connections", tunnelCleanupConnections)
	s.handle(http.MethodGet, base+"/{tunnel_id}/configurations", tunnelConfigGet)
	s.handle(http.MethodPut, base+"/{tunnel_id}/configurations", tunnelConfigPut)

	vn := "/accounts/{account_id}/teamnet/virtual_networks"
	s.handle(http.MethodGet, vn, vnetList)
	s.handle(http.MethodPost, vn, vnetCreate)
	s.handle(http.MethodDelete, vn+"/{virtual_network_id}", vnetDelete)
}

// tunnelNotFound: unknown tunnel IDs answer 404. SOURCED (relies): terraform removes a tunnel
// from state exactly on a 404 read,
// cloudflare/terraform-provider-cloudflare@65783c2:internal/services/zero_trust_tunnel_cloudflared/resource.go#L175.
// The error code and message are UNVERIFIED (not yet recorded).
func tunnelNotFound() response {
	return fail(http.StatusNotFound, 1003, "Tunnel not found")
}

func (c *reqCtx) tunnel() (*tunnel, *response) {
	t, found := c.account.tunnels[c.params["tunnel_id"]]
	if !found {
		r := tunnelNotFound()
		return nil, &r
	}
	return t, nil
}

func tunnelCreate(c *reqCtx) response {
	var req struct {
		Name         string `json:"name"`
		ConfigSrc    string `json:"config_src"`
		TunnelSecret string `json:"tunnel_secret"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.ConfigSrc == "" {
		req.ConfigSrc = "local"
	}
	if req.ConfigSrc != "local" && req.ConfigSrc != "cloudflare" { // spec enum; code UNVERIFIED
		return fail(http.StatusBadRequest, 1001, "config_src must be one of: local, cloudflare")
	}
	// Duplicate names among live tunnels answer 409. SOURCED (relies): cloudflared maps exactly
	// a 409 from this call to "tunnel with name already exists",
	// cloudflare/cloudflared@ad3c6d1:cfapi/tunnel.go#L117-L118. That a soft-deleted tunnel's
	// name can be reused, and the error code/message, are UNVERIFIED (not yet recorded).
	for _, other := range c.account.tunnels {
		if other.Name == req.Name && other.Deleted == nil {
			return fail(http.StatusConflict, 1013, "tunnel with name already exists")
		}
	}
	if req.TunnelSecret == "" {
		req.TunnelSecret = base64.StdEncoding.EncodeToString(randBytes(32))
	}
	t := &tunnel{ID: c.s.ids.next(uuidV4), Name: req.Name, ConfigSrc: req.ConfigSrc, Secret: req.TunnelSecret, Created: c.now, Seq: c.s.nextSeq()}
	inactive := c.now
	t.ConnsInactive = &inactive // 0040: conns_inactive_at == created_at
	c.account.tunnels[t.ID] = t
	ensureDefaultVnet(c)

	m := t.json(c.account.id, c.now)
	// 0040: the create response (not only GET /token) carries secrets.
	m["token"] = t.token(c.account.id)
	// credentials_file is redacted in 0040, so its shape is UNVERIFIED. The keys are those of the
	// credentials file cloudflared reads (SOURCED, tolerates only:
	// cloudflare/cloudflared@ad3c6d1:connection/connection.go#L65-L70, which ignores TunnelName).
	m["credentials_file"] = map[string]any{"AccountTag": c.account.id, "TunnelID": t.ID, "TunnelName": t.Name, "TunnelSecret": t.Secret}
	return ok(m)
}

// ensureDefaultVnet mirrors the side effect seen in 0160: creating the first tunnel makes
// Cloudflare autogenerate an undeletable "default" virtual network.
func ensureDefaultVnet(c *reqCtx) {
	for _, v := range c.account.vnets {
		if v.IsDefault && v.Deleted == nil {
			return
		}
	}
	v := &virtualNetwork{ID: c.s.ids.next(uuidV4), Name: "default", IsDefault: true, Created: c.now, Seq: c.s.nextSeq(),
		Comment: "This network was autogenerated because this account lacked a default one"}
	c.account.vnets[v.ID] = v
}

// unsupportedFilters rejects list filters the emulator does not implement, rather than silently
// returning unfiltered data.
func (c *reqCtx) unsupportedFilters(names ...string) *response {
	for _, n := range names {
		if c.query.Has(n) {
			r := fail(http.StatusBadRequest, 99998, "flarefake: list filter "+n+" is not emulated")
			return &r
		}
	}
	return nil
}

func tunnelList(c *reqCtx) response {
	if r := c.unsupportedFilters("existed_at", "was_active_at", "was_inactive_at"); r != nil {
		return *r
	}
	q := c.query
	var items []*tunnel
	for _, t := range sortedBySeq(c.account.tunnels, func(t *tunnel) int64 { return t.Seq }) {
		switch {
		case q.Get("name") != "" && t.Name != q.Get("name"),
			q.Get("uuid") != "" && t.ID != q.Get("uuid"),
			q.Get("include_prefix") != "" && !strings.HasPrefix(t.Name, q.Get("include_prefix")),
			q.Get("exclude_prefix") != "" && strings.HasPrefix(t.Name, q.Get("exclude_prefix")),
			q.Get("status") != "" && t.status() != q.Get("status"),
			q.Get("is_deleted") == "false" && t.Deleted != nil,
			q.Get("is_deleted") == "true" && t.Deleted == nil:
			continue
		}
		items = append(items, t)
	}
	// 0102: per_page defaults to 1000
	pageItems, page, perPage, _ := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 1000), 1000)
	out := make([]any, 0, len(pageItems))
	for _, t := range pageItems {
		out = append(out, t.json(c.account.id, c.now))
	}
	return okList(out, PageInfo{Count: len(out), Page: intp(page), PerPage: intp(perPage), TotalCount: intp(len(items))})
}

func tunnelGet(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	return ok(t.json(c.account.id, c.now)) // soft-deleted tunnels are still returned (0101)
}

func tunnelDelete(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	if t.connected() { // 0095
		return fail(http.StatusBadRequest, 1022, "This tunnel has active connections. Please stop all cloudflared replicas, or wait a few minutes for connections to close, then try again.")
	}
	// 0099: allowed even while Workers VPC services still reference the tunnel; soft delete.
	if t.Deleted == nil {
		d := c.now
		t.Deleted = &d
	}
	return ok(t.json(c.account.id, c.now))
}

func tunnelToken(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	return ok(t.token(c.account.id)) // 0041: result is a bare string
}

func tunnelConnections(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	out := []any{}
	for _, cl := range t.Clients {
		conns := []any{}
		for _, cn := range cl.Conns {
			conns = append(conns, cn.json(cl))
		}
		out = append(out, map[string]any{ // 0042
			"id": cl.ID, "features": []string{"allow_remote_config", "serialized_headers", "support_datagram_v2", "support_quic_eof", "management_logs"},
			"version": cl.Version, "arch": cl.Arch, "conns": conns, "run_at": tsMicro(cl.RunAt), "ha_status": nil,
		})
	}
	return ok(out)
}

func tunnelCleanupConnections(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	if t.connected() {
		now := c.now
		t.ConnsInactive, t.ConnsActiveAt = &now, nil
	}
	t.Clients = nil
	// result null: UNVERIFIED (not recorded). cloudflared checks only the status
	// (cloudflare/cloudflared@ad3c6d1:cfapi/tunnel.go#L248, tolerates any result), and the
	// spec's tunnel_empty_response accepts no value at all (responseAllowlist).
	return ok(nil)
}

func tunnelConfigGet(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	if t.Deleted != nil { // 0103
		return fail(http.StatusNotFound, 1055, "Configuration for tunnel not found")
	}
	created := t.Created
	if t.ConfigVersion > 0 {
		created = t.ConfigModified
	}
	var cfg any
	if t.Config != nil {
		cfg = t.Config
	}
	return ok(map[string]any{"tunnel_id": t.ID, "version": t.ConfigVersion, "config": cfg, "source": "cloudflare", "created_at": tsMicro(created)}) // 0044, 0046
}

func tunnelConfigPut(c *reqCtx) response {
	t, r := c.tunnel()
	if r != nil {
		return *r
	}
	if t.Deleted != nil {
		return fail(http.StatusNotFound, 1055, "Configuration for tunnel not found") // UNVERIFIED for PUT
	}
	var req struct {
		Config map[string]any `json:"config"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	ingress, _ := req.Config["ingress"].([]any)
	if len(ingress) == 0 { // 0051
		return fail(http.StatusBadRequest, 1056, "Bad Configuration: Validation failed: The config file doesn't contain any ingress rules\n")
	}
	last, _ := ingress[len(ingress)-1].(map[string]any)
	if strings.TrimSpace(asString(last["hostname"])) != "" || strings.TrimSpace(asString(last["path"])) != "" { // 0050
		return fail(http.StatusBadRequest, 1056, "Bad Configuration: Validation failed: The last ingress rule must match all URLs (i.e. it should not have a hostname or path filter)\n")
	}
	if _, has := req.Config["warp-routing"]; !has {
		req.Config["warp-routing"] = map[string]any{"enabled": false} // 0045: defaulted
	}
	// Full replace (0048/0049), version increments.
	t.Config = req.Config
	t.ConfigVersion++
	t.ConfigModified = c.now
	return ok(map[string]any{"tunnel_id": t.ID, "version": t.ConfigVersion, "config": t.Config, "source": "cloudflare", "created_at": tsMicro(t.ConfigModified)})
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func (v *virtualNetwork) json() map[string]any {
	return map[string]any{"id": v.ID, "comment": v.Comment, "name": v.Name, "is_default_network": v.IsDefault,
		"created_at": tsMicro(v.Created), "deleted_at": tsPtr(v.Deleted)}
}

func vnetList(c *reqCtx) response {
	q := c.query
	isDefault := q.Get("is_default_network")
	if isDefault == "" {
		isDefault = q.Get("is_default")
	}
	out := []any{}
	for _, v := range sortedBySeq(c.account.vnets, func(v *virtualNetwork) int64 { return v.Seq }) {
		switch {
		case q.Get("id") != "" && v.ID != q.Get("id"),
			q.Get("name") != "" && v.Name != q.Get("name"),
			isDefault == "true" && !v.IsDefault,
			isDefault == "false" && v.IsDefault,
			q.Get("is_deleted") == "false" && v.Deleted != nil,
			q.Get("is_deleted") == "true" && v.Deleted == nil:
			continue
		}
		out = append(out, v.json())
	}
	// 0160/0164: page-based result_info, per_page defaults to 1000 like tunnels.
	return okList(out, PageInfo{Count: len(out), Page: intp(1), PerPage: intp(1000), TotalCount: intp(len(out))})
}

// vnetCreate: not yet recorded. The response is the virtual network object as listed (0160).
// The body's default flag is is_default_network (SOURCED: cloudflared sends it,
// cloudflare/cloudflared@ad3c6d1:cfapi/virtual_network.go#L19); the spec keeps the deprecated
// is_default as an alias, accepted too. The missing-name error is UNVERIFIED.
func vnetCreate(c *reqCtx) response {
	var req struct {
		Name             string `json:"name"`
		Comment          string `json:"comment"`
		IsDefault        bool   `json:"is_default"`
		IsDefaultNetwork *bool  `json:"is_default_network"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.IsDefaultNetwork != nil {
		req.IsDefault = *req.IsDefaultNetwork
	}
	if req.Name == "" {
		return fail(http.StatusBadRequest, 1001, "name is required")
	}
	if req.IsDefault {
		for _, v := range c.account.vnets {
			if v.IsDefault && v.Deleted == nil {
				v.IsDefault = false
			}
		}
	}
	v := &virtualNetwork{ID: c.s.ids.next(uuidV4), Name: req.Name, Comment: req.Comment, IsDefault: req.IsDefault, Created: c.now, Seq: c.s.nextSeq()}
	c.account.vnets[v.ID] = v
	return ok(v.json())
}

func vnetDelete(c *reqCtx) response {
	v, found := c.account.vnets[c.params["virtual_network_id"]]
	if !found {
		// 404: SOURCED (relies): terraform drops a virtual network from state on a 404 read,
		// cloudflare/terraform-provider-cloudflare@65783c2:internal/services/zero_trust_tunnel_cloudflared_virtual_network/resource.go#L172
		// (GET; assumed for DELETE). Code and message UNVERIFIED.
		return fail(http.StatusNotFound, 1003, "Virtual network not found")
	}
	if v.IsDefault { // 0163
		return fail(http.StatusBadRequest, 1049, "Cannot delete the Virtual Network because: it is the default virtual network")
	}
	d := c.now
	v.Deleted = &d
	// The deleted object is returned: SOURCED (tolerates) cloudflared decodes the delete result
	// as a virtual network, cloudflare/cloudflared@ad3c6d1:cfapi/virtual_network.go#L98-L100.
	// Not recorded, so the exact shape is UNVERIFIED.
	return ok(v.json())
}

// ConnectTunnel simulates cloudflared replicas connecting (each opens `conns` HA connections;
// real cloudflared opens 4, recording 0042). Used by tests via Go or POST /_fake/.../connect.
func (s *Server) ConnectTunnel(accountID, tunnelID string, replicas, conns int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, found := s.accountLocked(accountID).tunnels[tunnelID]
	if !found || t.Deleted != nil {
		return false
	}
	now := s.Clock.Now()
	colos := []string{"sjc01", "lax01", "sjc02", "lax02"}
	for r := 0; r < replicas; r++ {
		cl := &tunnelClient{ID: uuidV4(), Arch: "linux_arm64", Version: "2025.11.1", RunAt: now}
		for i := 0; i < conns; i++ {
			cl.Conns = append(cl.Conns, tunnelConn{ColoName: colos[i%len(colos)], UUID: uuidV4(), Opened: now})
		}
		t.Clients = append(t.Clients, cl)
	}
	t.EverConnected = true
	t.ConnsActiveAt, t.ConnsInactive = &now, nil
	return true
}

// DisconnectTunnel simulates all cloudflared replicas stopping.
func (s *Server) DisconnectTunnel(accountID, tunnelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, found := s.accountLocked(accountID).tunnels[tunnelID]
	if !found {
		return false
	}
	now := s.Clock.Now()
	t.Clients = nil
	t.ConnsActiveAt, t.ConnsInactive = nil, &now // 0097
	return true
}
