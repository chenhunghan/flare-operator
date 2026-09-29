package fake

import (
	"net"
	"net/http"
	"sort"
	"time"
)

// Tunnel IP routes (Zero Trust private networks): /accounts/{account_id}/teamnet/routes.
// Source recordings: 0159, 0222 (the empty account-wide list). The rest follows cloudflared's
// `tunnel route ip add/show/delete` (SOURCED below) and the pinned spec.

type ipRoute struct {
	ID       string
	Network  *net.IPNet
	TunnelID string
	VNetID   string
	Comment  string
	Created  time.Time
	Deleted  *time.Time
	Seq      int64
}

func (s *Server) registerRoutes() {
	base := "/accounts/{account_id}/teamnet/routes"
	s.handle(http.MethodGet, base, routeList)
	s.handle(http.MethodPost, base, routeCreate)
	s.handle(http.MethodGet, base+"/{route_id}", routeGet)
	s.handle(http.MethodDelete, base+"/{route_id}", routeDelete)
	s.handle(http.MethodGet, base+"/ip/{ip}", routeByIP)
}

// json is the route as created (tunnel_route); detailed adds the names that list and lookups
// report (tunnel_teamnet). deleted_at is omitted while the route is live: the spec types it as a
// non-nullable date-time, and cloudflared tolerates both (Route.DeletedAt is a time.Time,
// ip_route.go#L29). Whether the API sends null instead is UNVERIFIED (no route recorded).
func (rt *ipRoute) json() map[string]any {
	m := map[string]any{
		"id": rt.ID, "network": rt.Network.String(), "tunnel_id": rt.TunnelID, "comment": rt.Comment,
		"virtual_network_id": rt.VNetID, "created_at": tsMicro(rt.Created),
	}
	if rt.Deleted != nil {
		m["deleted_at"] = tsMicro(*rt.Deleted)
	}
	return m
}

func (rt *ipRoute) detailed(a *account) map[string]any {
	m := rt.json()
	m["tun_type"] = "cfd_tunnel"
	if t, found := a.tunnels[rt.TunnelID]; found {
		m["tunnel_name"] = t.Name
	}
	if v, found := a.vnets[rt.VNetID]; found {
		m["virtual_network_name"] = v.Name
	}
	return m
}

func routeNotFound() response {
	return fail(http.StatusNotFound, 1003, "Route not found") // UNVERIFIED (not recorded)
}

// parseCIDR accepts a network in CIDR form and returns it canonicalized (host bits cleared, as
// net.ParseCIDR does and as cloudflared sends it, ip_route.go#L75-L88).
func parseCIDR(s string) (*net.IPNet, bool) {
	_, n, err := net.ParseCIDR(s)
	return n, err == nil && n != nil
}

func contains(outer, inner *net.IPNet) bool {
	oOnes, oBits := outer.Mask.Size()
	iOnes, iBits := inner.Mask.Size()
	return oBits == iBits && oOnes <= iOnes && outer.Contains(inner.IP)
}

// routeList: GET …/teamnet/routes. Filters: is_deleted, tun_types, tunnel_id,
// virtual_network_id, route_id, comment, network_subset (routes inside the given network) and
// network_superset (routes containing it), as the spec describes them; cloudflared sends both
// with the same network to find a route by network, SOURCED (relies)
// cloudflare/cloudflared@2026.9.3:cmd/cloudflared/tunnel/subcommand_context_teamnet.go#L46-L65,
// and pages until a short page, cfapi/base_client.go#L146-L162. Page-based result_info without
// total_pages and per_page 1000 by default (0159, 0222). The existed_at filter is not
// emulated. List order is creation order (UNVERIFIED).
func routeList(c *reqCtx) response {
	if r := c.unsupportedFilters("existed_at"); r != nil {
		return *r
	}
	q := c.query
	var subset, superset *net.IPNet
	for _, f := range []struct {
		name string
		into **net.IPNet
	}{{"network_subset", &subset}, {"network_superset", &superset}} {
		if v := q.Get(f.name); v != "" {
			n, valid := parseCIDR(v)
			if !valid {
				return fail(http.StatusBadRequest, 1001, "invalid "+f.name+": "+v) // UNVERIFIED
			}
			*f.into = n
		}
	}
	var items []*ipRoute
	for _, rt := range sortedBySeq(c.account.routes, func(rt *ipRoute) int64 { return rt.Seq }) {
		switch {
		case q.Get("is_deleted") == "true" && rt.Deleted == nil,
			q.Get("is_deleted") == "false" && rt.Deleted != nil, // 0159 lists with is_deleted=false; unset = all (spec)
			q.Get("tun_types") != "" && !containsCSV(q.Get("tun_types"), "cfd_tunnel"),
			q.Get("tunnel_id") != "" && rt.TunnelID != q.Get("tunnel_id"),
			q.Get("virtual_network_id") != "" && rt.VNetID != q.Get("virtual_network_id"),
			q.Get("route_id") != "" && rt.ID != q.Get("route_id"),
			q.Get("comment") != "" && rt.Comment != q.Get("comment"),
			subset != nil && !contains(subset, rt.Network),
			superset != nil && !contains(rt.Network, superset):
			continue
		}
		items = append(items, rt)
	}
	pageItems, page, perPage, _ := paginate(items, c.intQuery("page", 1), c.intQuery("per_page", 1000), 1000)
	out := make([]any, 0, len(pageItems))
	for _, rt := range pageItems {
		out = append(out, rt.detailed(c.account))
	}
	return okList(out, PageInfo{Count: len(out), Page: intp(page), PerPage: intp(perPage), TotalCount: intp(len(items))})
}

