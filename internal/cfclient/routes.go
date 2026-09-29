package cfclient

//go:generate go run ./internal/genroutes ../..

import (
	"regexp"
	"strings"
	"sync"
)

// OtherRoute is the route template of a path that matches no template of the pinned spec.
const OtherRoute = "other"

// RouteTemplate maps a Request.Path (concrete IDs and names) to the pinned OpenAPI spec's path
// template, e.g. "/accounts/0123…/storage/kv/namespaces/abcd" to
// "/accounts/{account_id}/storage/kv/namespaces/{namespace_id}". Static segments win over
// parameters, as in an HTTP router. A path the spec does not know maps to OtherRoute, so the
// result is always one of a fixed set of strings: safe as a metrics label (no raw IDs, bounded
// cardinality).
func RouteTemplate(p string) string {
	routesOnce.Do(buildRoutes)
	p = strings.Trim(p, "/")
	if p == "" {
		return OtherRoute
	}
	if t := routeRoot.match(strings.Split(p, "/")); t != "" {
		return t
	}
	return OtherRoute
}

type routeNode struct {
	static map[string]*routeNode
	param  *routeNode
	// mixed are segments that combine text and parameters, e.g. "{scan_id}.png".
	mixed    []mixedSegment
	template string // set on nodes that end a template
}

type mixedSegment struct {
	raw  string
	re   *regexp.Regexp
	node *routeNode
}

var segParam = regexp.MustCompile(`\{[^}]*\}`)

func (n *routeNode) mixedChild(s string) *routeNode {
	for _, m := range n.mixed {
		if m.raw == s {
			return m.node
		}
	}
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range segParam.FindAllStringIndex(s, -1) {
		b.WriteString(regexp.QuoteMeta(s[last:loc[0]]))
		b.WriteString(".+")
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(s[last:]))
	b.WriteString("$")
	c := &routeNode{}
	n.mixed = append(n.mixed, mixedSegment{raw: s, re: regexp.MustCompile(b.String()), node: c})
	return c
}

var (
	routesOnce sync.Once
	routeRoot  *routeNode
)

func buildRoutes() {
	routeRoot = &routeNode{}
	for _, t := range specRoutes {
		n := routeRoot
		for _, s := range strings.Split(strings.Trim(t, "/"), "/") {
			if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && strings.Count(s, "{") == 1 {
				if n.param == nil {
					n.param = &routeNode{}
				}
				n = n.param
				continue
			}
			if strings.Contains(s, "{") {
				n = n.mixedChild(s)
				continue
			}
			if n.static == nil {
				n.static = map[string]*routeNode{}
			}
			c := n.static[s]
			if c == nil {
				c = &routeNode{}
				n.static[s] = c
			}
			n = c
		}
		if n.template == "" {
			n.template = t
		}
	}
}

// match returns the template of the best match for segs ("" when none): a static segment is
// tried before a parameter, with backtracking.
func (n *routeNode) match(segs []string) string {
	if len(segs) == 0 {
		return n.template
	}
	if c := n.static[segs[0]]; c != nil {
		if t := c.match(segs[1:]); t != "" {
			return t
		}
	}
	for _, m := range n.mixed {
		if m.re.MatchString(segs[0]) {
			if t := m.node.match(segs[1:]); t != "" {
				return t
			}
		}
	}
	if n.param != nil && segs[0] != "" {
		return n.param.match(segs[1:])
	}
	return ""
}