// routeCreate: POST …/teamnet/routes {network, tunnel_id, comment, virtual_network_id}, the body
// cloudflared sends, SOURCED (relies) cloudflare/cloudflared@2026.9.3:cfapi/ip_route.go#L75-L88,L160-L175.
// An omitted virtual_network_id means the account's default network, SOURCED (statement)
// ip_route.go#L71. The route answers with its default network's ID filled in (UNVERIFIED).
// Errors for an unknown tunnel or network and for a network already routed in the same virtual
// network are UNVERIFIED (not recorded).
func routeCreate(c *reqCtx) response {
	var req struct {
		Network  string  `json:"network"`
		TunnelID string  `json:"tunnel_id"`
		Comment  string  `json:"comment"`
		VNetID   *string `json:"virtual_network_id"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	n, valid := parseCIDR(req.Network)
	if !valid {
		return fail(http.StatusBadRequest, 1001, "invalid network: "+req.Network) // UNVERIFIED
	}
	if t, found := c.account.tunnels[req.TunnelID]; !found || t.Deleted != nil {
		return fail(http.StatusBadRequest, 1001, "tunnel not found: "+req.TunnelID) // UNVERIFIED
	}
	vnet := ""
	if req.VNetID != nil && *req.VNetID != "" {
		if v, found := c.account.vnets[*req.VNetID]; !found || v.Deleted != nil {
			return fail(http.StatusBadRequest, 1001, "virtual network not found: "+*req.VNetID) // UNVERIFIED
		}
		vnet = *req.VNetID
	} else {
		for _, v := range c.account.vnets {
			if v.IsDefault && v.Deleted == nil {
				vnet = v.ID
			}
		}
	}
	for _, other := range c.account.routes {
		if other.Deleted == nil && other.VNetID == vnet && other.Network.String() == n.String() {
			return fail(http.StatusConflict, 1014, "route for this network already exists in the virtual network") // UNVERIFIED
		}
	}
	rt := &ipRoute{ID: c.s.ids.next(uuidV4), Network: n, TunnelID: req.TunnelID, VNetID: vnet, Comment: req.Comment,
		Created: c.now, Seq: c.s.nextSeq()}
	if c.account.routes == nil {
		c.account.routes = map[string]*ipRoute{}
	}
	c.account.routes[rt.ID] = rt
	return ok(rt.json())
}

// routeGet: GET …/routes/{id} answers the listed shape, deleted routes included (UNVERIFIED:
// not recorded, and cloudflared does not call it).
func routeGet(c *reqCtx) response {
	rt, found := c.account.routes[c.params["route_id"]]
	if !found {
		return routeNotFound()
	}
	return ok(rt.detailed(c.account))
}

// routeDelete soft-deletes and returns the route: cloudflared decodes the result as a Route,
// SOURCED (tolerates) cloudflare/cloudflared@2026.9.3:cfapi/ip_route.go#L177-L194. That it is a
// soft delete (listed with is_deleted=true) is UNVERIFIED, by analogy with tunnels (0101).
func routeDelete(c *reqCtx) response {
	rt, found := c.account.routes[c.params["route_id"]]
	if !found || rt.Deleted != nil {
		return routeNotFound()
	}
	d := c.now
	rt.Deleted = &d
	return ok(rt.json())
}

// routeByIP: GET …/routes/ip/{ip}[?virtual_network_id=] answers the live route with the
// longest prefix containing ip in the (default) virtual network, SOURCED (relies) cloudflared's
// `route ip get` decodes it as a DetailedRoute and treats a zero one as "no route",
// cfapi/ip_route.go#L196-L213. The answer for no match ({} here) is UNVERIFIED.
func routeByIP(c *reqCtx) response {
	ip := net.ParseIP(c.params["ip"])
	if ip == nil {
		return fail(http.StatusBadRequest, 1001, "invalid ip: "+c.params["ip"]) // UNVERIFIED
	}
	vnet := c.query.Get("virtual_network_id")
	if vnet == "" {
		for _, v := range c.account.vnets {
			if v.IsDefault && v.Deleted == nil {
				vnet = v.ID
			}
		}
	}
	var match []*ipRoute
	for _, rt := range c.account.routes {
		if rt.Deleted == nil && rt.VNetID == vnet && rt.Network.Contains(ip) {
			match = append(match, rt)
		}
	}
	if len(match) == 0 {
		return ok(map[string]any{})
	}
	sort.Slice(match, func(i, j int) bool {
		a, _ := match[i].Network.Mask.Size()
		b, _ := match[j].Network.Mask.Size()
		return a > b
	})
	return ok(match[0].detailed(c.account))
}

func containsCSV(csv, want string) bool {
	start := 0
	for i := 0; i <= len(csv); i++ {
		if i == len(csv) || csv[i] == ',' {
			if csv[start:i] == want {
				return true
			}
			start = i + 1
		}
	}
	return false
}
